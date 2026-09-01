package cli

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"reflect"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/charmbracelet/x/ansi"
	"github.com/svlocks/sheets/internal/app"
	"github.com/svlocks/sheets/internal/domain"
	"github.com/svlocks/sheets/internal/domain/temporal"
)

// Format is a supported command output format.
type Format string

const (
	FormatTable Format = "table"
	FormatJSON  Format = "json"
	FormatJSONL Format = "jsonl"
)

// ErrInvalidFormat indicates that a requested output format is unsupported.
var ErrInvalidFormat = errors.New("invalid output format")

// ParseFormat validates and normalizes a format name.
func ParseFormat(value string) (Format, error) {
	raw := value
	switch Format(strings.ToLower(strings.TrimSpace(raw))) {
	case FormatTable:
		return FormatTable, nil
	case FormatJSON:
		return FormatJSON, nil
	case FormatJSONL:
		return FormatJSONL, nil
	default:
		return "", fmt.Errorf("%w %q (want table, json, or jsonl)", ErrInvalidFormat, raw)
	}
}

// ValidateFormat reports whether value names a supported output format.
func ValidateFormat(value string) error {
	_, err := ParseFormat(value)
	return err
}

// Render writes one batch result in the requested format. Output is fully
// deterministic and does not inspect terminal state; callers choose the
// writer (stdout, a file, or a test buffer).
func Render(w io.Writer, format string, batch app.BatchResult) error {
	if w == nil {
		return errors.New("render output writer is nil")
	}
	parsed, err := ParseFormat(string(format))
	if err != nil {
		return err
	}

	// Rendering is incremental so large result sets do not require a second
	// whole-result buffer. A fixed-size output buffer avoids turning that into
	// one syscall for every JSON token or table row.
	buffered := bufio.NewWriterSize(w, 64<<10)
	switch parsed {
	case FormatTable:
		err = renderTable(buffered, batch)
	case FormatJSON:
		err = renderJSON(buffered, batch)
	case FormatJSONL:
		err = renderJSONL(buffered, batch)
	}
	if err == nil {
		err = buffered.Flush()
	}
	if err != nil {
		return fmt.Errorf("write %s output: %w", parsed, err)
	}
	return nil
}

func renderJSON(w io.Writer, batch app.BatchResult) error {
	stream := prettyJSONStream{writer: w}
	stream.write("{\n  \"results\": [")
	for statement, result := range batch.Results {
		if statement == 0 {
			stream.write("\n")
		} else {
			stream.write(",\n")
		}
		stream.write("    {\n      \"columns\": [")
		for index, column := range result.Columns {
			if index == 0 {
				stream.write("\n")
			} else {
				stream.write(",\n")
			}
			stream.write("        ")
			stream.value(column, 8)
		}
		if len(result.Columns) > 0 {
			stream.write("\n      ")
		}
		stream.write("],\n      \"rows\": [")
		for rowIndex, row := range result.Rows {
			if rowIndex == 0 {
				stream.write("\n")
			} else {
				stream.write(",\n")
			}
			stream.write("        [")
			for valueIndex, value := range row {
				if valueIndex == 0 {
					stream.write("\n")
				} else {
					stream.write(",\n")
				}
				stream.write("          ")
				stream.value(jsonValue(value), 10)
			}
			if len(row) > 0 {
				stream.write("\n        ")
			}
			stream.write("]")
		}
		if len(result.Rows) > 0 {
			stream.write("\n      ")
		}
		stream.write("],\n      \"summary\": ")
		stream.value(result.Summary, 6)
		if result.Page != nil {
			stream.write(",\n      \"page\": ")
			stream.value(result.Page, 6)
		}
		stream.write("\n    }")
	}
	if len(batch.Results) > 0 {
		stream.write("\n  ")
	}
	stream.write("]")
	if batch.Revision != nil {
		stream.write(",\n  \"revision\": ")
		stream.value(*batch.Revision, 2)
	}
	stream.write("\n}\n")
	return stream.err
}

type prettyJSONStream struct {
	writer io.Writer
	err    error
}

func (s *prettyJSONStream) write(value string) {
	if s.err != nil {
		return
	}
	written, err := io.WriteString(s.writer, value)
	if err == nil && written != len(value) {
		err = io.ErrShortWrite
	}
	s.err = err
}

func (s *prettyJSONStream) value(value any, indentation int) {
	if s.err != nil {
		return
	}
	encoded, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		s.err = err
		return
	}
	prefix := "\n" + strings.Repeat(" ", indentation)
	s.write(strings.ReplaceAll(string(encoded), "\n", prefix))
}

// JSONL records deliberately use a small envelope so a consumer can process
// rows without buffering a complete result. Statement indexes are zero-based.
type jsonlRow struct {
	Type      string   `json:"type"`
	Statement int      `json:"statement"`
	Columns   []string `json:"columns"`
	Values    []any    `json:"values"`
}

type jsonlResult struct {
	Type      string   `json:"type"`
	Statement int      `json:"statement"`
	Columns   []string `json:"columns"`
}

type jsonlSummary struct {
	Type      string      `json:"type"`
	Statement int         `json:"statement"`
	Summary   app.Summary `json:"summary"`
}

type jsonlRevision struct {
	Type     string          `json:"type"`
	Revision domain.Revision `json:"revision"`
}

type jsonlPage struct {
	Type      string          `json:"type"`
	Statement int             `json:"statement"`
	Page      domain.PageInfo `json:"page"`
}

func renderJSONL(w io.Writer, batch app.BatchResult) error {
	for statement, result := range batch.Results {
		columns := result.Columns
		if columns == nil {
			columns = []string{}
		}
		if len(result.Rows) == 0 {
			if err := writeJSONLine(w, jsonlResult{Type: "result", Statement: statement, Columns: columns}); err != nil {
				return err
			}
		} else {
			for _, row := range result.Rows {
				values := make([]any, len(row))
				for index, value := range row {
					values[index] = jsonValue(value)
				}
				if err := writeJSONLine(w, jsonlRow{
					Type: "row", Statement: statement, Columns: columns, Values: values,
				}); err != nil {
					return err
				}
			}
		}
		if result.Summary.Changed() {
			if err := writeJSONLine(w, jsonlSummary{Type: "summary", Statement: statement, Summary: result.Summary}); err != nil {
				return err
			}
		}
		if result.Page != nil && result.Page.Next != "" {
			if err := writeJSONLine(w, jsonlPage{Type: "page", Statement: statement, Page: *result.Page}); err != nil {
				return err
			}
		}
	}
	if batch.Revision != nil {
		if err := writeJSONLine(w, jsonlRevision{Type: "revision", Revision: *batch.Revision}); err != nil {
			return err
		}
	}
	return nil
}

// JSON has no representation for IEEE NaN or infinities. Cypher arithmetic
// can produce them, so machine formats use an explicit, unambiguous tagged
// object instead of failing after the query has already run.
func jsonValue(value any) any {
	return app.JSONValue(value)
}

func writeJSONLine(w io.Writer, record any) error {
	data, err := json.Marshal(record)
	if err != nil {
		return fmt.Errorf("encode JSONL record: %w", err)
	}
	data = append(data, '\n')
	written, err := w.Write(data)
	if err != nil {
		return fmt.Errorf("write JSONL output: %w", err)
	}
	if written != len(data) {
		return fmt.Errorf("write JSONL output: %w", io.ErrShortWrite)
	}
	return nil
}

func renderTable(w io.Writer, batch app.BatchResult) error {
	if len(batch.Results) == 0 {
		if _, err := io.WriteString(w, "No results.\n"); err != nil {
			return err
		}
	} else {
		for index, result := range batch.Results {
			if index > 0 {
				if _, err := io.WriteString(w, "\n"); err != nil {
					return err
				}
			}
			if _, err := fmt.Fprintf(w, "Statement %d\n", index+1); err != nil {
				return err
			}
			if err := renderTableResult(w, result); err != nil {
				return err
			}
			if result.Summary.Changed() {
				if err := renderSummary(w, result.Summary); err != nil {
					return err
				}
			}
			if result.Page != nil && result.Page.Next != "" {
				if _, err := fmt.Fprintf(w, "Next: %s\n", result.Page.Next); err != nil {
					return err
				}
			}
		}
	}
	if batch.Revision != nil {
		if _, err := fmt.Fprintf(w, "Revision: %d\n", *batch.Revision); err != nil {
			return err
		}
	}
	return nil
}

func renderTableResult(w io.Writer, result app.Result) error {
	columnCount := len(result.Columns)
	for _, row := range result.Rows {
		if len(row) > columnCount {
			columnCount = len(row)
		}
	}
	if columnCount == 0 {
		_, err := io.WriteString(w, "(no rows)\n")
		return err
	}

	columns := make([]string, columnCount)
	for index := range columns {
		if index < len(result.Columns) && result.Columns[index] != "" {
			columns[index] = safeTableString(result.Columns[index])
		} else {
			columns[index] = fmt.Sprintf("column_%d", index+1)
		}
	}
	widths := make([]int, columnCount)
	for index, column := range columns {
		widths[index] = displayWidth(column)
	}
	for _, row := range result.Rows {
		for column := range columnCount {
			value := tableCell(row, column)
			if width := displayWidth(value); width > widths[column] {
				widths[column] = width
			}
		}
	}

	if err := writeTableRow(w, columns, widths); err != nil {
		return err
	}
	separator := make([]string, len(widths))
	for index, width := range widths {
		separator[index] = strings.Repeat("-", width)
	}
	if err := writeTableRow(w, separator, widths); err != nil {
		return err
	}
	if len(result.Rows) == 0 {
		_, err := io.WriteString(w, "(no rows)\n")
		return err
	}
	rowText := make([]string, columnCount)
	for _, row := range result.Rows {
		for column := range rowText {
			rowText[column] = tableCell(row, column)
		}
		if err := writeTableRow(w, rowText, widths); err != nil {
			return err
		}
	}
	return nil
}

func tableCell(row []any, column int) string {
	if column >= len(row) {
		return "null"
	}
	return formatTableValue(row[column])
}

func writeTableRow(w io.Writer, cells []string, widths []int) error {
	var line strings.Builder
	for index, width := range widths {
		if index > 0 {
			line.WriteString(" | ")
		}
		cell := ""
		if index < len(cells) {
			cell = cells[index]
		}
		line.WriteString(cell)
		if index < len(widths)-1 {
			line.WriteString(strings.Repeat(" ", max(0, width-displayWidth(cell))))
		}
	}
	line.WriteByte('\n')
	_, err := io.WriteString(w, line.String())
	return err
}

func renderSummary(w io.Writer, summary app.Summary) error {
	entries := make([]string, 0, 9)
	for _, entry := range summaryEntries(summary) {
		if entry.value != 0 {
			entries = append(entries, fmt.Sprintf("%s=%d", entry.name, entry.value))
		}
	}
	_, err := fmt.Fprintf(w, "Summary: %s\n", strings.Join(entries, " "))
	return err
}

type summaryEntry struct {
	name  string
	value uint64
}

func summaryEntries(summary app.Summary) []summaryEntry {
	return []summaryEntry{
		{"nodes_created", summary.NodesCreated},
		{"nodes_updated", summary.NodesUpdated},
		{"nodes_deleted", summary.NodesDeleted},
		{"relationships_created", summary.RelationshipsCreated},
		{"relationships_updated", summary.RelationshipsUpdated},
		{"relationships_deleted", summary.RelationshipsDeleted},
		{"properties_set", summary.PropertiesSet},
		{"labels_added", summary.LabelsAdded},
		{"labels_removed", summary.LabelsRemoved},
	}
}

func formatTableValue(value any) string {
	if value == nil {
		return "null"
	}
	switch value := value.(type) {
	case string:
		return safeTableString(value)
	case bool:
		return strconv.FormatBool(value)
	case json.Number:
		return value.String()
	case int:
		return strconv.Itoa(value)
	case int8, int16, int32, int64:
		return strconv.FormatInt(reflect.ValueOf(value).Int(), 10)
	case uint, uint8, uint16, uint32, uint64, uintptr:
		return strconv.FormatUint(reflect.ValueOf(value).Uint(), 10)
	case float32:
		return formatFloat(float64(value), 32)
	case float64:
		return formatFloat(value, 64)
	case temporal.Date:
		return "date(" + value.String() + ")"
	case temporal.LocalTime:
		return "localtime(" + value.String() + ")"
	case temporal.Time:
		return "time(" + value.String() + ")"
	case temporal.LocalDateTime:
		return "localdatetime(" + value.String() + ")"
	case temporal.DateTime:
		return "datetime(" + value.String() + ")"
	case temporal.Duration:
		return "duration(" + value.String() + ")"
	case time.Time:
		return safeTableComposite("legacy_time(" + value.Format(time.RFC3339Nano) + "[" + value.Location().String() + "])")
	case time.Duration:
		return "legacy_duration(" + value.String() + ")"
	case []byte:
		return "bytes(" + base64.StdEncoding.EncodeToString(value) + ")"
	}

	// JSON gives maps, lists, and domain values a compact deterministic form
	// (encoding/json sorts string map keys).
	if encoded, err := json.Marshal(app.JSONValue(value)); err == nil {
		return safeTableComposite(string(encoded))
	}
	return safeTableString(fmt.Sprint(value))
}

func formatFloat(value float64, bits int) string {
	if math.IsNaN(value) {
		return "NaN"
	}
	if math.IsInf(value, 1) {
		return "+Inf"
	}
	if math.IsInf(value, -1) {
		return "-Inf"
	}
	return strconv.FormatFloat(value, 'g', -1, bits)
}

func safeTableString(value string) string {
	if value == "" {
		return `""`
	}
	needsQuote := strings.TrimSpace(value) != value || strings.Contains(value, "|")
	for _, character := range value {
		if unsafeTableRune(character) || character == utf8.RuneError {
			needsQuote = true
			break
		}
	}
	if needsQuote {
		return strconv.Quote(value)
	}
	return value
}

func safeTableComposite(value string) string {
	var result strings.Builder
	result.Grow(len(value))
	for _, character := range value {
		if !unsafeTableRune(character) {
			result.WriteRune(character)
			continue
		}
		switch {
		case character <= 0xff:
			fmt.Fprintf(&result, `\u%04x`, character)
		case character <= 0xffff:
			fmt.Fprintf(&result, `\u%04x`, character)
		default:
			fmt.Fprintf(&result, `\U%08x`, character)
		}
	}
	return result.String()
}

func unsafeTableRune(character rune) bool {
	if character < 0x20 || character == 0x7f || character >= 0x80 && character <= 0x9f {
		return true
	}
	return character == 0x061c || character == 0x200e || character == 0x200f ||
		character >= 0x202a && character <= 0x202e ||
		character >= 0x2066 && character <= 0x2069
}

func displayWidth(value string) int {
	return ansi.StringWidth(value)
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
