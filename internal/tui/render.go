package tui

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/glamour/v2"
	glamourStyles "charm.land/glamour/v2/styles"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/svlocks/sheets/internal/app"
	"github.com/svlocks/sheets/internal/domain"
)

// View renders a responsive alternate-screen application. Wide terminals use
// navigator/main/inspector columns; compact terminals devote the full body to
// the active view and expose the inspector with i/Enter.
func (m *Model) View() tea.View {
	m.hits = m.hits[:0]
	header := m.renderHeader()
	bodyHeight := max(3, m.height-4)
	var body string
	if m.overlay != overlayNone {
		body = m.renderOverlay(m.width, bodyHeight)
	} else if m.loading && len(m.nodes) == 0 {
		body = placeText(m.width, bodyHeight, "Loading graph…")
	} else if m.err != nil {
		body = placeText(m.width, bodyHeight, m.styles.danger.Render("Could not load graph\n")+m.err.Error()+"\n\nPress r to retry")
	} else {
		body = m.renderWorkspace(m.width, bodyHeight)
	}
	status := m.renderStatus()
	content := strings.Join([]string{header, fitBlock(body, m.width, bodyHeight), status}, "\n")
	view := tea.NewView(fitBlock(content, m.width, m.height))
	view.AltScreen = true
	view.MouseMode = tea.MouseModeCellMotion
	view.WindowTitle = "sheets — " + filepath.Base(m.backend.ProjectRoot())
	return view
}

func (m *Model) renderHeader() string {
	root := m.backend.ProjectRoot()
	if root == "" {
		root = "."
	}
	project := m.styles.title.Render(" sheets ") + " " + m.styles.subtle.Render(root)
	revision := fmt.Sprintf("Live · r%d", m.liveRev)
	if !m.snapshot.IsCurrent() {
		revision = fmt.Sprintf("HISTORICAL · r%d · READ-ONLY", snapshotRevision(m.snapshot))
	}
	first := joinSides(project, m.styles.banner.Render(revision), m.width)

	var tabs strings.Builder
	x := 0
	for index, name := range tabNames {
		text := fmt.Sprintf("%d %s", index+1, name)
		if m.width < 46 {
			text = fmt.Sprintf("%d %c", index+1, name[0])
		}
		style := m.styles.tab
		if Tab(index) == m.tab {
			style = m.styles.activeTab
		}
		rendered := style.Render(text)
		width := ansi.StringWidth(rendered)
		m.hits = append(m.hits, hitArea{Kind: hitTab, X0: x, X1: x + width - 1, Y0: 1, Y1: 1, Tab: Tab(index)})
		tabs.WriteString(rendered)
		x += width
	}
	context := "ctrl+k commands  ? help"
	second := joinSides(tabs.String(), m.styles.subtle.Render(context), m.width)
	banner := ""
	if !m.snapshot.IsCurrent() {
		banner = m.styles.banner.Render("◆ Historical snapshot: all mutations are disabled. History → l returns to Live.")
	} else {
		banner = m.styles.subtle.Render(fmt.Sprintf("%d nodes  ·  %d edges  ·  daemonless", len(m.nodes), len(m.edges)))
	}
	return strings.Join([]string{fitLine(first, m.width), fitLine(second, m.width), fitLine(banner, m.width)}, "\n")
}

func (m *Model) renderWorkspace(width, height int) string {
	if width >= 110 {
		navigatorWidth := min(32, max(24, width/4))
		inspectorWidth := min(42, max(30, width/3))
		mainWidth := width - navigatorWidth - inspectorWidth
		m.hitOffsetX = 0
		nav := m.panel("Navigator", m.renderOutline(navigatorWidth-2, height-2, true), navigatorWidth, height, false)
		m.hitOffsetX = navigatorWidth
		main := m.panel(m.tab.String(), m.renderMain(mainWidth-2, height-2), mainWidth, height, true)
		inspector := m.panel("Inspector", m.renderInspector(inspectorWidth-2, height-2), inspectorWidth, height, false)
		m.hitOffsetX = 0
		return lipgloss.JoinHorizontal(lipgloss.Top, nav, main, inspector)
	}
	m.hitOffsetX = 0
	return m.panel(m.tab.String(), m.renderMain(width-2, height-2), width, height, true)
}

func (m *Model) renderMain(width, height int) string {
	switch m.tab {
	case OutlineTab:
		return m.renderOutline(width, height, false)
	case GraphTab:
		return m.renderGraph(width, height)
	case QueryTab:
		return m.renderQuery(width, height)
	case HistoryTab:
		return m.renderHistory(width, height)
	default:
		return ""
	}
}

func (m *Model) panel(title, content string, width, height int, active bool) string {
	style := m.styles.panel
	if active {
		style = m.styles.activePanel
	}
	titleLine := m.styles.accent.Render(" " + title + " ")
	innerHeight := max(1, height-2)
	innerWidth := max(1, width-2)
	content = fitBlock(content, innerWidth, innerHeight)
	// A panel title lives in the first content row, avoiding border surgery and
	// keeping exact dimensions predictable across Unicode border sets.
	contentLines := strings.Split(content, "\n")
	if len(contentLines) > 0 {
		contentLines[0] = joinSides(titleLine, contentLines[0], innerWidth)
	}
	return style.Width(innerWidth).Height(innerHeight).Render(strings.Join(contentLines, "\n"))
}

func (m *Model) renderOutline(width, height int, compact bool) string {
	if len(m.outlineRows) == 0 {
		if strings.TrimSpace(m.search.Value()) != "" {
			return "No nodes match " + fmt.Sprintf("%q", m.search.Value()) + "\n\n/ change filter  esc clear"
		}
		return "The graph is empty.\n\nc create the first node"
	}
	start := m.outlineOffset
	if compact {
		start = max(0, m.outlineIndex-height/2)
	}
	lines := make([]string, 0, height)
	if filter := strings.TrimSpace(m.search.Value()); filter != "" {
		lines = append(lines, m.styles.accent.Render("Filter: "+filter))
	}
	for index := start; index < len(m.outlineRows) && len(lines) < height; index++ {
		row := m.outlineRows[index]
		if row.Section != "" && len(lines) < height {
			lines = append(lines, m.styles.subtle.Render("── "+row.Section+" "))
		}
		node := m.nodeByID[row.ID]
		marker := "  "
		if index == m.outlineIndex {
			marker = "› "
		}
		toggle := "•"
		if row.HasKids {
			toggle = "▾"
			if m.collapsed[row.ID] {
				toggle = "▸"
			}
		}
		kind := ""
		if row.Orphan {
			kind = " ◌"
		}
		if row.Cycle {
			kind = " ⟳"
		}
		position := ""
		if row.Position != nil {
			position = fmt.Sprintf(" #%d", *row.Position)
		} else if row.Depth > 0 {
			position = " ·"
		}
		indent := strings.Repeat("  ", row.Depth)
		text := marker + indent + toggle + " " + nodeTitle(node) + position + kind
		if !compact && len(node.Labels) > 0 {
			text += "  [" + strings.Join(node.Labels, ",") + "]"
		}
		text = fitLine(text, width)
		if index == m.outlineIndex {
			text = m.styles.selected.Width(width).Render(text)
		}
		lineY := 4 + len(lines)
		m.hits = append(m.hits, hitArea{Kind: hitNode, X0: m.hitOffsetX, X1: m.hitOffsetX + width - 1, Y0: lineY, Y1: lineY, Node: row.ID})
		lines = append(lines, text)
	}
	return strings.Join(lines, "\n")
}

func (m *Model) renderGraph(width, height int) string {
	if len(m.graphRows) == 0 {
		return "No topology to display.\n\nc create node  / filter"
	}
	lines := []string{m.styles.subtle.Render(fmt.Sprintf("zoom %d/3 · pan x:%d y:%d · +/- zoom · h/l pan · 0 reset", m.graphZoom, m.graphPanX, m.graphPanY))}
	for index := m.graphPanY; index < len(m.graphRows) && len(lines) < height; index++ {
		row := m.graphRows[index]
		for lineIndex, raw := range row.Lines {
			if len(lines) >= height {
				break
			}
			line := cutFrom(raw, m.graphPanX, width)
			if index == m.graphIndex && lineIndex == 0 {
				line = m.styles.selected.Width(width).Render(fitLine("› "+line, width))
			} else {
				line = "  " + line
			}
			if row.ID != "" {
				y := 4 + len(lines)
				m.hits = append(m.hits, hitArea{Kind: hitNode, X0: m.hitOffsetX, X1: m.hitOffsetX + width - 1, Y0: y, Y1: y, Node: row.ID})
			}
			lines = append(lines, fitLine(line, width))
		}
		if len(lines) < height {
			lines = append(lines, "")
		}
	}
	return strings.Join(lines, "\n")
}

func (m *Model) renderQuery(width, height int) string {
	queryHeight := max(4, min(9, height/3))
	paramsHeight := max(3, min(5, height/5))
	m.query.SetWidth(max(10, width-2))
	m.query.SetHeight(queryHeight - 1)
	m.params.SetWidth(max(10, width-2))
	m.params.SetHeight(paramsHeight - 1)
	queryTitle := "Cypher"
	if m.queryFocus == focusQuery {
		queryTitle = "▸ Cypher"
	}
	paramsTitle := "JSON parameters"
	if m.queryFocus == focusParams {
		paramsTitle = "▸ JSON parameters"
	}
	lines := []string{m.styles.accent.Render(queryTitle), m.query.View(), m.styles.accent.Render(paramsTitle), m.params.View()}
	used := queryHeight + paramsHeight
	resultHeight := max(2, height-used)
	resultTitle := "Results"
	if m.queryFocus == focusResults {
		resultTitle = "▸ Results"
	}
	result := m.renderResults(width, resultHeight-1)
	lines = append(lines, m.styles.accent.Render(resultTitle+"  ·  ctrl+r read  ctrl+x execute  tab focus"), result)
	return strings.Join(lines, "\n")
}

func (m *Model) renderResults(width, height int) string {
	if m.executing {
		return "Executing…"
	}
	if m.queryErr != nil {
		return m.styles.danger.Render("Error: ") + fitLine(m.queryErr.Error(), max(1, width-7))
	}
	if m.result == nil {
		return m.styles.subtle.Render("No result yet")
	}
	lines := make([]string, 0)
	for statementIndex, result := range m.result.Results {
		lines = append(lines, fmt.Sprintf("Statement %d  %s", statementIndex+1, summaryText(result.Summary)))
		if len(result.Columns) == 0 {
			continue
		}
		widths := make([]int, len(result.Columns))
		for index, column := range result.Columns {
			widths[index] = min(28, max(3, ansi.StringWidth(column)))
		}
		for _, row := range result.Rows {
			for index := range widths {
				if index < len(row) {
					widths[index] = min(28, max(widths[index], ansi.StringWidth(formatValue(row[index]))))
				}
			}
		}
		lines = append(lines, tableRow(result.Columns, widths))
		separator := make([]string, len(widths))
		for index, columnWidth := range widths {
			separator[index] = strings.Repeat("─", columnWidth)
		}
		lines = append(lines, strings.Join(separator, "─┼─"))
		for _, row := range result.Rows {
			values := make([]string, len(widths))
			for index := range values {
				if index < len(row) {
					values[index] = formatValue(row[index])
				}
			}
			lines = append(lines, tableRow(values, widths))
		}
	}
	if len(lines) == 0 {
		lines = append(lines, "Query completed with no rows")
	}
	start := min(max(0, m.resultY), max(0, len(lines)-1))
	visible := lines[start:min(len(lines), start+max(1, height))]
	for index := range visible {
		visible[index] = cutFrom(visible[index], m.resultX, width)
	}
	return strings.Join(visible, "\n")
}

func (m *Model) renderHistory(width, height int) string {
	if len(m.history) == 0 {
		return "No revisions yet.\n\nThe first mutation will create revision 1."
	}
	lines := []string{m.styles.subtle.Render("enter open snapshot · l return to Live · historical snapshots are read-only")}
	for index := m.historyOffset; index < len(m.history) && len(lines) < height; index++ {
		info := m.history[index]
		marker := "  "
		if index == m.historyIndex {
			marker = "› "
		}
		viewing := ""
		if (!m.snapshot.IsCurrent() && snapshotRevision(m.snapshot) == info.Revision) || (m.snapshot.IsCurrent() && info.Revision == m.liveRev) {
			viewing = "  ◆ viewing"
		}
		actor := info.Actor
		if actor == "" {
			actor = "unknown actor"
		}
		message := info.Message
		if message == "" {
			message = "no message"
		}
		text := fmt.Sprintf("%sr%-5d  %s  %-14s  %s%s", marker, info.Revision, info.Time.Local().Format("2006-01-02 15:04"), actor, message, viewing)
		text = fitLine(text, width)
		if index == m.historyIndex {
			text = m.styles.selected.Width(width).Render(text)
		}
		y := 4 + len(lines)
		m.hits = append(m.hits, hitArea{Kind: hitHistory, X0: m.hitOffsetX, X1: m.hitOffsetX + width - 1, Y0: y, Y1: y, Revision: info.Revision})
		lines = append(lines, text)
	}
	return strings.Join(lines, "\n")
}

func (m *Model) renderInspector(width, height int) string {
	if m.selected == "" {
		return "No node selected"
	}
	node, exists := m.nodeByID[m.selected]
	if !exists {
		return "Selected node is not present in this snapshot"
	}
	lines := []string{
		m.styles.title.Render(nodeTitle(node)),
		"ID  " + string(node.ID),
		"Labels  " + emptyAs(strings.Join(node.Labels, ", "), "—"),
		"Validity  " + validity(node.ValidFrom, node.ValidTo),
		"",
		m.styles.accent.Render(fmt.Sprintf("Properties · %d", len(node.Properties))),
	}
	properties, _ := json.MarshalIndent(node.Properties, "", "  ")
	lines = append(lines, strings.Split(string(properties), "\n")...)
	lines = append(lines, "", m.styles.accent.Render("Markdown body"))
	if strings.TrimSpace(node.Body) == "" {
		lines = append(lines, m.styles.subtle.Render("(empty)"))
	} else {
		lines = append(lines, strings.Split(m.renderMarkdown(node.Body, width), "\n")...)
	}
	incoming, outgoing := m.incidentEdges(node.ID)
	lines = append(lines, "", m.styles.accent.Render(fmt.Sprintf("Incoming · %d", len(incoming))))
	for _, edge := range incoming {
		lines = append(lines, m.edgeInspectorLine(edge, true))
	}
	if len(incoming) == 0 {
		lines = append(lines, "—")
	}
	lines = append(lines, "", m.styles.accent.Render(fmt.Sprintf("Outgoing · %d", len(outgoing))))
	for _, edge := range outgoing {
		lines = append(lines, m.edgeInspectorLine(edge, false))
		if len(edge.Properties) > 0 {
			encoded, _ := json.Marshal(edge.Properties)
			lines = append(lines, "    properties "+string(encoded))
		}
	}
	if len(outgoing) == 0 {
		lines = append(lines, "—")
	}
	start := min(max(0, m.inspectorScroll), max(0, len(lines)-1))
	return strings.Join(lines[start:min(len(lines), start+max(1, height))], "\n")
}

func (m *Model) edgeInspectorLine(edge domain.Edge, incoming bool) string {
	otherID := edge.To
	arrow := "→"
	if incoming {
		otherID = edge.From
		arrow = "←"
	}
	other := shortID(otherID)
	if node, ok := m.nodeByID[otherID]; ok {
		other = nodeTitle(node)
	}
	position := ""
	if edge.Position != nil {
		position = fmt.Sprintf(" #%d", *edge.Position)
	}
	return fmt.Sprintf("%s %s %s%s  edge:%s  %s", arrow, edge.Type, other, position, shortID(edge.ID), validity(edge.ValidFrom, edge.ValidTo))
}

func (m *Model) incidentEdges(id domain.EntityID) (incoming, outgoing []domain.Edge) {
	for _, edge := range m.edges {
		if edge.To == id {
			incoming = append(incoming, edge)
		}
		if edge.From == id {
			outgoing = append(outgoing, edge)
		}
	}
	sort.SliceStable(incoming, func(i, j int) bool { return incoming[i].ID < incoming[j].ID })
	sort.SliceStable(outgoing, func(i, j int) bool { return outgoing[i].ID < outgoing[j].ID })
	return incoming, outgoing
}

func (m *Model) renderMarkdown(source string, width int) string {
	style := glamourStyles.DarkStyle
	if !m.dark {
		style = glamourStyles.LightStyle
	}
	if m.noColor {
		style = glamourStyles.NoTTYStyle
	}
	renderer, err := glamour.NewTermRenderer(glamour.WithStandardStyle(style), glamour.WithWordWrap(max(10, width-2)))
	if err != nil {
		return source
	}
	rendered, err := renderer.Render(source)
	if err != nil {
		return source
	}
	return strings.TrimSpace(rendered)
}

func (m *Model) renderOverlay(width, height int) string {
	overlayWidth := min(78, max(28, width-8))
	overlayHeight := min(22, max(7, height-2))
	var title, content string
	switch m.overlay {
	case overlaySearch:
		title = "Search nodes"
		content = m.search.View() + "\n\nLive filter across IDs, labels, properties, titles, and Markdown.\nenter keep · esc clear"
		overlayHeight = 8
	case overlayPalette:
		title = "Command palette"
		lines := []string{m.palette.View(), ""}
		commands := m.filteredCommands()
		maxCommands := max(1, overlayHeight-6)
		start := 0
		if m.paletteIndex >= maxCommands {
			start = m.paletteIndex - maxCommands + 1
		}
		for index := start; index < len(commands) && len(lines)-2 < maxCommands; index++ {
			line := "  " + commands[index].Name
			if index == m.paletteIndex {
				line = m.styles.selected.Width(overlayWidth - 4).Render("› " + commands[index].Name)
			}
			y := (height-overlayHeight)/2 + 3 + len(lines)
			m.hits = append(m.hits, hitArea{Kind: hitOverlay, X0: (width - overlayWidth) / 2, X1: (width + overlayWidth) / 2, Y0: y, Y1: y, OverlayRow: index})
			lines = append(lines, line)
		}
		if len(commands) == 0 {
			lines = append(lines, "No matching commands")
		}
		content = strings.Join(lines, "\n")
	case overlayHelp:
		title = "Keyboard & mouse"
		content = helpText(m.tab)
		overlayWidth = min(84, max(30, width-6))
	case overlayInspector:
		title = "Inspector"
		content = m.renderInspector(overlayWidth-4, overlayHeight-4)
	case overlayMutation:
		title = mutationName(m.mutation) + " node"
		if m.mutation == mutationDelete {
			node := m.nodeByID[m.selected]
			content = m.styles.danger.Render("Delete "+nodeTitle(node)+"?") + "\n\nDETACH DELETE removes the node and every incident edge.\nThe previous revision remains available in History.\n\ny / enter confirm · n / esc cancel"
			overlayHeight = 11
		} else {
			content = mutationHint(m.mutation) + "\n\n" + m.mutationInput.View() + "\n\nctrl+s submit · esc cancel"
			if m.mutationErr != nil {
				content += "\n" + m.styles.danger.Render("Error: "+m.mutationErr.Error())
			}
		}
	}
	box := m.styles.activePanel.Width(max(1, overlayWidth-4)).Height(max(1, overlayHeight-2)).Padding(0, 1).Render(m.styles.title.Render(title) + "\n\n" + content)
	return lipgloss.Place(width, height, lipgloss.Center, lipgloss.Center, box)
}

func (m *Model) renderStatus() string {
	left := m.status
	if left == "" {
		left = "Ready"
	}
	if m.loading {
		left = "Loading…"
	}
	if m.executing {
		left = "Executing Cypher…"
	}
	right := statusKeys(m.tab, m.snapshot.IsCurrent())
	return fitLine(m.styles.status.Render(joinSides(left, right, m.width)), m.width)
}

func helpText(tab Tab) string {
	common := "GLOBAL\n  1–4 switch view*  ctrl+←/→ switch view   ctrl+k commands\n  ? help             r refresh              q quit\n  mouse click select tabs/items · wheel scroll\n  *outside the Query editors\n\n"
	switch tab {
	case OutlineTab:
		return common + "OUTLINE\n  j/k or ↑/↓ select  h/l collapse/expand   / filter\n  c create  e edit  p reparent  d delete  i inspect"
	case GraphTab:
		return common + "GRAPH\n  j/k select  h/l horizontal pan  pgup/pgdown vertical pan\n  +/- zoom detail  0 reset  / filter  enter/i inspect\n  c create  e edit  p reparent  d delete"
	case QueryTab:
		return common + "QUERY\n  tab/shift+tab focus editor, params, results\n  ctrl+r read-only query  ctrl+x execute  ctrl+enter read\n  result focus: j/k/h/l scroll"
	case HistoryTab:
		return common + "HISTORY\n  j/k select revision  enter open snapshot  l return to Live"
	default:
		return common
	}
}

func mutationHint(kind mutationKind) string {
	switch kind {
	case mutationCreate:
		return "Edit JSON. parent_id may be empty for a root; null position means unordered."
	case mutationEdit:
		return "Edit labels, the complete property map, and Markdown body."
	case mutationReparent:
		return "Set parent_id to a node ID, or empty to make this node a root."
	default:
		return ""
	}
}

func statusKeys(tab Tab, live bool) string {
	switch tab {
	case OutlineTab:
		if live {
			return "j/k select · / search · c/e/p/d mutate"
		}
		return "j/k select · / search · read-only"
	case GraphTab:
		return "j/k select · h/l pan · +/- zoom"
	case QueryTab:
		if live {
			return "ctrl+r read · ctrl+x execute"
		}
		return "ctrl+r read · historical read-only"
	case HistoryTab:
		return "enter open · l Live"
	default:
		return ""
	}
}

func summaryText(summary app.Summary) string {
	changes := []string{}
	appendChange := func(count uint64, name string) {
		if count > 0 {
			changes = append(changes, fmt.Sprintf("%d %s", count, name))
		}
	}
	appendChange(summary.NodesCreated, "nodes created")
	appendChange(summary.NodesUpdated, "nodes updated")
	appendChange(summary.NodesDeleted, "nodes deleted")
	appendChange(summary.RelationshipsCreated, "edges created")
	appendChange(summary.RelationshipsUpdated, "edges updated")
	appendChange(summary.RelationshipsDeleted, "edges deleted")
	if len(changes) == 0 {
		return "read-only"
	}
	return strings.Join(changes, ", ")
}

func tableRow(values []string, widths []int) string {
	cells := make([]string, len(widths))
	for index, width := range widths {
		value := ""
		if index < len(values) {
			value = values[index]
		}
		value = ansi.Truncate(value, width, "…")
		cells[index] = value + strings.Repeat(" ", max(0, width-ansi.StringWidth(value)))
	}
	return strings.Join(cells, " │ ")
}

func formatValue(value any) string {
	if value == nil {
		return "null"
	}
	if text, ok := value.(string); ok {
		return strings.ReplaceAll(text, "\n", "↵")
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return fmt.Sprint(value)
	}
	return string(encoded)
}

func validity(from domain.Revision, to *domain.Revision) string {
	end := "∞"
	if to != nil {
		end = fmt.Sprintf("r%d", *to)
	}
	return fmt.Sprintf("[r%d, %s)", from, end)
}

func emptyAs(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

func joinSides(left, right string, width int) string {
	space := width - ansi.StringWidth(left) - ansi.StringWidth(right)
	if space < 1 {
		return fitLine(left+" "+right, width)
	}
	return left + strings.Repeat(" ", space) + right
}

func fitLine(line string, width int) string {
	if width <= 0 {
		return ""
	}
	line = ansi.Truncate(strings.ReplaceAll(line, "\n", " "), width, "…")
	return line + strings.Repeat(" ", max(0, width-ansi.StringWidth(line)))
}

func cutFrom(line string, offset, width int) string {
	if width <= 0 {
		return ""
	}
	lineWidth := ansi.StringWidth(line)
	if offset >= lineWidth {
		return ""
	}
	return ansi.Cut(line, max(0, offset), min(lineWidth, max(0, offset)+width))
}

func fitBlock(content string, width, height int) string {
	lines := strings.Split(content, "\n")
	if len(lines) > height {
		lines = lines[:height]
	}
	for len(lines) < height {
		lines = append(lines, "")
	}
	for index := range lines {
		lines[index] = fitLine(lines[index], width)
	}
	return strings.Join(lines, "\n")
}

func placeText(width, height int, content string) string {
	return lipgloss.Place(width, height, lipgloss.Center, lipgloss.Center, content)
}
