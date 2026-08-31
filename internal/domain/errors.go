package domain

import (
	"errors"
	"fmt"
)

var (
	// ErrNotFound means the requested entity or revision is not visible.
	ErrNotFound = errors.New("not found")
	// ErrConflict means current graph state violates an optimistic assumption.
	ErrConflict = errors.New("conflict")
	// ErrReadOnly means a mutation was submitted through a read-only interface.
	ErrReadOnly = errors.New("read-only execution cannot mutate the graph")
	// ErrHistoricalWrite means a mutation targeted a historical snapshot.
	ErrHistoricalWrite = errors.New("historical snapshots are read-only")
)

// ConstraintError reports a graph invariant violation in a form frontends can
// present without inspecting storage-specific errors.
type ConstraintError struct {
	Constraint string
	Detail     string
}

func (e *ConstraintError) Error() string {
	if e.Detail == "" {
		return "graph constraint violated: " + e.Constraint
	}
	return fmt.Sprintf("graph constraint violated: %s: %s", e.Constraint, e.Detail)
}
