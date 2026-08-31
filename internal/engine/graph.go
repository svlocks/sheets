package engine

import (
	"context"
	"fmt"
	"sort"

	"github.com/svlocks/sheets/internal/domain"
	"github.com/svlocks/sheets/internal/store"
)

const graphPageSize = 1000

// GraphSnapshot is a complete immutable view returned to interactive clients.
// Callers receive values rather than engine-owned pointers.
type GraphSnapshot struct {
	Revision domain.Revision `json:"revision"`
	Nodes    []domain.Node   `json:"nodes"`
	Edges    []domain.Edge   `json:"edges"`
}

type memoryGraph struct {
	revision domain.Revision
	nodes    map[domain.EntityID]*domain.Node
	edges    map[domain.EntityID]*domain.Edge
	outgoing map[domain.EntityID][]*domain.Edge
	incoming map[domain.EntityID][]*domain.Edge
	writer   *store.WriteTx
}

func newMemoryGraph(revision domain.Revision, nodes []domain.Node, edges []domain.Edge, writer *store.WriteTx) *memoryGraph {
	graph := &memoryGraph{
		revision: revision,
		nodes:    make(map[domain.EntityID]*domain.Node, len(nodes)),
		edges:    make(map[domain.EntityID]*domain.Edge, len(edges)),
		outgoing: make(map[domain.EntityID][]*domain.Edge),
		incoming: make(map[domain.EntityID][]*domain.Edge),
		writer:   writer,
	}
	for index := range nodes {
		node := nodes[index]
		graph.nodes[node.ID] = &node
	}
	for index := range edges {
		edge := edges[index]
		graph.edges[edge.ID] = &edge
		graph.outgoing[edge.From] = append(graph.outgoing[edge.From], &edge)
		graph.incoming[edge.To] = append(graph.incoming[edge.To], &edge)
	}
	graph.sortAdjacency()
	return graph
}

func (g *memoryGraph) sortAdjacency() {
	less := func(left, right *domain.Edge) bool { return left.ID < right.ID }
	for id := range g.outgoing {
		sort.Slice(g.outgoing[id], func(i, j int) bool { return less(g.outgoing[id][i], g.outgoing[id][j]) })
	}
	for id := range g.incoming {
		sort.Slice(g.incoming[id], func(i, j int) bool { return less(g.incoming[id][i], g.incoming[id][j]) })
	}
}

func (g *memoryGraph) nodeValues() []domain.Node {
	ids := make([]domain.EntityID, 0, len(g.nodes))
	for id := range g.nodes {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	nodes := make([]domain.Node, len(ids))
	for index, id := range ids {
		nodes[index] = *g.nodes[id]
	}
	return nodes
}

func (g *memoryGraph) edgeValues() []domain.Edge {
	ids := make([]domain.EntityID, 0, len(g.edges))
	for id := range g.edges {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	edges := make([]domain.Edge, len(ids))
	for index, id := range ids {
		edges[index] = *g.edges[id]
	}
	return edges
}

func (g *memoryGraph) nodePointers() []*domain.Node {
	nodes := make([]*domain.Node, 0, len(g.nodes))
	for _, node := range g.nodes {
		nodes = append(nodes, node)
	}
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].ID < nodes[j].ID })
	return nodes
}

func (g *memoryGraph) createNode(input store.NodeInput) (*domain.Node, error) {
	if g.writer == nil {
		return nil, domain.ErrReadOnly
	}
	created, err := g.writer.CreateNode(input)
	if err != nil {
		return nil, err
	}
	node := created
	g.nodes[node.ID] = &node
	g.revision = g.writer.CurrentRevision()
	return &node, nil
}

func (g *memoryGraph) updateNode(id domain.EntityID, update store.NodeUpdate) (*domain.Node, bool, error) {
	if g.writer == nil {
		return nil, false, domain.ErrReadOnly
	}
	current, exists := g.nodes[id]
	if !exists {
		return nil, false, fmt.Errorf("%w: node %s", domain.ErrNotFound, id)
	}
	updated, err := g.writer.UpdateNode(id, update)
	if err != nil {
		return nil, false, err
	}
	changed := current.ValidFrom != updated.ValidFrom || current.Body != updated.Body ||
		!equalStringSlices(current.Labels, updated.Labels) || !equalValues(current.Properties, updated.Properties)
	*current = updated
	g.revision = g.writer.CurrentRevision()
	return current, changed, nil
}

func (g *memoryGraph) deleteNode(id domain.EntityID) ([]domain.EntityID, error) {
	if g.writer == nil {
		return nil, domain.ErrReadOnly
	}
	if _, exists := g.nodes[id]; !exists {
		return nil, fmt.Errorf("%w: node %s", domain.ErrNotFound, id)
	}
	incident := make([]domain.EntityID, 0, len(g.outgoing[id])+len(g.incoming[id]))
	seen := make(map[domain.EntityID]struct{})
	for _, edge := range append(append([]*domain.Edge(nil), g.outgoing[id]...), g.incoming[id]...) {
		if _, exists := seen[edge.ID]; !exists {
			incident = append(incident, edge.ID)
			seen[edge.ID] = struct{}{}
		}
	}
	if err := g.writer.DeleteNode(id); err != nil {
		return nil, err
	}
	delete(g.nodes, id)
	for _, edgeID := range incident {
		g.removeEdge(edgeID)
	}
	g.revision = g.writer.CurrentRevision()
	return incident, nil
}

func (g *memoryGraph) createEdge(input store.EdgeInput) (*domain.Edge, error) {
	if g.writer == nil {
		return nil, domain.ErrReadOnly
	}
	created, err := g.writer.CreateEdge(input)
	if err != nil {
		return nil, err
	}
	edge := created
	g.edges[edge.ID] = &edge
	g.outgoing[edge.From] = append(g.outgoing[edge.From], &edge)
	g.incoming[edge.To] = append(g.incoming[edge.To], &edge)
	g.sortAdjacency()
	g.revision = g.writer.CurrentRevision()
	return &edge, nil
}

func (g *memoryGraph) updateEdge(id domain.EntityID, update store.EdgeUpdate) (*domain.Edge, bool, error) {
	if g.writer == nil {
		return nil, false, domain.ErrReadOnly
	}
	current, exists := g.edges[id]
	if !exists {
		return nil, false, fmt.Errorf("%w: relationship %s", domain.ErrNotFound, id)
	}
	before := *current
	updated, err := g.writer.UpdateEdge(id, update)
	if err != nil {
		return nil, false, err
	}
	changed := before.ValidFrom != updated.ValidFrom || before.From != updated.From || before.To != updated.To ||
		before.Type != updated.Type || !equalIntPointers(before.Position, updated.Position) ||
		!equalValues(before.Properties, updated.Properties)
	if changed && (before.From != updated.From || before.To != updated.To) {
		g.removeAdjacency(&before)
		*current = updated
		g.outgoing[current.From] = append(g.outgoing[current.From], current)
		g.incoming[current.To] = append(g.incoming[current.To], current)
		g.sortAdjacency()
	} else {
		*current = updated
	}
	g.revision = g.writer.CurrentRevision()
	return current, changed, nil
}

func (g *memoryGraph) deleteEdge(id domain.EntityID) error {
	if g.writer == nil {
		return domain.ErrReadOnly
	}
	if _, exists := g.edges[id]; !exists {
		return fmt.Errorf("%w: relationship %s", domain.ErrNotFound, id)
	}
	if err := g.writer.DeleteEdge(id); err != nil {
		return err
	}
	g.removeEdge(id)
	g.revision = g.writer.CurrentRevision()
	return nil
}

func (g *memoryGraph) removeEdge(id domain.EntityID) {
	edge, exists := g.edges[id]
	if !exists {
		return
	}
	g.removeAdjacency(edge)
	delete(g.edges, id)
}

func (g *memoryGraph) removeAdjacency(edge *domain.Edge) {
	g.outgoing[edge.From] = withoutEdge(g.outgoing[edge.From], edge.ID)
	g.incoming[edge.To] = withoutEdge(g.incoming[edge.To], edge.ID)
}

func withoutEdge(edges []*domain.Edge, id domain.EntityID) []*domain.Edge {
	for index, edge := range edges {
		if edge.ID == id {
			return append(edges[:index], edges[index+1:]...)
		}
	}
	return edges
}

func equalStringSlices(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func equalIntPointers(left, right *int64) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}

func loadSnapshot(ctx context.Context, source *store.Store, snapshot domain.Snapshot) (*memoryGraph, error) {
	revision, err := source.ResolveSnapshot(ctx, snapshot)
	if err != nil {
		return nil, err
	}
	nodes, err := allNodes(ctx, source, snapshot)
	if err != nil {
		return nil, err
	}
	edges, err := allEdges(ctx, source, snapshot)
	if err != nil {
		return nil, err
	}
	return newMemoryGraph(revision, nodes, edges, nil), nil
}

func loadWriteGraph(tx *store.WriteTx) (*memoryGraph, error) {
	nodes, err := tx.ListNodes()
	if err != nil {
		return nil, err
	}
	edges, err := tx.ListEdges()
	if err != nil {
		return nil, err
	}
	return newMemoryGraph(tx.CurrentRevision(), nodes, edges, tx), nil
}

func allNodes(ctx context.Context, source *store.Store, snapshot domain.Snapshot) ([]domain.Node, error) {
	var result []domain.Node
	page := domain.Page{Limit: graphPageSize}
	for {
		nodes, info, err := source.ListNodes(ctx, snapshot, store.NodeFilter{}, page)
		if err != nil {
			return nil, err
		}
		result = append(result, nodes...)
		if info.Next == "" {
			return result, nil
		}
		page.After = info.Next
	}
}

func allEdges(ctx context.Context, source *store.Store, snapshot domain.Snapshot) ([]domain.Edge, error) {
	var result []domain.Edge
	page := domain.Page{Limit: graphPageSize}
	for {
		edges, info, err := source.ListEdges(ctx, snapshot, store.EdgeFilter{}, page)
		if err != nil {
			return nil, err
		}
		result = append(result, edges...)
		if info.Next == "" {
			return result, nil
		}
		page.After = info.Next
	}
}
