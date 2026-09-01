package tui

import (
	"fmt"
	"strings"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/svlocks/sheets/internal/domain"
)

type outlineEntry struct {
	id        domain.EntityID
	title     string
	subtitle  string
	position  *int64
	synthetic bool
	child     bool
}

func (e outlineEntry) String() string {
	if e.synthetic {
		return e.title
	}
	prefix := ""
	if e.child {
		prefix = "◇ "
	}
	if e.child && e.position != nil {
		prefix = fmt.Sprintf("%d. ", *e.position)
	}
	if e.subtitle == "" {
		return prefix + e.title
	}
	return prefix + e.title + "  ·  " + e.subtitle
}

type workKeyMap struct {
	Down, Up, PageDown, PageUp, HalfPageUp, HalfPageDown key.Binding
	GoToTop, GoToBottom, Toggle, Open, Close             key.Binding
}

func defaultWorkKeyMap() workKeyMap {
	return workKeyMap{
		Down:         binding("↓/j", "down", "down", "j", "ctrl+n"),
		Up:           binding("↑/k", "up", "up", "k", "ctrl+p"),
		PageDown:     binding("f/pgdn", "page down", "pgdown", " ", "f"),
		PageUp:       binding("b/pgup", "page up", "pgup", "b"),
		HalfPageDown: binding("d", "½ page down", "d", "ctrl+d"),
		HalfPageUp:   binding("u", "½ page up", "u", "ctrl+u"),
		GoToTop:      binding("g", "top", "g", "home"),
		GoToBottom:   binding("G", "bottom", "G", "shift+g", "end"),
		Toggle:       binding("⏎", "toggle", "enter"),
		Open:         binding("→/l", "open", "right", "l"),
		Close:        binding("←/h", "close", "left", "h"),
	}
}

type workRow struct {
	entry       outlineEntry
	depth       int
	hasChildren bool
}

type workModel struct {
	keys        workKeyMap
	rows        []workRow
	rowByID     map[domain.EntityID]int
	open        map[domain.EntityID]bool
	graph       graphState
	selected    domain.EntityID
	cursor      int
	offset      int
	width       int
	height      int
	styles      styleSet
	initialized bool
}

func newWorkModel(styles styleSet, _ bool) workModel {
	return workModel{
		keys: defaultWorkKeyMap(), rowByID: make(map[domain.EntityID]int),
		open: make(map[domain.EntityID]bool), width: 40, height: 20,
		styles: styles,
	}
}

func (m *workModel) setStyle(styles styleSet, _ bool) {
	m.styles = styles
}

func (m *workModel) setSize(width, height int) {
	m.width = clampSize(width, 1)
	m.height = clampSize(height, 1)
	m.ensureVisible()
}

func (m *workModel) setGraph(graph graphState) {
	previousSelection := m.selected
	for id := range graph.nodeByID {
		if _, known := m.open[id]; !known {
			// Preserve the established first-load behavior: the full hierarchy is
			// expanded, but only visible terminal rows are rendered.
			m.open[id] = true
		}
	}
	for id := range m.open {
		if _, exists := graph.nodeByID[id]; !exists {
			delete(m.open, id)
		}
	}
	m.graph = graph
	m.initialized = true
	m.rebuildRows()
	if _, exists := graph.nodeByID[previousSelection]; !exists {
		previousSelection = graph.firstNodeID()
	}
	m.selectID(previousSelection)
}

type pendingWorkRow struct {
	id       domain.EntityID
	depth    int
	position *int64
	child    bool
}

func (m *workModel) rebuildRows() {
	rows := make([]workRow, 0, len(m.graph.nodes)+2)
	rows = append(rows, workRow{entry: outlineEntry{
		title: fmt.Sprintf("All work (%d)", len(m.graph.nodes)), synthetic: true,
	}, hasChildren: len(m.graph.nodes) > 0})
	rowByID := make(map[domain.EntityID]int, len(m.graph.nodes))
	emitted := make(map[domain.EntityID]bool, len(m.graph.nodes))

	appendForest := func(roots []domain.EntityID, depth int) {
		stack := make([]pendingWorkRow, 0, len(roots))
		for index := len(roots) - 1; index >= 0; index-- {
			stack = append(stack, pendingWorkRow{id: roots[index], depth: depth})
		}
		for len(stack) > 0 {
			last := len(stack) - 1
			pending := stack[last]
			stack = stack[:last]
			if emitted[pending.id] {
				continue
			}
			node, exists := m.graph.nodeByID[pending.id]
			if !exists {
				continue
			}
			emitted[pending.id] = true
			rowByID[pending.id] = len(rows)
			children := m.graph.children[pending.id]
			rows = append(rows, workRow{
				entry: outlineEntry{
					id: pending.id, title: nodeTitle(node), subtitle: nodeSubtitle(node),
					position: pending.position, child: pending.child,
				},
				depth: pending.depth, hasChildren: len(children) > 0,
			})
			if !m.open[pending.id] {
				continue
			}
			for index := len(children) - 1; index >= 0; index-- {
				link := children[index]
				stack = append(stack, pendingWorkRow{
					id: link.child, depth: pending.depth + 1,
					position: link.edge.Position, child: true,
				})
			}
		}
	}

	appendForest(m.graph.roots, 1)
	if len(m.graph.unreachable) > 0 {
		rows = append(rows, workRow{entry: outlineEntry{title: "Unreachable / invalid", synthetic: true}, depth: 1, hasChildren: true})
		appendForest(m.graph.unreachable, 2)
	}
	m.rows = rows
	m.rowByID = rowByID
	if index, exists := rowByID[m.selected]; exists {
		m.cursor = index
	} else if m.cursor >= len(rows) {
		m.cursor = max(0, len(rows)-1)
	}
	m.syncSelection()
	m.ensureVisible()
}

func (m *workModel) selectID(id domain.EntityID) bool {
	if _, exists := m.graph.nodeByID[id]; !exists {
		m.selected = ""
		m.cursor = 0
		m.offset = 0
		return false
	}
	seen := make(map[domain.EntityID]bool)
	for current := id; current != "" && !seen[current]; {
		seen[current] = true
		parentEdge, ok := m.graph.parent[current]
		if !ok {
			break
		}
		m.open[parentEdge.From] = true
		current = parentEdge.From
	}
	m.rebuildRows()
	index, exists := m.rowByID[id]
	if !exists {
		m.selected = ""
		return false
	}
	m.cursor = index
	m.selected = id
	m.ensureVisible()
	return true
}

func (m *workModel) update(message any) tea.Cmd {
	msg, ok := message.(tea.KeyPressMsg)
	if !ok {
		return nil
	}
	switch {
	case key.Matches(msg, m.keys.Down):
		m.move(1)
	case key.Matches(msg, m.keys.Up):
		m.move(-1)
	case key.Matches(msg, m.keys.PageDown):
		m.move(max(1, m.height-1))
	case key.Matches(msg, m.keys.PageUp):
		m.move(-max(1, m.height-1))
	case key.Matches(msg, m.keys.HalfPageDown):
		m.move(max(1, m.height/2))
	case key.Matches(msg, m.keys.HalfPageUp):
		m.move(-max(1, m.height/2))
	case key.Matches(msg, m.keys.GoToTop):
		m.cursor = 0
		m.syncSelection()
		m.ensureVisible()
	case key.Matches(msg, m.keys.GoToBottom):
		m.cursor = max(0, len(m.rows)-1)
		m.syncSelection()
		m.ensureVisible()
	case key.Matches(msg, m.keys.Toggle):
		m.toggleSelected()
	case key.Matches(msg, m.keys.Open):
		m.openSelected()
	case key.Matches(msg, m.keys.Close):
		m.closeSelected()
	}
	return nil
}

func (m *workModel) move(delta int) {
	if len(m.rows) == 0 {
		return
	}
	m.cursor = min(len(m.rows)-1, max(0, m.cursor+delta))
	m.syncSelection()
	m.ensureVisible()
}

func (m *workModel) toggleSelected() {
	row, ok := m.selectedRow()
	if !ok || !row.hasChildren {
		return
	}
	m.open[row.entry.id] = !m.open[row.entry.id]
	m.rebuildRows()
}

func (m *workModel) openSelected() {
	row, ok := m.selectedRow()
	if !ok || !row.hasChildren || m.open[row.entry.id] {
		return
	}
	m.open[row.entry.id] = true
	m.rebuildRows()
}

func (m *workModel) closeSelected() {
	row, ok := m.selectedRow()
	if !ok {
		return
	}
	if row.hasChildren && m.open[row.entry.id] {
		m.open[row.entry.id] = false
		m.rebuildRows()
		return
	}
	if edge, exists := m.graph.parent[row.entry.id]; exists {
		m.selectID(edge.From)
	}
}

func (m *workModel) selectedRow() (workRow, bool) {
	if m.cursor < 0 || m.cursor >= len(m.rows) {
		return workRow{}, false
	}
	row := m.rows[m.cursor]
	return row, !row.entry.synthetic
}

func (m *workModel) syncSelection() {
	row, ok := m.selectedRow()
	if !ok {
		m.selected = ""
		return
	}
	m.selected = row.entry.id
}

func (m *workModel) clickRow(row int) {
	if row < 0 || m.offset+row >= len(m.rows) {
		return
	}
	m.cursor = m.offset + row
	m.syncSelection()
	m.ensureVisible()
}

func (m *workModel) ensureVisible() {
	if m.cursor < m.offset {
		m.offset = m.cursor
	}
	if m.cursor >= m.offset+m.height {
		m.offset = m.cursor - m.height + 1
	}
	maximum := max(0, len(m.rows)-m.height)
	m.offset = min(maximum, max(0, m.offset))
}

func (m workModel) view() string {
	if !m.initialized {
		return "Loading hierarchy…"
	}
	if len(m.graph.nodes) == 0 {
		return strings.Join([]string{
			"No work items yet.",
			"",
			"Press n to create the first node, or Ctrl+K to browse commands.",
		}, "\n")
	}
	end := min(len(m.rows), m.offset+m.height)
	lines := make([]string, 0, end-m.offset)
	for index := m.offset; index < end; index++ {
		lines = append(lines, m.renderRow(index, m.rows[index]))
	}
	return strings.Join(lines, "\n")
}

func (m workModel) renderRow(index int, row workRow) string {
	selection := "  "
	style := m.styles.subtle
	if index == m.cursor {
		selection = "> "
		style = m.styles.focusedTitle
	}
	marker := "  "
	if row.hasChildren {
		marker = "> "
		if row.entry.synthetic || m.open[row.entry.id] {
			marker = "v "
		}
	}
	indentWidth := max(0, m.width-ansi.StringWidth(selection)-ansi.StringWidth(marker)-8)
	depth := row.depth
	indent := ""
	if depth*2 <= indentWidth {
		indent = strings.Repeat("  ", depth)
	} else if depth > 0 {
		indent = fmt.Sprintf("…%d ", depth)
	}
	line := selection + indent + marker + row.entry.String()
	line = ansi.Truncate(line, m.width, "…")
	if index == m.cursor {
		return style.Render(line)
	}
	return line
}
