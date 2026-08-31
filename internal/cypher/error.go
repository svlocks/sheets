package cypher

import (
	"fmt"
	"strings"
)

// ParseError describes a syntax error at a concrete source position.
type ParseError struct {
	Position Position
	End      Position
	Message  string
}

func (e *ParseError) Error() string {
	if e == nil {
		return ""
	}
	return fmt.Sprintf("cypher:%d:%d: %s", e.Position.Line, e.Position.Column, e.Message)
}

// Errors permits callers to report every independently recoverable error from
// a semicolon-separated input, while still satisfying the error interface.
type Errors []*ParseError

func (e Errors) Error() string {
	parts := make([]string, 0, len(e))
	for _, item := range e {
		if item != nil {
			parts = append(parts, item.Error())
		}
	}
	return strings.Join(parts, "; ")
}

// Unwrap exposes the individual parse errors to errors.Is/errors.As aware
// callers.
func (e Errors) Unwrap() []error {
	items := make([]error, 0, len(e))
	for _, item := range e {
		if item != nil {
			items = append(items, item)
		}
	}
	return items
}
