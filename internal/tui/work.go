package tui

import (
	"fmt"
	"strings"

	"charm.land/bubbles/v2/tree"
	tea "charm.land/bubbletea/v2"
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

type workModel struct {
	tree        tree.Model
	root        *tree.Node
	nodes       map[domain.EntityID]*tree.Node
	graph       graphState
	selected    domain.EntityID
	width       int
	height      int
	dark        bool
	styles      styleSet
	initialized bool
}

func newWorkModel(styles styleSet, dark bool) workModel {
	root := tree.Root(outlineEntry{title: "All work", synthetic: true})
	model := tree.New(root, 40, 20)
	model.SetShowHelp(false)
	model.SetScrollOff(2)
	model.SetCursorCharacter(">")
	model.SetOpenCharacter("v")
	model.SetClosedCharacter(">")
	model.SetStyles(styles.treeStyles(dark))
	return workModel{
		tree: model, root: root, nodes: make(map[domain.EntityID]*tree.Node),
		width: 40, height: 20, styles: styles, dark: dark,
	}
}

func (m *workModel) setStyle(styles styleSet, dark bool) {
	m.styles = styles
	m.dark = dark
	m.tree.SetStyles(styles.treeStyles(dark))
}

func (m *workModel) setSize(width, height int) {
	m.width = clampSize(width, 1)
	m.height = clampSize(height, 1)
	m.tree.SetSize(m.width, m.height)
}

func (m *workModel) setGraph(graph graphState) {
	previousSelection := m.selected
	open := make(map[domain.EntityID]bool, len(m.nodes))
	for id, node := range m.nodes {
		open[id] = node.IsOpen()
	}

	root := tree.Root(outlineEntry{
		title: fmt.Sprintf("All work (%d)", len(graph.nodes)), synthetic: true,
	}).Open()
	nodeMap := make(map[domain.EntityID]*tree.Node, len(graph.nodes))
	path := make(map[domain.EntityID]bool)
	var build func(domain.EntityID, *int64, bool) *tree.Node
	build = func(id domain.EntityID, position *int64, isChild bool) *tree.Node {
		node, exists := graph.nodeByID[id]
		if !exists {
			return nil
		}
		entry := outlineEntry{id: id, title: nodeTitle(node), subtitle: nodeSubtitle(node), position: position, child: isChild}
		branch := tree.Root(entry)
		nodeMap[id] = branch
		if wasOpen, known := open[id]; known && !wasOpen {
			branch.Close()
		} else {
			branch.Open()
		}
		if path[id] {
			branch.SetValue(outlineEntry{id: id, title: nodeTitle(node) + " [cycle]", subtitle: nodeSubtitle(node), position: position, child: isChild})
			return branch
		}
		path[id] = true
		for _, link := range graph.children[id] {
			child := build(link.child, link.edge.Position, true)
			if child != nil {
				branch.Child(child)
			}
		}
		delete(path, id)
		return branch
	}
	for _, id := range graph.roots {
		if branch := build(id, nil, false); branch != nil {
			root.Child(branch)
		}
	}
	if len(graph.unreachable) > 0 {
		section := tree.Root(outlineEntry{title: "Unreachable / invalid", synthetic: true}).Open()
		for _, id := range graph.unreachable {
			if _, alreadyShown := nodeMap[id]; alreadyShown {
				continue
			}
			if branch := build(id, nil, false); branch != nil {
				section.Child(branch)
			}
		}
		root.Child(section)
	}

	m.graph = graph
	m.root = root
	m.nodes = nodeMap
	m.tree.SetNodes(root)
	m.tree.SetSize(m.width, m.height)
	m.initialized = true
	if _, exists := graph.nodeByID[previousSelection]; !exists {
		previousSelection = graph.firstNodeID()
	}
	m.selectID(previousSelection)
}

func (m *workModel) selectID(id domain.EntityID) bool {
	branch, exists := m.nodes[id]
	if !exists {
		m.selected = ""
		if m.root != nil {
			m.tree.SetYOffset(0)
		}
		return false
	}
	for current := id; current != ""; {
		parentEdge, ok := m.graph.parent[current]
		if !ok {
			break
		}
		if parent := m.nodes[parentEdge.From]; parent != nil {
			parent.Open()
		}
		current = parentEdge.From
	}
	m.tree.SetNodes(m.root)
	m.tree.SetYOffset(branch.YOffset())
	m.selected = id
	return true
}

func (m *workModel) update(message any) tea.Cmd {
	updated, cmd := m.tree.Update(message)
	m.tree = updated
	m.syncSelection()
	return cmd
}

func (m *workModel) syncSelection() {
	current := m.tree.NodeAtCurrentOffset()
	if current == nil {
		m.selected = ""
		return
	}
	entry, ok := current.GivenValue().(outlineEntry)
	if !ok || entry.synthetic {
		m.selected = ""
		return
	}
	m.selected = entry.id
}

func (m *workModel) clickRow(row int) {
	if row < 0 {
		return
	}
	m.tree.SetYOffset(m.tree.ViewportYOffset() + row)
	m.syncSelection()
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
	return m.tree.View()
}
