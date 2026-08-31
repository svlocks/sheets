// Package app contains the use-case boundary shared by sheets's CLI and TUI.
package app

import (
	"context"
	"fmt"

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
	if r.Query == "" {
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

// BatchResult preserves one result per submitted statement.
type BatchResult struct {
	Results  []Result         `json:"results"`
	Revision *domain.Revision `json:"revision,omitempty"`
}

// Result is a rectangular Cypher result plus execution statistics. Columns and
// row values use matching indexes so column ordering is deterministic.
type Result struct {
	Columns []string `json:"columns,omitempty"`
	Rows    [][]any  `json:"rows,omitempty"`
	Summary Summary  `json:"summary"`
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
	return s.NodesCreated+s.NodesUpdated+s.NodesDeleted+
		s.RelationshipsCreated+s.RelationshipsUpdated+s.RelationshipsDeleted+
		s.PropertiesSet+s.LabelsAdded+s.LabelsRemoved > 0
}
