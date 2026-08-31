package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/svlocks/sheets/internal/domain"
)

// NodeInput describes a node to create. An empty ID requests a UUIDv7.
type NodeInput struct {
	ID         domain.EntityID
	Labels     []string
	Properties domain.Properties
	Body       string
}

// NodeUpdate replaces only fields whose pointers are non-nil.
type NodeUpdate struct {
	Labels     *[]string
	Properties *domain.Properties
	Body       *string
}

// EdgeInput describes an edge to create. An empty ID requests a UUIDv7.
type EdgeInput struct {
	ID         domain.EntityID
	From       domain.EntityID
	Type       string
	To         domain.EntityID
	Position   *int64
	Properties domain.Properties
}

// EdgeUpdate replaces only selected fields. SetPosition distinguishes leaving
// Position unchanged from explicitly setting it to nil.
type EdgeUpdate struct {
	From        *domain.EntityID
	Type        *string
	To          *domain.EntityID
	SetPosition bool
	Position    *int64
	Properties  *domain.Properties
}

// WriteResult identifies the graph state after a Write callback. Changed is
// false when the callback performed only reads or idempotent updates.
type WriteResult struct {
	Revision domain.Revision
	Changed  bool
	Info     *domain.RevisionInfo
}

// WriteTx is the mutation surface passed to Store.Write. It must not be used
// after its callback returns and is not safe for concurrent use.
type WriteTx struct {
	conn     *sql.Conn
	ctx      context.Context
	store    *Store
	meta     RevisionMeta
	base     domain.Revision
	revision domain.Revision
	info     *domain.RevisionInfo
	done     bool
}

// Write runs fn in one BEGIN IMMEDIATE SQLite transaction. SQLite therefore
// serializes all writers, including writers in other OS processes. A revision
// is allocated lazily on the first effective mutation and at most once.
func (s *Store) Write(ctx context.Context, meta RevisionMeta, fn func(*WriteTx) error) (result WriteResult, err error) {
	if err := s.checkOpen(); err != nil {
		return result, err
	}
	if fn == nil {
		return result, fmt.Errorf("%w: nil write callback", ErrInvalidArgument)
	}
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return result, fmt.Errorf("acquire write connection: %w", err)
	}
	defer func() { err = errors.Join(err, conn.Close()) }()
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return result, fmt.Errorf("begin write: %w", err)
	}
	committed := false
	defer func() {
		if recovered := recover(); recovered != nil {
			_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
			panic(recovered)
		}
		if !committed {
			_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
		}
	}()

	var base int64
	if err := conn.QueryRowContext(ctx, "SELECT COALESCE(MAX(revision), 0) FROM revisions").Scan(&base); err != nil {
		return result, fmt.Errorf("read current revision: %w", err)
	}
	tx := &WriteTx{conn: conn, ctx: ctx, store: s, meta: meta, base: domain.Revision(base)}
	if err := fn(tx); err != nil {
		tx.done = true
		return result, err
	}
	if err := ctx.Err(); err != nil {
		tx.done = true
		return result, err
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		tx.done = true
		return result, fmt.Errorf("commit write: %w", err)
	}
	committed = true
	tx.done = true
	result.Revision = tx.base
	if tx.revision != 0 {
		result.Revision = tx.revision
		result.Changed = true
		info := *tx.info
		result.Info = &info
	}
	return result, nil
}

// CurrentRevision is the revision visible to operations in this transaction.
func (tx *WriteTx) CurrentRevision() domain.Revision {
	if tx.revision != 0 {
		return tx.revision
	}
	return tx.base
}

// Changed reports whether this callback has allocated a revision.
func (tx *WriteTx) Changed() bool { return tx.revision != 0 }

func (tx *WriteTx) checkActive() error {
	if tx == nil || tx.conn == nil || tx.done {
		return errors.New("write transaction is no longer active")
	}
	return nil
}

func (tx *WriteTx) ensureRevision() (domain.Revision, error) {
	if tx.revision != 0 {
		return tx.revision, nil
	}
	if tx.base >= domain.Revision(math.MaxInt64) {
		return 0, errors.New("revision space exhausted")
	}
	when := tx.meta.Time
	if when.IsZero() {
		when = tx.store.clock()
	}
	when = when.UTC()
	var previous sql.NullInt64
	if err := tx.conn.QueryRowContext(tx.ctx, "SELECT committed_ns FROM revisions ORDER BY revision DESC LIMIT 1").Scan(&previous); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf("read previous revision time: %w", err)
	}
	if previous.Valid && when.UnixNano() <= previous.Int64 {
		when = time.Unix(0, previous.Int64+1).UTC()
	}
	revision := tx.base + 1
	if _, err := tx.conn.ExecContext(tx.ctx,
		"INSERT INTO revisions(revision, committed_ns, actor, message) VALUES (?, ?, ?, ?)",
		int64(revision), when.UnixNano(), tx.meta.Actor, tx.meta.Message,
	); err != nil {
		return 0, fmt.Errorf("create revision: %w", err)
	}
	tx.revision = revision
	tx.info = &domain.RevisionInfo{Revision: revision, Time: when, Actor: tx.meta.Actor, Message: tx.meta.Message}
	return revision, nil
}

// GetNode returns the node visible after mutations already made in this Write.
func (tx *WriteTx) GetNode(id domain.EntityID) (domain.Node, error) {
	if err := tx.checkActive(); err != nil {
		return domain.Node{}, err
	}
	return scanNode(tx.conn.QueryRowContext(tx.ctx, `
		SELECT id, labels, properties, body, valid_from, valid_to
		FROM node_versions WHERE id = ? AND valid_to IS NULL`, string(id)))
}

// GetEdge returns the edge visible after mutations already made in this Write.
func (tx *WriteTx) GetEdge(id domain.EntityID) (domain.Edge, error) {
	if err := tx.checkActive(); err != nil {
		return domain.Edge{}, err
	}
	return scanEdge(tx.conn.QueryRowContext(tx.ctx, `
		SELECT id, from_id, type, to_id, position, properties, valid_from, valid_to
		FROM edge_versions WHERE id = ? AND valid_to IS NULL`, string(id)))
}

// CreateNode creates a stable identity and its first version.
func (tx *WriteTx) CreateNode(input NodeInput) (domain.Node, error) {
	if err := tx.checkActive(); err != nil {
		return domain.Node{}, err
	}
	id := input.ID
	if id == "" {
		var err error
		id, err = newUUIDv7()
		if err != nil {
			return domain.Node{}, err
		}
	}
	if err := validateID(id); err != nil {
		return domain.Node{}, err
	}
	labelsData, labels, err := encodeLabels(input.Labels)
	if err != nil {
		return domain.Node{}, fmt.Errorf("encode node labels: %w", err)
	}
	properties, err := encodeProperties(input.Properties)
	if err != nil {
		return domain.Node{}, fmt.Errorf("encode node properties: %w", err)
	}
	var exists int
	err = tx.conn.QueryRowContext(tx.ctx, "SELECT 1 FROM nodes WHERE id = ?", string(id)).Scan(&exists)
	if err == nil {
		return domain.Node{}, fmt.Errorf("%w: node %s already exists", domain.ErrConflict, id)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return domain.Node{}, fmt.Errorf("check node identity: %w", err)
	}
	revision, err := tx.ensureRevision()
	if err != nil {
		return domain.Node{}, err
	}
	if _, err := tx.conn.ExecContext(tx.ctx, "INSERT INTO nodes(id, created_revision) VALUES (?, ?)", string(id), int64(revision)); err != nil {
		return domain.Node{}, mapConflict("create node identity", err)
	}
	if _, err := tx.conn.ExecContext(tx.ctx, `
		INSERT INTO node_versions(id, valid_from, labels, properties, body)
		VALUES (?, ?, ?, ?, ?)`, string(id), int64(revision), labelsData, properties, input.Body); err != nil {
		return domain.Node{}, mapConflict("create node version", err)
	}
	return domain.Node{ID: id, Labels: labels, Properties: cloneProperties(input.Properties), Body: input.Body, ValidFrom: revision}, nil
}

// UpdateNode creates a new version when at least one value actually changes.
func (tx *WriteTx) UpdateNode(id domain.EntityID, update NodeUpdate) (domain.Node, error) {
	current, err := tx.GetNode(id)
	if err != nil {
		return domain.Node{}, err
	}
	next := current
	if update.Labels != nil {
		next.Labels = normalizeLabels(*update.Labels)
	}
	if update.Properties != nil {
		next.Properties = cloneProperties(*update.Properties)
	}
	if update.Body != nil {
		next.Body = *update.Body
	}
	labelsData, normalized, err := encodeLabels(next.Labels)
	if err != nil {
		return domain.Node{}, err
	}
	next.Labels = normalized
	properties, err := encodeProperties(next.Properties)
	if err != nil {
		return domain.Node{}, fmt.Errorf("encode node properties: %w", err)
	}
	currentLabels, _, _ := encodeLabels(current.Labels)
	currentProperties, err := encodeProperties(current.Properties)
	if err != nil {
		return domain.Node{}, err
	}
	if slices.Equal(labelsData, currentLabels) && slices.Equal(properties, currentProperties) && next.Body == current.Body {
		return current, nil
	}
	revision, err := tx.ensureRevision()
	if err != nil {
		return domain.Node{}, err
	}
	if err := tx.replaceNodeVersion(current, next, labelsData, properties, revision); err != nil {
		return domain.Node{}, err
	}
	next.ValidFrom, next.ValidTo = revision, nil
	return next, nil
}

func (tx *WriteTx) replaceNodeVersion(current, next domain.Node, labels, properties []byte, revision domain.Revision) error {
	if current.ValidFrom == revision {
		_, err := tx.conn.ExecContext(tx.ctx,
			"UPDATE node_versions SET labels = ?, properties = ?, body = ? WHERE id = ? AND valid_from = ?",
			labels, properties, next.Body, string(current.ID), int64(revision))
		return err
	}
	if _, err := tx.conn.ExecContext(tx.ctx,
		"UPDATE node_versions SET valid_to = ? WHERE id = ? AND valid_to IS NULL",
		int64(revision), string(current.ID)); err != nil {
		return fmt.Errorf("close node version: %w", err)
	}
	if _, err := tx.conn.ExecContext(tx.ctx, `
		INSERT INTO node_versions(id, valid_from, labels, properties, body)
		VALUES (?, ?, ?, ?, ?)`, string(current.ID), int64(revision), labels, properties, next.Body); err != nil {
		return fmt.Errorf("create node version: %w", err)
	}
	return nil
}

// DeleteNode closes the current node version and all incident current edges.
func (tx *WriteTx) DeleteNode(id domain.EntityID) error {
	current, err := tx.GetNode(id)
	if err != nil {
		return err
	}
	revision, err := tx.ensureRevision()
	if err != nil {
		return err
	}
	rows, err := tx.conn.QueryContext(tx.ctx, `
		SELECT id, from_id, type, to_id, position, properties, valid_from, valid_to
		FROM edge_versions
		WHERE valid_to IS NULL AND (from_id = ? OR to_id = ?)
		ORDER BY id`, string(id), string(id))
	if err != nil {
		return fmt.Errorf("list incident edges: %w", err)
	}
	var incident []domain.Edge
	for rows.Next() {
		edge, scanErr := scanEdge(rows)
		if scanErr != nil {
			return errors.Join(scanErr, rows.Close())
		}
		incident = append(incident, edge)
	}
	if err := rows.Err(); err != nil {
		return errors.Join(err, rows.Close())
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close incident edge rows: %w", err)
	}
	for _, edge := range incident {
		if err := tx.closeEdge(edge, revision); err != nil {
			return err
		}
	}
	if current.ValidFrom == revision {
		created, err := tx.identityCreatedAt("nodes", id)
		if err != nil {
			return err
		}
		if created == revision {
			if _, err := tx.conn.ExecContext(tx.ctx, "DELETE FROM nodes WHERE id = ?", string(id)); err != nil {
				return fmt.Errorf("delete new node: %w", err)
			}
		} else if _, err := tx.conn.ExecContext(tx.ctx,
			"DELETE FROM node_versions WHERE id = ? AND valid_from = ?", string(id), int64(revision)); err != nil {
			return fmt.Errorf("delete in-batch node version: %w", err)
		}
		return nil
	}
	if _, err := tx.conn.ExecContext(tx.ctx,
		"UPDATE node_versions SET valid_to = ? WHERE id = ? AND valid_to IS NULL",
		int64(revision), string(id)); err != nil {
		return fmt.Errorf("delete node: %w", err)
	}
	return nil
}

// CreateEdge creates a stable edge identity and its first version.
func (tx *WriteTx) CreateEdge(input EdgeInput) (domain.Edge, error) {
	if err := tx.checkActive(); err != nil {
		return domain.Edge{}, err
	}
	id := input.ID
	if id == "" {
		var err error
		id, err = newUUIDv7()
		if err != nil {
			return domain.Edge{}, err
		}
	}
	if err := validateID(id); err != nil {
		return domain.Edge{}, err
	}
	properties, err := encodeProperties(input.Properties)
	if err != nil {
		return domain.Edge{}, fmt.Errorf("encode edge properties: %w", err)
	}
	edge := domain.Edge{ID: id, From: input.From, Type: input.Type, To: input.To, Position: cloneInt64(input.Position), Properties: cloneProperties(input.Properties)}
	if err := tx.validateEdge(edge, ""); err != nil {
		return domain.Edge{}, err
	}
	var exists int
	err = tx.conn.QueryRowContext(tx.ctx, "SELECT 1 FROM edges WHERE id = ?", string(id)).Scan(&exists)
	if err == nil {
		return domain.Edge{}, fmt.Errorf("%w: edge %s already exists", domain.ErrConflict, id)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return domain.Edge{}, fmt.Errorf("check edge identity: %w", err)
	}
	revision, err := tx.ensureRevision()
	if err != nil {
		return domain.Edge{}, err
	}
	if _, err := tx.conn.ExecContext(tx.ctx, "INSERT INTO edges(id, created_revision) VALUES (?, ?)", string(id), int64(revision)); err != nil {
		return domain.Edge{}, mapConflict("create edge identity", err)
	}
	if _, err := tx.conn.ExecContext(tx.ctx, `
		INSERT INTO edge_versions(id, valid_from, from_id, type, to_id, position, properties)
		VALUES (?, ?, ?, ?, ?, ?, ?)`, string(id), int64(revision), string(input.From), input.Type, string(input.To), input.Position, properties); err != nil {
		return domain.Edge{}, mapConstraintError("create edge version", err)
	}
	edge.ValidFrom = revision
	return edge, nil
}

// UpdateEdge creates a new version when at least one value actually changes.
func (tx *WriteTx) UpdateEdge(id domain.EntityID, update EdgeUpdate) (domain.Edge, error) {
	current, err := tx.GetEdge(id)
	if err != nil {
		return domain.Edge{}, err
	}
	next := current
	if update.From != nil {
		next.From = *update.From
	}
	if update.Type != nil {
		next.Type = *update.Type
	}
	if update.To != nil {
		next.To = *update.To
	}
	if update.SetPosition {
		next.Position = cloneInt64(update.Position)
	}
	if update.Properties != nil {
		next.Properties = cloneProperties(*update.Properties)
	}
	if err := tx.validateEdge(next, id); err != nil {
		return domain.Edge{}, err
	}
	properties, err := encodeProperties(next.Properties)
	if err != nil {
		return domain.Edge{}, fmt.Errorf("encode edge properties: %w", err)
	}
	currentProperties, err := encodeProperties(current.Properties)
	if err != nil {
		return domain.Edge{}, err
	}
	if current.From == next.From && current.Type == next.Type && current.To == next.To && equalInt64(current.Position, next.Position) && slices.Equal(currentProperties, properties) {
		return current, nil
	}
	revision, err := tx.ensureRevision()
	if err != nil {
		return domain.Edge{}, err
	}
	if current.ValidFrom == revision {
		_, err = tx.conn.ExecContext(tx.ctx, `
			UPDATE edge_versions SET from_id = ?, type = ?, to_id = ?, position = ?, properties = ?
			WHERE id = ? AND valid_from = ?`, string(next.From), next.Type, string(next.To), next.Position, properties, string(id), int64(revision))
	} else {
		if _, err = tx.conn.ExecContext(tx.ctx,
			"UPDATE edge_versions SET valid_to = ? WHERE id = ? AND valid_to IS NULL",
			int64(revision), string(id)); err == nil {
			_, err = tx.conn.ExecContext(tx.ctx, `
				INSERT INTO edge_versions(id, valid_from, from_id, type, to_id, position, properties)
				VALUES (?, ?, ?, ?, ?, ?, ?)`, string(id), int64(revision), string(next.From), next.Type, string(next.To), next.Position, properties)
		}
	}
	if err != nil {
		return domain.Edge{}, mapConstraintError("update edge", err)
	}
	next.ValidFrom, next.ValidTo = revision, nil
	return next, nil
}

// DeleteEdge closes the current edge version.
func (tx *WriteTx) DeleteEdge(id domain.EntityID) error {
	current, err := tx.GetEdge(id)
	if err != nil {
		return err
	}
	revision, err := tx.ensureRevision()
	if err != nil {
		return err
	}
	return tx.closeEdge(current, revision)
}

func (tx *WriteTx) closeEdge(current domain.Edge, revision domain.Revision) error {
	if current.ValidFrom == revision {
		created, err := tx.identityCreatedAt("edges", current.ID)
		if err != nil {
			return err
		}
		if created == revision {
			if _, err := tx.conn.ExecContext(tx.ctx, "DELETE FROM edges WHERE id = ?", string(current.ID)); err != nil {
				return fmt.Errorf("delete new edge: %w", err)
			}
		} else if _, err := tx.conn.ExecContext(tx.ctx,
			"DELETE FROM edge_versions WHERE id = ? AND valid_from = ?", string(current.ID), int64(revision)); err != nil {
			return fmt.Errorf("delete in-batch edge version: %w", err)
		}
		return nil
	}
	if _, err := tx.conn.ExecContext(tx.ctx,
		"UPDATE edge_versions SET valid_to = ? WHERE id = ? AND valid_to IS NULL",
		int64(revision), string(current.ID)); err != nil {
		return fmt.Errorf("delete edge: %w", err)
	}
	return nil
}

func (tx *WriteTx) identityCreatedAt(table string, id domain.EntityID) (domain.Revision, error) {
	if table != "nodes" && table != "edges" {
		return 0, fmt.Errorf("invalid identity table %q", table)
	}
	var revision int64
	if err := tx.conn.QueryRowContext(tx.ctx,
		"SELECT created_revision FROM "+table+" WHERE id = ?", string(id)).Scan(&revision); err != nil {
		return 0, fmt.Errorf("read %s identity: %w", table, err)
	}
	return domain.Revision(revision), nil
}

func (tx *WriteTx) validateEdge(edge domain.Edge, exclude domain.EntityID) error {
	if edge.From == "" || edge.To == "" {
		return &domain.ConstraintError{Constraint: "edge_endpoint", Detail: "source and target IDs are required"}
	}
	if edge.Type == "" {
		return &domain.ConstraintError{Constraint: "edge_type", Detail: "relationship type is required"}
	}
	if edge.Type != "CHILD" && edge.Position != nil {
		return &domain.ConstraintError{Constraint: "edge_position", Detail: "position is only valid on CHILD edges"}
	}
	for role, id := range map[string]domain.EntityID{"source": edge.From, "target": edge.To} {
		var exists int
		err := tx.conn.QueryRowContext(tx.ctx,
			"SELECT 1 FROM node_versions WHERE id = ? AND valid_to IS NULL", string(id)).Scan(&exists)
		if errors.Is(err, sql.ErrNoRows) {
			return &domain.ConstraintError{Constraint: "edge_endpoint", Detail: role + " node " + string(id) + " is not current"}
		}
		if err != nil {
			return fmt.Errorf("check edge %s: %w", role, err)
		}
	}
	if edge.Type != "CHILD" {
		return nil
	}
	var other string
	err := tx.conn.QueryRowContext(tx.ctx, `
		SELECT id FROM edge_versions
		WHERE valid_to IS NULL AND type = 'CHILD' AND to_id = ? AND id <> ?
		LIMIT 1`, string(edge.To), string(exclude)).Scan(&other)
	if err == nil {
		return &domain.ConstraintError{Constraint: "child_parent", Detail: fmt.Sprintf("node %s already has parent edge %s", edge.To, other)}
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("check CHILD parent: %w", err)
	}
	cycle, err := tx.childPathExists(edge.To, edge.From, exclude)
	if err != nil {
		return err
	}
	if cycle {
		return &domain.ConstraintError{Constraint: "child_cycle", Detail: fmt.Sprintf("%s -> %s would form a cycle", edge.From, edge.To)}
	}
	return nil
}

// childPathExists deliberately walks in Go instead of a recursive CTE so
// cycle checks are not capped by SQLite's recursive query depth.
func (tx *WriteTx) childPathExists(start, goal, exclude domain.EntityID) (bool, error) {
	if start == goal {
		return true, nil
	}
	seen := map[domain.EntityID]struct{}{start: {}}
	queue := []domain.EntityID{start}
	for len(queue) > 0 {
		batchSize := min(len(queue), 400)
		batch := append([]domain.EntityID(nil), queue[:batchSize]...)
		queue = queue[batchSize:]
		placeholders := strings.TrimSuffix(strings.Repeat("?,", len(batch)), ",")
		args := make([]any, 0, len(batch)+1)
		for _, id := range batch {
			args = append(args, string(id))
		}
		args = append(args, string(exclude))
		rows, err := tx.conn.QueryContext(tx.ctx, `
			SELECT to_id FROM edge_versions
			WHERE valid_to IS NULL AND type = 'CHILD' AND from_id IN (`+placeholders+`) AND id <> ?`, args...)
		if err != nil {
			return false, fmt.Errorf("traverse CHILD graph: %w", err)
		}
		var found bool
		for rows.Next() {
			var raw string
			if err := rows.Scan(&raw); err != nil {
				return false, errors.Join(err, rows.Close())
			}
			id := domain.EntityID(raw)
			if id == goal {
				found = true
				break
			}
			if _, ok := seen[id]; !ok {
				seen[id] = struct{}{}
				queue = append(queue, id)
			}
		}
		if err := rows.Err(); err != nil {
			return false, errors.Join(err, rows.Close())
		}
		if err := rows.Close(); err != nil {
			return false, fmt.Errorf("close CHILD traversal rows: %w", err)
		}
		if found {
			return true, nil
		}
	}
	return false, nil
}

func newUUIDv7() (domain.EntityID, error) {
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return "", fmt.Errorf("generate entity ID: %w", err)
	}
	millis := uint64(time.Now().UnixMilli())
	id[0] = byte(millis >> 40)
	id[1] = byte(millis >> 32)
	id[2] = byte(millis >> 24)
	id[3] = byte(millis >> 16)
	id[4] = byte(millis >> 8)
	id[5] = byte(millis)
	id[6] = (id[6] & 0x0f) | 0x70
	id[8] = (id[8] & 0x3f) | 0x80
	var text [36]byte
	hex.Encode(text[0:8], id[0:4])
	text[8] = '-'
	hex.Encode(text[9:13], id[4:6])
	text[13] = '-'
	hex.Encode(text[14:18], id[6:8])
	text[18] = '-'
	hex.Encode(text[19:23], id[8:10])
	text[23] = '-'
	hex.Encode(text[24:36], id[10:16])
	return domain.EntityID(text[:]), nil
}

func validateID(id domain.EntityID) error {
	if id == "" || strings.IndexByte(string(id), 0) >= 0 {
		return fmt.Errorf("%w: invalid entity ID", ErrInvalidArgument)
	}
	return nil
}

func equalInt64(a, b *int64) bool {
	return a == nil && b == nil || a != nil && b != nil && *a == *b
}

func cloneInt64(value *int64) *int64 {
	if value == nil {
		return nil
	}
	copyOf := *value
	return &copyOf
}

func cloneProperties(properties domain.Properties) domain.Properties {
	if properties == nil {
		return nil
	}
	data, err := encodeProperties(properties)
	if err != nil {
		return properties
	}
	result, err := decodeProperties(data)
	if err != nil {
		return properties
	}
	return result
}

func mapConflict(operation string, err error) error {
	if strings.Contains(strings.ToLower(err.Error()), "constraint") {
		return fmt.Errorf("%w: %s: %v", domain.ErrConflict, operation, err)
	}
	return fmt.Errorf("%s: %w", operation, err)
}

func mapConstraintError(operation string, err error) error {
	message := strings.ToLower(err.Error())
	if strings.Contains(message, "one_current_child_parent") || strings.Contains(message, "edge_versions.to_id") {
		return &domain.ConstraintError{Constraint: "child_parent", Detail: "target already has a current parent"}
	}
	if strings.Contains(message, "constraint") {
		return fmt.Errorf("%w: %s: %v", domain.ErrConflict, operation, err)
	}
	return fmt.Errorf("%s: %w", operation, err)
}
