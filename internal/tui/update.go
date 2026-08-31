package tui

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/svlocks/sheets/internal/app"
	"github.com/svlocks/sheets/internal/domain"
)

type graphLoadedMsg struct {
	serial   uint64
	snapshot domain.Snapshot
	revision domain.Revision
	nodes    []domain.Node
	edges    []domain.Edge
	err      error
}

type historyLoadedMsg struct {
	history []domain.RevisionInfo
	err     error
}

type pollTickMsg time.Time

type revisionCheckedMsg struct {
	revision domain.Revision
	err      error
}

type executedMsg struct {
	serial   uint64
	mutation mutationKind
	result   app.BatchResult
	err      error
}

func (m *Model) loadGraphCmd(snapshot domain.Snapshot) tea.Cmd {
	m.loadSerial++
	serial := m.loadSerial
	m.loading = true
	return func() tea.Msg {
		if err := m.ctx.Err(); err != nil {
			return graphLoadedMsg{serial: serial, snapshot: snapshot, err: err}
		}
		var revision domain.Revision
		var err error
		if snapshot.IsCurrent() {
			revision, err = m.backend.CurrentRevision(m.ctx)
			if err != nil {
				return graphLoadedMsg{serial: serial, snapshot: snapshot, err: err}
			}
		} else if snapshot.Revision != nil {
			revision = *snapshot.Revision
		}
		nodes, edges, err := m.backend.Graph(m.ctx, snapshot)
		return graphLoadedMsg{serial: serial, snapshot: snapshot, revision: revision, nodes: nodes, edges: edges, err: err}
	}
}

func (m *Model) loadHistoryCmd() tea.Cmd {
	return func() tea.Msg {
		if err := m.ctx.Err(); err != nil {
			return historyLoadedMsg{err: err}
		}
		history, err := m.backend.Revisions(m.ctx)
		return historyLoadedMsg{history: history, err: err}
	}
}

func (m *Model) pollCmd() tea.Cmd {
	return tea.Tick(m.pollInterval, func(now time.Time) tea.Msg { return pollTickMsg(now) })
}

func (m *Model) checkRevisionCmd() tea.Cmd {
	return func() tea.Msg {
		revision, err := m.backend.CurrentRevision(m.ctx)
		return revisionCheckedMsg{revision: revision, err: err}
	}
}

func (m *Model) executeCmd(request app.ExecuteRequest, mutation mutationKind) tea.Cmd {
	m.execSerial++
	serial := m.execSerial
	m.executing = true
	return func() tea.Msg {
		if err := m.ctx.Err(); err != nil {
			return executedMsg{serial: serial, mutation: mutation, err: err}
		}
		result, err := m.backend.Execute(m.ctx, request)
		return executedMsg{serial: serial, mutation: mutation, result: result, err: err}
	}
}

// Update applies terminal and backend messages. It returns the same pointer so
// tests can inspect model state without type conversions or terminal setup.
func (m *Model) Update(message tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := message.(type) {
	case tea.WindowSizeMsg:
		m.resize(msg.Width, msg.Height)
		return m, nil
	case tea.BackgroundColorMsg:
		m.dark = msg.IsDark()
		m.styles = makeStyles(m.dark, m.noColor)
		return m, nil
	case graphLoadedMsg:
		return m, m.acceptGraph(msg)
	case historyLoadedMsg:
		if msg.err != nil {
			if !isCancellation(msg.err) {
				m.err = fmt.Errorf("load history: %w", msg.err)
			}
			return m, nil
		}
		m.history = append([]domain.RevisionInfo(nil), msg.history...)
		sort.SliceStable(m.history, func(i, j int) bool { return m.history[i].Revision > m.history[j].Revision })
		if len(m.history) > 0 && m.history[0].Revision > m.liveRev {
			m.liveRev = m.history[0].Revision
		}
		m.clampHistory()
		return m, nil
	case pollTickMsg:
		if m.ctx.Err() != nil || m.pollInterval <= 0 {
			return m, nil
		}
		return m, m.checkRevisionCmd()
	case revisionCheckedMsg:
		var cmds []tea.Cmd
		if msg.err != nil {
			if !isCancellation(msg.err) {
				m.status = "Refresh check failed: " + msg.err.Error()
			}
		} else {
			changed := msg.revision != m.liveRev
			m.liveRev = msg.revision
			if changed && m.snapshot.IsCurrent() {
				cmds = append(cmds, m.loadGraphCmd(domain.Snapshot{}), m.loadHistoryCmd())
			}
		}
		if m.ctx.Err() == nil && m.pollInterval > 0 {
			cmds = append(cmds, m.pollCmd())
		}
		return m, tea.Batch(cmds...)
	case executedMsg:
		return m, m.acceptExecution(msg)
	case tea.MouseClickMsg:
		return m, m.handleMouse(msg.Mouse())
	case tea.MouseWheelMsg:
		return m, m.handleWheel(msg.Mouse())
	case tea.KeyPressMsg:
		return m, m.handleKey(msg)
	}
	return m, nil
}

func (m *Model) acceptGraph(msg graphLoadedMsg) tea.Cmd {
	if msg.serial != m.loadSerial {
		return nil
	}
	m.loading = false
	if msg.err != nil {
		if !isCancellation(msg.err) {
			m.err = fmt.Errorf("load graph: %w", msg.err)
		}
		return nil
	}
	m.err = nil
	m.snapshot = msg.snapshot
	if msg.snapshot.IsCurrent() {
		m.liveRev = msg.revision
	}
	m.nodes = append([]domain.Node(nil), msg.nodes...)
	m.edges = append([]domain.Edge(nil), msg.edges...)
	m.nodeByID = make(map[domain.EntityID]domain.Node, len(m.nodes))
	for _, node := range m.nodes {
		m.nodeByID[node.ID] = node
	}
	previous := m.selected
	m.rebuildNavigation()
	if _, exists := m.nodeByID[previous]; exists {
		m.selectNode(previous)
	} else if len(m.outlineRows) > 0 {
		m.selectNode(m.outlineRows[0].ID)
	} else {
		m.selected = ""
	}
	mode := "live"
	if !m.snapshot.IsCurrent() {
		mode = fmt.Sprintf("revision %d", snapshotRevision(m.snapshot))
	}
	m.status = fmt.Sprintf("Loaded %d nodes and %d edges at %s", len(m.nodes), len(m.edges), mode)
	return nil
}

func (m *Model) acceptExecution(msg executedMsg) tea.Cmd {
	if msg.serial != m.execSerial {
		return nil
	}
	m.executing = false
	if msg.err != nil {
		if msg.mutation != 0 {
			m.mutationErr = msg.err
		} else {
			m.queryErr = msg.err
		}
		return nil
	}
	result := msg.result
	m.result = &result
	m.queryErr = nil
	if msg.mutation != 0 {
		m.overlay = overlayNone
		m.mutationErr = nil
		m.status = mutationName(msg.mutation) + " completed"
	}
	changed := msg.result.Revision != nil
	if !changed {
		for _, result := range msg.result.Results {
			if result.Summary.Changed() {
				changed = true
				break
			}
		}
	}
	if changed {
		return tea.Batch(m.loadGraphCmd(domain.Snapshot{}), m.loadHistoryCmd())
	}
	if msg.mutation != 0 {
		return nil
	}
	m.status = fmt.Sprintf("Query returned %d statement result(s)", len(msg.result.Results))
	return nil
}

func (m *Model) handleKey(msg tea.KeyPressMsg) tea.Cmd {
	key := msg.String()
	if key == "ctrl+c" {
		return tea.Quit
	}
	if m.overlay != overlayNone {
		return m.handleOverlayKey(msg)
	}
	if key == "ctrl+k" {
		m.openPalette()
		return nil
	}
	if key == "?" && m.tab != QueryTab {
		m.overlay = overlayHelp
		return nil
	}
	// Plain digits remain available inside the Cypher/JSON editors. Query users
	// can still switch tabs with ctrl+arrows, mouse, or the command palette.
	if key >= "1" && key <= "4" && m.tab != QueryTab {
		m.setTab(Tab(key[0] - '1'))
		return nil
	}
	if key == "ctrl+left" {
		m.setTab((m.tab + 3) % 4)
		return nil
	}
	if key == "ctrl+right" {
		m.setTab((m.tab + 1) % 4)
		return nil
	}
	if key == "q" && m.tab != QueryTab {
		return tea.Quit
	}
	if key == "r" && m.tab != QueryTab {
		m.status = "Refreshing…"
		return tea.Batch(m.loadGraphCmd(m.snapshot), m.loadHistoryCmd())
	}

	switch m.tab {
	case OutlineTab:
		return m.handleOutlineKey(key)
	case GraphTab:
		return m.handleGraphKey(key)
	case QueryTab:
		return m.handleQueryKey(msg)
	case HistoryTab:
		return m.handleHistoryKey(key)
	}
	return nil
}

func (m *Model) handleOutlineKey(key string) tea.Cmd {
	switch key {
	case "up", "k":
		m.moveOutline(-1)
	case "down", "j":
		m.moveOutline(1)
	case "home", "g":
		m.outlineIndex = 0
		m.syncOutlineSelection()
	case "end", "G":
		m.outlineIndex = max(0, len(m.outlineRows)-1)
		m.syncOutlineSelection()
	case "enter", "right", "l":
		m.toggleSelected(false)
	case "left", "h":
		m.toggleSelected(true)
	case "/":
		m.openSearch()
	case "i":
		m.overlay = overlayInspector
	case "c":
		m.openMutation(mutationCreate)
	case "e":
		m.openMutation(mutationEdit)
	case "p":
		m.openMutation(mutationReparent)
	case "d":
		m.openMutation(mutationDelete)
	}
	return nil
}

func (m *Model) handleGraphKey(key string) tea.Cmd {
	switch key {
	case "up", "k":
		m.moveGraph(-1)
	case "down", "j":
		m.moveGraph(1)
	case "left", "h":
		m.graphPanX = max(0, m.graphPanX-4)
	case "right", "l":
		m.graphPanX += 4
	case "pgup":
		m.graphPanY = max(0, m.graphPanY-5)
	case "pgdown":
		m.graphPanY = min(max(0, len(m.graphRows)-1), m.graphPanY+5)
	case "+", "=":
		m.graphZoom = min(3, m.graphZoom+1)
		m.rebuildGraph()
	case "-", "_":
		m.graphZoom = max(1, m.graphZoom-1)
		m.rebuildGraph()
	case "0":
		m.graphPanX, m.graphPanY, m.graphZoom = 0, 0, 2
		m.rebuildGraph()
	case "/":
		m.openSearch()
	case "i", "enter":
		m.overlay = overlayInspector
	case "c":
		m.openMutation(mutationCreate)
	case "e":
		m.openMutation(mutationEdit)
	case "p":
		m.openMutation(mutationReparent)
	case "d":
		m.openMutation(mutationDelete)
	}
	return nil
}

func (m *Model) handleQueryKey(msg tea.KeyPressMsg) tea.Cmd {
	key := msg.String()
	switch key {
	case "ctrl+r", "ctrl+enter":
		return m.runQuery(true)
	case "ctrl+x":
		return m.runQuery(false)
	case "tab":
		m.cycleQueryFocus(1)
		return nil
	case "shift+tab":
		m.cycleQueryFocus(-1)
		return nil
	case "ctrl+j":
		if m.queryFocus == focusResults {
			m.resultY++
			return nil
		}
	case "ctrl+u":
		if m.queryFocus == focusResults {
			m.resultY = max(0, m.resultY-1)
			return nil
		}
	}
	var cmd tea.Cmd
	if m.queryFocus == focusParams {
		m.params, cmd = m.params.Update(msg)
	} else if m.queryFocus == focusQuery {
		m.query, cmd = m.query.Update(msg)
	} else {
		switch key {
		case "up", "k":
			m.resultY = max(0, m.resultY-1)
		case "down", "j":
			m.resultY++
		case "left", "h":
			m.resultX = max(0, m.resultX-4)
		case "right", "l":
			m.resultX += 4
		}
	}
	return cmd
}

func (m *Model) handleHistoryKey(key string) tea.Cmd {
	switch key {
	case "up", "k":
		m.moveHistory(-1)
	case "down", "j":
		m.moveHistory(1)
	case "enter":
		return m.openSelectedRevision()
	case "l":
		return m.returnLive()
	case "home", "g":
		m.historyIndex = 0
	case "end", "G":
		m.historyIndex = max(0, len(m.history)-1)
	}
	return nil
}

func (m *Model) handleOverlayKey(msg tea.KeyPressMsg) tea.Cmd {
	key := msg.String()
	switch m.overlay {
	case overlayHelp, overlayInspector:
		if key == "esc" || key == "?" || key == "q" || key == "i" {
			m.overlay = overlayNone
		} else if m.overlay == overlayInspector {
			switch key {
			case "up", "k":
				m.inspectorScroll = max(0, m.inspectorScroll-1)
			case "down", "j":
				m.inspectorScroll++
			case "pgup":
				m.inspectorScroll = max(0, m.inspectorScroll-8)
			case "pgdown":
				m.inspectorScroll += 8
			}
		}
	case overlaySearch:
		if key == "esc" {
			m.search.SetValue("")
			m.search.Blur()
			m.overlay = overlayNone
			m.rebuildNavigation()
			return nil
		}
		if key == "enter" {
			m.search.Blur()
			m.overlay = overlayNone
			return nil
		}
		var cmd tea.Cmd
		m.search, cmd = m.search.Update(msg)
		m.rebuildNavigation()
		return cmd
	case overlayPalette:
		if key == "esc" {
			m.palette.Blur()
			m.overlay = overlayNone
			return nil
		}
		commands := m.filteredCommands()
		switch key {
		case "up", "ctrl+p":
			m.paletteIndex = max(0, m.paletteIndex-1)
			return nil
		case "down", "ctrl+n":
			m.paletteIndex = min(max(0, len(commands)-1), m.paletteIndex+1)
			return nil
		case "enter":
			if len(commands) > 0 {
				return m.runPaletteCommand(commands[m.paletteIndex])
			}
			return nil
		}
		var cmd tea.Cmd
		m.palette, cmd = m.palette.Update(msg)
		m.paletteIndex = 0
		return cmd
	case overlayMutation:
		if key == "esc" || (m.mutation == mutationDelete && key == "n") {
			m.mutationInput.Blur()
			m.overlay = overlayNone
			m.mutationErr = nil
			return nil
		}
		if m.mutation == mutationDelete {
			if key == "y" || key == "enter" {
				return m.submitMutation()
			}
			return nil
		}
		if key == "ctrl+s" {
			return m.submitMutation()
		}
		var cmd tea.Cmd
		m.mutationInput, cmd = m.mutationInput.Update(msg)
		return cmd
	}
	return nil
}

func (m *Model) runQuery(readOnly bool) tea.Cmd {
	query := strings.TrimSpace(m.query.Value())
	if query == "" {
		m.queryErr = errors.New("query is empty")
		return nil
	}
	params := make(map[string]any)
	if err := decodeObject(m.params.Value(), &params); err != nil {
		m.queryErr = fmt.Errorf("parameters: %w", err)
		return nil
	}
	if !m.snapshot.IsCurrent() && !readOnly {
		m.queryErr = errors.New("historical mode is read-only; return to Live before executing mutations")
		return nil
	}
	m.queryErr = nil
	return m.executeCmd(app.ExecuteRequest{
		Query: query, Params: params, Snapshot: m.snapshot, ReadOnly: readOnly,
		Actor: "tui", Message: "query console",
	}, 0)
}

func (m *Model) cycleQueryFocus(delta int) {
	m.query.Blur()
	m.params.Blur()
	m.queryFocus = focusArea((int(m.queryFocus) + delta + 3) % 3)
	if m.queryFocus == focusQuery {
		_ = m.query.Focus()
	} else if m.queryFocus == focusParams {
		_ = m.params.Focus()
	}
}

func (m *Model) openSelectedRevision() tea.Cmd {
	if len(m.history) == 0 {
		m.status = "No revisions yet"
		return nil
	}
	m.clampHistory()
	revision := m.history[m.historyIndex].Revision
	m.status = fmt.Sprintf("Opening revision %d…", revision)
	return m.loadGraphCmd(domain.Snapshot{Revision: &revision})
}

func (m *Model) returnLive() tea.Cmd {
	if m.snapshot.IsCurrent() {
		m.status = "Already viewing Live"
		return nil
	}
	m.status = "Returning to Live…"
	return m.loadGraphCmd(domain.Snapshot{})
}

func (m *Model) handleMouse(mouse tea.Mouse) tea.Cmd {
	if mouse.Button != tea.MouseLeft {
		return nil
	}
	for _, hit := range m.hits {
		if mouse.X < hit.X0 || mouse.X > hit.X1 || mouse.Y < hit.Y0 || mouse.Y > hit.Y1 {
			continue
		}
		switch hit.Kind {
		case hitTab:
			m.setTab(hit.Tab)
		case hitNode:
			m.selectNode(hit.Node)
		case hitHistory:
			for index, info := range m.history {
				if info.Revision == hit.Revision {
					m.historyIndex = index
					break
				}
			}
		case hitOverlay:
			if m.overlay == overlayPalette {
				commands := m.filteredCommands()
				if hit.OverlayRow >= 0 && hit.OverlayRow < len(commands) {
					m.paletteIndex = hit.OverlayRow
					return m.runPaletteCommand(commands[hit.OverlayRow])
				}
			}
		}
		break
	}
	return nil
}

func (m *Model) handleWheel(mouse tea.Mouse) tea.Cmd {
	delta := 1
	if mouse.Button == tea.MouseWheelUp {
		delta = -1
	}
	if m.overlay == overlayInspector {
		m.inspectorScroll = max(0, m.inspectorScroll+delta*3)
		return nil
	}
	switch m.tab {
	case OutlineTab:
		m.moveOutline(delta * 3)
	case GraphTab:
		m.moveGraph(delta * 2)
	case QueryTab:
		m.resultY = max(0, m.resultY+delta*3)
	case HistoryTab:
		m.moveHistory(delta * 3)
	}
	return nil
}

func (m *Model) setTab(tab Tab) {
	m.tab = tab
	if tab != QueryTab {
		m.query.Blur()
		m.params.Blur()
	} else if m.queryFocus == focusQuery {
		_ = m.query.Focus()
	} else if m.queryFocus == focusParams {
		_ = m.params.Focus()
	}
}

func (m *Model) resize(width, height int) {
	m.width, m.height = max(30, width), max(10, height)
	mainWidth := max(20, width-6)
	if width >= 100 && m.showInspector {
		mainWidth = max(30, width*2/3-8)
	}
	m.query.SetWidth(mainWidth)
	m.params.SetWidth(mainWidth)
	m.query.SetHeight(max(4, min(10, height/3)))
	m.params.SetHeight(max(3, min(6, height/5)))
	m.search.SetWidth(min(58, max(20, width-12)))
	m.palette.SetWidth(min(62, max(22, width-12)))
	m.mutationInput.SetWidth(min(74, max(24, width-14)))
	m.mutationInput.SetHeight(min(16, max(7, height-10)))
	m.ensureVisible()
}

func isCancellation(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

func snapshotRevision(snapshot domain.Snapshot) domain.Revision {
	if snapshot.Revision != nil {
		return *snapshot.Revision
	}
	return 0
}
