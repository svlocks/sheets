package tui

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/svlocks/sheets/internal/app"
	"github.com/svlocks/sheets/internal/domain"
)

type childLink struct {
	child domain.EntityID
	edge  domain.Edge
}

// graphState is an immutable presentation index for one exact graph revision.
// It is constructed by an asynchronous load command and then shared by the
// workspace submodels.
type graphState struct {
	nodes       []domain.Node
	edges       []domain.Edge
	nodeByID    map[domain.EntityID]domain.Node
	edgeByID    map[domain.EntityID]domain.Edge
	children    map[domain.EntityID][]childLink
	parent      map[domain.EntityID]domain.Edge
	incoming    map[domain.EntityID][]domain.Edge
	outgoing    map[domain.EntityID][]domain.Edge
	roots       []domain.EntityID
	unreachable []domain.EntityID
}

func newGraphState(nodes []domain.Node, edges []domain.Edge) graphState {
	g := graphState{
		nodes:    append([]domain.Node(nil), nodes...),
		edges:    append([]domain.Edge(nil), edges...),
		nodeByID: make(map[domain.EntityID]domain.Node, len(nodes)),
		edgeByID: make(map[domain.EntityID]domain.Edge, len(edges)),
		children: make(map[domain.EntityID][]childLink),
		parent:   make(map[domain.EntityID]domain.Edge),
		incoming: make(map[domain.EntityID][]domain.Edge),
		outgoing: make(map[domain.EntityID][]domain.Edge),
	}
	sort.Slice(g.nodes, func(i, j int) bool { return g.nodes[i].ID < g.nodes[j].ID })
	sort.Slice(g.edges, func(i, j int) bool { return g.edges[i].ID < g.edges[j].ID })
	for _, node := range g.nodes {
		g.nodeByID[node.ID] = node
	}
	for _, edge := range g.edges {
		g.edgeByID[edge.ID] = edge
		g.outgoing[edge.From] = append(g.outgoing[edge.From], edge)
		g.incoming[edge.To] = append(g.incoming[edge.To], edge)
		if edge.Type == "CHILD" {
			if _, fromExists := g.nodeByID[edge.From]; fromExists {
				if _, toExists := g.nodeByID[edge.To]; toExists {
					g.children[edge.From] = append(g.children[edge.From], childLink{child: edge.To, edge: edge})
					g.parent[edge.To] = edge
				}
			}
		}
	}

	for parent := range g.children {
		links := g.children[parent]
		sort.SliceStable(links, func(i, j int) bool {
			left, right := links[i], links[j]
			switch {
			case left.edge.Position != nil && right.edge.Position == nil:
				return true
			case left.edge.Position == nil && right.edge.Position != nil:
				return false
			case left.edge.Position != nil && right.edge.Position != nil && *left.edge.Position != *right.edge.Position:
				return *left.edge.Position < *right.edge.Position
			}
			leftTitle := strings.ToLower(nodeTitle(g.nodeByID[left.child]))
			rightTitle := strings.ToLower(nodeTitle(g.nodeByID[right.child]))
			if leftTitle != rightTitle {
				return leftTitle < rightTitle
			}
			return left.child < right.child
		})
		g.children[parent] = links
	}
	for id := range g.nodeByID {
		if _, hasParent := g.parent[id]; !hasParent {
			g.roots = append(g.roots, id)
		}
	}
	g.sortNodeIDs(g.roots)

	// Valid stores cannot contain CHILD cycles, but keeping unreachable nodes
	// visible makes imported/corrupt test data fail safely instead of vanishing.
	seen := make(map[domain.EntityID]bool, len(g.nodes))
	stack := append([]domain.EntityID(nil), g.roots...)
	for len(stack) > 0 {
		last := len(stack) - 1
		id := stack[last]
		stack = stack[:last]
		if seen[id] {
			continue
		}
		seen[id] = true
		children := g.children[id]
		for index := len(children) - 1; index >= 0; index-- {
			stack = append(stack, children[index].child)
		}
	}
	for id := range g.nodeByID {
		if !seen[id] {
			g.unreachable = append(g.unreachable, id)
		}
	}
	g.sortNodeIDs(g.unreachable)
	return g
}

func (g graphState) sortNodeIDs(ids []domain.EntityID) {
	sort.SliceStable(ids, func(i, j int) bool {
		left := strings.ToLower(nodeTitle(g.nodeByID[ids[i]]))
		right := strings.ToLower(nodeTitle(g.nodeByID[ids[j]]))
		if left != right {
			return left < right
		}
		return ids[i] < ids[j]
	})
}

func (g graphState) firstNodeID() domain.EntityID {
	if len(g.roots) > 0 {
		return g.roots[0]
	}
	if len(g.nodes) > 0 {
		return g.nodes[0].ID
	}
	return ""
}

func (g graphState) descendants(id domain.EntityID) map[domain.EntityID]bool {
	result := make(map[domain.EntityID]bool)
	stack := []domain.EntityID{id}
	for len(stack) > 0 {
		last := len(stack) - 1
		parent := stack[last]
		stack = stack[:last]
		for _, link := range g.children[parent] {
			if result[link.child] {
				continue
			}
			result[link.child] = true
			stack = append(stack, link.child)
		}
	}
	return result
}

func nodeTitle(node domain.Node) string {
	if value, ok := node.Properties["title"].(string); ok && strings.TrimSpace(value) != "" {
		return terminalLine(truncateRunes(strings.TrimSpace(value), maxNodeTitleRunes))
	}
	if len(node.Labels) > 0 {
		return terminalLine(truncateRunes(node.Labels[0], maxDisplayLabelRunes)) + " " + shortID(node.ID)
	}
	return "Node " + shortID(node.ID)
}

func nodeSubtitle(node domain.Node) string {
	displayed := min(len(node.Labels), maxDisplayLabels)
	parts := make([]string, 0, displayed+2)
	for _, label := range node.Labels[:displayed] {
		parts = append(parts, terminalLine(truncateRunes(label, maxDisplayLabelRunes)))
	}
	if displayed < len(node.Labels) {
		parts = append(parts, fmt.Sprintf("+%d labels", len(node.Labels)-displayed))
	}
	parts = append(parts, shortID(node.ID))
	return strings.Join(parts, " · ")
}

func shortID(id domain.EntityID) string {
	value := terminalLine(string(id))
	if len(value) <= 8 {
		return value
	}
	// Sheets uses time-ordered UUIDs, so IDs created close together share a
	// long prefix. The random suffix is the compact portion that actually helps
	// people distinguish adjacent nodes and relationship endpoints.
	return value[len(value)-8:]
}

func nodeSearchValue(node domain.Node) string {
	return boundedSearchText(nodeTitle(node), string(node.ID), node.Labels, node.Properties, node.Body)
}

func edgeSearchValue(edge domain.Edge, graph graphState) string {
	return boundedSearchText(
		string(edge.ID), edge.Type, string(edge.From), string(edge.To),
		nodeTitle(graph.nodeByID[edge.From]), nodeTitle(graph.nodeByID[edge.To]), edge.Properties,
	)
}

func prettyJSON(value any) string {
	encoded, err := json.MarshalIndent(app.JSONValue(value), "", "  ")
	if err != nil {
		return fmt.Sprint(value)
	}
	return terminalSafeJSON(string(encoded))
}
