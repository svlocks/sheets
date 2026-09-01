// Package domain defines the durable concepts shared by sheets's storage,
// query, and presentation layers. It intentionally contains no persistence or
// frontend behavior.
package domain

import "time"

// Revision identifies one committed graph state. Revision zero represents the
// empty state before the first mutation.
type Revision uint64

// EntityID is the stable external identifier of a node or edge. Implementations
// issue lowercase UUIDv7 strings so identifiers are sortable by creation time.
type EntityID string

// Properties is a schema-free set of Cypher-compatible values. Supported
// values are null, booleans, strings, signed integers, floating-point numbers,
// byte slices, temporal values, lists, and nested string-keyed maps.
type Properties map[string]any

// Node is one node as visible in a graph snapshot.
type Node struct {
	ID         EntityID   `json:"id"`
	Labels     []string   `json:"labels,omitempty"`
	Properties Properties `json:"properties,omitempty"`
	Body       string     `json:"body,omitempty"`
	ValidFrom  Revision   `json:"valid_from"`
	ValidTo    *Revision  `json:"valid_to,omitempty"`
}

// Edge is one directed relationship as visible in a graph snapshot. Position
// is meaningful for CHILD edges and nil denotes an unordered child.
type Edge struct {
	ID         EntityID   `json:"id"`
	From       EntityID   `json:"from"`
	Type       string     `json:"type"`
	To         EntityID   `json:"to"`
	Position   *int64     `json:"position,omitempty"`
	Properties Properties `json:"properties,omitempty"`
	ValidFrom  Revision   `json:"valid_from"`
	ValidTo    *Revision  `json:"valid_to,omitempty"`
}

// RevisionInfo describes the transaction that produced a graph state.
type RevisionInfo struct {
	Revision Revision  `json:"revision"`
	Time     time.Time `json:"time"`
	Actor    string    `json:"actor,omitempty"`
	Message  string    `json:"message,omitempty"`
}

// Snapshot chooses a historical graph state. At most one selector may be set;
// the zero value means the current graph.
type Snapshot struct {
	Revision *Revision
	Time     *time.Time
}

// IsCurrent reports whether the snapshot selects the live graph.
func (s Snapshot) IsCurrent() bool { return s.Revision == nil && s.Time == nil }

// Page describes a stable, bounded result window. After is an opaque cursor
// supplied by a preceding response.
type Page struct {
	Limit int
	After string
}

// RevisionOrder selects the stable traversal order for revision history.
// The zero value is ascending to preserve the ordering of the original
// revision-listing API.
type RevisionOrder uint8

const (
	RevisionOrderAscending RevisionOrder = iota
	RevisionOrderDescending
)

// Valid reports whether order is a supported revision ordering.
func (order RevisionOrder) Valid() bool {
	return order == RevisionOrderAscending || order == RevisionOrderDescending
}

// String returns the public spelling of order.
func (order RevisionOrder) String() string {
	switch order {
	case RevisionOrderAscending:
		return "ascending"
	case RevisionOrderDescending:
		return "descending"
	default:
		return "unknown"
	}
}

// RevisionPage describes a bounded revision-history window. Cursor is an
// opaque token returned by a preceding page in the same order.
type RevisionPage struct {
	Limit  int           `json:"limit,omitempty"`
	Cursor string        `json:"cursor,omitempty"`
	Order  RevisionOrder `json:"order,omitempty"`
}

// PageInfo accompanies a paginated result.
type PageInfo struct {
	Next string `json:"next,omitempty"`
}
