package cli

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestParameterInputLoad(t *testing.T) {
	got, err := (parameterInput{
		Object: `{"name":"pay","count":3,"nested":{"ready":true}}`,
		Values: []string{`ratio=1.5`, `items=[1,2]`},
	}).load(strings.NewReader(""))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"name":   "pay",
		"count":  int64(3),
		"nested": map[string]any{"ready": true},
		"ratio":  1.5,
		"items":  []any{int64(1), int64(2)},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("params = %#v, want %#v", got, want)
	}
}

func TestParameterInputReadsFileAndStdin(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "params.json")
	if err := os.WriteFile(path, []byte(`{"source":"file"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	fromFile, err := (parameterInput{Object: "@" + path}).load(strings.NewReader(""))
	if err != nil {
		t.Fatal(err)
	}
	if fromFile["source"] != "file" {
		t.Fatalf("file params = %#v", fromFile)
	}

	fromStdin, err := (parameterInput{Object: "-"}).load(strings.NewReader(`{"source":"stdin"}`))
	if err != nil {
		t.Fatal(err)
	}
	if fromStdin["source"] != "stdin" {
		t.Fatalf("stdin params = %#v", fromStdin)
	}
}

func TestParameterInputRejectsDuplicatesAndInvalidAssignments(t *testing.T) {
	for _, input := range []parameterInput{
		{Object: `{"x":1}`, Values: []string{"x=2"}},
		{Values: []string{"missing-separator"}},
		{Values: []string{"=1"}},
		{Values: []string{"x=not-json"}},
	} {
		if _, err := input.load(strings.NewReader("")); err == nil {
			t.Fatalf("load(%#v) succeeded", input)
		}
	}
}

func TestParameterInputRejectsTrailingJSON(t *testing.T) {
	if _, err := (parameterInput{Object: `{"x":1} {"y":2}`}).load(strings.NewReader("")); err == nil {
		t.Fatal("trailing JSON was accepted")
	}
}

func TestParameterInputPreservesIntegerPrecisionAndRejectsOverflow(t *testing.T) {
	params, err := (parameterInput{Values: []string{"exact=9007199254740993"}}).load(strings.NewReader(""))
	if err != nil {
		t.Fatal(err)
	}
	if got := params["exact"]; got != int64(9007199254740993) {
		t.Fatalf("exact integer = %#v (%T)", got, got)
	}
	for _, value := range []string{"too_big=9223372036854775808", "too_small=-9223372036854775809"} {
		if _, err := (parameterInput{Values: []string{value}}).load(strings.NewReader("")); err == nil || !strings.Contains(err.Error(), "signed 64-bit") {
			t.Fatalf("overflow %q error = %v", value, err)
		}
	}
}
