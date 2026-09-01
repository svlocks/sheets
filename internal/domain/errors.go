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
	// ErrInvalidText means durable text is not valid UTF-8 or violates a
	// field-specific text representation rule.
	ErrInvalidText = errors.New("invalid durable text")
	// ErrResourceLimit means a durable value exceeds a published byte or
	// collection limit. Callers may use errors.As to inspect ResourceLimitError.
	ErrResourceLimit = errors.New("durable value exceeds resource limit")
)

// ResourceLimitError reports the exact byte or collection limit exceeded by a
// durable value. Actual and Limit use the unit named by Unit.
type ResourceLimitError struct {
	Field  string
	Unit   string
	Limit  int
	Actual int
}

func (e *ResourceLimitError) Error() string {
	return fmt.Sprintf("%s has %d %s; maximum is %d", e.Field, e.Actual, e.Unit, e.Limit)
}

// Unwrap makes every ResourceLimitError match ErrResourceLimit.
func (e *ResourceLimitError) Unwrap() error { return ErrResourceLimit }

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
