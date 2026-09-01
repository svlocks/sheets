// Package app contains the use-case boundary shared by sheets's CLI and TUI.
package app

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/svlocks/sheets/internal/domain"
)

// ExecuteRequest is one atomic Cypher request. Query may contain multiple
// semicolon-delimited statements. Mutating statements share one transaction and
// revision. ReadOnly asks the engine to reject them before execution.
type ExecuteRequest struct {
	Query    string
	Params   map[string]any
	Snapshot domain.Snapshot
	ReadOnly bool
	Actor    string
	Message  string
}

// Validate checks request invariants that do not require parsing Cypher.
func (r ExecuteRequest) Validate() error {
	if strings.TrimSpace(r.Query) == "" {
		return fmt.Errorf("query is empty")
	}
	if r.Snapshot.Revision != nil && r.Snapshot.Time != nil {
		return fmt.Errorf("revision and time snapshots are mutually exclusive")
	}
	return nil
}

// Executor is the complete graph-language boundary used by frontends.
type Executor interface {
	Execute(context.Context, ExecuteRequest) (BatchResult, error)
}

// ErrStreamingMutation reports that a request cannot be delivered as rows
// while preserving the all-or-nothing visibility of a graph mutation. Callers
// may retry the request through Executor, which publishes results only after
// the write transaction commits.
var ErrStreamingMutation = errors.New("mutating requests cannot stream results")

// ResultEventKind identifies one event in a streamed Cypher result. A result
// always starts with ResultStart, contains zero or more ResultRow events, and
// finishes with ResultEnd before the next statement begins.
type ResultEventKind string

const (
	ResultStart ResultEventKind = "result_start"
	ResultRow   ResultEventKind = "result_row"
	ResultEnd   ResultEventKind = "result_end"
)

// ResultEvent is the bounded read-result boundary used by streaming
// frontends. Statement indexes are zero-based. Columns is populated on
// ResultStart, Values on ResultRow, and Summary/Page on ResultEnd.
//
// Values are detached from engine-owned graph state and remain valid after the
// emitter returns. Emitters are invoked synchronously: returning blocks query
// execution, which provides natural backpressure. If execution fails after
// rows were emitted, the caller retains that valid event prefix but will not
// receive ResultEnd for the interrupted statement.
type ResultEvent struct {
	Kind      ResultEventKind
	Statement int
	Columns   []string
	Values    []any
	Summary   Summary
	Page      *domain.PageInfo
}

// ResultEmitter consumes one result event. Returning an error aborts execution
// immediately; implementations should preserve writer and cancellation error
// causes so errors.Is remains useful to the caller.
type ResultEmitter func(ResultEvent) error

// StreamExecutor incrementally delivers read results without retaining the
// complete output table. Blocking Cypher operators such as ORDER BY, DISTINCT,
// and aggregation may still retain their bounded working set before the first
// row is emitted. Mutations return ErrStreamingMutation before execution.
type StreamExecutor interface {
	ExecuteStream(context.Context, ExecuteRequest, ResultEmitter) error
}

// RevisionPager is the bounded revision-history read boundary shared by
// frontends. Implementations must treat PageInfo.Next as opaque.
type RevisionPager interface {
	ListRevisionPage(context.Context, domain.RevisionPage) ([]domain.RevisionInfo, domain.PageInfo, error)
}

// BatchResult preserves one result per submitted statement.
type BatchResult struct {
	Results  []Result         `json:"results"`
	Revision *domain.Revision `json:"revision,omitempty"`
}

// Result is a rectangular Cypher result plus execution statistics. Columns and
// row values use matching indexes so column ordering is deterministic.
type Result struct {
	Columns []string         `json:"columns,omitempty"`
	Rows    [][]any          `json:"rows,omitempty"`
	Summary Summary          `json:"summary"`
	Page    *domain.PageInfo `json:"page,omitempty"`
}

// Summary describes the observable effects of one statement.
type Summary struct {
	NodesCreated         uint64 `json:"nodes_created,omitempty"`
	NodesUpdated         uint64 `json:"nodes_updated,omitempty"`
	NodesDeleted         uint64 `json:"nodes_deleted,omitempty"`
	RelationshipsCreated uint64 `json:"relationships_created,omitempty"`
	RelationshipsUpdated uint64 `json:"relationships_updated,omitempty"`
	RelationshipsDeleted uint64 `json:"relationships_deleted,omitempty"`
	PropertiesSet        uint64 `json:"properties_set,omitempty"`
	LabelsAdded          uint64 `json:"labels_added,omitempty"`
	LabelsRemoved        uint64 `json:"labels_removed,omitempty"`
}

// Changed reports whether a statement modified graph state.
func (s Summary) Changed() bool {
	return s.NodesCreated != 0 || s.NodesUpdated != 0 || s.NodesDeleted != 0 ||
		s.RelationshipsCreated != 0 || s.RelationshipsUpdated != 0 || s.RelationshipsDeleted != 0 ||
		s.PropertiesSet != 0 || s.LabelsAdded != 0 || s.LabelsRemoved != 0
}
