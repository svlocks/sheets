package tui

import (
	"fmt"
	"path/filepath"
	"strconv"
	"strings"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

const (
	wideTerminalWidth = 108
	minimumWidth      = 44
	minimumHeight     = 12
)

type layoutGeometry struct {
	headerHeight  int
	contentTop    int
	contentHeight int
	footerHeight  int
	wide          bool
	primaryWidth  int
	detailWidth   int
}

func (m *Model) geometry() layoutGeometry {
	header := 1
	if m.historical() {
		header++
	}
	footer := 2
	content := max(1, m.height-header-footer)
	geometry := layoutGeometry{
		headerHeight: header, contentTop: header, contentHeight: content,
		footerHeight: footer, wide: m.width >= wideTerminalWidth && content >= 12,
		primaryWidth: m.width, detailWidth: 0,
	}
	if geometry.wide && m.workspace != QueryWorkspace {
		geometry.primaryWidth = min(58, max(38, m.width*9/20))
		geometry.detailWidth = max(1, m.width-geometry.primaryWidth)
	}
	return geometry
}

func (m *Model) layoutComponents() tea.Cmd {
	geometry := m.geometry()
	bodyHeight := max(1, geometry.contentHeight-3)
	primaryWidth := max(1, geometry.primaryWidth-2)
	detailWidth := max(1, geometry.detailWidth-2)
	if !geometry.wide {
		detailWidth = max(1, m.width-2)
	}
	m.work.setSize(primaryWidth, bodyHeight)
	m.relationships.setSize(primaryWidth, bodyHeight)
	m.timeline.setSize(primaryWidth, bodyHeight)
	m.query.setSize(max(1, m.width-2), bodyHeight)
	commands := []tea.Cmd{m.inspector.setSize(detailWidth, bodyHeight)}
	m.help.SetWidth(max(1, m.width-2))
	modalWidth, modalHeight := m.modalContentDimensions()
	m.helpViewport.SetWidth(modalWidth)
	m.helpViewport.SetHeight(modalHeight)
	if m.overlay.kind == overlayFinder || m.overlay.kind == overlayCommands {
		m.overlay.picker.setSize(modalWidth, modalHeight)
	}
	if m.form != nil {
		m.form.setSize(modalWidth, modalHeight)
	}
	if m.overlay.kind == overlayHelp {
		m.refreshHelpContent()
	}
	return tea.Batch(commands...)
}

func (m *Model) modalDimensions() (int, int) {
	geometry := m.geometry()
	width := min(96, max(30, m.width-8))
	if m.width < 64 {
		width = max(20, m.width-4)
	}
	height := min(30, max(8, geometry.contentHeight-4))
	return width, height
}

// modalContentDimensions returns the widget area inside the panel border and
// below its title. Modal children are real Bubbles/Huh components and must be
// sized to this area rather than relying on final string clipping.
func (m *Model) modalContentDimensions() (int, int) {
	width, height := m.modalDimensions()
	return max(1, width-2), max(1, height-3)
}

func (m *Model) render() string {
	if m.width <= 0 || m.height <= 0 {
		return ""
	}
	header := m.renderHeader()
	content := m.renderContent()
	status := m.renderStatus()
	helpLine := m.renderShortHelp()
	result := lipgloss.JoinVertical(lipgloss.Left, header, content, status, helpLine)
	result = fitBlock(result, m.width, m.height)
	if m.noColor {
		result = stripANSIColors(result)
	}
	return result
}

func (m *Model) renderHeader() string {
	prefix := m.headerPrefix()
	var result strings.Builder
	result.WriteString(prefix)
	for workspace := WorkWorkspace; workspace <= TimelineWorkspace; workspace++ {
		result.WriteString(m.renderTab(workspace))
	}
	header := fitLine(result.String(), m.width)
	if !m.historical() {
		return header
	}
	state := fmt.Sprintf("revision %d", m.loadedRevision)
	if m.loadingGraph && !m.loadTarget.IsCurrent() && m.loadTarget.Revision != nil {
		state = fmt.Sprintf("loading revision %d", *m.loadTarget.Revision)
	} else if m.loadingGraph && m.loadTarget.IsCurrent() && !m.snapshot.IsCurrent() {
		state += " · returning to Live"
	}
	banner := fmt.Sprintf("READ-ONLY HISTORY · %s · live revision %d · press F6 to return to Live", state, m.liveRevision)
	return header + "\n" + fitLine(m.styles.readOnly.Render(banner), m.width)
}

func (m *Model) renderTab(workspace Workspace) string {
	name := workspace.String()
	if m.width < 76 {
		name = [...]string{"Work", "Rel", "Query", "Time"}[workspace]
	}
	if m.noColor {
		if workspace == m.workspace {
			return fmt.Sprintf("[F%d %s]", int(workspace)+1, name)
		}
		return fmt.Sprintf(" F%d %s ", int(workspace)+1, name)
	}
	label := fmt.Sprintf(" F%d %s ", int(workspace)+1, name)
	if workspace == m.workspace {
		return m.styles.activeTab.Render(label)
	}
	return m.styles.tab.Render(label)
}

func (m *Model) headerPrefix() string {
	if m.width < 56 {
		return ""
	}
	if m.width < 90 {
		if m.noColor {
			return " sheets  "
		}
		return " " + m.styles.appName.Render("sheets") + "  "
	}
	root := "project"
	if m.backend != nil {
		if value := filepath.Base(m.backend.ProjectRoot()); value != "." && value != "" {
			root = value
		}
	}
	plain := " sheets · " + ansi.Truncate(root, 20, "…") + "  "
	if m.noColor {
		return plain
	}
	return " " + m.styles.appName.Render("sheets") + m.styles.project.Render(" · "+ansi.Truncate(root, 20, "…")+"  ")
}

func (m *Model) renderContent() string {
	geometry := m.geometry()
	if m.width < minimumWidth || m.height < minimumHeight {
		message := fmt.Sprintf("Terminal is %d×%d. Resize to at least %d×%d for the interactive workspace. Ctrl+C still quits.", m.width, m.height, minimumWidth, minimumHeight)
		return fitBlock(wrapText(message, m.width), m.width, geometry.contentHeight)
	}
	if m.overlay.kind != overlayNone {
		return m.renderOverlay(geometry)
	}
	if m.loadingGraph && len(m.graph.nodes) == 0 && !m.work.initialized {
		return m.renderPane("Loading graph", m.spinner.View()+" Reading an exact graph snapshot…", m.width, geometry.contentHeight, true, false)
	}
	if m.graphErr != nil && !m.work.initialized && (m.workspace == WorkWorkspace || m.workspace == RelationshipsWorkspace) {
		body := "The graph could not be loaded.\n\n" + errorText(m.graphErr) + "\n\nPress F5 to retry."
		return m.renderPane("Graph unavailable", wrapText(body, max(1, m.width-2)), m.width, geometry.contentHeight, true, false)
	}
	if m.workspace == TimelineWorkspace && m.historyErr != nil && !m.historyReady {
		body := "The revision timeline could not be loaded.\n\n" + errorText(m.historyErr) + "\n\nPress F5 to retry."
		return m.renderPane("Timeline unavailable", wrapText(body, max(1, m.width-2)), m.width, geometry.contentHeight, true, false)
	}
	switch m.workspace {
	case WorkWorkspace:
		return m.renderPrimaryAndInspector("Hierarchy", m.work.view(), geometry)
	case RelationshipsWorkspace:
		return m.renderPrimaryAndInspector("Relationships · / filters", m.relationships.view(), geometry)
	case QueryWorkspace:
		return m.renderPane("Query · Tab moves focus · Ctrl+R reads · Ctrl+X executes", m.query.view(), m.width, geometry.contentHeight, true, false)
	case TimelineWorkspace:
		return m.renderPrimaryAndInspector("Timeline · Enter opens revision", m.timeline.view(), geometry)
	default:
		return fitBlock("Unknown workspace", m.width, geometry.contentHeight)
	}
}

func (m *Model) renderPrimaryAndInspector(title, primary string, geometry layoutGeometry) string {
	if geometry.wide {
		left := m.renderPane(title, primary, geometry.primaryWidth, geometry.contentHeight, m.focus == focusPrimary, false)
		right := m.renderPane("Details · Markdown · Tab focuses", m.inspector.view(), geometry.detailWidth, geometry.contentHeight, m.focus == focusInspector, false)
		return lipgloss.JoinHorizontal(lipgloss.Top, left, right)
	}
	if m.focus == focusInspector {
		return m.renderPane("Details · Tab returns", m.inspector.view(), m.width, geometry.contentHeight, true, false)
	}
	return m.renderPane(title+" · Tab opens details", primary, m.width, geometry.contentHeight, true, false)
}

func (m *Model) renderOverlay(geometry layoutGeometry) string {
	width, height := m.modalDimensions()
	var title, body string
	switch m.overlay.kind {
	case overlayFinder, overlayCommands:
		title = m.overlay.picker.title() + " · Esc closes"
		body = m.overlay.picker.view()
	case overlayForm:
		if m.form == nil {
			title, body = "Form", "The form is no longer available. Press Esc."
		} else {
			title = m.form.title + " · Esc cancels"
			body = m.form.view()
		}
	case overlayHelp:
		title = "Help · Esc or F10 closes"
		body = m.helpViewport.View()
	case overlayOperationError:
		title = "Operation failed"
		body = "The graph was not changed.\n\n" + errorText(m.operationErr) + "\n\nPress r to retry the exact request, or Esc to close."
		body = wrapText(body, max(1, width-2))
	default:
		title, body = "Overlay", "Press Esc to close."
	}
	panel := m.renderPane(title, body, width, height, true, true)
	return lipgloss.Place(m.width, geometry.contentHeight, lipgloss.Center, lipgloss.Center, panel)
}

func (m *Model) renderPane(title, body string, width, height int, focused, modal bool) string {
	style := m.styles.border
	if focused {
		style = m.styles.focusedBorder
	}
	if modal {
		style = m.styles.modalBorder
	}
	frameWidth, frameHeight := style.GetFrameSize()
	innerWidth := max(1, width-frameWidth)
	innerHeight := max(1, height-frameHeight)
	titleStyle := m.styles.paneTitle
	prefix := "  "
	if focused {
		titleStyle = m.styles.focusedTitle
		prefix = "> "
	}
	titleLine := fitLine(titleStyle.Render(prefix+title), innerWidth)
	body = fitBlock(body, innerWidth, max(0, innerHeight-1))
	content := titleLine
	if innerHeight > 1 {
		content += "\n" + body
	}
	// Lip Gloss v2 treats Width and Height as the complete block dimensions,
	// including borders. The content itself is clipped to the frame-adjusted
	// inner dimensions above.
	return style.Width(width).Height(height).Render(content)
}

func (m *Model) renderStatus() string {
	parts := make([]string, 0, 4)
	if m.notice.text != "" {
		parts = append(parts, m.notice.text)
	}
	if m.busy() {
		activity := "working"
		switch {
		case m.executing:
			activity = "executing Cypher"
		case m.loadingGraph:
			activity = "loading graph"
		case m.loadingHistory:
			activity = "loading timeline"
		case m.inspector.loading:
			activity = "rendering Markdown"
		}
		parts = append(parts, m.spinner.View()+" "+activity)
	}
	mode := fmt.Sprintf("revision %d", m.loadedRevision)
	if m.historical() {
		mode += " · READ-ONLY"
	} else {
		mode += " · Live"
	}
	parts = append(parts, mode)
	parts = append(parts, fmt.Sprintf("%d nodes · %d relationships", len(m.graph.nodes), len(m.graph.edges)))
	if m.graphErr != nil {
		parts = append(parts, "graph refresh failed")
	}
	if m.historyErr != nil {
		parts = append(parts, "timeline refresh failed")
	}
	line := strings.Join(parts, "  |  ")
	style := m.styles.status
	switch m.notice.level {
	case noticeError:
		style = m.styles.error
	case noticeSuccess:
		style = m.styles.success
	}
	return fitLine(style.Render(line), m.width)
}

func (m *Model) renderShortHelp() string {
	m.help.SetWidth(m.width)
	return fitLine(m.help.ShortHelpView(m.contextShortHelp()), m.width)
}

func (m *Model) contextShortHelp() []key.Binding {
	if m.overlay.kind != overlayNone {
		switch m.overlay.kind {
		case overlayFinder, overlayCommands:
			return []key.Binding{m.keys.Open, m.keys.Back}
		case overlayForm:
			return []key.Binding{
				binding("enter / tab", "next or submit", "enter", "tab"),
				binding("shift+tab", "previous field", "shift+tab"),
				binding("esc", "cancel form", "esc"),
			}
		case overlayHelp:
			return []key.Binding{m.keys.Back}
		case overlayOperationError:
			return []key.Binding{binding("r", "retry", "r"), m.keys.Back}
		}
	}
	global := []key.Binding{m.keys.Palette, m.keys.Help}
	if m.historical() {
		global = append([]key.Binding{m.keys.ReturnLive}, global...)
	}
	switch m.workspace {
	case WorkWorkspace:
		if m.focus == focusInspector {
			return append(global, m.keys.TogglePane, m.keys.Back)
		}
		local := []key.Binding{m.work.tree.KeyMap.Up, m.work.tree.KeyMap.Down, m.keys.Open, enabledBinding(m.keys.NewNode, !m.historical()), m.keys.Find}
		return append(global, local...)
	case RelationshipsWorkspace:
		local := []key.Binding{m.relationships.list.KeyMap.CursorUp, m.relationships.list.KeyMap.CursorDown, m.relationships.list.KeyMap.Filter, enabledBinding(m.keys.Edit, !m.historical()), enabledBinding(m.keys.Connect, !m.historical())}
		return append(global, local...)
	case QueryWorkspace:
		return append(global, m.keys.TogglePane, m.keys.RunQuery, enabledBinding(m.keys.ExecQuery, !m.historical()), m.keys.PreviousTab, m.keys.NextTab)
	case TimelineWorkspace:
		return append(global, m.timeline.list.KeyMap.CursorUp, m.timeline.list.KeyMap.CursorDown, m.keys.Open, m.timeline.list.KeyMap.Filter)
	default:
		return global
	}
}

func (m *Model) contextFullHelp() [][]key.Binding {
	global := []key.Binding{m.keys.Palette, m.keys.Help, m.keys.Refresh, m.keys.PreviousTab, m.keys.NextTab, m.keys.Quit}
	workspaces := []key.Binding{m.keys.Work, m.keys.Relationships, m.keys.Query, m.keys.Timeline}
	if m.historical() {
		global = append([]key.Binding{m.keys.ReturnLive}, global...)
	}
	effectiveOverlay := m.overlay.kind
	if effectiveOverlay == overlayHelp {
		effectiveOverlay = m.overlayReturn
	}
	if effectiveOverlay != overlayNone {
		global = []key.Binding{m.keys.Help, m.keys.Refresh, m.keys.PreviousTab, m.keys.NextTab, m.keys.Quit}
		if m.historical() {
			global = append([]key.Binding{m.keys.ReturnLive}, global...)
		}
		var local []key.Binding
		switch effectiveOverlay {
		case overlayFinder, overlayCommands:
			local = []key.Binding{m.keys.Open, m.keys.Back}
		case overlayForm:
			local = []key.Binding{
				binding("enter / tab", "next or submit", "enter", "tab"),
				binding("shift+tab", "previous field", "shift+tab"),
				binding("alt+enter", "newline in text", "alt+enter", "ctrl+j"),
				binding("esc", "cancel form", "esc"),
			}
		case overlayOperationError:
			local = []key.Binding{binding("r", "retry exact request", "r"), m.keys.Back}
		}
		return [][]key.Binding{local, workspaces, global}
	}
	var local []key.Binding
	switch m.workspace {
	case WorkWorkspace:
		local = []key.Binding{m.work.tree.KeyMap.Up, m.work.tree.KeyMap.Down, m.work.tree.KeyMap.Open, m.work.tree.KeyMap.Close, m.work.tree.KeyMap.Toggle, m.keys.TogglePane, m.keys.Find, enabledBinding(m.keys.NewNode, !m.historical()), enabledBinding(m.keys.Edit, !m.historical()), enabledBinding(m.keys.Move, !m.historical()), enabledBinding(m.keys.Connect, !m.historical()), enabledBinding(m.keys.Delete, !m.historical())}
	case RelationshipsWorkspace:
		local = []key.Binding{m.relationships.list.KeyMap.CursorUp, m.relationships.list.KeyMap.CursorDown, m.relationships.list.KeyMap.Filter, m.keys.TogglePane, enabledBinding(m.keys.Edit, !m.historical()), enabledBinding(m.keys.Connect, !m.historical()), enabledBinding(m.keys.Delete, !m.historical())}
	case QueryWorkspace:
		local = []key.Binding{m.keys.TogglePane, m.keys.RunQuery, enabledBinding(m.keys.ExecQuery, !m.historical()), m.keys.PreviousSet, m.keys.NextSet}
	case TimelineWorkspace:
		local = []key.Binding{m.timeline.list.KeyMap.CursorUp, m.timeline.list.KeyMap.CursorDown, m.timeline.list.KeyMap.Filter, m.keys.Open, m.keys.TogglePane}
	}
	return [][]key.Binding{local, workspaces, global}
}

func (m *Model) refreshHelpContent() {
	workflow := "\n\nWorkflow notes\n" +
		"• Work is the structural CHILD hierarchy. Ordered children show a number; ◇ means unordered.\n" +
		"• Relationships contains every edge, including CHILD, and / filters the real list.\n" +
		"• Query read mode rejects writes. Write-capable execution always asks for confirmation.\n" +
		"• Opening a Timeline revision makes every workspace read-only until Return to Live.\n" +
		"• F1–F4 switch workspaces from any focus; Ctrl+K exposes every primary action when no modal is open."
	m.helpViewport.SetContent(m.help.FullHelpView(m.contextFullHelp()) + workflow)
}

func (m *Model) workspaceAt(x int) (Workspace, bool) {
	position := ansi.StringWidth(m.headerPrefix())
	for workspace := WorkWorkspace; workspace <= TimelineWorkspace; workspace++ {
		width := ansi.StringWidth(m.renderTab(workspace))
		if x >= position && x < position+width {
			return workspace, true
		}
		position += width
	}
	return 0, false
}

func (m *Model) workRowAt(x, y int) (int, bool) {
	geometry := m.geometry()
	if y < geometry.contentTop+2 || y >= geometry.contentTop+geometry.contentHeight-1 {
		return 0, false
	}
	if x < 1 || x >= geometry.primaryWidth-1 {
		return 0, false
	}
	if !geometry.wide && m.focus != focusPrimary {
		return 0, false
	}
	return y - geometry.contentTop - 2, true
}

func fitBlock(value string, width, height int) string {
	if width <= 0 || height <= 0 {
		return ""
	}
	lines := strings.Split(strings.ReplaceAll(value, "\r\n", "\n"), "\n")
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

func wrapText(value string, width int) string {
	if width <= 0 {
		return ""
	}
	value = ansi.Wordwrap(value, width, "")
	return ansi.Hardwrap(value, width, true)
}

func errorText(err error) string {
	if err == nil {
		return "Unknown error"
	}
	return err.Error()
}

// stripANSIColors removes SGR foreground/background parameters while leaving
// non-color terminal affordances such as the input cursor's reverse-video cell
// intact. Some composed third-party fields retain their own default colors even
// after a colorless theme is supplied, so the accessibility boundary is the
// final deterministic render.
func stripANSIColors(value string) string {
	var result strings.Builder
	for index := 0; index < len(value); {
		start := strings.Index(value[index:], "\x1b[")
		if start < 0 {
			result.WriteString(value[index:])
			break
		}
		start += index
		result.WriteString(value[index:start])
		end := strings.IndexByte(value[start+2:], 'm')
		if end < 0 {
			result.WriteString(value[start:])
			break
		}
		end += start + 2
		parameters := strings.Split(value[start+2:end], ";")
		kept := make([]string, 0, len(parameters))
		for parameterIndex := 0; parameterIndex < len(parameters); parameterIndex++ {
			parameter, err := strconv.Atoi(parameters[parameterIndex])
			if err != nil {
				kept = append(kept, parameters[parameterIndex])
				continue
			}
			if parameter == 38 || parameter == 48 {
				if parameterIndex+1 < len(parameters) {
					mode, _ := strconv.Atoi(parameters[parameterIndex+1])
					switch mode {
					case 5:
						parameterIndex += min(2, len(parameters)-parameterIndex-1)
					case 2:
						parameterIndex += min(4, len(parameters)-parameterIndex-1)
					}
				}
				continue
			}
			if (parameter >= 30 && parameter <= 37) || (parameter >= 40 && parameter <= 47) || parameter == 39 || parameter == 49 || (parameter >= 90 && parameter <= 107) {
				continue
			}
			kept = append(kept, parameters[parameterIndex])
		}
		if len(kept) > 0 {
			result.WriteString("\x1b[")
			result.WriteString(strings.Join(kept, ";"))
			result.WriteByte('m')
		}
		index = end + 1
	}
	return result.String()
}
