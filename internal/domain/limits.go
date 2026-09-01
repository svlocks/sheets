package domain

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// Durable graph limits are byte based so their behavior is independent of the
// Unicode code points used by a client. They bound one value, never graph size
// or entity cardinality.
const (
	MaxCanonicalPropertyBytes = 64 << 20
	MaxEncodedLabelsBytes     = 64 << 20
	MaxNodeBodyBytes          = 64 << 20
	MaxPropertyScalarBytes    = 16 << 20
	MaxRevisionMessageBytes   = 1 << 20
	MaxNameBytes              = 64 << 10

	MaxRevisionActorBytes    = MaxNameBytes
	MaxLabelBytes            = MaxNameBytes
	MaxRelationshipTypeBytes = MaxNameBytes
	MaxPropertyKeyBytes      = MaxNameBytes
	MaxTimeZoneNameBytes     = MaxNameBytes

	MaxPropertyDepth  = 128
	MaxPropertyValues = 1_000_000

	// Derived lookup structures have independent amplification budgets. A
	// canonical property value may contain many nested values without indexing
	// them, while only top-level scalars and normalized labels consume these
	// durable B-tree budgets.
	MaxLabelsPerNode                  = 4_096
	MaxIndexedPropertiesPerVersion    = 4_096
	MaxDerivedLabelBytesPerVersion    = 16 << 20
	MaxDerivedPropertyBytesPerVersion = 32 << 20
)

// ValidateText validates one durable UTF-8 string and its byte ceiling. Empty
// strings and NUL code points are permitted; callers enforce field-specific
// representation rules.
func ValidateText(field, value string, maximumBytes int) error {
	if !utf8.ValidString(value) {
		return fmt.Errorf("%w: %s is not valid UTF-8", ErrInvalidText, field)
	}
	if len(value) > maximumBytes {
		return &ResourceLimitError{Field: field, Unit: "bytes", Limit: maximumBytes, Actual: len(value)}
	}
	return nil
}

// ValidateTextWithoutNUL applies ValidateText and additionally rejects an
// embedded NUL for fields used as SQLite graph names. Property strings and map
// keys deliberately do not use this helper: their canonical JSON and bound
// parameter representations preserve NUL exactly.
func ValidateTextWithoutNUL(field, value string, maximumBytes int) error {
	if err := ValidateText(field, value, maximumBytes); err != nil {
		return err
	}
	if strings.IndexByte(value, 0) >= 0 {
		return fmt.Errorf("%w: %s contains a NUL byte", ErrInvalidText, field)
	}
	return nil
}

// ValidateBytes validates the byte ceiling for one durable binary value.
func ValidateBytes(field string, value []byte, maximumBytes int) error {
	if len(value) > maximumBytes {
		return &ResourceLimitError{Field: field, Unit: "bytes", Limit: maximumBytes, Actual: len(value)}
	}
	return nil
}
