package tui

import (
	"fmt"
	"strings"

	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"charm.land/glamour/v2"
	"github.com/svlocks/sheets/internal/domain"
)

type inspectorRenderedMsg struct {
	serial uint64
	text   string
	err    error
}

type inspectorModel struct {
	viewport viewport.Model
	markdown string
	rendered string
	serial   uint64
	width    int
	height   int
	dark     bool
	noColor  bool
	loading  bool
	err      error
}

func newInspectorModel(dark, noColor bool) inspectorModel {
	view := viewport.New(viewport.WithWidth(50), viewport.WithHeight(20))
	view.SoftWrap = true
	view.FillHeight = true
	return inspectorModel{viewport: view, width: 50, height: 20, dark: dark, noColor: noColor}
}

func (m *inspectorModel) setAppearance(dark, noColor bool) tea.Cmd {
	if m.dark == dark && m.noColor == noColor {
		return nil
	}
	m.dark = dark
	m.noColor = noColor
	return m.rerender()
}

func (m *inspectorModel) setSize(width, height int) tea.Cmd {
	width = clampSize(width, 1)
	height = clampSize(height, 1)
	changed := width != m.width
	m.width, m.height = width, height
	m.viewport.SetWidth(width)
	m.viewport.SetHeight(height)
	if changed {
		return m.rerender()
	}
	return nil
}

func (m *inspectorModel) inspectNode(node domain.Node, graph graphState) tea.Cmd {
	m.markdown = nodeMarkdown(node, graph)
	m.viewport.GotoTop()
	return m.rerender()
}

func (m *inspectorModel) inspectEdge(edge domain.Edge, graph graphState) tea.Cmd {
	m.markdown = edgeMarkdown(edge, graph)
	m.viewport.GotoTop()
	return m.rerender()
}

func (m *inspectorModel) inspectRevision(info domain.RevisionInfo, live domain.Revision) tea.Cmd {
	m.markdown = revisionMarkdown(info, live)
	m.viewport.GotoTop()
	return m.rerender()
}

func (m *inspectorModel) clear(message string) tea.Cmd {
	m.markdown = "# Details\n\n" + message
	m.viewport.GotoTop()
	return m.rerender()
}

func (m *inspectorModel) rerender() tea.Cmd {
	if m.markdown == "" {
		return nil
	}
	m.serial++
	serial := m.serial
	markdown := m.markdown
	width := clampSize(m.width, 20)
	dark, noColor := m.dark, m.noColor
	m.loading = true
	m.err = nil
	return func() tea.Msg {
		style := "dark"
		if noColor {
			style = "notty"
		} else if !dark {
			style = "light"
		}
		renderer, err := glamour.NewTermRenderer(
			glamour.WithStandardStyle(style),
			glamour.WithWordWrap(max(20, width-1)),
			glamour.WithPreservedNewLines(),
		)
		if err != nil {
			return inspectorRenderedMsg{serial: serial, err: err}
		}
		text, err := renderer.Render(markdown)
		return inspectorRenderedMsg{serial: serial, text: strings.TrimSpace(text), err: err}
	}
}

func (m *inspectorModel) apply(message inspectorRenderedMsg) {
	if message.serial != m.serial {
		return
	}
	m.loading = false
	m.err = message.err
	if message.err != nil {
		m.rendered = m.markdown
	} else {
		m.rendered = message.text
	}
	m.viewport.SetContent(m.rendered)
}

func (m *inspectorModel) update(msg tea.Msg) tea.Cmd {
	updated, cmd := m.viewport.Update(msg)
	m.viewport = updated
	return cmd
}

func (m inspectorModel) view() string {
	if m.markdown == "" {
		return "Select a node, relationship, or revision to inspect it."
	}
	if m.rendered == "" && m.loading {
		return "Rendering details…"
	}
	return m.viewport.View()
}

func nodeMarkdown(node domain.Node, graph graphState) string {
	var result strings.Builder
	fmt.Fprintf(&result, "# %s\n\n", escapeMarkdown(nodeTitle(node)))
	fmt.Fprintf(&result, "`%s`\n\n", node.ID)
	if len(node.Labels) == 0 {
		result.WriteString("**Labels:** _(none)_\n\n")
	} else {
		displayed := min(len(node.Labels), maxDisplayLabels)
		labels := make([]string, displayed)
		for index, label := range node.Labels[:displayed] {
			labels[index] = truncateRunes(label, maxDisplayLabelRunes)
		}
		text := strings.Join(labels, ", ")
		if displayed < len(node.Labels) {
			text += fmt.Sprintf(" (+%d more)", len(node.Labels)-displayed)
		}
		fmt.Fprintf(&result, "**Labels:** %s\n\n", escapeMarkdown(text))
	}
	fmt.Fprintf(&result, "**Validity:** revision %d", node.ValidFrom)
	if node.ValidTo == nil {
		result.WriteString(" → current\n\n")
	} else {
		fmt.Fprintf(&result, " → %d\n\n", *node.ValidTo)
	}

	result.WriteString("## Properties\n\n```json\n")
	result.WriteString(terminalBlock(prettyJSONPreview(node.Properties)))
	result.WriteString("\n```\n\n")

	result.WriteString("## Hierarchy\n\n")
	if parent, ok := graph.parent[node.ID]; ok {
		fmt.Fprintf(&result, "**Parent:** %s (`%s`)", escapeMarkdown(nodeTitle(graph.nodeByID[parent.From])), parent.From)
		if parent.Position == nil {
			result.WriteString(" · unordered\n\n")
		} else {
			fmt.Fprintf(&result, " · position %d\n\n", *parent.Position)
		}
	} else {
		result.WriteString("**Parent:** root\n\n")
	}
	children := graph.children[node.ID]
	if len(children) == 0 {
		result.WriteString("**Children:** none\n\n")
	} else {
		result.WriteString("**Children:**\n\n")
		for _, link := range children[:min(len(children), maxInspectorListItems)] {
			position := "unordered"
			if link.edge.Position != nil {
				position = fmt.Sprintf("position %d", *link.edge.Position)
			}
			fmt.Fprintf(&result, "- %s (`%s`) · %s\n", escapeMarkdown(nodeTitle(graph.nodeByID[link.child])), link.child, position)
		}
		if len(children) > maxInspectorListItems {
			fmt.Fprintf(&result, "- … %d additional children omitted from this preview\n", len(children)-maxInspectorListItems)
		}
		result.WriteByte('\n')
	}

	result.WriteString("## Relationships\n\n")
	count := 0
	omitted := 0
	for _, edge := range graph.incoming[node.ID] {
		if edge.Type == "CHILD" {
			continue
		}
		if count >= maxInspectorListItems {
			omitted++
			continue
		}
		count++
		fmt.Fprintf(&result, "- ← **%s** from %s (`%s`)\n", escapeMarkdown(edge.Type), escapeMarkdown(nodeTitle(graph.nodeByID[edge.From])), edge.From)
	}
	for _, edge := range graph.outgoing[node.ID] {
		if edge.Type == "CHILD" {
			continue
		}
		if count >= maxInspectorListItems {
			omitted++
			continue
		}
		count++
		fmt.Fprintf(&result, "- **%s** → %s (`%s`)\n", escapeMarkdown(edge.Type), escapeMarkdown(nodeTitle(graph.nodeByID[edge.To])), edge.To)
	}
	if count == 0 {
		result.WriteString("No non-hierarchy relationships.\n")
	} else if omitted > 0 {
		fmt.Fprintf(&result, "- … %d additional relationships omitted from this preview\n", omitted)
	}

	result.WriteString("\n## Body\n\n")
	if strings.TrimSpace(node.Body) == "" {
		result.WriteString("_(empty)_\n")
	} else {
		body := truncateRunes(node.Body, maxInspectorBodyRunes)
		result.WriteString(truncateRunes(terminalBlock(body), maxInspectorBodyRunes))
		result.WriteByte('\n')
	}
	return result.String()
}

func edgeMarkdown(edge domain.Edge, graph graphState) string {
	from, fromOK := graph.nodeByID[edge.From]
	to, toOK := graph.nodeByID[edge.To]
	fromTitle, toTitle := "Missing node", "Missing node"
	if fromOK {
		fromTitle = nodeTitle(from)
	}
	if toOK {
		toTitle = nodeTitle(to)
	}
	var result strings.Builder
	fmt.Fprintf(&result, "# %s\n\n", escapeMarkdown(edge.Type))
	fmt.Fprintf(&result, "`%s`\n\n", edge.ID)
	fmt.Fprintf(&result, "**From:** %s (`%s`)\n\n", escapeMarkdown(fromTitle), edge.From)
	fmt.Fprintf(&result, "**To:** %s (`%s`)\n\n", escapeMarkdown(toTitle), edge.To)
	if edge.Type == "CHILD" {
		if edge.Position == nil {
			result.WriteString("**Sibling order:** unordered\n\n")
		} else {
			fmt.Fprintf(&result, "**Sibling position:** %d\n\n", *edge.Position)
		}
	}
	fmt.Fprintf(&result, "**Validity:** revision %d", edge.ValidFrom)
	if edge.ValidTo == nil {
		result.WriteString(" → current\n\n")
	} else {
		fmt.Fprintf(&result, " → %d\n\n", *edge.ValidTo)
	}
	result.WriteString("## Properties\n\n```json\n")
	result.WriteString(terminalBlock(prettyJSONPreview(edge.Properties)))
	result.WriteString("\n```\n")
	return result.String()
}

func revisionMarkdown(info domain.RevisionInfo, live domain.Revision) string {
	when := "Before the first commit"
	if !info.Time.IsZero() {
		when = info.Time.Local().Format("Monday, 2 January 2006 at 15:04:05 MST")
	}
	actor := truncateRunes(info.Actor, maxTimelineTextRunes)
	if strings.TrimSpace(actor) == "" {
		actor = "_(not recorded)_"
	} else {
		actor = escapeMarkdown(actor)
	}
	message := truncateRunes(info.Message, maxTimelineTextRunes)
	if strings.TrimSpace(message) == "" {
		message = "_(not recorded)_"
	} else {
		message = escapeMarkdown(message)
	}
	state := "Historical"
	if info.Revision == live {
		state = "Current live revision"
	}
	return fmt.Sprintf("# Revision %d\n\n**State:** %s\n\n**Committed:** %s\n\n**Actor:** %s\n\n**Message:** %s\n\nPress **Enter** to open this exact graph state. Historical snapshots are read-only.", info.Revision, state, when, actor, message)
}

func escapeMarkdown(value string) string {
	value = terminalLine(value)
	replacer := strings.NewReplacer("\\", "\\\\", "*", "\\*", "_", "\\_", "`", "\\`", "[", "\\[", "]", "\\]")
	return replacer.Replace(value)
}
