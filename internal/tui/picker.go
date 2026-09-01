package tui

import (
	"charm.land/bubbles/v2/list"
	tea "charm.land/bubbletea/v2"
	"github.com/svlocks/sheets/internal/domain"
)

type overlayKind uint8

const (
	overlayNone overlayKind = iota
	overlayFinder
	overlayCommands
	overlayForm
	overlayHelp
	overlayOperationError
)

type pickerKind uint8

const (
	pickerNodes pickerKind = iota + 1
	pickerCommands
)

type nodePickerItem struct {
	id          domain.EntityID
	title       string
	description string
	filter      string
}

func (i nodePickerItem) Title() string       { return i.title }
func (i nodePickerItem) Description() string { return i.description }
func (i nodePickerItem) FilterValue() string { return i.filter }

type commandID uint8

const (
	commandWork commandID = iota + 1
	commandRelationships
	commandQuery
	commandTimeline
	commandFind
	commandCreate
	commandEditNode
	commandMoveNode
	commandConnect
	commandDeleteNode
	commandEditRelationship
	commandDeleteRelationship
	commandDetails
	commandReturnLive
	commandRefresh
	commandHelp
)

type commandItem struct {
	id          commandID
	title       string
	description string
	keywords    string
}

func (i commandItem) Title() string       { return i.title }
func (i commandItem) Description() string { return i.description }
func (i commandItem) FilterValue() string { return i.title + " " + i.description + " " + i.keywords }

type pickerModel struct {
	kind          pickerKind
	serial        uint64
	filteredValue string
	openWhenReady bool
	list          list.Model
}

func newNodePicker(serial uint64, graph graphState, styles styleSet, dark bool, width, height int) pickerModel {
	items := make([]list.Item, 0, len(graph.nodes))
	for _, node := range graph.nodes {
		items = append(items, nodePickerItem{
			id: node.ID, title: nodeTitle(node), description: nodeSubtitle(node), filter: nodeSearchValue(node),
		})
	}
	return newPicker(serial, pickerNodes, "Find work · type to filter", items, styles, dark, width, height)
}

func newCommandPicker(serial uint64, styles styleSet, dark bool, width, height int, historical bool) pickerModel {
	items := []list.Item{
		commandItem{id: commandWork, title: "Open Work", description: "Browse the hierarchy and node details", keywords: "tree outline tasks f1"},
		commandItem{id: commandRelationships, title: "Open Relationships", description: "Browse every graph edge", keywords: "connections edges f2"},
		commandItem{id: commandQuery, title: "Open Query", description: "Run Cypher and inspect result rows", keywords: "console cypher f3"},
		commandItem{id: commandTimeline, title: "Open Timeline", description: "Browse and open historical revisions", keywords: "history revisions f4"},
		commandItem{id: commandFind, title: "Find work item", description: "Fuzzy-search titles, labels, IDs, properties, and bodies", keywords: "search jump slash"},
		commandItem{id: commandCreate, title: "Create node", description: "Add a root or child work item", keywords: "new add task n"},
		commandItem{id: commandEditNode, title: "Edit selected node", description: "Change labels, properties, and Markdown body", keywords: "update e"},
		commandItem{id: commandMoveNode, title: "Move / order selected node", description: "Choose a parent and ordered or unordered position", keywords: "reparent hierarchy m"},
		commandItem{id: commandConnect, title: "Connect nodes", description: "Create an arbitrary non-CHILD relationship", keywords: "edge relationship c"},
		commandItem{id: commandDeleteNode, title: "Delete selected node", description: "Confirm a detach delete and review consequences", keywords: "remove d"},
		commandItem{id: commandEditRelationship, title: "Edit selected relationship", description: "Replace schema-free relationship properties", keywords: "edge properties"},
		commandItem{id: commandDeleteRelationship, title: "Delete selected relationship", description: "Confirm removal of one edge", keywords: "edge remove"},
		commandItem{id: commandDetails, title: "Focus details", description: "Move focus to the scrollable inspector", keywords: "inspect body markdown"},
		commandItem{id: commandRefresh, title: "Refresh now", description: "Reload the selected snapshot and revision timeline", keywords: "reload r"},
		commandItem{id: commandHelp, title: "Keyboard and workflow help", description: "Show all contextual commands", keywords: "shortcuts question"},
	}
	if historical {
		items = append(items, commandItem{id: commandReturnLive, title: "Return to Live", description: "Leave read-only history and load the current graph", keywords: "current writable L"})
	}
	return newPicker(serial, pickerCommands, "Commands · type to filter", items, styles, dark, width, height)
}

func newPicker(serial uint64, kind pickerKind, title string, items []list.Item, styles styleSet, dark bool, width, height int) pickerModel {
	delegate := list.NewDefaultDelegate()
	delegate.SetSpacing(0)
	listStyles, itemStyles := styles.listStyles(dark)
	delegate.Styles = itemStyles
	model := list.New(items, delegate, clampSize(width, 20), clampSize(height, 6))
	model.Title = title
	model.Styles = listStyles
	model.DisableQuitKeybindings()
	model.SetShowHelp(false)
	model.SetShowPagination(true)
	model.SetStatusBarItemName("match", "matches")
	model.SetFilterState(list.Filtering)
	_ = model.FilterInput.Focus()
	return pickerModel{kind: kind, serial: serial, filteredValue: model.FilterValue(), list: model}
}

func (m *pickerModel) update(msg tea.Msg) tea.Cmd {
	updated, cmd := m.list.Update(msg)
	m.list = updated
	return scopeListCmd(listTargetPicker, m.serial, m.list.FilterValue(), cmd)
}

func (m *pickerModel) setSize(width, height int) {
	m.list.SetSize(clampSize(width, 20), clampSize(height, 6))
}

func (m pickerModel) view() string { return m.list.View() }

func (m pickerModel) selectedNode() (domain.EntityID, bool) {
	item, ok := m.list.SelectedItem().(nodePickerItem)
	if !ok {
		return "", false
	}
	return item.id, true
}

func (m pickerModel) selectedCommand() (commandID, bool) {
	item, ok := m.list.SelectedItem().(commandItem)
	if !ok {
		return 0, false
	}
	return item.id, true
}

func (m pickerModel) filtering() bool { return m.list.SettingFilter() }

func (m pickerModel) title() string {
	switch m.kind {
	case pickerNodes:
		return "Find work"
	case pickerCommands:
		return "Command palette"
	default:
		return "Picker"
	}
}
