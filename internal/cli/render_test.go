package cli

import (
	"bytes"
	"errors"
	"math"
	"strings"
	"testing"

	"github.com/svlocks/sheets/internal/app"
	"github.com/svlocks/sheets/internal/domain"
)

func TestParseFormat(t *testing.T) {
	for _, test := range []struct {
		input string
		want  Format
	}{
		{"table", FormatTable},
		{" JSON ", FormatJSON},
		{"JsonL", FormatJSONL},
	} {
		got, err := ParseFormat(test.input)
		if err != nil || got != test.want {
			t.Errorf("ParseFormat(%q) = %q, %v; want %q", test.input, got, err, test.want)
		}
	}
	if _, err := ParseFormat("yaml"); !errors.Is(err, ErrInvalidFormat) {
		t.Fatalf("ParseFormat(yaml) error = %v; errors.Is(_, ErrInvalidFormat) is false", err)
	}
}

func TestRenderJSONExactTaggedEnvelope(t *testing.T) {
	revision := domain.Revision(7)
	batch := app.BatchResult{
		Results: []app.Result{{
			Columns: []string{"name", "active"},
			Rows:    [][]any{{"Ada", true}},
			Summary: app.Summary{NodesCreated: 2},
		}},
		Revision: &revision,
	}
	var output bytes.Buffer
	if err := Render(&output, "json", batch); err != nil {
		t.Fatal(err)
	}
	want := `{
  "results": [
    {
      "columns": [
        "name",
        "active"
      ],
      "rows": [
        [
          "Ada",
          true
        ]
      ],
      "summary": {
        "nodes_created": 2
      }
    }
  ],
  "revision": 7
}
`
	if output.String() != want {
		t.Fatalf("JSON output =\n%swant =\n%s", output.String(), want)
	}
}

func TestRenderJSONLExactRecords(t *testing.T) {
	revision := domain.Revision(9)
	batch := app.BatchResult{
		Results: []app.Result{
			{
				Columns: []string{"name", "active"},
				Rows:    [][]any{{"Ada", true}, {nil, false}},
				Summary: app.Summary{NodesCreated: 2},
			},
			{Columns: []string{"empty"}},
		},
		Revision: &revision,
	}
	var output bytes.Buffer
	if err := Render(&output, "jsonl", batch); err != nil {
		t.Fatal(err)
	}
	want := strings.Join([]string{
		`{"type":"row","statement":0,"columns":["name","active"],"values":["Ada",true]}`,
		`{"type":"row","statement":0,"columns":["name","active"],"values":[null,false]}`,
		`{"type":"summary","statement":0,"summary":{"nodes_created":2}}`,
		`{"type":"result","statement":1,"columns":["empty"]}`,
		`{"type":"revision","revision":9}`,
		"",
	}, "\n")
	if output.String() != want {
		t.Fatalf("JSONL output =\n%swant =\n%s", output.String(), want)
	}
}

func TestRenderTableExactAndSafeValues(t *testing.T) {
	revision := domain.Revision(3)
	node := domain.Node{ID: "n1", Labels: []string{"Task"}, Body: "line\nnext"}
	batch := app.BatchResult{
		Results: []app.Result{
			{
				Columns: []string{"value", "node", "list"},
				Rows: [][]any{{
					"danger\x1b[31m|\ntext",
					node,
					[]any{int64(4), false},
				}},
				Summary: app.Summary{NodesCreated: 1, PropertiesSet: 2},
			},
			{Columns: []string{"nothing"}},
		},
		Revision: &revision,
	}
	var output bytes.Buffer
	if err := Render(&output, "table", batch); err != nil {
		t.Fatal(err)
	}
	want := "Statement 1\n" +
		"value                   | node                                                             | list\n" +
		"----------------------- | ---------------------------------------------------------------- | ---------\n" +
		`"danger\x1b[31m|\ntext" | {"id":"n1","labels":["Task"],"body":"line\nnext","valid_from":0} | [4,false]` + "\n" +
		"Summary: nodes_created=1 properties_set=2\n\n" +
		"Statement 2\n" +
		"nothing\n" +
		"-------\n" +
		"(no rows)\n" +
		"Revision: 3\n"
	if output.String() != want {
		t.Fatalf("table output =\n%q\nwant =\n%q", output.String(), want)
	}
}

func TestRenderEmptyAndNarrowResults(t *testing.T) {
	var output bytes.Buffer
	if err := Render(&output, "table", app.BatchResult{}); err != nil {
		t.Fatal(err)
	}
	if got, want := output.String(), "No results.\n"; got != want {
		t.Fatalf("empty table = %q, want %q", got, want)
	}
	output.Reset()
	result := app.BatchResult{Results: []app.Result{{Rows: [][]any{{1, 2}}}}}
	if err := Render(&output, string(FormatTable), result); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "column_1 | column_2") {
		t.Fatalf("unnamed columns output = %q", output.String())
	}
}

type failingWriter struct {
	remaining int
}

func (w *failingWriter) Write(data []byte) (int, error) {
	if w.remaining <= 0 {
		return 0, errors.New("writer failed")
	}
	if len(data) > w.remaining {
		count := w.remaining
		w.remaining = 0
		return count, errors.New("writer failed")
	}
	w.remaining -= len(data)
	return len(data), nil
}

func TestRenderPropagatesWriterErrors(t *testing.T) {
	for _, format := range []string{"table", "json", "jsonl"} {
		err := Render(&failingWriter{remaining: 1}, format, app.BatchResult{Results: []app.Result{{Rows: [][]any{{"value"}}}}})
		if err == nil || !strings.Contains(err.Error(), "writer failed") {
			t.Errorf("Render(%s) error = %v; want writer failure", format, err)
		}
	}
}

func TestMachineFormatsUseStableEmptyArraysAndTaggedNonFiniteFloats(t *testing.T) {
	batch := app.BatchResult{Results: []app.Result{{Rows: [][]any{{
		math.NaN(), math.Inf(1), math.Inf(-1), map[string]any{"nested": math.NaN()},
	}}}}}
	var output bytes.Buffer
	if err := Render(&output, "json", batch); err != nil {
		t.Fatal(err)
	}
	for _, fragment := range []string{
		`"columns": []`,
		`"$float": "NaN"`,
		`"$float": "+Infinity"`,
		`"$float": "-Infinity"`,
	} {
		if !strings.Contains(output.String(), fragment) {
			t.Fatalf("JSON output %q lacks %q", output.String(), fragment)
		}
	}

	output.Reset()
	if err := Render(&output, "json", app.BatchResult{Results: []app.Result{{}}}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), `"columns": []`) || !strings.Contains(output.String(), `"rows": []`) {
		t.Fatalf("empty JSON shape = %s", output.String())
	}

	output.Reset()
	if err := Render(&output, "jsonl", batch); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), `"$float":"NaN"`) {
		t.Fatalf("JSONL output = %s", output.String())
	}
}
