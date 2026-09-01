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

// UnsupportedFeatureError means the official grammar recognized a construct,
// but the AST/executor capability contract does not assign it semantics. This
// is deliberately distinct from a syntax error so callers never mistake a
// rejected feature for malformed input or allow it to be reinterpreted.
type UnsupportedFeatureError struct {
	Span    Span
	Feature string
	Detail  string
}

func (e *UnsupportedFeatureError) Error() string {
	if e == nil {
		return ""
	}
	detail := e.Detail
	if detail == "" {
		detail = "recognized by the pinned grammar but not supported by sheets"
	}
	return fmt.Sprintf("cypher:%d:%d: recognized but unsupported %s: %s", e.Span.Start.Line, e.Span.Start.Column, e.Feature, detail)
}

// Location returns the exact recognized source construct.
func (e *UnsupportedFeatureError) Location() Span {
	if e == nil {
		return Span{}
	}
	return e.Span
}

// Errors permits callers to report every independently recoverable syntax or
// capability error from a semicolon-separated input.
type Errors []error

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
