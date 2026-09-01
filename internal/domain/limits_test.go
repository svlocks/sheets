package domain

import (
	"errors"
	"strings"
	"testing"
)

func TestValidateTextUsesUTF8ByteLimitsAndFieldSpecificNULRules(t *testing.T) {
	for _, sample := range []struct {
		name    string
		value   string
		limit   int
		wantErr error
	}{
		{name: "below", value: "abc", limit: 4},
		{name: "exact", value: "four", limit: 4},
		{name: "above", value: "fives", limit: 4, wantErr: ErrResourceLimit},
		{name: "unicode bytes exact", value: "éé", limit: 4},
		{name: "unicode bytes above", value: "éé", limit: 3, wantErr: ErrResourceLimit},
		{name: "invalid UTF-8", value: string([]byte{0xff}), limit: 4, wantErr: ErrInvalidText},
		{name: "NUL preserved", value: "a\x00b", limit: 3},
	} {
		t.Run(sample.name, func(t *testing.T) {
			err := ValidateText("sample", sample.value, sample.limit)
			if !errors.Is(err, sample.wantErr) || sample.wantErr == nil && err != nil {
				t.Fatalf("ValidateText() error = %v, want %v", err, sample.wantErr)
			}
			if sample.wantErr == ErrResourceLimit {
				var limit *ResourceLimitError
				if !errors.As(err, &limit) || limit.Unit != "bytes" || limit.Limit != sample.limit || limit.Actual != len(sample.value) {
					t.Fatalf("resource limit detail = %#v", limit)
				}
			}
		})
	}

	if err := ValidateTextWithoutNUL("name", "a\x00b", 3); !errors.Is(err, ErrInvalidText) {
		t.Fatalf("ValidateTextWithoutNUL() error = %v", err)
	}
}

func TestPublishedDurableTextLimitsBoundary(t *testing.T) {
	// Reuse one allocation so exercising the published 64 MiB ceiling does not
	// multiply the test process's peak memory.
	buffer := strings.Repeat("x", MaxNodeBodyBytes+1)
	tests := []struct {
		name  string
		limit int
	}{
		{name: "name", limit: MaxNameBytes},
		{name: "message", limit: MaxRevisionMessageBytes},
		{name: "property scalar", limit: MaxPropertyScalarBytes},
		{name: "body", limit: MaxNodeBodyBytes},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := ValidateText(test.name, buffer[:test.limit-1], test.limit); err != nil {
				t.Fatalf("limit-1: %v", err)
			}
			if err := ValidateText(test.name, buffer[:test.limit], test.limit); err != nil {
				t.Fatalf("limit: %v", err)
			}
			if err := ValidateText(test.name, buffer[:test.limit+1], test.limit); !errors.Is(err, ErrResourceLimit) {
				t.Fatalf("limit+1: %v", err)
			}
		})
	}
	multibyte := strings.Repeat("é", MaxNameBytes/2)
	if err := ValidateText("multibyte name", multibyte, MaxNameBytes); err != nil {
		t.Fatalf("multibyte exact byte limit: %v", err)
	}
	if err := ValidateText("multibyte name", multibyte+"x", MaxNameBytes); !errors.Is(err, ErrResourceLimit) {
		t.Fatalf("multibyte byte limit+1: %v", err)
	}
}

func FuzzValidateText(f *testing.F) {
	f.Add("Markdown **and** Unicode: 雪", 64, false)
	f.Add("contains\x00nul", 64, true)
	f.Add(string([]byte{0xff}), 1, false)
	f.Fuzz(func(t *testing.T, value string, requestedLimit int, withoutNUL bool) {
		limit := requestedLimit
		if limit < 0 {
			limit = 0
		}
		if limit > 1<<20 {
			limit = 1 << 20
		}
		if withoutNUL {
			_ = ValidateTextWithoutNUL("fuzz", value, limit)
		} else {
			_ = ValidateText("fuzz", value, limit)
		}
	})
}
