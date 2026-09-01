package tui

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/svlocks/sheets/internal/app"
	"github.com/svlocks/sheets/internal/domain"
	"github.com/svlocks/sheets/internal/domain/temporal"
)

type fakeSnapshot struct {
	nodes []domain.Node
	edges []domain.Edge
}

type fakeBackend struct {
	mu           sync.Mutex
	root         string
	revision     domain.Revision
	snapshots    map[domain.Revision]fakeSnapshot
	revisions    []domain.RevisionInfo
	requests     []app.ExecuteRequest
	currentReads int
	currentErr   error
	historyErr   error
	executeErr   error
	nextResult   app.BatchResult
}

func (b *fakeBackend) ProjectRoot() string {
	if b.root == "" {
		return "/tmp/example"
	}
	return b.root
}

func (b *fakeBackend) CurrentRevision(context.Context) (domain.Revision, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.currentReads++
	return b.revision, b.currentErr
}

func (b *fakeBackend) Revisions(context.Context) ([]domain.RevisionInfo, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]domain.RevisionInfo(nil), b.revisions...), b.historyErr
}

func (b *fakeBackend) Execute(_ context.Context, request app.ExecuteRequest) (app.BatchResult, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.requests = append(b.requests, request)
	if b.executeErr != nil {
		return app.BatchResult{}, b.executeErr
	}
	if request.Query != snapshotQuery {
		return b.nextResult, nil
	}
	revision := b.revision
	if request.Snapshot.Revision != nil {
		revision = *request.Snapshot.Revision
	}
	snapshot := b.snapshots[revision]
	nodeRows := make([][]any, len(snapshot.nodes))
	for index, node := range snapshot.nodes {
		nodeRows[index] = []any{node}
	}
	edgeRows := make([][]any, len(snapshot.edges))
	for index, edge := range snapshot.edges {
		edgeRows[index] = []any{edge}
	}
	return app.BatchResult{Results: []app.Result{
		{Columns: []string{"node"}, Rows: nodeRows},
		{Columns: []string{"relationship"}, Rows: edgeRows},
	}}, nil
}

func task(id, title string) domain.Node {
	return domain.Node{
		ID: domain.EntityID(id), Labels: []string{"Task"},
		Properties: domain.Properties{"title": title}, ValidFrom: 1,
	}
}

func child(id, from, to string, position *int64) domain.Edge {
	return domain.Edge{
		ID: domain.EntityID(id), From: domain.EntityID(from), To: domain.EntityID(to),
		Type: "CHILD", Position: position, ValidFrom: 1,
	}
}

func TestSnapshotLoadsThroughExecutorAtPinnedRevision(t *testing.T) {
	backend := &fakeBackend{
		revision: 7,
		snapshots: map[domain.Revision]fakeSnapshot{
			7: {nodes: []domain.Node{task("n1", "One")}, edges: []domain.Edge{{ID: "e1", From: "n1", To: "n1", Type: "RELATES_TO"}}},
		},
	}
	loaded, err := loadSnapshot(context.Background(), backend, domain.Snapshot{})
	if err != nil {
		t.Fatalf("loadSnapshot() error = %v", err)
	}
	if loaded.revision != 7 || len(loaded.graph.nodes) != 1 || len(loaded.graph.edges) != 1 {
		t.Fatalf("loaded = revision %d, %d nodes, %d edges", loaded.revision, len(loaded.graph.nodes), len(loaded.graph.edges))
	}
	if backend.currentReads != 1 || len(backend.requests) != 1 {
		t.Fatalf("backend calls = %d token reads, %d executor calls", backend.currentReads, len(backend.requests))
	}
	request := backend.requests[0]
	if !request.ReadOnly || request.Query != snapshotQuery || request.Snapshot.Revision == nil || *request.Snapshot.Revision != 7 {
		t.Fatalf("snapshot executor request = %#v", request)
	}
}

func TestHierarchyUsesOrderedThenUnorderedChildrenAtArbitraryDepth(t *testing.T) {
	zero, two := int64(0), int64(2)
	graph := newGraphState(
		[]domain.Node{task("root", "Root"), task("a", "Alpha"), task("b", "Beta"), task("c", "Charlie"), task("deep", "Deep")},
		[]domain.Edge{child("e-c", "root", "c", nil), child("e-b", "root", "b", &two), child("e-a", "root", "a", &zero), child("e-deep", "a", "deep", &zero)},
	)
	work := newWorkModel(makeStyles(true, true), true)
	work.setGraph(graph)
	view := work.view()
	alpha, beta, charlie, deep := strings.Index(view, "Alpha"), strings.Index(view, "Beta"), strings.Index(view, "Charlie"), strings.Index(view, "Deep")
	if alpha < 0 || alpha >= beta || beta >= charlie {
		t.Fatalf("ordered/unordered hierarchy rendered in wrong order:\n%s", view)
	}
	if deep < alpha || graph.parent["deep"].From != "a" {
		t.Fatalf("deep child was not retained:\n%s", view)
	}
	if !work.selectID("deep") || work.selected != "deep" {
		t.Fatalf("selectID(deep) selected %q", work.selected)
	}
}

func TestDeepWorkOutlineIsIterativeAndViewportBounded(t *testing.T) {
	const depth = 25_000
	nodes := make([]domain.Node, depth)
	edges := make([]domain.Edge, 0, depth-1)
	for index := range nodes {
		id := domain.EntityID(fmt.Sprintf("n-%05d", index))
		nodes[index] = task(string(id), fmt.Sprintf("Node %d", index))
		if index > 0 {
			edges = append(edges, child(
				fmt.Sprintf("e-%05d", index), string(nodes[index-1].ID), string(id), nil,
			))
		}
	}
	graph := newGraphState(nodes, edges)
	work := newWorkModel(makeStyles(true, true), true)
	work.setSize(60, 12)
	work.setGraph(graph)
	deepest := nodes[len(nodes)-1].ID
	if !work.selectID(deepest) || work.selected != deepest {
		t.Fatalf("deepest selection = %q, want %q", work.selected, deepest)
	}
	if got := strings.Count(work.view(), "\n") + 1; got > work.height {
		t.Fatalf("deep outline rendered %d lines into a %d-line viewport", got, work.height)
	}
	if descendants := graph.descendants(nodes[0].ID); len(descendants) != depth-1 {
		t.Fatalf("root descendant count = %d, want %d", len(descendants), depth-1)
	}
}

func TestUntrustedTerminalTextCannotInjectControls(t *testing.T) {
	malicious := "safe\x1b]52;c;payload\a\u202etxt\xff\nnext"
	line := terminalLine(malicious)
	if !utf8.ValidString(line) {
		t.Fatalf("sanitized line is invalid UTF-8: %q", line)
	}
	for _, control := range []string{"\x1b", "\a", "\u202e"} {
		if strings.Contains(line, control) {
			t.Fatalf("sanitized line retains terminal control %q: %q", control, line)
		}
	}
	for _, visible := range []string{`\x1b`, `\x07`, `\u202e`, `\uFFFD`, "↵"} {
		if !strings.Contains(line, visible) {
			t.Errorf("sanitized line %q lacks visible marker %q", line, visible)
		}
	}
	if got := queryCell(malicious); strings.ContainsRune(got, '\x1b') || !utf8.ValidString(got) {
		t.Fatalf("query cell is unsafe: %q", got)
	}
	block := terminalBlock("one\n\x1b[31mtwo")
	if !strings.Contains(block, "one\n") || strings.ContainsRune(block, '\x1b') {
		t.Fatalf("terminal block did not preserve only safe structure: %q", block)
	}
}

func TestWriteQueryPreviewTruncatesOnRuneBoundary(t *testing.T) {
	preview := truncateRunes(strings.Repeat("界", 601), 600)
	if !utf8.ValidString(preview) || utf8.RuneCountInString(preview) != 601 || !strings.HasSuffix(preview, "…") {
		t.Fatalf("truncated preview = %q, runes=%d", preview, utf8.RuneCountInString(preview))
	}
}

func TestExactEditFieldsEscapeTerminalControlsWithoutChangingStoredValues(t *testing.T) {
	node := task("unsafe", "placeholder")
	originalTitle := "title\u202e"
	originalLabel := "label\u0085"
	originalBody := "line\n\x1b[31m\u202ebody"
	originalProperty := "property\u2066value"
	node.Properties["title"] = originalTitle
	node.Properties["note"] = originalProperty
	node.Labels = []string{originalLabel}
	node.Body = originalBody

	form := newEditNodeForm(1, node, 80, 24, true, true)
	data := form.data.(*nodeFormData)
	for name, value := range map[string]string{
		"title": data.title, "labels": data.labels, "properties": data.properties, "body": data.body,
	} {
		for _, character := range value {
			if character != '\n' && character != '\t' && isTerminalControl(character) {
				t.Fatalf("%s editor contains unsafe rune U+%04X: %q", name, character, value)
			}
		}
	}
	request, err := form.request()
	if err != nil {
		t.Fatal(err)
	}
	properties := request.Params["properties"].(domain.Properties)
	if properties["title"] != originalTitle || properties["note"] != originalProperty || request.Params["body"] != originalBody {
		t.Fatalf("unchanged exact values were altered: properties=%#v body=%#v", properties, request.Params["body"])
	}
	if !reflectStringSlicesEqual(request.Query, node.Labels, request.Params) {
		t.Fatalf("unchanged labels were altered: query=%s params=%#v", request.Query, request.Params)
	}
}

func reflectStringSlicesEqual(query string, labels []string, _ map[string]any) bool {
	for _, label := range labels {
		if !strings.Contains(query, cypherIdentifierName(label)) {
			return false
		}
	}
	return true
}

func TestTextInputMakesPastedPresentationControlsVisible(t *testing.T) {
	model := NewModel(context.Background(), &fakeBackend{}, WithPollInterval(0), WithNoColor(true))
	model.workspace = QueryWorkspace
	_, _ = model.Update(tea.KeyPressMsg{Code: '\u202e', Text: "\u202e"})
	value := model.query.cypher.Value()
	if strings.ContainsRune(value, '\u202e') || !strings.Contains(value, `\u202e`) {
		t.Fatalf("query editor retained an unsafe pasted control: %q", value)
	}
}

func TestQueryPresentationCopiesAreBounded(t *testing.T) {
	rows := make([][]any, maxQueryTableRows+1)
	for index := range rows {
		rows[index] = []any{strings.Repeat("x", maxQueryCellWidth*4)}
	}
	query := newQueryModel(makeStyles(true, true), true)
	query.setSize(300, 30)
	query.setResult(app.BatchResult{Results: []app.Result{{Columns: []string{"value"}, Rows: rows}}}, nil)
	if got := len(query.table.Rows()); got != maxQueryTableRows {
		t.Fatalf("materialized table rows = %d, want %d", got, maxQueryTableRows)
	}
	if width := ansi.StringWidth(query.table.Rows()[0][0]); width > maxQueryCellWidth {
		t.Fatalf("materialized cell width = %d, want <= %d", width, maxQueryCellWidth)
	}
	resultView, _ := query.resultViews()
	if !strings.Contains(resultView, "showing first 10000/10001 rows") {
		t.Fatalf("bounded-result disclosure is missing: %q", resultView)
	}
}

func TestQueryPresentationBoundsRowsTimesColumnsAndColumnTitles(t *testing.T) {
	columns := make([]string, maxQueryTableColumns+44)
	for index := range columns {
		columns[index] = fmt.Sprintf("column-%d-%s", index, strings.Repeat("x", maxQueryCellWidth*4))
	}
	rows := make([][]any, maxQueryTableRows)
	query := newQueryModel(makeStyles(true, true), true)
	query.setSize(2_000, 30)
	result := app.Result{Columns: columns, Rows: rows}
	query.setResult(app.BatchResult{Results: []app.Result{result}}, nil)
	wantRows, wantColumns := queryDisplayShape(result)
	if got := len(query.table.Rows()); got != wantRows || wantRows*wantColumns > maxQueryTableCells {
		t.Fatalf("materialized shape = %dx%d, want %dx%d within %d cells", got, len(query.table.Columns()), wantRows, wantColumns, maxQueryTableCells)
	}
	if got := len(query.table.Columns()); got != wantColumns {
		t.Fatalf("materialized columns = %d, want %d", got, wantColumns)
	}
	if width := ansi.StringWidth(query.table.Columns()[0].Title); width > maxQueryCellWidth {
		t.Fatalf("column title width = %d, want <= %d", width, maxQueryCellWidth)
	}
	resultView, _ := query.resultViews()
	if !strings.Contains(resultView, fmt.Sprintf("first %d/%d columns", wantColumns, len(columns))) {
		t.Fatalf("column-bound disclosure is missing: %q", resultView)
	}
}

func TestHierarchyAndSearchPresentationDoNotCopyHugeFields(t *testing.T) {
	node := task("large", strings.Repeat("界", maxNodeTitleRunes*10))
	node.Labels = []string{strings.Repeat("label", maxDisplayLabelRunes*10)}
	node.Properties["payload"] = strings.Repeat("p", maxSearchTextBytes*100)
	node.Body = strings.Repeat("b", maxSearchTextBytes*100)
	if got := utf8.RuneCountInString(nodeTitle(node)); got > maxNodeTitleRunes+1 {
		t.Fatalf("node title contains %d runes, want <= %d", got, maxNodeTitleRunes+1)
	}
	if search := nodeSearchValue(node); len(search) > maxSearchTextBytes {
		t.Fatalf("search copy has %d bytes, want <= %d", len(search), maxSearchTextBytes)
	}
}

func TestPollDoesNotReloadUntilRevisionTokenChanges(t *testing.T) {
	backend := &fakeBackend{revision: 4, snapshots: map[domain.Revision]fakeSnapshot{4: {}}}
	model := NewModel(context.Background(), backend, WithPollInterval(0), WithNoColor(true))
	model.liveRevision = 4
	initialSerial := model.loadSeq
	if cmd := model.applyRevisionChecked(revisionCheckedMsg{revision: 4}); cmd != nil {
		// A disabled poll interval produces a nil Batch command.
		_ = cmd
	}
	if model.loadSeq != initialSerial || len(backend.requests) != 0 {
		t.Fatalf("unchanged token started graph load: serial %d -> %d, requests %d", initialSerial, model.loadSeq, len(backend.requests))
	}
	_ = model.applyRevisionChecked(revisionCheckedMsg{revision: 5})
	if model.loadSeq != initialSerial+1 || !model.loadingGraph {
		t.Fatalf("changed token did not schedule load: serial %d, loading %v", model.loadSeq, model.loadingGraph)
	}
	if len(backend.requests) != 0 {
		t.Fatal("async graph query ran inside Update instead of its command")
	}
}

func TestStaleSnapshotResultIsIgnored(t *testing.T) {
	model := NewModel(context.Background(), &fakeBackend{}, WithPollInterval(0), WithNoColor(true))
	model.loadSeq = 2
	oldGraph := newGraphState([]domain.Node{task("old", "Old")}, nil)
	_ = model.applySnapshotLoaded(snapshotLoadedMsg{serial: 1, loaded: snapshotLoad{graph: oldGraph, revision: 1}})
	if len(model.graph.nodes) != 0 {
		t.Fatal("stale snapshot mutated the model")
	}
	newGraph := newGraphState([]domain.Node{task("new", "New")}, nil)
	_ = model.applySnapshotLoaded(snapshotLoadedMsg{serial: 2, loaded: snapshotLoad{graph: newGraph, revision: 2}})
	if _, ok := model.graph.nodeByID["new"]; !ok {
		t.Fatal("current snapshot was not applied")
	}
}

func TestHistoricalModeIsProminentAndBlocksMutationForms(t *testing.T) {
	model := NewModel(context.Background(), &fakeBackend{}, WithPollInterval(0), WithNoColor(true))
	revision := domain.Revision(3)
	model.loadSeq = 1
	graph := newGraphState([]domain.Node{task("n1", "One")}, nil)
	_ = model.applySnapshotLoaded(snapshotLoadedMsg{serial: 1, snapshot: domain.Snapshot{Revision: &revision}, loaded: snapshotLoad{graph: graph, revision: revision}})
	_ = model.openCreateForm()
	if model.overlay.kind == overlayForm {
		t.Fatal("historical mode opened a mutation form")
	}
	if !strings.Contains(model.notice.text, "read-only") {
		t.Fatalf("notice = %q", model.notice.text)
	}
	view := model.View().Content
	if !strings.Contains(view, "READ-ONLY HISTORY") || !strings.Contains(view, "return to Live") {
		t.Fatalf("historical banner missing:\n%s", view)
	}
}

func TestCreateFormBuildsParameterizedCypher(t *testing.T) {
	graph := newGraphState([]domain.Node{task("parent", "Parent")}, nil)
	form := newCreateNodeForm(1, graph, "parent", 80, 24, true, true)
	data := form.data.(*nodeFormData)
	data.title = "Ship it"
	data.labels = "Task, Needs Review"
	data.properties = `{"priority": 2, "nested": {"ok": true}}`
	data.position = "4"
	data.body = "# Notes"
	request, err := form.request()
	if err != nil {
		t.Fatalf("request() error = %v", err)
	}
	if strings.Contains(request.Query, "Ship it") || !strings.Contains(request.Query, "$properties") || !strings.Contains(request.Query, "[:CHILD {position: $position}]") {
		t.Fatalf("query was not safely parameterized: %s", request.Query)
	}
	if got := request.Params["position"]; got != int64(4) {
		t.Fatalf("position param = %#v", got)
	}
	properties := request.Params["properties"].(domain.Properties)
	if properties["title"] != "Ship it" || properties["priority"] != int64(2) {
		t.Fatalf("properties = %#v", properties)
	}
	if !strings.Contains(request.Query, ":`Needs Review`") {
		t.Fatalf("label was not escaped: %s", request.Query)
	}
}

func TestPropertyFormsPreserveTypedNestedValuesAndCLIIntegerRules(t *testing.T) {
	zone := time.FixedZone("Audit/Offset", -6*60*60)
	when := time.Date(2026, time.August, 31, 12, 34, 56, 789, zone)
	date, _ := temporal.ParseDate("1984-10-11")
	localTime, _ := temporal.ParseLocalTime("12:31:14.645876123")
	offsetTime, _ := temporal.ParseTime("12:31:14.645876123+01:00")
	localDateTime := temporal.NewLocalDateTime(date, localTime)
	dateTime, _ := temporal.NewDateTime(localDateTime, "Europe/Stockholm")
	cypherDuration, _ := temporal.NewDuration(-7, 14, -4, 500_000_000)
	node := task("typed", "Typed")
	node.Properties["finite_float"] = float64(1)
	node.Properties["nan"] = math.NaN()
	node.Properties["when"] = when
	node.Properties["duration"] = 90 * time.Minute
	node.Properties["bytes"] = []byte{0, 1, 2, 255}
	node.Properties["date"] = date
	node.Properties["local_time"] = localTime
	node.Properties["offset_time"] = offsetTime
	node.Properties["local_datetime"] = localDateTime
	node.Properties["zoned_datetime"] = dateTime
	node.Properties["cypher_duration"] = cypherDuration
	node.Properties["position"] = "node-local-position"
	node.Properties["nested"] = map[string]any{"values": []any{math.Inf(1), when}}

	form := newEditNodeForm(1, node, 80, 24, true, true)
	data := form.data.(*nodeFormData)
	if !strings.Contains(data.properties, `"$float": "+Infinity"`) || !strings.Contains(data.properties, `"$float": "NaN"`) {
		t.Fatalf("editable JSON lost non-finite values:\n%s", data.properties)
	}
	for _, tag := range []string{"$date", "$local_time", "$offset_time", "$local_datetime", "$zoned_datetime", "$cypher_duration", "$legacy_time", "$legacy_duration", "$bytes"} {
		if !strings.Contains(data.properties, `"`+tag+`"`) {
			t.Fatalf("editable JSON lost %s:\n%s", tag, data.properties)
		}
	}
	request, err := form.request()
	if err != nil {
		t.Fatalf("typed edit request error = %v", err)
	}
	properties := request.Params["properties"].(domain.Properties)
	if _, exists := properties["position"]; exists || request.Params["node_position"] != "node-local-position" || !strings.Contains(request.Query, "n.position = $node_position") {
		t.Fatalf("node position was not preserved explicitly: query=%s params=%#v", request.Query, request.Params)
	}
	if value, ok := properties["finite_float"].(float64); !ok || value != 1 {
		t.Fatalf("finite float changed type/value: %#v (%T)", properties["finite_float"], properties["finite_float"])
	}
	if value, ok := properties["nan"].(float64); !ok || !math.IsNaN(value) {
		t.Fatalf("NaN did not round-trip: %#v", properties["nan"])
	}
	if value, ok := properties["when"].(time.Time); !ok || !value.Equal(when) || value.Location().String() != when.Location().String() {
		t.Fatalf("temporal value did not round-trip: %#v", properties["when"])
	}
	if value, ok := properties["duration"].(time.Duration); !ok || value != 90*time.Minute {
		t.Fatalf("duration did not round-trip: %#v (%T)", properties["duration"], properties["duration"])
	}
	if value, ok := properties["bytes"].([]byte); !ok || !bytes.Equal(value, []byte{0, 1, 2, 255}) {
		t.Fatalf("bytes did not round-trip: %#v (%T)", properties["bytes"], properties["bytes"])
	}
	if value, ok := properties["date"].(temporal.Date); !ok || !value.Equal(date) {
		t.Fatalf("date did not round-trip: %#v", properties["date"])
	}
	if value, ok := properties["local_time"].(temporal.LocalTime); !ok || !value.Equal(localTime) {
		t.Fatalf("local time did not round-trip: %#v", properties["local_time"])
	}
	if value, ok := properties["offset_time"].(temporal.Time); !ok || !value.Equal(offsetTime) {
		t.Fatalf("offset time did not round-trip: %#v", properties["offset_time"])
	}
	if value, ok := properties["local_datetime"].(temporal.LocalDateTime); !ok || !value.Equal(localDateTime) {
		t.Fatalf("local date-time did not round-trip: %#v", properties["local_datetime"])
	}
	if value, ok := properties["zoned_datetime"].(temporal.DateTime); !ok || !value.Equal(dateTime) {
		t.Fatalf("zoned date-time did not round-trip: %#v", properties["zoned_datetime"])
	}
	if value, ok := properties["cypher_duration"].(temporal.Duration); !ok || !value.Equal(cypherDuration) {
		t.Fatalf("Cypher duration did not round-trip: %#v", properties["cypher_duration"])
	}
	nested := properties["nested"].(domain.Properties)["values"].([]any)
	if value, ok := nested[0].(float64); !ok || !math.IsInf(value, 1) {
		t.Fatalf("nested infinity did not round-trip: %#v", nested)
	}
	if _, ok := nested[1].(time.Time); !ok {
		t.Fatalf("nested temporal value changed type: %#v", nested[1])
	}

	edge := domain.Edge{ID: "edge", From: "a", To: "b", Type: "REL", Properties: domain.Properties{"body": "ordinary edge property"}}
	edgeForm := newEditRelationshipForm(2, edge, 80, 24, true, true)
	edgeRequest, err := edgeForm.request()
	if err != nil {
		t.Fatalf("edge body request error = %v", err)
	}
	if edgeRequest.Params["relationship_body"] != "ordinary edge property" || !strings.Contains(edgeRequest.Query, "r.body = $relationship_body") {
		t.Fatalf("edge body was silently dropped: query=%s params=%#v", edgeRequest.Query, edgeRequest.Params)
	}

	if _, err := decodeProperties(`{"value":9223372036854775808}`); err == nil || !strings.Contains(err.Error(), "signed 64-bit") {
		t.Fatalf("overflowing JSON integer error = %v", err)
	}
	if _, err := decodeNodeProperties(`{"body":"ambiguous"}`); err == nil {
		t.Fatal("node properties accepted reserved body")
	}
	if _, err := decodeRelationshipProperties(`{"position":1}`); err == nil {
		t.Fatal("relationship properties accepted reserved position")
	}
	if _, err := decodeProperties(`{"x":1,"\u0078":2}`); err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("duplicate property key error = %v", err)
	}
	params, err := decodeParams(`{"value":{"$float":"NaN"}}`)
	if err != nil {
		t.Fatalf("decodeParams() error = %v", err)
	}
	if _, ok := params["value"].(map[string]any); !ok {
		t.Fatalf("TUI console reinterpreted plain CLI JSON parameter: %#v", params["value"])
	}
	typedParams, err := decodeParams(prettyJSON(domain.Properties{"date": date, "nested": []any{dateTime, cypherDuration}}))
	if err != nil {
		t.Fatalf("typed decodeParams() error = %v", err)
	}
	if _, ok := typedParams["date"].(temporal.Date); !ok {
		t.Fatalf("typed date parameter = %#v", typedParams["date"])
	}
	for _, test := range []struct {
		value any
		want  string
	}{
		{date, "date(1984-10-11)"}, {localTime, "localtime(12:31:14.645876123)"},
		{offsetTime, "time(12:31:14.645876123+01:00)"},
		{localDateTime, "localdatetime(1984-10-11T12:31:14.645876123)"},
		{dateTime, "datetime(1984-10-11T12:31:14.645876123+01:00[Europe/Stockholm])"},
		{cypherDuration, "duration(P-7M14DT-3.5S)"},
	} {
		if got := queryCell(test.value); got != test.want {
			t.Errorf("queryCell(%T) = %q, want %q", test.value, got, test.want)
		}
	}
}

func TestMoveAndRelationshipRequestsPreserveGraphSemantics(t *testing.T) {
	position := int64(1)
	graph := newGraphState(
		[]domain.Node{task("a", "A"), task("b", "B"), task("c", "C")},
		[]domain.Edge{child("old", "a", "b", &position)},
	)
	move := newMoveNodeForm(1, graph, graph.nodeByID["b"], 80, 24, true, true)
	data := move.data.(*moveFormData)
	data.parent = "c"
	data.position = ""
	request, err := move.request()
	if err != nil {
		t.Fatalf("move request error = %v", err)
	}
	if !strings.Contains(request.Query, "DELETE old") || !strings.Contains(request.Query, "CREATE (p)-[:CHILD]->(n)") {
		t.Fatalf("move is not atomic: %s", request.Query)
	}
	parentMatch, deleteOld := strings.Index(request.Query, "MATCH (p)"), strings.Index(request.Query, "DELETE old")
	if parentMatch < 0 || deleteOld < 0 || parentMatch > deleteOld {
		t.Fatalf("move deletes before resolving its destination: %s", request.Query)
	}

	unchanged := newMoveNodeForm(2, graph, graph.nodeByID["b"], 80, 24, true, true)
	unchangedRequest, err := unchanged.request()
	if err != nil {
		t.Fatalf("unchanged move request error = %v", err)
	}
	if strings.Contains(unchangedRequest.Query, "DELETE") || strings.Contains(unchangedRequest.Query, "CREATE") || strings.Contains(unchangedRequest.Query, " SET ") {
		t.Fatalf("unchanged move would create a revision: %s", unchangedRequest.Query)
	}
	reorder := newMoveNodeForm(3, graph, graph.nodeByID["b"], 80, 24, true, true)
	reorder.data.(*moveFormData).position = ""
	reorderRequest, err := reorder.request()
	if err != nil {
		t.Fatalf("unordered move request error = %v", err)
	}
	if !strings.Contains(reorderRequest.Query, "SET old.position = $position") || reorderRequest.Params["position"] != nil {
		t.Fatalf("ordered-to-unordered move = %s %#v", reorderRequest.Query, reorderRequest.Params)
	}

	connect := newConnectionForm(2, graph, "a", 80, 24, true, true)
	connection := connect.data.(*connectionFormData)
	connection.to = "c"
	connection.typeName = "BLOCKED BY"
	connection.properties = `{"reason":"waiting"}`
	relationshipRequest, err := connect.request()
	if err != nil {
		t.Fatalf("relationship request error = %v", err)
	}
	if !strings.Contains(relationshipRequest.Query, "[r:`BLOCKED BY`]") {
		t.Fatalf("relationship type was not escaped: %s", relationshipRequest.Query)
	}
	connection.typeName = "child"
	if _, err := connect.request(); err == nil {
		t.Fatal("generic relationship form accepted CHILD")
	}
}

func TestDestructiveFormsRequireConfirmation(t *testing.T) {
	graph := newGraphState([]domain.Node{task("n", "Node")}, nil)
	form := newDeleteNodeForm(1, graph, graph.nodeByID["n"], 80, 20, true, true)
	if _, err := form.request(); err == nil {
		t.Fatal("delete request succeeded without confirmation")
	}
	form.data.(*confirmFormData).confirmed = true
	request, err := form.request()
	if err != nil {
		t.Fatalf("confirmed request error = %v", err)
	}
	if request.Query != "MATCH (n) WHERE elementId(n) = $id DETACH DELETE n" {
		t.Fatalf("delete query = %q", request.Query)
	}
}

func TestQueryReadRunsDirectlyButWriteRequiresConfirmation(t *testing.T) {
	backend := &fakeBackend{}
	model := NewModel(context.Background(), backend, WithPollInterval(0), WithNoColor(true))
	model.workspace = QueryWorkspace
	model.query.cypher.SetValue("RETURN $value")
	model.query.params.SetValue(`{"value": 1}`)
	cmd := model.runQuery(true)
	if cmd == nil || model.pending == nil || !model.pending.request.ReadOnly {
		t.Fatal("read-only query was not scheduled directly")
	}
	if model.pending.request.Params["value"] != int64(1) {
		t.Fatalf("params = %#v", model.pending.request.Params)
	}
	model.executing = false
	model.pending = nil
	_ = model.runQuery(false)
	if model.overlay.kind != overlayForm || model.form == nil || model.form.purpose != formExecuteQuery {
		t.Fatal("write-capable query did not open a confirmation form")
	}
	if len(backend.requests) != 0 {
		t.Fatal("query executed before its asynchronous command / confirmation")
	}
}

func TestFocusAndBackBehavior(t *testing.T) {
	model := NewModel(context.Background(), &fakeBackend{}, WithPollInterval(0), WithNoColor(true))
	_, _ = model.Update(keyPress("tab"))
	if model.focus != focusInspector {
		t.Fatalf("Tab focus = %v", model.focus)
	}
	_, _ = model.Update(keyPress("esc"))
	if model.focus != focusPrimary {
		t.Fatalf("Esc focus = %v", model.focus)
	}
	_ = model.openCreateForm()
	if model.overlay.kind != overlayForm {
		t.Fatal("create form did not open")
	}
	_, _ = model.Update(keyPress("esc"))
	if model.overlay.kind != overlayNone || model.form != nil {
		t.Fatal("Esc did not cancel visible form")
	}
}

func TestResponsiveNoColorRenderingAndTinyState(t *testing.T) {
	model := NewModel(context.Background(), &fakeBackend{root: "/tmp/demo"}, WithPollInterval(0), WithNoColor(true))
	model.loadSeq = 1
	graph := newGraphState([]domain.Node{task("n", "A task")}, nil)
	_ = model.applySnapshotLoaded(snapshotLoadedMsg{serial: 1, loaded: snapshotLoad{graph: graph, revision: 1}})
	for _, size := range []struct{ width, height int }{{60, 18}, {140, 40}} {
		_, _ = model.Update(tea.WindowSizeMsg{Width: size.width, Height: size.height})
		view := model.View().Content
		if containsANSIColor(view) {
			t.Fatalf("NO_COLOR output contains ANSI color at %dx%d: %q", size.width, size.height, view)
		}
		lines := strings.Split(view, "\n")
		if len(lines) != size.height {
			t.Fatalf("rendered %d lines at height %d", len(lines), size.height)
		}
		for lineNumber, line := range lines {
			if ansi.StringWidth(line) != size.width {
				t.Fatalf("line %d width = %d, want %d: %q", lineNumber, ansi.StringWidth(line), size.width, line)
			}
		}
	}
	for workspace := WorkWorkspace; workspace <= TimelineWorkspace; workspace++ {
		model.workspace = workspace
		_ = model.layoutComponents()
		if view := model.View().Content; containsANSIColor(view) {
			t.Fatalf("NO_COLOR workspace %s contains ANSI color: %q", workspace, view)
		} else if workspace == QueryWorkspace && strings.Contains(view, "\n|…") {
			t.Fatalf("query layout introduced truncation-only rows:\n%s", view)
		}
	}
	model.workspace = WorkWorkspace
	_ = model.openCreateForm()
	if view := model.View().Content; containsANSIColor(view) {
		t.Fatalf("NO_COLOR Huh form contains ANSI color: %q", view)
	}
	model.form = nil
	model.overlay.kind = overlayNone
	_, _ = model.Update(tea.WindowSizeMsg{Width: 32, Height: 8})
	if view := model.View().Content; !strings.Contains(view, "Resize") {
		t.Fatalf("tiny state missing resize guidance:\n%s", view)
	}
}

var ansiColorPattern = regexp.MustCompile(`\x1b\[(?:3[0-9]|4[0-9]|9[0-7]|10[0-7]|38;|48;)`)

func containsANSIColor(value string) bool { return ansiColorPattern.MatchString(value) }

func TestNoColorActiveWorkspaceHasStructuralMarker(t *testing.T) {
	model := NewModel(context.Background(), &fakeBackend{}, WithPollInterval(0), WithNoColor(true))
	header := strings.Split(model.renderHeader(), "\n")[0]
	if !strings.Contains(header, "[F1 Work]") {
		t.Fatalf("active no-color tab is not distinguishable: %q", header)
	}
}

func TestFinderUsesBubblesFilteringAndOpensSelectedNode(t *testing.T) {
	model := NewModel(context.Background(), &fakeBackend{}, WithPollInterval(0), WithNoColor(true))
	model.graph = newGraphState([]domain.Node{task("a", "Alpha"), task("b", "Beta")}, nil)
	model.work.setGraph(model.graph)
	_ = model.openFinder()
	if !model.overlay.picker.filtering() {
		t.Fatal("finder did not start in Bubbles filtering state")
	}
	_, _ = model.Update(keyPress("b"))
	if model.overlay.picker.list.FilterInput.Value() != "b" {
		t.Fatalf("finder filter = %q", model.overlay.picker.list.FilterInput.Value())
	}
}

func TestDecliningConfirmationClosesFormWithoutExecution(t *testing.T) {
	model := NewModel(context.Background(), &fakeBackend{}, WithPollInterval(0), WithNoColor(true))
	graph := newGraphState([]domain.Node{task("n", "Node")}, nil)
	model.graph = graph
	model.work.setGraph(graph)
	model.formSeq = 1
	model.form = newDeleteNodeForm(1, graph, graph.nodeByID["n"], 80, 20, true, true)
	model.overlay.kind = overlayForm
	_ = model.submitForm(formSubmittedMsg{serial: 1})
	if model.form != nil || model.overlay.kind != overlayNone || model.executing {
		t.Fatal("declined confirmation did not cancel cleanly")
	}
}

func TestSelectedRowPreservesDuplicateColumnNames(t *testing.T) {
	model := newQueryModel(makeStyles(true, true), true)
	model.setResult(app.BatchResult{Results: []app.Result{{
		Columns: []string{"value", "value"}, Rows: [][]any{{"first", "second"}},
	}}}, nil)
	detail := model.rowViewport.GetContent()
	if !strings.Contains(detail, "1. value") || !strings.Contains(detail, "first") || !strings.Contains(detail, "2. value") || !strings.Contains(detail, "second") {
		t.Fatalf("duplicate columns were not preserved:\n%s", detail)
	}
}

func TestEmptyAndErrorStatesAreActionable(t *testing.T) {
	model := NewModel(context.Background(), &fakeBackend{}, WithPollInterval(0), WithNoColor(true))
	model.loadSeq = 1
	_ = model.applySnapshotLoaded(snapshotLoadedMsg{serial: 1, loaded: snapshotLoad{graph: newGraphState(nil, nil)}})
	if view := model.View().Content; !strings.Contains(view, "No work items yet") || !strings.Contains(view, "create") {
		t.Fatalf("empty state is not actionable:\n%s", view)
	}
	model.loadSeq = 2
	_ = model.applySnapshotLoaded(snapshotLoadedMsg{serial: 2, err: errors.New("database unavailable")})
	if !strings.Contains(model.notice.text, "database unavailable") {
		t.Fatalf("load error notice = %q", model.notice.text)
	}
}

func TestMouseCanSwitchVisibleWorkspaceTabs(t *testing.T) {
	model := NewModel(context.Background(), &fakeBackend{}, WithPollInterval(0), WithNoColor(true))
	start := ansi.StringWidth(model.headerPrefix())
	firstWidth := ansi.StringWidth(model.renderTab(WorkWorkspace))
	click := tea.MouseClickMsg{X: start + firstWidth + 1, Y: 0, Button: tea.MouseLeft}
	_, _ = model.Update(click)
	if model.workspace != RelationshipsWorkspace {
		t.Fatalf("mouse selected workspace %v", model.workspace)
	}
}

func TestQueryResultKeepsFullSelectedRowInViewport(t *testing.T) {
	model := newQueryModel(makeStyles(true, true), true)
	model.setSize(100, 24)
	model.setResult(app.BatchResult{Results: []app.Result{{
		Columns: []string{"name", "payload"},
		Rows:    [][]any{{"first", map[string]any{"long": strings.Repeat("x", 100)}}},
	}}}, nil)
	if !strings.Contains(model.rowViewport.GetContent(), strings.Repeat("x", 100)) {
		t.Fatalf("selected row detail was truncated: %s", model.rowViewport.GetContent())
	}
	if len(model.table.Columns()) != 2 {
		t.Fatalf("table columns = %#v", model.table.Columns())
	}
}

func TestInspectorRejectsStaleRender(t *testing.T) {
	inspector := newInspectorModel(true, true)
	first := inspector.clear("first")
	firstMessage := first().(inspectorRenderedMsg)
	second := inspector.clear("second")
	secondMessage := second().(inspectorRenderedMsg)
	inspector.apply(firstMessage)
	if inspector.rendered != "" {
		t.Fatal("stale inspector render was applied")
	}
	inspector.apply(secondMessage)
	if !strings.Contains(inspector.rendered, "second") {
		t.Fatalf("current inspector render = %q", inspector.rendered)
	}
}

func keyPress(value string) tea.KeyPressMsg {
	switch value {
	case "tab":
		return tea.KeyPressMsg{Code: tea.KeyTab}
	case "esc":
		return tea.KeyPressMsg{Code: tea.KeyEscape}
	case "enter":
		return tea.KeyPressMsg{Code: tea.KeyEnter}
	case "ctrl+r":
		return tea.KeyPressMsg{Code: 'r', Mod: tea.ModCtrl}
	case "ctrl+pgup":
		return tea.KeyPressMsg{Code: tea.KeyPgUp, Mod: tea.ModCtrl}
	case "ctrl+pgdown":
		return tea.KeyPressMsg{Code: tea.KeyPgDown, Mod: tea.ModCtrl}
	case "f1":
		return tea.KeyPressMsg{Code: tea.KeyF1}
	case "f2":
		return tea.KeyPressMsg{Code: tea.KeyF2}
	case "f3":
		return tea.KeyPressMsg{Code: tea.KeyF3}
	case "f4":
		return tea.KeyPressMsg{Code: tea.KeyF4}
	case "f10":
		return tea.KeyPressMsg{Code: tea.KeyF10}
	default:
		return tea.KeyPressMsg{Code: []rune(value)[0], Text: value}
	}
}

func lastCommandMessage(t *testing.T, cmd tea.Cmd) tea.Msg {
	t.Helper()
	if cmd == nil {
		t.Fatal("command is nil")
	}
	msg := cmd()
	for {
		batch, ok := msg.(tea.BatchMsg)
		if !ok {
			return msg
		}
		if len(batch) == 0 {
			t.Fatal("command returned an empty batch")
		}
		msg = batch[len(batch)-1]()
	}
}

func TestAsyncListFiltersAreRoutedToTheirOrigin(t *testing.T) {
	model := NewModel(context.Background(), &fakeBackend{}, WithPollInterval(0), WithNoColor(true))
	model.workspace = RelationshipsWorkspace
	model.graph = newGraphState(
		[]domain.Node{task("a", "Alpha"), task("b", "Beta"), task("c", "Charlie")},
		[]domain.Edge{
			{ID: "z-blocks", From: "a", To: "b", Type: "BLOCKS"},
			{ID: "a-child", From: "a", To: "c", Type: "CHILD"},
		},
	)
	_ = model.relationships.setGraph(model.graph)
	_ = model.refreshInspector()
	_, _ = model.Update(keyPress("/"))
	var cmd tea.Cmd
	for _, value := range []string{"B", "L", "O", "C", "K"} {
		_, cmd = model.Update(keyPress(value))
	}
	filterMsg := lastCommandMessage(t, cmd)
	if _, ok := filterMsg.(listFilterMatchesMsg); !ok {
		t.Fatalf("filter command returned %T, want scoped list message", filterMsg)
	}
	_, _ = model.Update(filterMsg)
	_, _ = model.Update(keyPress("enter"))
	visible := model.relationships.list.VisibleItems()
	if len(visible) != 1 || visible[0].(relationshipItem).edge.ID != "z-blocks" {
		t.Fatalf("filtered relationships = %#v", visible)
	}
	if !strings.Contains(model.inspector.markdown, "# BLOCKS") {
		t.Fatalf("filter selection left stale inspector content: %q", model.inspector.markdown)
	}
}

func TestCommandPaletteFiltersAndActivatesWithOneEnter(t *testing.T) {
	model := NewModel(context.Background(), &fakeBackend{}, WithPollInterval(0), WithNoColor(true))
	_ = model.openCommands()
	var cmd tea.Cmd
	for _, value := range []string{"t", "i", "m", "e", "l", "i", "n", "e"} {
		_, cmd = model.Update(keyPress(value))
	}
	_, _ = model.Update(lastCommandMessage(t, cmd))
	_, _ = model.Update(keyPress("enter"))
	if model.workspace != TimelineWorkspace || model.overlay.kind != overlayNone {
		t.Fatalf("palette activation left workspace=%v overlay=%v", model.workspace, model.overlay.kind)
	}
}

func TestPickerEscapeClosesImmediatelyWhileFiltering(t *testing.T) {
	model := NewModel(context.Background(), &fakeBackend{}, WithPollInterval(0), WithNoColor(true))
	_ = model.openCommands()
	_, _ = model.Update(keyPress("x"))
	_, _ = model.Update(keyPress("esc"))
	if model.overlay.kind != overlayNone {
		t.Fatalf("Esc left picker overlay %v open", model.overlay.kind)
	}
}

func TestTimelineDefaultsToNewestRevisionInEitherStartupOrder(t *testing.T) {
	revisions := []domain.RevisionInfo{{Revision: 1}, {Revision: 2}, {Revision: 3}}
	graph := newGraphState([]domain.Node{task("n", "Current")}, nil)

	snapshotFirst := NewModel(context.Background(), &fakeBackend{}, WithPollInterval(0), WithNoColor(true))
	snapshotFirst.loadSeq = 1
	_ = snapshotFirst.applySnapshotLoaded(snapshotLoadedMsg{serial: 1, loaded: snapshotLoad{graph: graph, revision: 3}})
	if len(snapshotFirst.timeline.list.Items()) != 0 {
		t.Fatal("snapshot populated a provisional revision-0 timeline before history was ready")
	}
	snapshotFirst.historySeq = 1
	_ = snapshotFirst.applyHistoryLoaded(historyLoadedMsg{serial: 1, revisions: revisions})
	if selected, _ := snapshotFirst.timeline.selectedRevision(); selected != 3 {
		t.Fatalf("snapshot-first selection = %d, want live revision 3", selected)
	}

	historyFirst := NewModel(context.Background(), &fakeBackend{}, WithPollInterval(0), WithNoColor(true))
	historyFirst.historySeq = 1
	_ = historyFirst.applyHistoryLoaded(historyLoadedMsg{serial: 1, revisions: revisions})
	historyFirst.loadSeq = 1
	_ = historyFirst.applySnapshotLoaded(snapshotLoadedMsg{serial: 1, loaded: snapshotLoad{graph: graph, revision: 3}})
	if selected, _ := historyFirst.timeline.selectedRevision(); selected != 3 {
		t.Fatalf("history-first selection = %d, want live revision 3", selected)
	}
}

func TestCompactQueryKeepsEveryFocusedSectionVisible(t *testing.T) {
	model := NewModel(context.Background(), &fakeBackend{}, WithPollInterval(0), WithNoColor(true))
	model.workspace = QueryWorkspace
	_, _ = model.Update(tea.WindowSizeMsg{Width: 60, Height: 18})
	if !model.query.singleSection {
		t.Fatal("short query layout did not enter focused-section mode")
	}
	checks := []struct {
		focus queryFocus
		text  string
	}{
		{queryFocusCypher, "CYPHER"},
		{queryFocusParams, "PARAMETERS"},
		{queryFocusResults, "RESULTS"},
		{queryFocusRow, "SELECTED ROW"},
	}
	for _, check := range checks {
		model.query.focus = check.focus
		view := model.query.view()
		if !strings.Contains(view, check.text) {
			t.Fatalf("focus %v is invisible:\n%s", check.focus, view)
		}
		if lines := strings.Count(view, "\n") + 1; lines > model.query.height {
			t.Fatalf("focus %v rendered %d lines into height %d", check.focus, lines, model.query.height)
		}
	}
}

func TestWideQueryGuidanceDoesNotWidenComposition(t *testing.T) {
	model := NewModel(context.Background(), &fakeBackend{}, WithPollInterval(0), WithNoColor(true))
	model.workspace = QueryWorkspace
	_, _ = model.Update(tea.WindowSizeMsg{Width: 132, Height: 38})
	for lineNumber, line := range strings.Split(model.query.view(), "\n") {
		if width := ansi.StringWidth(line); width > model.query.width {
			t.Fatalf("query line %d width = %d, limit %d: %q", lineNumber, width, model.query.width, line)
		}
	}
}

func TestTinyResizeGuidanceRetainsBothRequiredDimensions(t *testing.T) {
	model := NewModel(context.Background(), &fakeBackend{}, WithPollInterval(0), WithNoColor(true))
	_, _ = model.Update(tea.WindowSizeMsg{Width: 43, Height: 11})
	view := model.View().Content
	if !strings.Contains(view, "44×12") || !strings.Contains(view, "Ctrl+C") {
		t.Fatalf("tiny guidance lost its action or dimensions:\n%s", view)
	}
}

func TestShortIDsDisambiguateTimeOrderedUUIDs(t *testing.T) {
	left := domain.EntityID("01a05a8c-87c7-77fc-9f3a-7b759098eb81")
	right := domain.EntityID("01a05a8c-87c8-7cc1-b4f1-ff57284be4a7")
	if shortID(left) == shortID(right) {
		t.Fatalf("compact IDs collide: %q", shortID(left))
	}
}

func TestInitialLoadErrorsRemainActionableAfterNoticeClears(t *testing.T) {
	model := NewModel(context.Background(), &fakeBackend{}, WithPollInterval(0), WithNoColor(true))
	model.loadSeq = 1
	_ = model.applySnapshotLoaded(snapshotLoadedMsg{serial: 1, err: errors.New("database unavailable")})
	model.notice.text = ""
	if view := model.View().Content; !strings.Contains(view, "Graph unavailable") || !strings.Contains(view, "F5") {
		t.Fatalf("graph load error is not persistently actionable:\n%s", view)
	}
	model.workspace = TimelineWorkspace
	model.historySeq = 1
	_ = model.applyHistoryLoaded(historyLoadedMsg{serial: 1, err: errors.New("history unavailable")})
	model.notice.text = ""
	if view := model.View().Content; !strings.Contains(view, "Timeline unavailable") || !strings.Contains(view, "F5") {
		t.Fatalf("history load error is not persistently actionable:\n%s", view)
	}
}

func TestNoMatchEditKeepsExactRetryRequest(t *testing.T) {
	model := NewModel(context.Background(), &fakeBackend{}, WithPollInterval(0), WithNoColor(true))
	model.execSeq = 1
	model.executing = true
	operation := pendingOperation{
		kind: executionMutation, purpose: formEditNode,
		request: app.ExecuteRequest{Query: "MATCH (n) WHERE elementId(n) = $id SET n = $properties RETURN n"},
	}
	_ = model.applyExecutionFinished(executionFinishedMsg{serial: 1, operation: operation, result: app.BatchResult{Results: []app.Result{{}}}})
	if model.overlay.kind != overlayOperationError || model.pending == nil || model.pending.request.Query != operation.request.Query {
		t.Fatalf("no-match edit lost retry state: overlay=%v pending=%#v", model.overlay.kind, model.pending)
	}
}

func TestNoOpRootMoveIsNotMisreportedAsAConflict(t *testing.T) {
	model := NewModel(context.Background(), &fakeBackend{}, WithPollInterval(0), WithNoColor(true))
	model.execSeq = 1
	model.executing = true
	node := task("root", "Root")
	operation := pendingOperation{kind: executionMutation, purpose: formMoveNode, request: app.ExecuteRequest{Query: "MATCH (n) RETURN n"}}
	_ = model.applyExecutionFinished(executionFinishedMsg{
		serial: 1, operation: operation,
		result: app.BatchResult{Results: []app.Result{{Rows: [][]any{{node}}}}},
	})
	if model.overlay.kind == overlayOperationError || model.operationErr != nil {
		t.Fatalf("matched no-op move was treated as a conflict: %v", model.operationErr)
	}
}

func TestFormValidationIsVisibleAtMinimumWidth(t *testing.T) {
	model := NewModel(context.Background(), &fakeBackend{}, WithPollInterval(0), WithNoColor(true))
	_, _ = model.Update(tea.WindowSizeMsg{Width: minimumWidth, Height: minimumHeight})
	_ = model.openCreateForm()
	_, _ = model.Update(keyPress("enter"))
	if !strings.Contains(model.notice.text, "cannot be empty") {
		t.Fatalf("form validation notice = %q", model.notice.text)
	}
	status := model.renderStatus()
	if !strings.Contains(status, "cannot be empty") {
		t.Fatalf("minimum-width status hid validation feedback: %q", status)
	}
}

func TestCrossFieldRequestErrorKeepsFormEditable(t *testing.T) {
	model := NewModel(context.Background(), &fakeBackend{}, WithPollInterval(0), WithNoColor(true))
	model.form = newCreateNodeForm(9, newGraphState(nil, nil), "", 80, 24, true, true)
	data := model.form.data.(*nodeFormData)
	data.title = "Root"
	data.position = "1"
	model.overlay.kind = overlayForm
	_ = model.submitForm(formSubmittedMsg{serial: 9})
	if model.overlay.kind != overlayForm || model.form == nil || model.operationErr != nil || model.pending != nil {
		t.Fatalf("request validation escaped the form: overlay=%v form=%v error=%v pending=%v", model.overlay.kind, model.form != nil, model.operationErr, model.pending)
	}
	if !strings.Contains(model.notice.text, "position requires a parent") {
		t.Fatalf("cross-field validation notice = %q", model.notice.text)
	}
}

func TestMinimumHeightFormKeepsSelectedOptionVisible(t *testing.T) {
	graph := newGraphState([]domain.Node{task("a", "Alpha"), task("z", "Zulu")}, nil)
	form := newCreateNodeForm(1, graph, "z", 38, 5, true, true)
	form.data.(*nodeFormData).title = "New node"
	_ = form.form.NextField()
	_ = form.form.NextField()
	view := form.view()
	if !strings.Contains(view, "> Zulu · z") {
		t.Fatalf("minimum-height selector hid its selected option:\n%s", view)
	}
	if strings.HasPrefix(view, "Create node\n") {
		t.Fatalf("form duplicated the surrounding modal title:\n%s", view)
	}
}

func TestRevisionTimelineIncludesInitialStateAndNewestFirst(t *testing.T) {
	styles := makeStyles(true, true)
	timeline := newTimelineModel(styles, true)
	timeline.setRevisions([]domain.RevisionInfo{
		{Revision: 1, Time: time.Unix(1, 0), Message: "one"},
		{Revision: 2, Time: time.Unix(2, 0), Message: "two"},
	}, 2)
	items := timeline.list.Items()
	if len(items) != 3 {
		t.Fatalf("timeline item count = %d", len(items))
	}
	if items[0].(revisionItem).info.Revision != 2 || items[2].(revisionItem).info.Revision != 0 {
		t.Fatalf("timeline order = %#v", items)
	}
}

func TestHistoricalPollTracksLiveWithoutReplacingSnapshot(t *testing.T) {
	backend := &fakeBackend{revision: 9}
	model := NewModel(context.Background(), backend, WithPollInterval(0), WithNoColor(true))
	historical := domain.Revision(3)
	model.snapshot.Revision = &historical
	model.liveRevision = 8
	initialLoad := model.loadSeq
	_ = model.applyRevisionChecked(revisionCheckedMsg{revision: 9})
	if model.liveRevision != 9 || model.loadSeq != initialLoad || model.snapshot.Revision == nil || *model.snapshot.Revision != 3 {
		t.Fatalf("historical poll changed snapshot: live=%d load=%d snapshot=%v", model.liveRevision, model.loadSeq, model.snapshot.Revision)
	}
}

func TestPendingHistoricalSelectionSurvivesPollAndScopesQueries(t *testing.T) {
	backend := &fakeBackend{revision: 4}
	model := NewModel(context.Background(), backend, WithPollInterval(0), WithNoColor(true))
	model.loadedRevision = 3
	model.liveRevision = 3
	target := domain.Revision(2)
	_ = model.startSnapshotLoad(domain.Snapshot{Revision: &target})
	pendingLoad := model.loadSeq
	if !model.historical() || model.selectedSnapshot().Revision == nil || *model.selectedSnapshot().Revision != target {
		t.Fatalf("pending historical intent was not visible: historical=%v target=%#v", model.historical(), model.selectedSnapshot())
	}
	model.workspace = QueryWorkspace
	model.query.cypher.SetValue("RETURN 1")
	model.query.params.SetValue("{}")
	_ = model.runQuery(true)
	if model.pending == nil || model.pending.request.Snapshot.Revision == nil || *model.pending.request.Snapshot.Revision != target {
		t.Fatalf("query bypassed pending historical target: %#v", model.pending)
	}
	_ = model.applyRevisionChecked(revisionCheckedMsg{revision: 4})
	if model.loadSeq != pendingLoad || model.liveRevision != 4 || model.selectedSnapshot().Revision == nil || *model.selectedSnapshot().Revision != target {
		t.Fatalf("poll canceled pending history: load=%d live=%d target=%#v", model.loadSeq, model.liveRevision, model.selectedSnapshot())
	}
	if !strings.Contains(model.renderHeader(), "loading revision 2") {
		t.Fatalf("pending-history banner = %q", model.renderHeader())
	}
}

func TestDecodeSnapshotRejectsMalformedExecutorShape(t *testing.T) {
	_, _, err := decodeSnapshot(app.BatchResult{Results: []app.Result{{}}})
	if err == nil || !strings.Contains(err.Error(), "want 2") {
		t.Fatalf("decodeSnapshot error = %v", err)
	}
}

func TestExecutionErrorsKeepMutationRetryButNotQueryModal(t *testing.T) {
	model := NewModel(context.Background(), &fakeBackend{}, WithPollInterval(0), WithNoColor(true))
	mutation := pendingOperation{kind: executionMutation, purpose: formEditNode, request: app.ExecuteRequest{Query: "RETURN 1"}}
	model.execSeq = 1
	model.executing = true
	_ = model.applyExecutionFinished(executionFinishedMsg{serial: 1, operation: mutation, err: errors.New("conflict")})
	if model.overlay.kind != overlayOperationError || model.pending == nil {
		t.Fatal("mutation error did not expose retry state")
	}
	query := pendingOperation{kind: executionQuery, request: app.ExecuteRequest{Query: "bad"}}
	model.execSeq = 2
	model.executing = true
	_ = model.applyExecutionFinished(executionFinishedMsg{serial: 2, operation: query, err: errors.New("syntax")})
	if model.overlay.kind != overlayNone || model.query.err == nil {
		t.Fatal("query error did not stay in query workspace")
	}
	console := pendingOperation{kind: executionConsole, purpose: formExecuteQuery, request: app.ExecuteRequest{Query: "CREATE (:Task)"}}
	model.execSeq = 3
	model.executing = true
	_ = model.applyExecutionFinished(executionFinishedMsg{serial: 3, operation: console, err: errors.New("busy")})
	if model.overlay.kind != overlayOperationError || model.pending == nil || model.pending.request.Query != console.request.Query {
		t.Fatal("confirmed write-capable query error lost exact retry state")
	}
}

func TestNoBlockingExecutorCallOccursInsideUpdate(t *testing.T) {
	backend := &fakeBackend{revision: 1, snapshots: map[domain.Revision]fakeSnapshot{1: {}}}
	model := NewModel(context.Background(), backend, WithPollInterval(0), WithNoColor(true))
	cmd := model.startSnapshotLoad(domain.Snapshot{})
	if len(backend.requests) != 0 || backend.currentReads != 0 {
		t.Fatal("startSnapshotLoad performed backend I/O synchronously")
	}
	batch, ok := cmd().(tea.BatchMsg)
	if !ok || len(batch) != 2 {
		t.Fatalf("load command returned an unexpected batch")
	}
	message := batch[1]()
	if _, ok := message.(snapshotLoadedMsg); !ok {
		t.Fatalf("load command returned %T", message)
	}
	if len(backend.requests) != 1 || backend.currentReads != 1 {
		t.Fatalf("load command calls = %d executor, %d token", len(backend.requests), backend.currentReads)
	}
}

func TestSuccessfulCreateSelectsReturnedNodeAfterSnapshotReload(t *testing.T) {
	model := NewModel(context.Background(), &fakeBackend{}, WithPollInterval(0), WithNoColor(true))
	model.execSeq = 1
	model.executing = true
	created := task("created", "Created")
	revision := domain.Revision(2)
	operation := pendingOperation{kind: executionMutation, purpose: formCreateNode}
	_ = model.applyExecutionFinished(executionFinishedMsg{
		serial: 1, operation: operation,
		result: app.BatchResult{
			Results:  []app.Result{{Rows: [][]any{{created}}, Summary: app.Summary{NodesCreated: 1}}},
			Revision: &revision,
		},
	})
	if model.selectAfterLoad != created.ID || !model.loadingGraph {
		t.Fatalf("post-create selection = %q, loading = %v", model.selectAfterLoad, model.loadingGraph)
	}
	serial := model.loadSeq
	graph := newGraphState([]domain.Node{task("old", "Old"), created}, nil)
	_ = model.applySnapshotLoaded(snapshotLoadedMsg{serial: serial, loaded: snapshotLoad{graph: graph, revision: revision}})
	if model.work.selected != created.ID || model.selectAfterLoad != "" {
		t.Fatalf("reloaded selection = %q, pending = %q", model.work.selected, model.selectAfterLoad)
	}
}

func TestNoMatchMutationOffersExactRetryAndStaleExecutionIsIgnored(t *testing.T) {
	model := NewModel(context.Background(), &fakeBackend{}, WithPollInterval(0), WithNoColor(true))
	operation := pendingOperation{
		kind: executionMutation, purpose: formDeleteNode,
		request: app.ExecuteRequest{Query: "MATCH (n) WHERE elementId(n) = $id DETACH DELETE n"},
	}
	model.execSeq = 2
	model.executing = true
	_ = model.applyExecutionFinished(executionFinishedMsg{serial: 1, operation: operation})
	if !model.executing || model.overlay.kind != overlayNone {
		t.Fatal("stale execution result mutated active execution state")
	}
	_ = model.applyExecutionFinished(executionFinishedMsg{serial: 2, operation: operation})
	if model.executing || model.overlay.kind != overlayOperationError || model.pending == nil {
		t.Fatal("no-match mutation did not expose retry state")
	}
	if model.operationErr == nil || !strings.Contains(model.operationErr.Error(), "matched no current graph entity") {
		t.Fatalf("no-match error = %v", model.operationErr)
	}
}

func TestResponsiveHeaderAndPaneComposition(t *testing.T) {
	model := NewModel(context.Background(), &fakeBackend{}, WithPollInterval(0), WithNoColor(true))
	model.graph = newGraphState([]domain.Node{task("n", "Node")}, nil)
	model.work.setGraph(model.graph)
	model.inspector.markdown = "# Sentinel"
	model.inspector.rendered = "INSPECTOR_SENTINEL"
	model.inspector.viewport.SetContent(model.inspector.rendered)

	_, _ = model.Update(tea.WindowSizeMsg{Width: minimumWidth, Height: 20})
	header := strings.Split(model.renderHeader(), "\n")[0]
	for _, tab := range []string{"F1 Work", "F2 Rel", "F3 Query", "F4 Time"} {
		if !strings.Contains(header, tab) {
			t.Fatalf("minimum-width header hides %q: %q", tab, header)
		}
	}

	_, _ = model.Update(tea.WindowSizeMsg{Width: 140, Height: 36})
	if view := model.View().Content; !strings.Contains(view, "INSPECTOR_SENTINEL") {
		t.Fatalf("wide layout did not compose primary and inspector panes:\n%s", view)
	}
	_, _ = model.Update(tea.WindowSizeMsg{Width: 80, Height: 28})
	if view := model.View().Content; strings.Contains(view, "INSPECTOR_SENTINEL") {
		t.Fatalf("compact primary pane leaked inspector content:\n%s", view)
	}
	_, _ = model.Update(keyPress("tab"))
	if view := model.View().Content; !strings.Contains(view, "INSPECTOR_SENTINEL") {
		t.Fatalf("compact details pane was not reachable with Tab:\n%s", view)
	}
}

func TestWorkspaceNavigationNeverConsumesPrintableEditorOrFormInput(t *testing.T) {
	model := NewModel(context.Background(), &fakeBackend{}, WithPollInterval(0), WithNoColor(true))
	model.workspace = QueryWorkspace
	model.query.focus = queryFocusCypher
	model.query.cypher.SetValue("")
	_ = model.query.focusCurrent()
	_, _ = model.Update(keyPress("1"))
	if model.workspace != QueryWorkspace || model.query.cypher.Value() != "1" {
		t.Fatalf("Cypher digit changed workspace or was lost: workspace=%v value=%q", model.workspace, model.query.cypher.Value())
	}
	_, _ = model.Update(keyPress("f1"))
	if model.workspace != WorkWorkspace {
		t.Fatalf("F1 workspace = %v", model.workspace)
	}
	_, _ = model.Update(keyPress("f3"))
	model.query.focus = queryFocusParams
	model.query.params.SetValue("")
	_ = model.query.focusCurrent()
	_, _ = model.Update(keyPress("2"))
	if model.workspace != QueryWorkspace || model.query.params.Value() != "2" {
		t.Fatalf("parameter digit changed workspace or was lost: workspace=%v value=%q", model.workspace, model.query.params.Value())
	}
	_, _ = model.Update(keyPress("ctrl+pgdown"))
	if model.workspace != TimelineWorkspace {
		t.Fatalf("Ctrl+PageDown workspace = %v", model.workspace)
	}
	_, _ = model.Update(keyPress("ctrl+pgup"))
	if model.workspace != QueryWorkspace {
		t.Fatalf("Ctrl+PageUp workspace = %v", model.workspace)
	}

	_, _ = model.Update(keyPress("f1"))
	_ = model.openCreateForm()
	_, _ = model.Update(keyPress("3"))
	data := model.form.data.(*nodeFormData)
	if data.title != "3" || model.workspace != WorkWorkspace {
		t.Fatalf("form digit changed workspace or was lost: workspace=%v title=%q", model.workspace, data.title)
	}
	_, _ = model.Update(keyPress("f2"))
	if model.workspace != RelationshipsWorkspace || model.overlay.kind != overlayForm || model.form == nil {
		t.Fatalf("F2 discarded active form: workspace=%v overlay=%v form=%v", model.workspace, model.overlay.kind, model.form != nil)
	}
	_, _ = model.Update(keyPress("4"))
	if data.title != "34" || model.workspace != RelationshipsWorkspace {
		t.Fatalf("second form digit changed workspace or was lost: workspace=%v title=%q", model.workspace, data.title)
	}
}

func TestGlobalHelpRestoresActiveForm(t *testing.T) {
	model := NewModel(context.Background(), &fakeBackend{}, WithPollInterval(0), WithNoColor(true))
	_ = model.openCreateForm()
	form := model.form
	_, _ = model.Update(keyPress("f10"))
	if model.overlay.kind != overlayHelp || model.overlayReturn != overlayForm {
		t.Fatalf("F10 did not stack help over form: overlay=%v return=%v", model.overlay.kind, model.overlayReturn)
	}
	_, _ = model.Update(keyPress("f10"))
	if model.overlay.kind != overlayForm || model.form != form {
		t.Fatal("closing global help did not restore the active form")
	}
}

func TestModalWidgetsUseInnerPanelDimensions(t *testing.T) {
	model := NewModel(context.Background(), &fakeBackend{}, WithPollInterval(0), WithNoColor(true))
	_, _ = model.Update(tea.WindowSizeMsg{Width: 140, Height: 40})
	_ = model.openHelp()
	_ = model.layoutComponents()
	wantWidth, wantHeight := model.modalContentDimensions()
	if model.helpViewport.Width() != wantWidth || model.helpViewport.Height() != wantHeight {
		t.Fatalf("help viewport = %dx%d, want modal content %dx%d", model.helpViewport.Width(), model.helpViewport.Height(), wantWidth, wantHeight)
	}
	if outerWidth, _ := model.modalDimensions(); model.helpViewport.Width() >= outerWidth {
		t.Fatalf("help viewport width %d was sized to or beyond outer panel %d", model.helpViewport.Width(), outerWidth)
	}
}

func TestHistoricalShortHelpOmitsMutationAffordances(t *testing.T) {
	model := NewModel(context.Background(), &fakeBackend{}, WithPollInterval(0), WithNoColor(true))
	revision := domain.Revision(3)
	model.snapshot.Revision = &revision
	model.width = 180
	help := model.renderShortHelp()
	if !strings.Contains(help, "return live") {
		t.Fatalf("historical help omitted Return Live: %q", help)
	}
	if strings.Contains(help, "new node") {
		t.Fatalf("historical help advertised a disabled mutation: %q", help)
	}
}

func TestStripANSIColorsPreservesNonColorCursorAffordance(t *testing.T) {
	input := "\x1b[31mred\x1b[0m \x1b[38;5;212mextended\x1b[39m \x1b[7mcursor\x1b[27m"
	output := stripANSIColors(input)
	if containsANSIColor(output) || strings.Contains(output, "38;5;212") {
		t.Fatalf("color SGR survived: %q", output)
	}
	if !strings.Contains(output, "\x1b[7mcursor\x1b[27m") {
		t.Fatalf("reverse-video cursor affordance was removed: %q", output)
	}
}
