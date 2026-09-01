package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/svlocks/sheets/internal/domain"
)

const (
	maxPredicateTerms = 256
	maxPredicateIDs   = 100_000
	directSetTerms    = 64
	maxDirectJoins    = 60
)

// NodePredicate is a storage-native, Cypher-independent node predicate. IDs,
// labels, and exact top-level scalar properties are index-backed. Composite
// property values remain correct but require residual evaluation.
type NodePredicate struct {
	IDs        []domain.EntityID
	AllLabels  []string
	Properties domain.Properties
}

// EdgePredicate is a storage-native, Cypher-independent edge predicate. Every
// non-empty field is conjunctive; values within one field are alternatives.
type EdgePredicate struct {
	IDs        []domain.EntityID
	FromIDs    []domain.EntityID
	ToIDs      []domain.EntityID
	Types      []string
	Properties domain.Properties
}

// GraphReader is the common graph-read surface for immutable ReadViews and an
// active WriteTx. It deliberately describes graph primitives rather than query
// language syntax.
type GraphReader interface {
	Revision() domain.Revision
	ScanNodes(context.Context, NodePredicate, domain.Page) ([]domain.Node, domain.PageInfo, error)
	ScanEdges(context.Context, EdgePredicate, domain.Page) ([]domain.Edge, domain.PageInfo, error)
	GetNodes(context.Context, []domain.EntityID) ([]domain.Node, error)
	GetEdges(context.Context, []domain.EntityID) ([]domain.Edge, error)
	CountNodes(context.Context, NodePredicate) (uint64, error)
	CountEdges(context.Context, EdgePredicate) (uint64, error)
}

// ReadView binds all reads and cursors to one immutable exact revision. It
// holds no SQLite transaction or connection and is safe for concurrent use.
type ReadView struct {
	store    *Store
	revision domain.Revision
}

var _ GraphReader = (*ReadView)(nil)
var _ GraphReader = (*WriteTx)(nil)

// View resolves snapshot once and returns a reusable immutable graph reader.
func (s *Store) View(ctx context.Context, snapshot domain.Snapshot) (*ReadView, error) {
	if ctx == nil {
		return nil, fmt.Errorf("%w: nil context", ErrInvalidArgument)
	}
	revision, err := s.ResolveSnapshot(ctx, snapshot)
	if err != nil {
		return nil, err
	}
	return &ReadView{store: s, revision: revision}, nil
}

// Revision returns the exact revision bound to this view.
func (v *ReadView) Revision() domain.Revision {
	if v == nil {
		return 0
	}
	return v.revision
}

// Revision returns the graph state visible inside this write transaction.
func (tx *WriteTx) Revision() domain.Revision { return tx.CurrentRevision() }

func (v *ReadView) queryer() (queryer, error) {
	if v == nil || v.store == nil {
		return nil, ErrClosed
	}
	if err := v.store.checkOpen(); err != nil {
		return nil, err
	}
	return v.store.db, nil
}

func (tx *WriteTx) queryer() (queryer, error) {
	if err := tx.checkActive(); err != nil {
		return nil, err
	}
	return tx.conn, nil
}

func (v *ReadView) ScanNodes(ctx context.Context, predicate NodePredicate, page domain.Page) ([]domain.Node, domain.PageInfo, error) {
	db, err := v.queryer()
	if err != nil {
		return nil, domain.PageInfo{}, err
	}
	return scanNodes(ctx, db, v.revision, predicate, page)
}

func (tx *WriteTx) ScanNodes(ctx context.Context, predicate NodePredicate, page domain.Page) ([]domain.Node, domain.PageInfo, error) {
	db, err := tx.queryer()
	if err != nil {
		return nil, domain.PageInfo{}, err
	}
	return scanNodes(ctx, db, tx.CurrentRevision(), predicate, page)
}

func (v *ReadView) ScanEdges(ctx context.Context, predicate EdgePredicate, page domain.Page) ([]domain.Edge, domain.PageInfo, error) {
	db, err := v.queryer()
	if err != nil {
		return nil, domain.PageInfo{}, err
	}
	return scanEdges(ctx, db, v.revision, predicate, page)
}

func (tx *WriteTx) ScanEdges(ctx context.Context, predicate EdgePredicate, page domain.Page) ([]domain.Edge, domain.PageInfo, error) {
	db, err := tx.queryer()
	if err != nil {
		return nil, domain.PageInfo{}, err
	}
	return scanEdges(ctx, db, tx.CurrentRevision(), predicate, page)
}

func (v *ReadView) GetNodes(ctx context.Context, ids []domain.EntityID) ([]domain.Node, error) {
	db, err := v.queryer()
	if err != nil {
		return nil, err
	}
	return getNodes(ctx, db, v.revision, ids)
}

func (tx *WriteTx) GetNodes(ctx context.Context, ids []domain.EntityID) ([]domain.Node, error) {
	db, err := tx.queryer()
	if err != nil {
		return nil, err
	}
	return getNodes(ctx, db, tx.CurrentRevision(), ids)
}

func (v *ReadView) GetEdges(ctx context.Context, ids []domain.EntityID) ([]domain.Edge, error) {
	db, err := v.queryer()
	if err != nil {
		return nil, err
	}
	return getEdges(ctx, db, v.revision, ids)
}

func (tx *WriteTx) GetEdges(ctx context.Context, ids []domain.EntityID) ([]domain.Edge, error) {
	db, err := tx.queryer()
	if err != nil {
		return nil, err
	}
	return getEdges(ctx, db, tx.CurrentRevision(), ids)
}

func (v *ReadView) CountNodes(ctx context.Context, predicate NodePredicate) (uint64, error) {
	db, err := v.queryer()
	if err != nil {
		return 0, err
	}
	return countNodes(ctx, db, v.revision, predicate)
}

func (tx *WriteTx) CountNodes(ctx context.Context, predicate NodePredicate) (uint64, error) {
	db, err := tx.queryer()
	if err != nil {
		return 0, err
	}
	return countNodes(ctx, db, tx.CurrentRevision(), predicate)
}

func (v *ReadView) CountEdges(ctx context.Context, predicate EdgePredicate) (uint64, error) {
	db, err := v.queryer()
	if err != nil {
		return 0, err
	}
	return countEdges(ctx, db, v.revision, predicate)
}

func (tx *WriteTx) CountEdges(ctx context.Context, predicate EdgePredicate) (uint64, error) {
	db, err := tx.queryer()
	if err != nil {
		return 0, err
	}
	return countEdges(ctx, db, tx.CurrentRevision(), predicate)
}

type compiledProperties struct {
	exact       map[string][]byte
	scalars     []indexedProperty
	hasResidual bool
}

type nodePlan struct {
	ids         []domain.EntityID
	labels      []string
	properties  compiledProperties
	fingerprint string
}

type edgePlan struct {
	ids, from, to []domain.EntityID
	types         []string
	properties    compiledProperties
	fingerprint   string
}

func compileNodePredicate(predicate NodePredicate) (nodePlan, error) {
	ids, err := normalizeEntityIDs(predicate.IDs)
	if err != nil {
		return nodePlan{}, err
	}
	_, labels, err := encodeLabels(predicate.AllLabels)
	if err != nil {
		return nodePlan{}, fmt.Errorf("%w: node labels: %v", ErrInvalidArgument, err)
	}
	if err := checkTermCount("node labels", len(labels)); err != nil {
		return nodePlan{}, err
	}
	properties, err := compileProperties(predicate.Properties)
	if err != nil {
		return nodePlan{}, err
	}
	spec := struct {
		IDs        []domain.EntityID `json:"ids,omitempty"`
		Labels     []string          `json:"labels,omitempty"`
		Properties map[string]string `json:"properties,omitempty"`
	}{ids, labels, encodedPropertyStrings(properties.exact)}
	fingerprint, err := predicateFingerprint(spec)
	if err != nil {
		return nodePlan{}, err
	}
	return nodePlan{ids: ids, labels: labels, properties: properties, fingerprint: fingerprint}, nil
}

func compileEdgePredicate(predicate EdgePredicate) (edgePlan, error) {
	ids, err := normalizeEntityIDs(predicate.IDs)
	if err != nil {
		return edgePlan{}, err
	}
	from, err := normalizeEntityIDs(predicate.FromIDs)
	if err != nil {
		return edgePlan{}, err
	}
	to, err := normalizeEntityIDs(predicate.ToIDs)
	if err != nil {
		return edgePlan{}, err
	}
	types := normalizeStrings(predicate.Types)
	if err := checkTermCount("edge types", len(types)); err != nil {
		return edgePlan{}, err
	}
	for _, edgeType := range types {
		if err := domain.ValidateTextWithoutNUL("relationship type", edgeType, domain.MaxRelationshipTypeBytes); err != nil {
			return edgePlan{}, fmt.Errorf("%w: %w", ErrInvalidArgument, err)
		}
	}
	properties, err := compileProperties(predicate.Properties)
	if err != nil {
		return edgePlan{}, err
	}
	spec := struct {
		IDs        []domain.EntityID `json:"ids,omitempty"`
		From       []domain.EntityID `json:"from,omitempty"`
		To         []domain.EntityID `json:"to,omitempty"`
		Types      []string          `json:"types,omitempty"`
		Properties map[string]string `json:"properties,omitempty"`
	}{ids, from, to, types, encodedPropertyStrings(properties.exact)}
	fingerprint, err := predicateFingerprint(spec)
	if err != nil {
		return edgePlan{}, err
	}
	return edgePlan{ids: ids, from: from, to: to, types: types, properties: properties, fingerprint: fingerprint}, nil
}

func compileProperties(properties domain.Properties) (compiledProperties, error) {
	if len(properties) > maxPredicateTerms {
		return compiledProperties{}, fmt.Errorf("%w: property predicate exceeds %d terms", ErrInvalidArgument, maxPredicateTerms)
	}
	result := compiledProperties{exact: make(map[string][]byte, len(properties))}
	keys := make([]string, 0, len(properties))
	for key := range properties {
		// SQLite TEXT and JSON object keys both preserve embedded NUL bytes.  Do
		// not reject an otherwise valid schema-free property key here: writers
		// and residual comparison support it, and bound SQL parameters avoid any
		// C-string ambiguity.
		if err := domain.ValidateText("property key", key, domain.MaxPropertyKeyBytes); err != nil {
			return compiledProperties{}, fmt.Errorf("%w: %w", ErrInvalidArgument, err)
		}
		keys = append(keys, key)
	}
	slices.Sort(keys)
	for _, key := range keys {
		state := encodeState{visiting: make(map[encodeReference]struct{})}
		value, err := state.encodeValue(properties[key], 0)
		if err != nil {
			return compiledProperties{}, fmt.Errorf("%w: property %q: %w", ErrInvalidArgument, key, err)
		}
		encodedSize := canonicalEncodedValueSize(value)
		if encodedSize > int64(maxPropertyBytes) {
			limitErr := &domain.ResourceLimitError{
				Field: "encoded property predicate", Unit: "bytes",
				Limit: maxPropertyBytes, Actual: int(encodedSize),
			}
			return compiledProperties{}, fmt.Errorf("%w: property %q: %w", ErrInvalidArgument, key, limitErr)
		}
		data, err := json.Marshal(value)
		if err != nil {
			return compiledProperties{}, fmt.Errorf("%w: property %q: %v", ErrInvalidArgument, key, err)
		}
		if len(data) > maxPropertyBytes {
			return compiledProperties{}, fmt.Errorf("%w: property %q exceeds %d encoded bytes", ErrInvalidArgument, key, maxPropertyBytes)
		}
		result.exact[key] = data
		if value.Kind == "map" || value.Kind == "list" {
			result.hasResidual = true
			continue
		}
		result.scalars = append(result.scalars, indexedProperty{key: key, kind: value.Kind, value: data})
	}
	return result, nil
}

func scanNodes(ctx context.Context, db queryer, revision domain.Revision, predicate NodePredicate, page domain.Page) ([]domain.Node, domain.PageInfo, error) {
	if ctx == nil {
		return nil, domain.PageInfo{}, fmt.Errorf("%w: nil context", ErrInvalidArgument)
	}
	limit, err := pageLimit(page.Limit)
	if err != nil {
		return nil, domain.PageInfo{}, err
	}
	plan, err := compileNodePredicate(predicate)
	if err != nil {
		return nil, domain.PageInfo{}, err
	}
	after, err := decodeGraphCursor(page.After, "nodes", revision, plan.fingerprint)
	if err != nil {
		return nil, domain.PageInfo{}, err
	}
	query, args, err := nodeSQL(plan, revision, after, !plan.properties.hasResidual, limit+1, false)
	if err != nil {
		return nil, domain.PageInfo{}, err
	}
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, domain.PageInfo{}, fmt.Errorf("scan nodes: %w", err)
	}
	defer func() { _ = rows.Close() }()
	nodes := make([]domain.Node, 0, limit+1)
	for rows.Next() {
		node, err := scanNode(rows)
		if err != nil {
			return nil, domain.PageInfo{}, err
		}
		if matchesProperties(node.Properties, plan.properties.exact) {
			nodes = append(nodes, node)
			if len(nodes) > limit {
				break
			}
		}
	}
	if err := rows.Err(); err != nil {
		return nil, domain.PageInfo{}, err
	}
	if err := rows.Close(); err != nil {
		return nil, domain.PageInfo{}, fmt.Errorf("close node scan: %w", err)
	}
	var info domain.PageInfo
	if len(nodes) > limit {
		nodes = nodes[:limit]
		info.Next = encodeGraphCursor("nodes", revision, plan.fingerprint, string(nodes[len(nodes)-1].ID))
	}
	return nodes, info, nil
}

func scanEdges(ctx context.Context, db queryer, revision domain.Revision, predicate EdgePredicate, page domain.Page) ([]domain.Edge, domain.PageInfo, error) {
	if ctx == nil {
		return nil, domain.PageInfo{}, fmt.Errorf("%w: nil context", ErrInvalidArgument)
	}
	limit, err := pageLimit(page.Limit)
	if err != nil {
		return nil, domain.PageInfo{}, err
	}
	plan, err := compileEdgePredicate(predicate)
	if err != nil {
		return nil, domain.PageInfo{}, err
	}
	after, err := decodeGraphCursor(page.After, "edges", revision, plan.fingerprint)
	if err != nil {
		return nil, domain.PageInfo{}, err
	}
	query, args, err := edgeSQL(plan, revision, after, !plan.properties.hasResidual, limit+1, false)
	if err != nil {
		return nil, domain.PageInfo{}, err
	}
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, domain.PageInfo{}, fmt.Errorf("scan edges: %w", err)
	}
	defer func() { _ = rows.Close() }()
	edges := make([]domain.Edge, 0, limit+1)
	for rows.Next() {
		edge, err := scanEdge(rows)
		if err != nil {
			return nil, domain.PageInfo{}, err
		}
		if matchesProperties(edge.Properties, plan.properties.exact) {
			edges = append(edges, edge)
			if len(edges) > limit {
				break
			}
		}
	}
	if err := rows.Err(); err != nil {
		return nil, domain.PageInfo{}, err
	}
	if err := rows.Close(); err != nil {
		return nil, domain.PageInfo{}, fmt.Errorf("close edge scan: %w", err)
	}
	var info domain.PageInfo
	if len(edges) > limit {
		edges = edges[:limit]
		info.Next = encodeGraphCursor("edges", revision, plan.fingerprint, string(edges[len(edges)-1].ID))
	}
	return edges, info, nil
}

func countNodes(ctx context.Context, db queryer, revision domain.Revision, predicate NodePredicate) (uint64, error) {
	if ctx == nil {
		return 0, fmt.Errorf("%w: nil context", ErrInvalidArgument)
	}
	plan, err := compileNodePredicate(predicate)
	if err != nil {
		return 0, err
	}
	if !plan.properties.hasResidual {
		query, args, err := nodeSQL(plan, revision, "", false, 0, true)
		if err != nil {
			return 0, err
		}
		rows, err := db.QueryContext(ctx, query, args...)
		if err != nil {
			return 0, fmt.Errorf("count nodes: %w", err)
		}
		defer func() { _ = rows.Close() }()
		if !rows.Next() {
			return 0, errors.Join(sql.ErrNoRows, rows.Err())
		}
		var count int64
		if err := rows.Scan(&count); err != nil {
			return 0, fmt.Errorf("count nodes: %w", err)
		}
		return uint64(count), rows.Close()
	}
	return countNodeResidual(ctx, db, revision, plan)
}

func countEdges(ctx context.Context, db queryer, revision domain.Revision, predicate EdgePredicate) (uint64, error) {
	if ctx == nil {
		return 0, fmt.Errorf("%w: nil context", ErrInvalidArgument)
	}
	plan, err := compileEdgePredicate(predicate)
	if err != nil {
		return 0, err
	}
	if !plan.properties.hasResidual {
		query, args, err := edgeSQL(plan, revision, "", false, 0, true)
		if err != nil {
			return 0, err
		}
		rows, err := db.QueryContext(ctx, query, args...)
		if err != nil {
			return 0, fmt.Errorf("count edges: %w", err)
		}
		defer func() { _ = rows.Close() }()
		if !rows.Next() {
			return 0, errors.Join(sql.ErrNoRows, rows.Err())
		}
		var count int64
		if err := rows.Scan(&count); err != nil {
			return 0, fmt.Errorf("count edges: %w", err)
		}
		return uint64(count), rows.Close()
	}
	return countEdgeResidual(ctx, db, revision, plan)
}

func countNodeResidual(ctx context.Context, db queryer, revision domain.Revision, plan nodePlan) (uint64, error) {
	query, args, err := nodeSQL(plan, revision, "", false, 0, false)
	if err != nil {
		return 0, err
	}
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return 0, fmt.Errorf("count nodes: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var count uint64
	for rows.Next() {
		node, err := scanNode(rows)
		if err != nil {
			return 0, err
		}
		if matchesProperties(node.Properties, plan.properties.exact) {
			count++
		}
	}
	return count, rows.Err()
}

func countEdgeResidual(ctx context.Context, db queryer, revision domain.Revision, plan edgePlan) (uint64, error) {
	query, args, err := edgeSQL(plan, revision, "", false, 0, false)
	if err != nil {
		return 0, err
	}
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return 0, fmt.Errorf("count edges: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var count uint64
	for rows.Next() {
		edge, err := scanEdge(rows)
		if err != nil {
			return 0, err
		}
		if matchesProperties(edge.Properties, plan.properties.exact) {
			count++
		}
	}
	return count, rows.Err()
}

func nodeSQL(plan nodePlan, revision domain.Revision, after string, bounded bool, limit int, count bool) (string, []any, error) {
	columns := "nv.id, nv.labels, nv.properties, nv.body, nv.valid_from, nv.valid_to"
	if count {
		columns = "COUNT(*)"
	}
	query := "SELECT " + columns + " FROM node_versions AS nv"
	args := make([]any, 0, 3+len(plan.labels)+len(plan.properties.scalars)*3)
	if len(plan.labels)+len(plan.properties.scalars) <= maxDirectJoins {
		for index, label := range plan.labels {
			alias := fmt.Sprintf("label_%d", index)
			query += " JOIN node_version_labels AS " + alias +
				" ON " + alias + ".id = nv.id AND " + alias + ".valid_from = nv.valid_from AND " + alias + ".label = ?"
			args = append(args, label)
		}
		for index, property := range plan.properties.scalars {
			alias := fmt.Sprintf("property_%d", index)
			query += " JOIN node_property_index AS " + alias +
				" ON " + alias + ".id = nv.id AND " + alias + ".valid_from = nv.valid_from" +
				" AND " + alias + ".key = ? AND " + alias + ".kind = ? AND " + alias + ".value = ?"
			args = append(args, property.key, property.kind, property.value)
		}
	} else {
		query, args = appendGroupedLabels(query, args, "nv", plan.labels)
		query, args = appendGroupedProperties(query, args, "nv", "node_property_index", plan.properties.scalars)
	}
	query += " WHERE nv.valid_from <= ? AND (nv.valid_to IS NULL OR nv.valid_to > ?)"
	args = append(args, int64(revision), int64(revision))
	var err error
	if !count {
		query += " AND nv.id > ?"
		args = append(args, after)
	}
	if len(plan.ids) > 0 {
		query, args, err = appendEntitySet(query, args, "nv.id", plan.ids)
		if err != nil {
			return "", nil, err
		}
	}
	if !count {
		query += " ORDER BY nv.id"
		if bounded {
			query += " LIMIT ?"
			args = append(args, limit)
		}
	}
	return query, args, nil
}

func edgeSQL(plan edgePlan, revision domain.Revision, after string, bounded bool, limit int, count bool) (string, []any, error) {
	columns := "ev.id, ev.from_id, ev.type, ev.to_id, ev.position, ev.properties, ev.valid_from, ev.valid_to"
	if count {
		columns = "COUNT(*)"
	}
	query := "SELECT " + columns + " FROM edge_versions AS ev"
	args := make([]any, 0, 3+len(plan.properties.scalars)*3)
	if len(plan.properties.scalars) <= maxDirectJoins {
		for index, property := range plan.properties.scalars {
			alias := fmt.Sprintf("property_%d", index)
			query += " JOIN edge_property_index AS " + alias +
				" ON " + alias + ".id = ev.id AND " + alias + ".valid_from = ev.valid_from" +
				" AND " + alias + ".key = ? AND " + alias + ".kind = ? AND " + alias + ".value = ?"
			args = append(args, property.key, property.kind, property.value)
		}
	} else {
		query, args = appendGroupedProperties(query, args, "ev", "edge_property_index", plan.properties.scalars)
	}
	query += " WHERE ev.valid_from <= ? AND (ev.valid_to IS NULL OR ev.valid_to > ?)"
	args = append(args, int64(revision), int64(revision))
	var err error
	if !count {
		query += " AND ev.id > ?"
		args = append(args, after)
	}
	fields := []struct {
		column string
		ids    []domain.EntityID
	}{{"ev.id", plan.ids}, {"ev.from_id", plan.from}, {"ev.to_id", plan.to}}
	for _, field := range fields {
		if len(field.ids) == 0 {
			continue
		}
		query, args, err = appendEntitySet(query, args, field.column, field.ids)
		if err != nil {
			return "", nil, err
		}
	}
	if len(plan.types) > 0 {
		query, args, err = appendStringSet(query, args, "ev.type", plan.types)
		if err != nil {
			return "", nil, err
		}
	}
	if !count {
		query += " ORDER BY ev.id"
		if bounded {
			query += " LIMIT ?"
			args = append(args, limit)
		}
	}
	return query, args, nil
}

// SQLite limits one SELECT to 64 joined tables. Small predicates use one
// selective index join per term so its planner can choose the best driver.
// Large predicates collapse each family into one indexed OR scan followed by
// an exact intersection, preserving the advertised predicate bound without
// exposing SQLite's join-table limit to callers.
func appendGroupedLabels(query string, args []any, entityAlias string, labels []string) (string, []any) {
	if len(labels) == 0 {
		return query, args
	}
	query += " JOIN (SELECT id, valid_from FROM node_version_labels WHERE label IN (" +
		strings.TrimSuffix(strings.Repeat("?,", len(labels)), ",") +
		") GROUP BY id, valid_from HAVING COUNT(*) = ?) AS matched_labels" +
		" ON matched_labels.id = " + entityAlias + ".id" +
		" AND matched_labels.valid_from = " + entityAlias + ".valid_from"
	for _, label := range labels {
		args = append(args, label)
	}
	return query, append(args, len(labels))
}

func appendGroupedProperties(query string, args []any, entityAlias, table string, properties []indexedProperty) (string, []any) {
	if len(properties) == 0 {
		return query, args
	}
	query += " JOIN (SELECT id, valid_from FROM " + table + " WHERE ("
	for index, property := range properties {
		if index > 0 {
			query += " OR "
		}
		query += "(key = ? AND kind = ? AND value = ?)"
		args = append(args, property.key, property.kind, property.value)
	}
	query += ") GROUP BY id, valid_from HAVING COUNT(*) = ?) AS matched_properties" +
		" ON matched_properties.id = " + entityAlias + ".id" +
		" AND matched_properties.valid_from = " + entityAlias + ".valid_from"
	return query, append(args, len(properties))
}

func getNodes(ctx context.Context, db queryer, revision domain.Revision, ids []domain.EntityID) ([]domain.Node, error) {
	if ctx == nil {
		return nil, fmt.Errorf("%w: nil context", ErrInvalidArgument)
	}
	ids, err := normalizeEntityIDs(ids)
	if err != nil || len(ids) == 0 {
		return nil, err
	}
	query := `SELECT nv.id, nv.labels, nv.properties, nv.body, nv.valid_from, nv.valid_to
		FROM node_versions AS nv
		WHERE nv.valid_from <= ? AND (nv.valid_to IS NULL OR nv.valid_to > ?)`
	args := []any{int64(revision), int64(revision)}
	query, args, err = appendEntitySet(query, args, "nv.id", ids)
	if err != nil {
		return nil, err
	}
	query += " ORDER BY nv.id"
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("get nodes: %w", err)
	}
	defer func() { _ = rows.Close() }()
	result := make([]domain.Node, 0, len(ids))
	for rows.Next() {
		node, err := scanNode(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, node)
	}
	return result, rows.Err()
}

func getEdges(ctx context.Context, db queryer, revision domain.Revision, ids []domain.EntityID) ([]domain.Edge, error) {
	if ctx == nil {
		return nil, fmt.Errorf("%w: nil context", ErrInvalidArgument)
	}
	ids, err := normalizeEntityIDs(ids)
	if err != nil || len(ids) == 0 {
		return nil, err
	}
	query := `SELECT ev.id, ev.from_id, ev.type, ev.to_id, ev.position, ev.properties, ev.valid_from, ev.valid_to
		FROM edge_versions AS ev
		WHERE ev.valid_from <= ? AND (ev.valid_to IS NULL OR ev.valid_to > ?)`
	args := []any{int64(revision), int64(revision)}
	query, args, err = appendEntitySet(query, args, "ev.id", ids)
	if err != nil {
		return nil, err
	}
	query += " ORDER BY ev.id"
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("get edges: %w", err)
	}
	defer func() { _ = rows.Close() }()
	result := make([]domain.Edge, 0, len(ids))
	for rows.Next() {
		edge, err := scanEdge(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, edge)
	}
	return result, rows.Err()
}

func matchesProperties(actual domain.Properties, exact map[string][]byte) bool {
	for key, wanted := range exact {
		value, exists := actual[key]
		if !exists {
			return false
		}
		state := encodeState{visiting: make(map[encodeReference]struct{})}
		encoded, err := state.encodeValue(value, 0)
		if err != nil {
			return false
		}
		data, err := json.Marshal(encoded)
		if err != nil || !bytes.Equal(data, wanted) {
			return false
		}
	}
	return true
}

func normalizeEntityIDs(values []domain.EntityID) ([]domain.EntityID, error) {
	if len(values) > maxPredicateIDs {
		return nil, fmt.Errorf("%w: entity IDs exceed %d terms", ErrInvalidArgument, maxPredicateIDs)
	}
	result := append([]domain.EntityID(nil), values...)
	for _, id := range result {
		if !utf8.ValidString(string(id)) || strings.IndexByte(string(id), 0) >= 0 {
			return nil, fmt.Errorf("%w: entity ID is not valid UTF-8", ErrInvalidArgument)
		}
	}
	slices.Sort(result)
	return slices.Compact(result), nil
}

func normalizeStrings(values []string) []string {
	result := append([]string(nil), values...)
	slices.Sort(result)
	return slices.Compact(result)
}

func checkTermCount(name string, count int) error {
	if count > maxPredicateTerms {
		return fmt.Errorf("%w: %s exceed %d terms", ErrInvalidArgument, name, maxPredicateTerms)
	}
	return nil
}

func jsonIDs(ids []domain.EntityID) (string, error) {
	encoded, err := json.Marshal(ids)
	if err != nil {
		return "", fmt.Errorf("encode entity IDs: %w", err)
	}
	return string(encoded), nil
}

func appendEntitySet(query string, args []any, column string, values []domain.EntityID) (string, []any, error) {
	if len(values) <= directSetTerms {
		query += " AND " + column + " IN (" + strings.TrimSuffix(strings.Repeat("?,", len(values)), ",") + ")"
		for _, value := range values {
			args = append(args, string(value))
		}
		return query, args, nil
	}
	encoded, err := jsonIDs(values)
	if err != nil {
		return "", nil, err
	}
	query += " AND " + column + " IN (SELECT value FROM json_each(?))"
	return query, append(args, encoded), nil
}

func appendStringSet(query string, args []any, column string, values []string) (string, []any, error) {
	if len(values) <= directSetTerms {
		query += " AND " + column + " IN (" + strings.TrimSuffix(strings.Repeat("?,", len(values)), ",") + ")"
		for _, value := range values {
			args = append(args, value)
		}
		return query, args, nil
	}
	encoded, err := json.Marshal(values)
	if err != nil {
		return "", nil, fmt.Errorf("encode string predicate: %w", err)
	}
	query += " AND " + column + " IN (SELECT value FROM json_each(?))"
	return query, append(args, string(encoded)), nil
}

func encodedPropertyStrings(values map[string][]byte) map[string]string {
	if len(values) == 0 {
		return nil
	}
	result := make(map[string]string, len(values))
	for key, value := range values {
		result[key] = base64.RawURLEncoding.EncodeToString(value)
	}
	return result
}

func predicateFingerprint(value any) (string, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return "", fmt.Errorf("fingerprint graph predicate: %w", err)
	}
	sum := sha256.Sum256(encoded)
	return base64.RawURLEncoding.EncodeToString(sum[:]), nil
}

type graphCursor struct {
	Version     int             `json:"v"`
	Kind        string          `json:"k"`
	Revision    domain.Revision `json:"r"`
	Fingerprint string          `json:"f"`
	After       string          `json:"a"`
}

func encodeGraphCursor(kind string, revision domain.Revision, fingerprint, after string) string {
	encoded, _ := json.Marshal(graphCursor{1, kind, revision, fingerprint, after})
	return base64.RawURLEncoding.EncodeToString(encoded)
}

func decodeGraphCursor(cursor, kind string, revision domain.Revision, fingerprint string) (string, error) {
	if cursor == "" {
		return "", nil
	}
	decoded, err := parseGraphCursor(cursor)
	if err != nil {
		return "", err
	}
	if decoded.Kind != kind || decoded.Revision != revision || decoded.Fingerprint != fingerprint {
		return "", fmt.Errorf("%w: page cursor belongs to a different graph view or predicate", ErrInvalidArgument)
	}
	if err := validateID(domain.EntityID(decoded.After)); err != nil {
		return "", fmt.Errorf("%w: invalid graph page cursor key", ErrInvalidArgument)
	}
	return decoded.After, nil
}

func graphCursorRevision(cursor, kind string) (domain.Revision, error) {
	decoded, err := parseGraphCursor(cursor)
	if err != nil {
		return 0, err
	}
	if decoded.Kind != kind {
		return 0, fmt.Errorf("%w: page cursor has the wrong graph kind", ErrInvalidArgument)
	}
	return decoded.Revision, nil
}

func parseGraphCursor(cursor string) (graphCursor, error) {
	data, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil || len(data) > 4096 {
		return graphCursor{}, fmt.Errorf("%w: invalid graph page cursor", ErrInvalidArgument)
	}
	var result graphCursor
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&result); err != nil || ensureJSONEnd(decoder) != nil {
		return graphCursor{}, fmt.Errorf("%w: invalid graph page cursor", ErrInvalidArgument)
	}
	canonical, err := json.Marshal(result)
	if err != nil || !bytes.Equal(canonical, data) || result.Version != 1 || result.Fingerprint == "" || result.After == "" {
		return graphCursor{}, fmt.Errorf("%w: invalid graph page cursor", ErrInvalidArgument)
	}
	return result, nil
}
