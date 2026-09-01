package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/svlocks/sheets/internal/app"
	"github.com/svlocks/sheets/internal/domain"
	"github.com/svlocks/sheets/internal/domain/temporal"
	"github.com/svlocks/sheets/internal/engine"
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

func TestRenderTemporalValuesAreTypedReadableAndParameterRoundTrippable(t *testing.T) {
	date, _ := temporal.ParseDate("1984-10-11")
	localTime, _ := temporal.ParseLocalTime("12:31:14.645876123")
	offsetTime, _ := temporal.ParseTime("12:31:14.645876123+01:00")
	localDateTime := temporal.NewLocalDateTime(date, localTime)
	dateTime, _ := temporal.NewDateTime(localDateTime, "Europe/Stockholm")
	duration, _ := temporal.NewDuration(-7, 14, -4, 500_000_000)
	legacyTime := time.Date(2026, 8, 31, 9, 10, 11, 12, time.FixedZone("legacy", -5*3600))
	values := []any{date, localTime, offsetTime, localDateTime, dateTime, duration, legacyTime, 90 * time.Minute, []byte{0, 255}}
	batch := app.BatchResult{Results: []app.Result{{
		Columns: []string{"date", "local_time", "offset_time", "local_datetime", "datetime", "duration", "legacy_time", "legacy_duration", "bytes"},
		Rows:    [][]any{values},
	}}}
	for _, format := range []string{"json", "jsonl"} {
		var output bytes.Buffer
		if err := Render(&output, format, batch); err != nil {
			t.Fatal(err)
		}
		for _, tag := range []string{
			`"$date"`, `"$local_time"`, `"$offset_time"`, `"$local_datetime"`,
			`"$zoned_datetime"`, `"$cypher_duration"`, `"$legacy_time"`,
			`"$legacy_duration"`, `"$bytes"`,
		} {
			if !strings.Contains(output.String(), tag) {
				t.Errorf("%s output lacks %s: %s", format, tag, output.String())
			}
		}
	}
	var table bytes.Buffer
	if err := Render(&table, "table", batch); err != nil {
		t.Fatal(err)
	}
	for _, readable := range []string{
		"date(1984-10-11)", "localtime(12:31:14.645876123)",
		"time(12:31:14.645876123+01:00)", "localdatetime(1984-10-11T12:31:14.645876123)",
		"datetime(1984-10-11T12:31:14.645876123+01:00[Europe/Stockholm])",
		"duration(P-7M14DT-3.5S)", "legacy_time(", "legacy_duration(1h30m0s)", "bytes(AP8=)",
	} {
		if !strings.Contains(table.String(), readable) {
			t.Errorf("table output lacks %q: %s", readable, table.String())
		}
	}

	for index, value := range values {
		data, err := json.Marshal(jsonValue(value))
		if err != nil {
			t.Fatal(err)
		}
		params, err := (parameterInput{Values: []string{"value=" + string(data)}}).load(strings.NewReader(""))
		if err != nil {
			t.Fatalf("round-trip parameter %d (%T): %v\n%s", index, value, err, data)
		}
		got := params["value"]
		switch value := value.(type) {
		case time.Time:
			decoded, ok := got.(time.Time)
			if !ok || !decoded.Equal(value) || decoded.Location().String() != value.Location().String() {
				t.Errorf("legacy time parameter = %#v", got)
			}
		default:
			if !reflect.DeepEqual(got, value) {
				t.Errorf("parameter %d = %#v (%T), want %#v (%T)", index, got, got, value, value)
			}
		}
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

func TestRenderJSONDoesNotMutatePathValues(t *testing.T) {
	date, _ := temporal.ParseDate("1984-10-11")
	path := engine.PathValue{
		Nodes:         []domain.Node{{Properties: domain.Properties{"score": math.NaN(), "date": date}}},
		Relationships: []domain.Edge{{Properties: domain.Properties{"weight": math.Inf(1), "nested": []any{date}}}},
	}
	batch := app.BatchResult{Results: []app.Result{{Rows: [][]any{{path}}}}}
	var output bytes.Buffer
	if err := Render(&output, "json", batch); err != nil {
		t.Fatal(err)
	}
	if score, ok := path.Nodes[0].Properties["score"].(float64); !ok || !math.IsNaN(score) {
		t.Fatalf("Render mutated path node property to %#v", path.Nodes[0].Properties["score"])
	}
	if weight, ok := path.Relationships[0].Properties["weight"].(float64); !ok || !math.IsInf(weight, 1) {
		t.Fatalf("Render mutated path relationship property to %#v", path.Relationships[0].Properties["weight"])
	}
	if _, ok := path.Nodes[0].Properties["date"].(temporal.Date); !ok {
		t.Fatalf("Render mutated path temporal property to %#v", path.Nodes[0].Properties["date"])
	}
	if got := strings.Count(output.String(), `"$date"`); got != 2 {
		t.Fatalf("nested path temporal envelope count = %d, output = %s", got, output.String())
	}
}

func TestRenderTableUsesTerminalCellWidths(t *testing.T) {
	batch := app.BatchResult{Results: []app.Result{{
		Columns: []string{"x", "value"},
		Rows:    [][]any{{"界", 1}, {"e\u0301", 2}},
	}}}
	var output bytes.Buffer
	if err := Render(&output, "table", batch); err != nil {
		t.Fatal(err)
	}
	want := "Statement 1\n" +
		"x  | value\n" +
		"-- | -----\n" +
		"界 | 1\n" +
		"e\u0301  | 2\n"
	if output.String() != want {
		t.Fatalf("table output =\n%q\nwant =\n%q", output.String(), want)
	}
}

func TestRenderTableEscapesTerminalPresentationControlsEverywhere(t *testing.T) {
	danger := "prefix\u0085\u202e\x1b[31msuffix"
	batch := app.BatchResult{Results: []app.Result{{
		Columns: []string{danger, "nested"},
		Rows:    [][]any{{danger, map[string]any{"value": danger}}},
	}}}
	var output bytes.Buffer
	if err := Render(&output, "table", batch); err != nil {
		t.Fatal(err)
	}
	for _, character := range output.String() {
		if character != '\n' && unsafeTableRune(character) {
			t.Fatalf("table output contains unsafe terminal rune U+%04X: %q", character, output.String())
		}
	}
	for _, escaped := range []string{`\u0085`, `\u202e`, `\x1b`} {
		if !strings.Contains(output.String(), escaped) {
			t.Errorf("table output lacks visible escape %q: %q", escaped, output.String())
		}
	}
}

func BenchmarkRenderTableLarge(b *testing.B) {
	rows := make([][]any, 10_000)
	for index := range rows {
		rows[index] = []any{int64(index), fmt.Sprintf("row-%05d", index), strings.Repeat("value", 8)}
	}
	batch := app.BatchResult{Results: []app.Result{{
		Columns: []string{"index", "name", "description"},
		Rows:    rows,
	}}}
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if err := Render(io.Discard, "table", batch); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkRenderJSONLarge(b *testing.B) {
	rows := make([][]any, 10_000)
	for index := range rows {
		rows[index] = []any{int64(index), fmt.Sprintf("row-%05d", index), strings.Repeat("value", 8)}
	}
	batch := app.BatchResult{Results: []app.Result{{
		Columns: []string{"index", "name", "description"},
		Rows:    rows,
	}}}
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if err := Render(io.Discard, "json", batch); err != nil {
			b.Fatal(err)
		}
	}
}
