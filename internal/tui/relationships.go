package tui

import (
	"fmt"
	"strings"

	"charm.land/bubbles/v2/list"
	tea "charm.land/bubbletea/v2"
	"github.com/svlocks/sheets/internal/domain"
)

type relationshipItem struct {
	edge        domain.Edge
	title       string
	description string
	filter      string
}

func (i relationshipItem) Title() string       { return i.title }
func (i relationshipItem) Description() string { return i.description }
func (i relationshipItem) FilterValue() string { return i.filter }

type relationshipsModel struct {
	list      list.Model
	graph     graphState
	filterSeq uint64
	width     int
	height    int
	dark      bool
	styles    styleSet
}

func newRelationshipsModel(styles styleSet, dark bool) relationshipsModel {
	delegate := list.NewDefaultDelegate()
	delegate.SetSpacing(0)
	listStyles, itemStyles := styles.listStyles(dark)
	delegate.Styles = itemStyles
	model := list.New(nil, delegate, 60, 20)
	model.Title = "Relationships"
	model.Styles = listStyles
	model.DisableQuitKeybindings()
	model.SetShowTitle(false)
	model.SetShowHelp(false)
	model.SetShowPagination(true)
	model.SetStatusBarItemName("relationship", "relationships")
	return relationshipsModel{list: model, width: 60, height: 20, dark: dark, styles: styles}
}

func (m *relationshipsModel) setStyle(styles styleSet, dark bool) {
	m.styles = styles
	m.dark = dark
	listStyles, itemStyles := styles.listStyles(dark)
	m.list.Styles = listStyles
	delegate := list.NewDefaultDelegate()
	delegate.SetSpacing(0)
	delegate.Styles = itemStyles
	m.list.SetDelegate(delegate)
}

func (m *relationshipsModel) setSize(width, height int) {
	m.width = clampSize(width, 1)
	m.height = clampSize(height, 1)
	m.list.SetSize(m.width, m.height)
}

func (m *relationshipsModel) setGraph(graph graphState) tea.Cmd {
	selected := m.selectedID()
	items := make([]list.Item, 0, len(graph.edges))
	for _, edge := range graph.edges {
		from := graph.nodeByID[edge.From]
		to := graph.nodeByID[edge.To]
		kind := "connection"
		position := ""
		if edge.Type == "CHILD" {
			kind = "hierarchy"
			if edge.Position == nil {
				position = " · unordered"
			} else {
				position = fmt.Sprintf(" · position %d", *edge.Position)
			}
		}
		properties := ""
		if len(edge.Properties) > 0 {
			properties = fmt.Sprintf(" · %d properties", len(edge.Properties))
		}
		typeName := terminalLine(truncateRunes(edge.Type, maxFormShortRunes))
		items = append(items, relationshipItem{
			edge:        edge,
			title:       fmt.Sprintf("%s  —%s→  %s", nodeTitle(from), typeName, nodeTitle(to)),
			description: fmt.Sprintf("%s · %s → %s%s%s", kind, shortID(edge.From), shortID(edge.To), position, properties),
			filter:      edgeSearchValue(edge, graph),
		})
	}
	m.graph = graph
	m.filterSeq++
	cmd := m.list.SetItems(items)
	if selected != "" {
		for index, item := range m.list.Items() {
			if relationship, ok := item.(relationshipItem); ok && relationship.edge.ID == selected {
				m.list.Select(index)
				break
			}
		}
	}
	return scopeListCmd(listTargetRelationships, 0, m.filterSeq, m.list.FilterValue(), cmd)
}

func (m *relationshipsModel) update(msg tea.Msg) tea.Cmd {
	beforeFilter := m.list.FilterValue()
	updated, cmd := m.list.Update(msg)
	m.list = updated
	if beforeFilter != m.list.FilterValue() {
		m.filterSeq++
	}
	return scopeListCmd(listTargetRelationships, 0, m.filterSeq, m.list.FilterValue(), cmd)
}

func (m *relationshipsModel) selectedID() domain.EntityID {
	item, ok := m.list.SelectedItem().(relationshipItem)
	if !ok {
		return ""
	}
	return item.edge.ID
}

func (m *relationshipsModel) selectedEdge() (domain.Edge, bool) {
	item, ok := m.list.SelectedItem().(relationshipItem)
	if !ok {
		return domain.Edge{}, false
	}
	return item.edge, true
}

func (m *relationshipsModel) filtering() bool {
	return m.list.SettingFilter()
}

func (m *relationshipsModel) wheel(delta int) {
	if delta < 0 {
		for range -delta {
			m.list.CursorUp()
		}
		return
	}
	for range delta {
		m.list.CursorDown()
	}
}

func (m relationshipsModel) view() string {
	if len(m.graph.edges) == 0 && !m.list.SettingFilter() {
		guidance := "Select a node in Work and press c to connect it to another node."
		if len(m.graph.nodes) == 0 {
			guidance = "Create a node in Work first; one node is enough for a self-relationship."
		}
		return strings.Join([]string{
			"No relationships yet.",
			"",
			guidance,
		}, "\n")
	}
	return m.list.View()
}
