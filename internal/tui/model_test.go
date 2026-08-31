package tui

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/svlocks/sheets/internal/app"
	"github.com/svlocks/sheets/internal/domain"
)

type fakeBackend struct {
	root          string
	revision      domain.Revision
	current       GraphSnapshot
	snapshots     map[domain.Revision]GraphSnapshot
	history       []domain.RevisionInfo
	revisionErr   error
	graphErr      error
	historyErr    error
	executeErr    error
	requests      []app.ExecuteRequest
	result        app.BatchResult
	revisionCalls int
	graphCalls    int
}

var _ Backend = (*fakeBackend)(nil)

func (f *fakeBackend) ProjectRoot() string { return f.root }

func (f *fakeBackend) CurrentRevision(context.Context) (domain.Revision, error) {
	f.revisionCalls++
	return f.revision, f.revisionErr
}

func (f *fakeBackend) Graph(_ context.Context, snapshot domain.Snapshot) ([]domain.Node, []domain.Edge, error) {
	f.graphCalls++
	if f.graphErr != nil {
		return nil, nil, f.graphErr
	}
	graph := f.current
	if snapshot.Revision != nil {
		graph = f.snapshots[*snapshot.Revision]
	}
	return append([]domain.Node(nil), graph.Nodes...), append([]domain.Edge(nil), graph.Edges...), nil
}

func (f *fakeBackend) Revisions(context.Context) ([]domain.RevisionInfo, error) {
	return append([]domain.RevisionInfo(nil), f.history...), f.historyErr
}

func (f *fakeBackend) Execute(_ context.Context, request app.ExecuteRequest) (app.BatchResult, error) {
	f.requests = append(f.requests, request)
	return f.result, f.executeErr
}

func testGraph() GraphSnapshot {
	position0, position2 := int64(0), int64(2)
	return GraphSnapshot{
		Nodes: []domain.Node{
			{ID: "root", Labels: []string{"Project"}, Properties: domain.Properties{"title": "Alpha"}, Body: "# Alpha\nRoot body", ValidFrom: 1},
			{ID: "late", Labels: []string{"Task"}, Properties: domain.Properties{"title": "Late"}, ValidFrom: 2},
			{ID: "first", Labels: []string{"Task"}, Properties: domain.Properties{"title": "First", "priority": int64(1)}, ValidFrom: 2},
			{ID: "free", Labels: []string{"Task"}, Properties: domain.Properties{"title": "Unordered"}, ValidFrom: 2},
			{ID: "deep", Properties: domain.Properties{"name": "Deep"}, ValidFrom: 3},
			{ID: "orphan", Labels: []string{"Lost"}, Properties: domain.Properties{"title": "Orphan"}, ValidFrom: 3},
		},
		Edges: []domain.Edge{
			{ID: "e-late", From: "root", Type: "CHILD", To: "late", Position: &position2, ValidFrom: 2},
			{ID: "e-first", From: "root", Type: "CHILD", To: "first", Position: &position0, ValidFrom: 2},
			{ID: "e-free", From: "root", Type: "CHILD", To: "free", ValidFrom: 2},
			{ID: "e-deep", From: "first", Type: "CHILD", To: "deep", ValidFrom: 3},
			{ID: "e-orphan", From: "missing", Type: "CHILD", To: "orphan", ValidFrom: 3},
			{ID: "e-link", From: "deep", Type: "BLOCKS", To: "late", Properties: domain.Properties{"reason": "test"}, ValidFrom: 3},
			{ID: "e-dangling", From: "gone", Type: "REF", To: "root", ValidFrom: 3},
		},
	}
}

func loadedModel(t *testing.T, options ...Option) (*Model, *fakeBackend) {
	t.Helper()
	graph := testGraph()
	backend := &fakeBackend{
		root: "/work/project", revision: 3, current: graph,
		snapshots: map[domain.Revision]GraphSnapshot{1: {Nodes: graph.Nodes[:1]}},
		history: []domain.RevisionInfo{
			{Revision: 1, Time: time.Date(2026, 1, 2, 3, 4, 0, 0, time.UTC), Actor: "sam", Message: "created"},
			{Revision: 3, Time: time.Date(2026, 1, 3, 3, 4, 0, 0, time.UTC), Actor: "agent", Message: "organized"},
		},
	}
	options = append(options, WithPollInterval(0), WithNoColor(true))
	m := NewModel(context.Background(), backend, options...)
	load := m.loadGraphCmd(domain.Snapshot{})
	m.Update(load())
	history := m.loadHistoryCmd()
	m.Update(history())
	return m, backend
}

func TestOutlineBuildsArbitraryDepthOrderedAndOrphanRows(t *testing.T) {
	m, _ := loadedModel(t)
	var ids []domain.EntityID
	depth := map[domain.EntityID]int{}
	var orphan outlineRow
	for _, row := range m.outlineRows {
		ids = append(ids, row.ID)
		depth[row.ID] = row.Depth
		if row.ID == "orphan" {
			orphan = row
		}
	}
	want := []domain.EntityID{"root", "first", "deep", "late", "free", "orphan"}
	if strings.Join(entityStrings(ids), ",") != strings.Join(entityStrings(want), ",") {
		t.Fatalf("outline order = %v, want %v", ids, want)
	}
	if depth["deep"] != 2 {
		t.Fatalf("deep depth = %d, want 2", depth["deep"])
	}
	if !orphan.Orphan || orphan.Section != "Orphans" {
		t.Fatalf("orphan row = %+v", orphan)
	}

	m.selectNode("first")
	m.toggleSelected(false)
	if containsOutline(m.outlineRows, "deep") {
		t.Fatal("collapsed child's descendant remained visible")
	}
	m.search.SetValue("priority")
	m.rebuildNavigation()
	if len(m.outlineRows) != 1 || m.outlineRows[0].ID != "first" {
		t.Fatalf("property filter rows = %+v", m.outlineRows)
	}
}

func TestOutlineKeepsCyclesVisible(t *testing.T) {
	m, _ := loadedModel(t)
	m.nodes = []domain.Node{
		{ID: "a", Properties: domain.Properties{"title": "A"}},
		{ID: "b", Properties: domain.Properties{"title": "B"}},
	}
	m.nodeByID = map[domain.EntityID]domain.Node{"a": m.nodes[0], "b": m.nodes[1]}
	m.edges = []domain.Edge{{ID: "ab", From: "a", Type: "CHILD", To: "b"}, {ID: "ba", From: "b", Type: "CHILD", To: "a"}}
	m.search.SetValue("")
	m.rebuildOutline()
	if len(m.outlineRows) != 2 {
		t.Fatalf("cycle rows = %+v", m.outlineRows)
	}
	if m.outlineRows[0].Section != "Orphans / cycles" || !m.outlineRows[0].Orphan {
		t.Fatalf("cycle component not clearly marked: %+v", m.outlineRows)
	}
}

func TestGraphRowsIncludeEdgesAndDanglingSources(t *testing.T) {
	m, _ := loadedModel(t)
	rendered := m.renderGraph(100, 30)
	for _, want := range []string{"BLOCKS→ Late", "<missing:gone> ─REF→ root", "zoom 2/3"} {
		if !strings.Contains(rendered, want) {
			t.Errorf("graph missing %q:\n%s", want, rendered)
		}
	}
	previous := m.graphZoom
	m.handleGraphKey("+")
	if m.graphZoom != previous+1 {
		t.Fatalf("zoom = %d", m.graphZoom)
	}
	m.handleGraphKey("l")
	if m.graphPanX != 4 {
		t.Fatalf("pan x = %d", m.graphPanX)
	}
}

func TestKeyNavigationTabsAndHistoricalWriteProtection(t *testing.T) {
	m, _ := loadedModel(t)
	m.Update(printableKey("2"))
	if m.tab != GraphTab {
		t.Fatalf("tab = %v", m.tab)
	}
	before := m.selected
	m.Update(printableKey("j"))
	if m.selected == before {
		t.Fatal("j did not move graph selection")
	}
	revision := domain.Revision(1)
	load := m.loadGraphCmd(domain.Snapshot{Revision: &revision})
	m.Update(load())
	m.openMutation(mutationCreate)
	if m.overlay != overlayNone || !strings.Contains(m.status, "read-only") {
		t.Fatalf("historical mutation not blocked: overlay=%v status=%q", m.overlay, m.status)
	}
}

func TestQueryReadAndExecRequestsAndJSONNumbers(t *testing.T) {
	m, backend := loadedModel(t)
	m.setTab(QueryTab)
	m.query.SetValue("RETURN $count")
	m.params.SetValue(`{"count": 7, "ratio": 1.5}`)
	cmd := m.handleQueryKey(ctrlKey('r'))
	if cmd == nil {
		t.Fatal("read query produced no command")
	}
	m.Update(cmd())
	if len(backend.requests) != 1 || !backend.requests[0].ReadOnly {
		t.Fatalf("request = %+v", backend.requests)
	}
	if _, ok := backend.requests[0].Params["count"].(int64); !ok {
		t.Fatalf("integer param type = %T", backend.requests[0].Params["count"])
	}

	m.params.SetValue(`{"broken":`)
	if cmd := m.handleQueryKey(ctrlKey('r')); cmd != nil || m.queryErr == nil {
		t.Fatalf("invalid JSON command=%v error=%v", cmd, m.queryErr)
	}

	revision := domain.Revision(1)
	m.snapshot = domain.Snapshot{Revision: &revision}
	m.params.SetValue("{}")
	if cmd := m.handleQueryKey(ctrlKey('x')); cmd != nil || !strings.Contains(m.queryErr.Error(), "read-only") {
		t.Fatalf("historical exec command=%v error=%v", cmd, m.queryErr)
	}
}

func TestMutationFormsExecuteCypherOnly(t *testing.T) {
	m, backend := loadedModel(t)
	m.selectNode("first")
	m.openMutation(mutationEdit)
	m.mutationInput.SetValue(`{"labels":["Task","Needs Review"],"properties":{"title":"Changed","rank":2},"body":"# Done"}`)
	cmd := m.submitMutation()
	if cmd == nil {
		t.Fatalf("edit error: %v", m.mutationErr)
	}
	m.Update(cmd())
	if len(backend.requests) != 1 {
		t.Fatalf("requests = %d", len(backend.requests))
	}
	request := backend.requests[0]
	for _, fragment := range []string{"MATCH (n)", "elementId(n) = $id", "SET n = $properties", "REMOVE n:Task", "SET n:Task:`Needs Review`"} {
		if !strings.Contains(request.Query, fragment) {
			t.Errorf("edit query missing %q: %s", fragment, request.Query)
		}
	}
	if request.ReadOnly || request.Actor != "tui" {
		t.Fatalf("mutation metadata = %+v", request)
	}
	if _, ok := request.Params["properties"].(domain.Properties)["rank"].(int64); !ok {
		t.Fatalf("rank type = %T", request.Params["properties"].(domain.Properties)["rank"])
	}

	m.openMutation(mutationReparent)
	m.mutationInput.SetValue(`{"parent_id":"root","position":4}`)
	cmd = m.submitMutation()
	cmd()
	if got := backend.requests[1].Query; !strings.Contains(got, "DELETE old") || !strings.Contains(got, "CREATE (p)-[:CHILD {position: $position}]->(n)") {
		t.Fatalf("reparent query = %s", got)
	}

	m.openMutation(mutationDelete)
	cmd = m.submitMutation()
	cmd()
	if got := backend.requests[2].Query; got != "MATCH (n) WHERE elementId(n) = $id DETACH DELETE n" {
		t.Fatalf("delete query = %s", got)
	}
}

func TestHistoryLoadsWholeSnapshotAndReturnsLive(t *testing.T) {
	m, backend := loadedModel(t)
	m.setTab(HistoryTab)
	// History is sorted newest first; choose revision 1.
	m.historyIndex = 1
	cmd := m.openSelectedRevision()
	m.Update(cmd())
	if m.snapshot.IsCurrent() || snapshotRevision(m.snapshot) != 1 || len(m.nodes) != 1 {
		t.Fatalf("snapshot=%+v nodes=%d", m.snapshot, len(m.nodes))
	}
	if backend.graphCalls < 2 {
		t.Fatalf("graph calls = %d", backend.graphCalls)
	}
	view := ansi.Strip(m.View().Content)
	if !strings.Contains(view, "HISTORICAL · r1 · READ-ONLY") {
		t.Fatalf("historical banner missing:\n%s", view)
	}
	cmd = m.returnLive()
	m.Update(cmd())
	if !m.snapshot.IsCurrent() || len(m.nodes) != len(backend.current.Nodes) {
		t.Fatalf("did not return live: snapshot=%+v nodes=%d", m.snapshot, len(m.nodes))
	}
}

func TestResponsiveRenderHasExactBoundsAndInspector(t *testing.T) {
	m, _ := loadedModel(t)
	for _, size := range []tea.WindowSizeMsg{{Width: 132, Height: 36}, {Width: 64, Height: 20}, {Width: 30, Height: 10}} {
		m.Update(size)
		view := ansi.Strip(m.View().Content)
		lines := strings.Split(view, "\n")
		if len(lines) != size.Height {
			t.Errorf("%dx%d line count = %d", size.Width, size.Height, len(lines))
		}
		for lineNo, line := range lines {
			if ansi.StringWidth(line) != size.Width {
				t.Fatalf("%dx%d line %d width = %d: %q", size.Width, size.Height, lineNo, ansi.StringWidth(line), line)
			}
		}
		if size.Width >= 46 {
			if !strings.Contains(view, "Outline") || !strings.Contains(view, "Graph") || !strings.Contains(view, "Query") || !strings.Contains(view, "History") {
				t.Fatalf("tabs missing at %dx%d", size.Width, size.Height)
			}
		} else if !strings.Contains(view, "1 O") || !strings.Contains(view, "4 H") {
			t.Fatalf("compact tabs missing at %dx%d", size.Width, size.Height)
		}
		if size.Width >= 110 && !strings.Contains(view, "Inspector") {
			t.Fatalf("wide inspector missing:\n%s", view)
		}
	}
}

func TestInspectorIncludesPropertiesMarkdownEdgesAndValidity(t *testing.T) {
	m, _ := loadedModel(t)
	m.selectNode("deep")
	rendered := ansi.Strip(m.renderInspector(70, 100))
	for _, want := range []string{"ID  deep", "Validity  [r3, ∞)", "Markdown body", "Incoming · 1", "Outgoing · 1", "BLOCKS", "properties {\"reason\":\"test\"}"} {
		if !strings.Contains(rendered, want) {
			t.Errorf("inspector missing %q:\n%s", want, rendered)
		}
	}
}

func TestRevisionPollingUsesTokenAndReloadsOnlyLive(t *testing.T) {
	m, backend := loadedModel(t)
	initialCalls := backend.graphCalls
	backend.revision = 4
	_, cmd := m.Update(revisionCheckedMsg{revision: 4})
	if cmd == nil || m.liveRev != 4 {
		t.Fatalf("live invalidation cmd=%v revision=%d", cmd, m.liveRev)
	}
	batch, ok := cmd().(tea.BatchMsg)
	if !ok || len(batch) != 2 {
		t.Fatalf("reload message = %T %#v", cmd(), cmd())
	}
	for _, child := range batch {
		m.Update(child())
	}
	if backend.graphCalls != initialCalls+1 {
		t.Fatalf("graph calls = %d", backend.graphCalls)
	}

	revision := domain.Revision(1)
	m.snapshot = domain.Snapshot{Revision: &revision}
	_, cmd = m.Update(revisionCheckedMsg{revision: 5})
	if cmd != nil {
		t.Fatalf("historical token change started command: %T", cmd())
	}
}

func TestCanceledLoadDoesNotCallBackendOrSurfaceError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	backend := &fakeBackend{root: "/tmp/project"}
	m := NewModel(ctx, backend, WithPollInterval(0))
	cancel()
	cmd := m.loadGraphCmd(domain.Snapshot{})
	m.Update(cmd())
	if backend.graphCalls != 0 || backend.revisionCalls != 0 {
		t.Fatalf("backend called after cancellation: revision=%d graph=%d", backend.revisionCalls, backend.graphCalls)
	}
	if m.err != nil {
		t.Fatalf("cancellation surfaced as UI error: %v", m.err)
	}
}

func TestLoadingAndBackendErrorsRender(t *testing.T) {
	backend := &fakeBackend{root: "/tmp/project", graphErr: errors.New("disk unavailable")}
	m := NewModel(context.Background(), backend, WithPollInterval(0), WithNoColor(true))
	cmd := m.loadGraphCmd(domain.Snapshot{})
	m.Update(cmd())
	view := ansi.Strip(m.View().Content)
	if !strings.Contains(view, "disk unavailable") || !strings.Contains(view, "Press r to retry") {
		t.Fatalf("error view:\n%s", view)
	}
}

func TestMouseSelectsRenderedNodeAndTab(t *testing.T) {
	m, _ := loadedModel(t)
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m.View()
	var nodeHit, graphTab hitArea
	for _, hit := range m.hits {
		if hit.Kind == hitNode && hit.Node != m.selected && nodeHit.Node == "" {
			nodeHit = hit
		}
		if hit.Kind == hitTab && hit.Tab == GraphTab {
			graphTab = hit
		}
	}
	if nodeHit.Node == "" {
		t.Fatal("no secondary node hit area")
	}
	m.Update(tea.MouseClickMsg(tea.Mouse{X: nodeHit.X0, Y: nodeHit.Y0, Button: tea.MouseLeft}))
	if m.selected != nodeHit.Node {
		t.Fatalf("mouse selected %q, want %q", m.selected, nodeHit.Node)
	}
	m.Update(tea.MouseClickMsg(tea.Mouse{X: graphTab.X0, Y: graphTab.Y0, Button: tea.MouseLeft}))
	if m.tab != GraphTab {
		t.Fatalf("mouse tab = %v", m.tab)
	}
}

func printableKey(text string) tea.KeyPressMsg {
	runes := []rune(text)
	return tea.KeyPressMsg(tea.Key{Text: text, Code: runes[0]})
}

func ctrlKey(code rune) tea.KeyPressMsg {
	return tea.KeyPressMsg(tea.Key{Code: code, Mod: tea.ModCtrl})
}

func entityStrings(ids []domain.EntityID) []string {
	result := make([]string, len(ids))
	for index, id := range ids {
		result[index] = string(id)
	}
	return result
}

func containsOutline(rows []outlineRow, id domain.EntityID) bool {
	for _, row := range rows {
		if row.ID == id {
			return true
		}
	}
	return false
}
