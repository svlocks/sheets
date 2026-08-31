package tui

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/svlocks/sheets/internal/domain"
)

func (m *Model) rebuildNavigation() {
	m.rebuildOutline()
	m.rebuildGraph()
	m.ensureVisible()
}

func (m *Model) rebuildOutline() {
	type child struct {
		id       domain.EntityID
		position *int64
	}
	children := make(map[domain.EntityID][]child)
	hasParent := make(map[domain.EntityID]bool)
	missingParent := make(map[domain.EntityID]bool)
	for _, edge := range m.edges {
		if !strings.EqualFold(edge.Type, "CHILD") {
			continue
		}
		_, fromExists := m.nodeByID[edge.From]
		_, toExists := m.nodeByID[edge.To]
		if !toExists {
			continue
		}
		if !fromExists {
			missingParent[edge.To] = true
			continue
		}
		hasParent[edge.To] = true
		children[edge.From] = append(children[edge.From], child{id: edge.To, position: edge.Position})
	}
	for parent := range children {
		sort.SliceStable(children[parent], func(i, j int) bool {
			a, b := children[parent][i], children[parent][j]
			if a.position != nil && b.position == nil {
				return true
			}
			if a.position == nil && b.position != nil {
				return false
			}
			if a.position != nil && b.position != nil && *a.position != *b.position {
				return *a.position < *b.position
			}
			return nodeTitle(m.nodeByID[a.id]) < nodeTitle(m.nodeByID[b.id])
		})
	}

	query := strings.ToLower(strings.TrimSpace(m.search.Value()))
	if query != "" {
		rows := make([]outlineRow, 0)
		for _, node := range m.nodes {
			if nodeMatches(node, query) {
				rows = append(rows, outlineRow{ID: node.ID, Orphan: missingParent[node.ID], HasKids: len(children[node.ID]) > 0})
			}
		}
		sort.SliceStable(rows, func(i, j int) bool { return nodeTitle(m.nodeByID[rows[i].ID]) < nodeTitle(m.nodeByID[rows[j].ID]) })
		m.outlineRows = rows
		m.restoreOutlineIndex()
		return
	}

	var roots, orphans []domain.EntityID
	for _, node := range m.nodes {
		switch {
		case missingParent[node.ID]:
			orphans = append(orphans, node.ID)
		case !hasParent[node.ID]:
			roots = append(roots, node.ID)
		}
	}
	sortIDsByTitle(roots, m.nodeByID)
	sortIDsByTitle(orphans, m.nodeByID)

	rows := make([]outlineRow, 0, len(m.nodes))
	seen := make(map[domain.EntityID]bool, len(m.nodes))
	var markHidden func(domain.EntityID)
	markHidden = func(id domain.EntityID) {
		if seen[id] {
			return
		}
		seen[id] = true
		for _, child := range children[id] {
			markHidden(child.id)
		}
	}
	var visit func(domain.EntityID, int, bool, map[domain.EntityID]bool, *int64)
	visit = func(id domain.EntityID, depth int, orphan bool, path map[domain.EntityID]bool, position *int64) {
		if path[id] {
			for index := range rows {
				if rows[index].ID == id {
					rows[index].Orphan = true
					rows[index].Cycle = true
					break
				}
			}
			return
		}
		if seen[id] {
			return
		}
		seen[id] = true
		path[id] = true
		row := outlineRow{ID: id, Depth: depth, Orphan: orphan, HasKids: len(children[id]) > 0, Position: position}
		if len(rows) == 0 && !orphan {
			row.Section = "Roots"
		}
		rows = append(rows, row)
		if !m.collapsed[id] {
			for _, child := range children[id] {
				visit(child.id, depth+1, orphan, path, child.position)
			}
		} else {
			for _, child := range children[id] {
				markHidden(child.id)
			}
		}
		delete(path, id)
	}
	for _, id := range roots {
		visit(id, 0, false, make(map[domain.EntityID]bool), nil)
	}
	for _, id := range orphans {
		before := len(rows)
		visit(id, 0, true, make(map[domain.EntityID]bool), nil)
		if len(rows) > before {
			rows[before].Section = "Orphans"
		}
	}
	// Corrupt imports or future relaxed graph constraints may contain cycles or
	// disconnected components without roots. Keep them visible and explicit.
	remaining := make([]domain.EntityID, 0)
	for _, node := range m.nodes {
		if !seen[node.ID] {
			remaining = append(remaining, node.ID)
		}
	}
	sortIDsByTitle(remaining, m.nodeByID)
	for _, id := range remaining {
		before := len(rows)
		visit(id, 0, true, make(map[domain.EntityID]bool), nil)
		if len(rows) > before {
			rows[before].Section = "Orphans / cycles"
		}
	}
	m.outlineRows = rows
	m.restoreOutlineIndex()
}

func (m *Model) rebuildGraph() {
	query := strings.ToLower(strings.TrimSpace(m.search.Value()))
	nodes := append([]domain.Node(nil), m.nodes...)
	sort.SliceStable(nodes, func(i, j int) bool {
		return nodeTitle(nodes[i]) < nodeTitle(nodes[j])
	})
	rows := make([]graphRow, 0, len(nodes))
	for _, node := range nodes {
		if query != "" && !nodeMatches(node, query) {
			continue
		}
		lines := []string{graphNodeLine(node, m.graphZoom)}
		for _, edge := range sortedEdgesFor(node.ID, m.edges) {
			target, exists := m.nodeByID[edge.To]
			targetText := "<missing:" + shortID(edge.To) + ">"
			if exists {
				targetText = nodeTitle(target)
			}
			line := "  ├─" + edge.Type + "→ " + targetText
			if edge.Position != nil {
				line += fmt.Sprintf("  #%d", *edge.Position)
			}
			if m.graphZoom >= 3 {
				line += "  " + shortID(edge.ID)
			}
			lines = append(lines, line)
		}
		if len(lines) == 1 {
			lines = append(lines, "  └─ no outgoing edges")
		} else {
			lines[len(lines)-1] = strings.Replace(lines[len(lines)-1], "  ├─", "  └─", 1)
		}
		rows = append(rows, graphRow{ID: node.ID, Lines: lines})
	}
	// Preserve dangling source edges even when their source node is missing.
	for _, edge := range m.edges {
		if _, exists := m.nodeByID[edge.From]; exists {
			continue
		}
		line := fmt.Sprintf("◌ <missing:%s> ─%s→ %s", shortID(edge.From), edge.Type, shortID(edge.To))
		rows = append(rows, graphRow{Lines: []string{line}})
	}
	m.graphRows = rows
	m.restoreGraphIndex()
}

func graphNodeLine(node domain.Node, zoom int) string {
	title := nodeTitle(node)
	switch zoom {
	case 1:
		return "● " + title
	case 3:
		return fmt.Sprintf("● %s  [%s]  %s  (%d properties)", title, strings.Join(node.Labels, ", "), shortID(node.ID), len(node.Properties))
	default:
		labels := ""
		if len(node.Labels) > 0 {
			labels = "  [" + strings.Join(node.Labels, ", ") + "]"
		}
		return "● " + title + labels + "  " + shortID(node.ID)
	}
}

func sortedEdgesFor(from domain.EntityID, edges []domain.Edge) []domain.Edge {
	result := make([]domain.Edge, 0)
	for _, edge := range edges {
		if edge.From == from {
			result = append(result, edge)
		}
	}
	sort.SliceStable(result, func(i, j int) bool {
		if result[i].Type != result[j].Type {
			return result[i].Type < result[j].Type
		}
		if result[i].Position != nil && result[j].Position == nil {
			return true
		}
		if result[i].Position == nil && result[j].Position != nil {
			return false
		}
		if result[i].Position != nil && result[j].Position != nil && *result[i].Position != *result[j].Position {
			return *result[i].Position < *result[j].Position
		}
		return result[i].ID < result[j].ID
	})
	return result
}

func (m *Model) moveOutline(delta int) {
	if len(m.outlineRows) == 0 {
		return
	}
	m.outlineIndex = min(len(m.outlineRows)-1, max(0, m.outlineIndex+delta))
	m.syncOutlineSelection()
}

func (m *Model) syncOutlineSelection() {
	if len(m.outlineRows) == 0 {
		return
	}
	m.outlineIndex = min(len(m.outlineRows)-1, max(0, m.outlineIndex))
	m.selectNode(m.outlineRows[m.outlineIndex].ID)
	m.ensureVisible()
}

func (m *Model) moveGraph(delta int) {
	if len(m.graphRows) == 0 {
		return
	}
	m.graphIndex = min(len(m.graphRows)-1, max(0, m.graphIndex+delta))
	if m.graphRows[m.graphIndex].ID != "" {
		m.selectNode(m.graphRows[m.graphIndex].ID)
	}
	m.ensureVisible()
}

func (m *Model) moveHistory(delta int) {
	if len(m.history) == 0 {
		return
	}
	m.historyIndex = min(len(m.history)-1, max(0, m.historyIndex+delta))
	m.ensureVisible()
}

func (m *Model) selectNode(id domain.EntityID) {
	if id == "" {
		return
	}
	m.selected = id
	m.inspectorScroll = 0
	for index, row := range m.outlineRows {
		if row.ID == id {
			m.outlineIndex = index
			break
		}
	}
	for index, row := range m.graphRows {
		if row.ID == id {
			m.graphIndex = index
			break
		}
	}
}

func (m *Model) toggleSelected(collapseOnly bool) {
	if m.selected == "" {
		return
	}
	if collapseOnly {
		if !m.collapsed[m.selected] {
			m.collapsed[m.selected] = true
			m.rebuildOutline()
			return
		}
		// A second left on a collapsed/leaf node selects its structural parent.
		for _, edge := range m.edges {
			if strings.EqualFold(edge.Type, "CHILD") && edge.To == m.selected {
				m.selectNode(edge.From)
				return
			}
		}
		return
	}
	m.collapsed[m.selected] = !m.collapsed[m.selected]
	m.rebuildOutline()
}

func (m *Model) restoreOutlineIndex() {
	for index, row := range m.outlineRows {
		if row.ID == m.selected {
			m.outlineIndex = index
			return
		}
	}
	m.outlineIndex = min(max(0, m.outlineIndex), max(0, len(m.outlineRows)-1))
}

func (m *Model) restoreGraphIndex() {
	for index, row := range m.graphRows {
		if row.ID == m.selected {
			m.graphIndex = index
			return
		}
	}
	m.graphIndex = min(max(0, m.graphIndex), max(0, len(m.graphRows)-1))
}

func (m *Model) clampHistory() {
	m.historyIndex = min(max(0, m.historyIndex), max(0, len(m.history)-1))
}

func (m *Model) ensureVisible() {
	visible := max(3, m.height-8)
	if m.outlineIndex < m.outlineOffset {
		m.outlineOffset = m.outlineIndex
	} else if m.outlineIndex >= m.outlineOffset+visible {
		m.outlineOffset = m.outlineIndex - visible + 1
	}
	if m.graphIndex < m.graphPanY {
		m.graphPanY = m.graphIndex
	} else if m.graphIndex >= m.graphPanY+visible/2 {
		m.graphPanY = max(0, m.graphIndex-visible/2+1)
	}
	if m.historyIndex < m.historyOffset {
		m.historyOffset = m.historyIndex
	} else if m.historyIndex >= m.historyOffset+visible {
		m.historyOffset = m.historyIndex - visible + 1
	}
}

func nodeTitle(node domain.Node) string {
	for _, key := range []string{"title", "name", "summary"} {
		if value, ok := node.Properties[key].(string); ok && strings.TrimSpace(value) != "" {
			return value
		}
	}
	if len(node.Labels) > 0 {
		return strings.Join(node.Labels, ":") + " · " + shortID(node.ID)
	}
	return shortID(node.ID)
}

func nodeMatches(node domain.Node, query string) bool {
	parts := []string{string(node.ID), node.Body, strings.Join(node.Labels, " "), nodeTitle(node)}
	if encoded, err := json.Marshal(node.Properties); err == nil {
		parts = append(parts, string(encoded))
	}
	return strings.Contains(strings.ToLower(strings.Join(parts, "\n")), query)
}

func sortIDsByTitle(ids []domain.EntityID, nodes map[domain.EntityID]domain.Node) {
	sort.SliceStable(ids, func(i, j int) bool {
		a, b := nodeTitle(nodes[ids[i]]), nodeTitle(nodes[ids[j]])
		if a == b {
			return ids[i] < ids[j]
		}
		return a < b
	})
}

func shortID(id domain.EntityID) string {
	text := string(id)
	if len(text) <= 8 {
		return text
	}
	return text[:8]
}
