package tui

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"sort"
	"strings"
	"time"

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
	var visit func(domain.EntityID)
	visit = func(id domain.EntityID) {
		if seen[id] {
			return
		}
		seen[id] = true
		for _, child := range g.children[id] {
			visit(child.child)
		}
	}
	for _, id := range g.roots {
		visit(id)
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
	var visit func(domain.EntityID)
	visit = func(parent domain.EntityID) {
		for _, link := range g.children[parent] {
			if result[link.child] {
				continue
			}
			result[link.child] = true
			visit(link.child)
		}
	}
	visit(id)
	return result
}

func nodeTitle(node domain.Node) string {
	if value, ok := node.Properties["title"].(string); ok && strings.TrimSpace(value) != "" {
		return strings.TrimSpace(value)
	}
	if len(node.Labels) > 0 {
		return node.Labels[0] + " " + shortID(node.ID)
	}
	return "Node " + shortID(node.ID)
}

func nodeSubtitle(node domain.Node) string {
	parts := append([]string(nil), node.Labels...)
	parts = append(parts, shortID(node.ID))
	return strings.Join(parts, " · ")
}

func shortID(id domain.EntityID) string {
	value := string(id)
	if len(value) <= 8 {
		return value
	}
	// Sheets uses time-ordered UUIDs, so IDs created close together share a
	// long prefix. The random suffix is the compact portion that actually helps
	// people distinguish adjacent nodes and relationship endpoints.
	return value[len(value)-8:]
}

func nodeSearchValue(node domain.Node) string {
	return strings.Join([]string{
		nodeTitle(node),
		string(node.ID),
		strings.Join(node.Labels, " "),
		stableJSON(node.Properties),
		node.Body,
	}, " ")
}

func edgeSearchValue(edge domain.Edge, graph graphState) string {
	return strings.Join([]string{
		string(edge.ID), edge.Type, string(edge.From), string(edge.To),
		nodeTitle(graph.nodeByID[edge.From]), nodeTitle(graph.nodeByID[edge.To]),
		stableJSON(edge.Properties),
	}, " ")
}

func stableJSON(value any) string {
	encoded, err := json.Marshal(jsonSafeValue(value))
	if err != nil {
		return fmt.Sprint(value)
	}
	return string(encoded)
}

func prettyJSON(value any) string {
	encoded, err := json.MarshalIndent(jsonSafeValue(value), "", "  ")
	if err != nil {
		return fmt.Sprint(value)
	}
	return string(encoded)
}

// jsonSafeValue mirrors the CLI's unambiguous tagged representation for IEEE
// non-finite values while recursively preserving ordinary JSON encodings for
// graph entities, temporal values, durations, bytes, lists, and maps. Query
// rows and editable property objects must not degrade to fmt's Go syntax just
// because one deeply nested float is NaN or infinite.
func jsonSafeValue(value any) any {
	switch value := value.(type) {
	case nil, bool, string, json.Number, time.Time:
		return value
	case time.Duration:
		return int64(value)
	case float64:
		return safeFloat(value)
	case float32:
		return safeFloat(float64(value))
	case []byte:
		return base64.StdEncoding.EncodeToString(value)
	}
	return jsonSafeReflect(reflect.ValueOf(value))
}

func safeFloat(value float64) any {
	switch {
	case math.IsNaN(value):
		return map[string]any{"$float": "NaN"}
	case math.IsInf(value, 1):
		return map[string]any{"$float": "+Infinity"}
	case math.IsInf(value, -1):
		return map[string]any{"$float": "-Infinity"}
	default:
		return value
	}
}

func jsonSafeReflect(value reflect.Value) any {
	if !value.IsValid() {
		return nil
	}
	for value.Kind() == reflect.Interface || value.Kind() == reflect.Pointer {
		if value.IsNil() {
			return nil
		}
		value = value.Elem()
	}
	switch value.Kind() {
	case reflect.Map:
		if value.Type().Key().Kind() != reflect.String {
			return fmt.Sprint(value.Interface())
		}
		result := make(map[string]any, value.Len())
		iterator := value.MapRange()
		for iterator.Next() {
			result[iterator.Key().String()] = jsonSafeValue(iterator.Value().Interface())
		}
		return result
	case reflect.Slice, reflect.Array:
		result := make([]any, value.Len())
		for index := range result {
			result[index] = jsonSafeValue(value.Index(index).Interface())
		}
		return result
	case reflect.Struct:
		result := make(map[string]any)
		typeInfo := value.Type()
		for index := 0; index < value.NumField(); index++ {
			fieldInfo := typeInfo.Field(index)
			if fieldInfo.PkgPath != "" {
				continue
			}
			tag := fieldInfo.Tag.Get("json")
			name, options, _ := strings.Cut(tag, ",")
			if name == "-" {
				continue
			}
			if name == "" {
				name = fieldInfo.Name
			}
			field := value.Field(index)
			if strings.Contains(options, "omitempty") && field.IsZero() {
				continue
			}
			result[name] = jsonSafeValue(field.Interface())
		}
		return result
	case reflect.Invalid, reflect.Chan, reflect.Func, reflect.UnsafePointer:
		return fmt.Sprint(value.Interface())
	default:
		return value.Interface()
	}
}
