package store

import (
	"context"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/svlocks/sheets/internal/domain"
)

const (
	defaultPageSize = 100
	maxPageSize     = 1000
)

// NodeFilter restricts ListNodes. A node must have every requested label.
type NodeFilter struct {
	Labels []string
}

// EdgeFilter restricts ListEdges. Empty Types accepts every type.
type EdgeFilter struct {
	From  *domain.EntityID
	To    *domain.EntityID
	Types []string
}

type rowScanner interface {
	Scan(dest ...any) error
}

type queryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

// CurrentRevision returns the most recently committed revision, or zero for
// an empty graph.
func (s *Store) CurrentRevision(ctx context.Context) (domain.Revision, error) {
	if err := s.checkOpen(); err != nil {
		return 0, err
	}
	var revision int64
	if err := s.db.QueryRowContext(ctx, "SELECT COALESCE(MAX(revision), 0) FROM revisions").Scan(&revision); err != nil {
		return 0, fmt.Errorf("read current revision: %w", err)
	}
	return domain.Revision(revision), nil
}

// ResolveSnapshot validates and resolves a current, revision, or timestamp
// selector to one concrete revision. A timestamp before the first commit
// resolves to revision zero.
func (s *Store) ResolveSnapshot(ctx context.Context, snapshot domain.Snapshot) (domain.Revision, error) {
	if err := s.checkOpen(); err != nil {
		return 0, err
	}
	if snapshot.Revision != nil && snapshot.Time != nil {
		return 0, fmt.Errorf("%w: snapshot has both revision and time", ErrInvalidArgument)
	}
	if snapshot.Revision != nil {
		revision := *snapshot.Revision
		if revision == 0 {
			return 0, nil
		}
		if revision > domain.Revision(^uint64(0)>>1) {
			return 0, fmt.Errorf("%w: revision %d", domain.ErrNotFound, revision)
		}
		var exists int
		err := s.db.QueryRowContext(ctx, "SELECT 1 FROM revisions WHERE revision = ?", int64(revision)).Scan(&exists)
		if errors.Is(err, sql.ErrNoRows) {
			return 0, fmt.Errorf("%w: revision %d", domain.ErrNotFound, revision)
		}
		if err != nil {
			return 0, fmt.Errorf("resolve revision: %w", err)
		}
		return revision, nil
	}
	if snapshot.Time != nil {
		var revision int64
		if err := s.db.QueryRowContext(ctx,
			"SELECT COALESCE(MAX(revision), 0) FROM revisions WHERE committed_ns <= ?",
			snapshot.Time.UnixNano()).Scan(&revision); err != nil {
			return 0, fmt.Errorf("resolve timestamp: %w", err)
		}
		return domain.Revision(revision), nil
	}
	return s.CurrentRevision(ctx)
}

// Revision returns metadata for one committed revision.
func (s *Store) Revision(ctx context.Context, revision domain.Revision) (domain.RevisionInfo, error) {
	if err := s.checkOpen(); err != nil {
		return domain.RevisionInfo{}, err
	}
	if revision == 0 {
		return domain.RevisionInfo{}, fmt.Errorf("%w: revision 0 is the pre-history state", domain.ErrNotFound)
	}
	var raw int64
	var ns int64
	var info domain.RevisionInfo
	err := s.db.QueryRowContext(ctx,
		"SELECT revision, committed_ns, actor, message FROM revisions WHERE revision = ?", int64(revision)).
		Scan(&raw, &ns, &info.Actor, &info.Message)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.RevisionInfo{}, fmt.Errorf("%w: revision %d", domain.ErrNotFound, revision)
	}
	if err != nil {
		return domain.RevisionInfo{}, fmt.Errorf("read revision: %w", err)
	}
	info.Revision = domain.Revision(raw)
	info.Time = timeFromUnixNano(ns)
	return info, nil
}

// ListRevisions returns revision metadata in ascending order.
func (s *Store) ListRevisions(ctx context.Context, page domain.Page) ([]domain.RevisionInfo, domain.PageInfo, error) {
	if err := s.checkOpen(); err != nil {
		return nil, domain.PageInfo{}, err
	}
	limit, err := pageLimit(page.Limit)
	if err != nil {
		return nil, domain.PageInfo{}, err
	}
	var after uint64
	if page.After != "" {
		text, err := decodeCursor(page.After)
		if err != nil {
			return nil, domain.PageInfo{}, err
		}
		after, err = strconv.ParseUint(text, 10, 63)
		if err != nil {
			return nil, domain.PageInfo{}, fmt.Errorf("%w: invalid revision cursor", ErrInvalidArgument)
		}
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT revision, committed_ns, actor, message FROM revisions
		WHERE revision > ? ORDER BY revision LIMIT ?`, int64(after), limit+1)
	if err != nil {
		return nil, domain.PageInfo{}, fmt.Errorf("list revisions: %w", err)
	}
	defer rows.Close()
	infos := make([]domain.RevisionInfo, 0, limit+1)
	for rows.Next() {
		var raw, ns int64
		var info domain.RevisionInfo
		if err := rows.Scan(&raw, &ns, &info.Actor, &info.Message); err != nil {
			return nil, domain.PageInfo{}, err
		}
		info.Revision = domain.Revision(raw)
		info.Time = timeFromUnixNano(ns)
		infos = append(infos, info)
	}
	if err := rows.Err(); err != nil {
		return nil, domain.PageInfo{}, err
	}
	var pageInfo domain.PageInfo
	if len(infos) > limit {
		infos = infos[:limit]
		pageInfo.Next = encodeCursor(strconv.FormatUint(uint64(infos[len(infos)-1].Revision), 10))
	}
	return infos, pageInfo, nil
}

// GetNode reads a node at snapshot.
func (s *Store) GetNode(ctx context.Context, id domain.EntityID, snapshot domain.Snapshot) (domain.Node, error) {
	revision, err := s.ResolveSnapshot(ctx, snapshot)
	if err != nil {
		return domain.Node{}, err
	}
	return scanNode(s.db.QueryRowContext(ctx, `
		SELECT id, labels, properties, body, valid_from, valid_to
		FROM node_versions
		WHERE id = ? AND valid_from <= ? AND (valid_to IS NULL OR valid_to > ?)`,
		string(id), int64(revision), int64(revision)))
}

// GetEdge reads an edge at snapshot.
func (s *Store) GetEdge(ctx context.Context, id domain.EntityID, snapshot domain.Snapshot) (domain.Edge, error) {
	revision, err := s.ResolveSnapshot(ctx, snapshot)
	if err != nil {
		return domain.Edge{}, err
	}
	return scanEdge(s.db.QueryRowContext(ctx, `
		SELECT id, from_id, type, to_id, position, properties, valid_from, valid_to
		FROM edge_versions
		WHERE id = ? AND valid_from <= ? AND (valid_to IS NULL OR valid_to > ?)`,
		string(id), int64(revision), int64(revision)))
}

// ListNodes returns ID-ordered nodes at snapshot using an opaque keyset cursor.
func (s *Store) ListNodes(ctx context.Context, snapshot domain.Snapshot, filter NodeFilter, page domain.Page) ([]domain.Node, domain.PageInfo, error) {
	revision, err := s.ResolveSnapshot(ctx, snapshot)
	if err != nil {
		return nil, domain.PageInfo{}, err
	}
	return listNodes(ctx, s.db, revision, filter, page)
}

// ListEdges returns ID-ordered edges at snapshot using an opaque keyset cursor.
func (s *Store) ListEdges(ctx context.Context, snapshot domain.Snapshot, filter EdgeFilter, page domain.Page) ([]domain.Edge, domain.PageInfo, error) {
	revision, err := s.ResolveSnapshot(ctx, snapshot)
	if err != nil {
		return nil, domain.PageInfo{}, err
	}
	return listEdges(ctx, s.db, revision, filter, page)
}

// ListNodes returns all current nodes visible inside the write transaction.
// Results are stable-sorted by ID and include earlier changes in the callback.
func (tx *WriteTx) ListNodes() ([]domain.Node, error) {
	if err := tx.checkActive(); err != nil {
		return nil, err
	}
	rows, err := tx.conn.QueryContext(tx.ctx, `
		SELECT id, labels, properties, body, valid_from, valid_to
		FROM node_versions WHERE valid_to IS NULL ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("list transaction nodes: %w", err)
	}
	defer rows.Close()
	var nodes []domain.Node
	for rows.Next() {
		node, err := scanNode(rows)
		if err != nil {
			return nil, err
		}
		nodes = append(nodes, node)
	}
	return nodes, rows.Err()
}

// ListEdges returns all current edges visible inside the write transaction.
// Results are stable-sorted by ID and include earlier changes in the callback.
func (tx *WriteTx) ListEdges() ([]domain.Edge, error) {
	if err := tx.checkActive(); err != nil {
		return nil, err
	}
	rows, err := tx.conn.QueryContext(tx.ctx, `
		SELECT id, from_id, type, to_id, position, properties, valid_from, valid_to
		FROM edge_versions WHERE valid_to IS NULL ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("list transaction edges: %w", err)
	}
	defer rows.Close()
	var edges []domain.Edge
	for rows.Next() {
		edge, err := scanEdge(rows)
		if err != nil {
			return nil, err
		}
		edges = append(edges, edge)
	}
	return edges, rows.Err()
}

func listNodes(ctx context.Context, db queryer, revision domain.Revision, filter NodeFilter, page domain.Page) ([]domain.Node, domain.PageInfo, error) {
	limit, err := pageLimit(page.Limit)
	if err != nil {
		return nil, domain.PageInfo{}, err
	}
	after, err := decodeOptionalCursor(page.After)
	if err != nil {
		return nil, domain.PageInfo{}, err
	}
	rows, err := db.QueryContext(ctx, `
		SELECT id, labels, properties, body, valid_from, valid_to
		FROM node_versions
		WHERE valid_from <= ? AND (valid_to IS NULL OR valid_to > ?) AND id > ?
		ORDER BY id`, int64(revision), int64(revision), after)
	if err != nil {
		return nil, domain.PageInfo{}, fmt.Errorf("list nodes: %w", err)
	}
	defer rows.Close()
	wanted := normalizeLabels(filter.Labels)
	nodes := make([]domain.Node, 0, limit+1)
	for rows.Next() {
		node, err := scanNode(rows)
		if err != nil {
			return nil, domain.PageInfo{}, err
		}
		if hasLabels(node.Labels, wanted) {
			nodes = append(nodes, node)
			if len(nodes) > limit {
				break
			}
		}
	}
	if err := rows.Err(); err != nil {
		return nil, domain.PageInfo{}, err
	}
	var pageInfo domain.PageInfo
	if len(nodes) > limit {
		nodes = nodes[:limit]
		pageInfo.Next = encodeCursor(string(nodes[len(nodes)-1].ID))
	}
	return nodes, pageInfo, nil
}

func listEdges(ctx context.Context, db queryer, revision domain.Revision, filter EdgeFilter, page domain.Page) ([]domain.Edge, domain.PageInfo, error) {
	limit, err := pageLimit(page.Limit)
	if err != nil {
		return nil, domain.PageInfo{}, err
	}
	after, err := decodeOptionalCursor(page.After)
	if err != nil {
		return nil, domain.PageInfo{}, err
	}
	query := `SELECT id, from_id, type, to_id, position, properties, valid_from, valid_to
		FROM edge_versions
		WHERE valid_from <= ? AND (valid_to IS NULL OR valid_to > ?) AND id > ?`
	args := []any{int64(revision), int64(revision), after}
	if filter.From != nil {
		query += " AND from_id = ?"
		args = append(args, string(*filter.From))
	}
	if filter.To != nil {
		query += " AND to_id = ?"
		args = append(args, string(*filter.To))
	}
	query += " ORDER BY id"
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, domain.PageInfo{}, fmt.Errorf("list edges: %w", err)
	}
	defer rows.Close()
	types := append([]string(nil), filter.Types...)
	slices.Sort(types)
	edges := make([]domain.Edge, 0, limit+1)
	for rows.Next() {
		edge, err := scanEdge(rows)
		if err != nil {
			return nil, domain.PageInfo{}, err
		}
		if len(types) == 0 || slices.Contains(types, edge.Type) {
			edges = append(edges, edge)
			if len(edges) > limit {
				break
			}
		}
	}
	if err := rows.Err(); err != nil {
		return nil, domain.PageInfo{}, err
	}
	var pageInfo domain.PageInfo
	if len(edges) > limit {
		edges = edges[:limit]
		pageInfo.Next = encodeCursor(string(edges[len(edges)-1].ID))
	}
	return edges, pageInfo, nil
}

func scanNode(row rowScanner) (domain.Node, error) {
	var rawID string
	var labelsData, propertiesData []byte
	var from int64
	var to sql.NullInt64
	var node domain.Node
	if err := row.Scan(&rawID, &labelsData, &propertiesData, &node.Body, &from, &to); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return domain.Node{}, domain.ErrNotFound
		}
		return domain.Node{}, fmt.Errorf("scan node: %w", err)
	}
	labels, err := decodeLabels(labelsData)
	if err != nil {
		return domain.Node{}, err
	}
	properties, err := decodeProperties(propertiesData)
	if err != nil {
		return domain.Node{}, err
	}
	node.ID = domain.EntityID(rawID)
	node.Labels = labels
	node.Properties = properties
	node.ValidFrom = domain.Revision(from)
	if to.Valid {
		value := domain.Revision(to.Int64)
		node.ValidTo = &value
	}
	return node, nil
}

func scanEdge(row rowScanner) (domain.Edge, error) {
	var rawID, fromID, toID string
	var position, to sql.NullInt64
	var propertiesData []byte
	var from int64
	var edge domain.Edge
	if err := row.Scan(&rawID, &fromID, &edge.Type, &toID, &position, &propertiesData, &from, &to); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return domain.Edge{}, domain.ErrNotFound
		}
		return domain.Edge{}, fmt.Errorf("scan edge: %w", err)
	}
	properties, err := decodeProperties(propertiesData)
	if err != nil {
		return domain.Edge{}, err
	}
	edge.ID = domain.EntityID(rawID)
	edge.From = domain.EntityID(fromID)
	edge.To = domain.EntityID(toID)
	edge.Properties = properties
	edge.ValidFrom = domain.Revision(from)
	if position.Valid {
		edge.Position = cloneInt64(&position.Int64)
	}
	if to.Valid {
		value := domain.Revision(to.Int64)
		edge.ValidTo = &value
	}
	return edge, nil
}

func pageLimit(limit int) (int, error) {
	if limit < 0 || limit > maxPageSize {
		return 0, fmt.Errorf("%w: page limit must be between 0 and %d", ErrInvalidArgument, maxPageSize)
	}
	if limit == 0 {
		return defaultPageSize, nil
	}
	return limit, nil
}

func hasLabels(labels, wanted []string) bool {
	for _, label := range wanted {
		if !slices.Contains(labels, label) {
			return false
		}
	}
	return true
}

func encodeCursor(value string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(value))
}

func decodeOptionalCursor(cursor string) (string, error) {
	if cursor == "" {
		return "", nil
	}
	return decodeCursor(cursor)
}

func decodeCursor(cursor string) (string, error) {
	data, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil || strings.IndexByte(string(data), 0) >= 0 {
		return "", fmt.Errorf("%w: invalid page cursor", ErrInvalidArgument)
	}
	return string(data), nil
}

func timeFromUnixNano(ns int64) time.Time { return time.Unix(0, ns).UTC() }
