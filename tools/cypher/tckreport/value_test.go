package main

import (
	"math"
	"strconv"
	"testing"
	"time"

	"github.com/svlocks/sheets/internal/domain"
)

func TestActualTemporalValuesUseM23TCKStringNotation(t *testing.T) {
	tests := []struct {
		value any
		want  string
	}{
		{time.Date(2015, 7, 21, 0, 0, 0, 0, time.UTC), `string:"2015-07-21"`},
		{time.Date(2015, 7, 21, 21, 40, 32, 142000000, time.Local), `string:"2015-07-21T21:40:32.142"`},
		{time.Date(0, 1, 1, 21, 40, 32, 142000000, time.Local), `string:"21:40:32.142"`},
		{time.Date(0, 1, 1, 21, 40, 32, 0, time.FixedZone("", 3600)), `string:"21:40:32+01:00"`},
		{14*24*time.Hour + 16*time.Hour + 12*time.Minute, `string:"P14DT16H12M"`},
	}
	for _, test := range tests {
		normalized, err := normalizeActual(test.value)
		if err != nil {
			t.Fatal(err)
		}
		if got := normalized.key(false); got != test.want {
			t.Errorf("normalizeActual(%v) = %s, want %s", test.value, got, test.want)
		}
	}
}

func TestExpectedGraphValuesMatchRuntimeValues(t *testing.T) {
	expected, err := parseExpectedValue(`(:B:A {name: 'Ada', values: [1, null]})`)
	if err != nil {
		t.Fatal(err)
	}
	actual, err := normalizeActual(domain.Node{
		Labels: []string{"A", "B"},
		Properties: domain.Properties{
			"name":   "Ada",
			"values": []any{int64(1), nil},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if expected.key(false) != actual.key(false) {
		t.Fatalf("expected %s, actual %s", expected.key(false), actual.key(false))
	}
}

func TestExpectedPathDirectionsAreStable(t *testing.T) {
	value, err := parseExpectedValue(`<(:A)<-[:T]-(:B)-[:U]->(:C)>`)
	if err != nil {
		t.Fatal(err)
	}
	if got := value.key(false); got == "" {
		t.Fatal("empty normalized path")
	}
}

func TestNegativeZeroNormalizesLikePositiveZero(t *testing.T) {
	negative, err := normalizeActual(math.Copysign(0, -1))
	if err != nil {
		t.Fatal(err)
	}
	positive, err := parseExpectedValue("0.0")
	if err != nil {
		t.Fatal(err)
	}
	if negative.key(false) != positive.key(false) {
		t.Fatalf("negative zero %q != positive zero %q", negative.key(false), positive.key(false))
	}
}

func TestExpectedStringsUseM23ValueNotationEscapes(t *testing.T) {
	for source, want := range map[string]string{
		`'\''`:   "'",
		`'\\'`:   `\`,
		"'a\nb'": "a\nb",
	} {
		value, err := parseExpectedValue(source)
		if err != nil {
			t.Fatalf("parseExpectedValue(%q): %v", source, err)
		}
		if got := value.key(false); got != `string:`+strconv.Quote(want) {
			t.Fatalf("parseExpectedValue(%q) = %s", source, got)
		}
	}

	if _, err := parseExpectedValue(`'\u263A'`); err == nil {
		t.Fatal("TCK value notation unexpectedly accepted a Cypher Unicode escape")
	}
}
