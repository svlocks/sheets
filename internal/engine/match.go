package engine

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/svlocks/sheets/internal/cypher"
	"github.com/svlocks/sheets/internal/domain"
	"github.com/svlocks/sheets/internal/domain/temporal"
)

// Path is the runtime representation of a matched graph path.
type Path struct {
	Nodes         []*domain.Node `json:"nodes"`
	Relationships []*domain.Edge `json:"relationships"`
}

// PathValue is the detached representation returned to callers.
type PathValue struct {
	Nodes         []domain.Node `json:"nodes"`
	Relationships []domain.Edge `json:"relationships"`
}

type pathExpansionBudget struct {
	limit int64
	used  int64
}

func (b *pathExpansionBudget) take(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if b == nil || b.limit <= 0 {
		return nil
	}
	b.used++
	if b.used > b.limit {
		return fmt.Errorf("path expansion limit of %d exceeded; add a finite relationship bound or a more selective pattern", b.limit)
	}
	return nil
}

func (p Path) cloneValues() PathValue {
	result := PathValue{
		Nodes:         make([]domain.Node, len(p.Nodes)),
		Relationships: make([]domain.Edge, len(p.Relationships)),
	}
	for index, node := range p.Nodes {
		result.Nodes[index] = freezeValue(node).(domain.Node)
	}
	for index, edge := range p.Relationships {
		result.Relationships[index] = freezeValue(edge).(domain.Edge)
	}
	return result
}

func matchClauseRows(graph *memoryGraph, evaluator evaluator, input []row, clause *cypher.MatchClause) ([]row, error) {
	introduced := patternVariables(clause.Patterns)
	result := make([]row, 0)
	for _, inputRow := range input {
		if err := evaluator.ctx.Err(); err != nil {
			return nil, err
		}
		matches := []row{cloneRow(inputRow)}
		var err error
		for _, pattern := range clause.Patterns {
			matches, err = matchPattern(graph, evaluator, matches, pattern)
			if err != nil {
				return nil, err
			}
			if len(matches) == 0 {
				break
			}
		}
		if clause.Where != nil {
			matches, err = filterRows(evaluator, matches, clause.Where)
			if err != nil {
				return nil, err
			}
		}
		if len(matches) == 0 && clause.Optional {
			if err := evaluator.rows.take(evaluator.ctx, 1); err != nil {
				return nil, err
			}
			optional := cloneRow(inputRow)
			for _, variable := range introduced {
				if _, exists := optional[variable]; !exists {
					optional[variable] = nil
				}
			}
			result = append(result, optional)
			continue
		}
		if err := evaluator.rows.take(evaluator.ctx, len(matches)); err != nil {
			return nil, err
		}
		result = append(result, matches...)
	}
	return result, nil
}

func matchPattern(graph *memoryGraph, evaluator evaluator, input []row, pattern cypher.PatternPart) ([]row, error) {
	result := make([]row, 0)
	for _, inputRow := range input {
		if err := evaluator.ctx.Err(); err != nil {
			return nil, err
		}
		first := pattern.Element.Nodes[0]
		candidates, err := candidateNodes(graph, evaluator, inputRow, first)
		if err != nil {
			return nil, err
		}
		for _, candidate := range candidates {
			if err := evaluator.ctx.Err(); err != nil {
				return nil, err
			}
			next := cloneRow(inputRow)
			if !bindValue(next, first.Variable.Name, candidate) {
				continue
			}
			path := Path{Nodes: []*domain.Node{candidate}}
			matched, err := extendPattern(graph, evaluator, next, pattern.Element, 0, path)
			if err != nil {
				return nil, err
			}
			for _, candidateRow := range matched {
				if bindValue(candidateRow, pattern.Variable.Name, pathForRow(pattern.Element, candidateRow, path)) {
					if err := evaluator.rows.take(evaluator.ctx, 1); err != nil {
						return nil, err
					}
					result = append(result, candidateRow)
				}
			}
		}
	}
	return result, nil
}

// matchedPathRow keeps the concrete path alongside a row until its named path
// variable is bound. It is never exposed outside pattern matching.
const internalPathKey = "\x00sheets.path"
const expressionPathKey = "\x00sheets.expression-path"

func (e *queryExecution) evaluatePatternRows(element cypher.PatternElement, variable cypher.Identifier, values row) ([]row, error) {
	return matchPattern(e.graph, e.evaluator, []row{cloneRow(values)}, cypher.PatternPart{
		Variable: variable,
		Element:  element,
	})
}

func (e *queryExecution) evaluatePattern(element cypher.PatternElement, values row) ([]Path, error) {
	matched, err := e.evaluatePatternRows(element, cypher.Identifier{Name: expressionPathKey}, values)
	if err != nil {
		return nil, err
	}
	paths := make([]Path, 0, len(matched))
	for _, candidate := range matched {
		path, ok := candidate[expressionPathKey].(Path)
		if ok {
			paths = append(paths, path)
		}
	}
	return paths, nil
}

// evaluateShortestPattern performs breadth-first expansion rather than asking
// evaluatePattern to enumerate every trail and then discarding all but the
// shortest. A shortest relationship trail is node-simple, so level-based
// predecessor tracking is sufficient and preserves deterministic edge order.
func (e *queryExecution) evaluateShortestPattern(element cypher.PatternElement, values row, all bool) (any, error) {
	if len(element.Nodes) != 2 || len(element.Relationships) != 1 {
		// The generic matcher remains the semantic fallback for compound pattern
		// expressions. The common variable-length shortest-path form below never
		// enumerates longer paths.
		paths, err := e.evaluatePattern(element, values)
		if err != nil || len(paths) == 0 {
			return nil, err
		}
		return selectShortestPaths(paths, all), nil
	}
	if e.graph == nil {
		return nil, errors.New("shortestPath requires a graph context")
	}
	startPattern, targetPattern := element.Nodes[0], element.Nodes[1]
	relationship := element.Relationships[0]
	// Use the same static boundary reported by EXPLAIN. In particular, a named
	// variable-length relationship can be correlated to an outer trail and a
	// dynamic/lower bound above one requires relationship-trail state. Keeping
	// those shapes on the generic matcher avoids an execution-time algorithm
	// switch that the physical plan did not disclose.
	if !shortestPatternUsesBFS(element) {
		paths, fallbackErr := e.evaluatePattern(element, values)
		if fallbackErr != nil || len(paths) == 0 {
			return nil, fallbackErr
		}
		return selectShortestPaths(paths, all), nil
	}
	minimum, maximum, err := relationshipBounds(e.evaluator, values, relationship, len(e.graph.edges))
	if err != nil {
		return nil, err
	}
	starts, err := candidateNodes(e.graph, e.evaluator, values, startPattern)
	if err != nil {
		return nil, err
	}
	if len(starts) == 0 {
		return nil, nil
	}
	var result []Path
	for _, start := range starts {
		paths, err := shortestPathsFrom(e.evaluator, e.graph, values, start, targetPattern, relationship, minimum, maximum)
		if err != nil {
			return nil, err
		}
		result = append(result, paths...)
	}
	if len(result) == 0 {
		return nil, nil
	}
	return selectShortestPaths(result, all), nil
}

func selectShortestPaths(paths []Path, all bool) any {
	minimum := len(paths[0].Relationships)
	for _, path := range paths[1:] {
		if len(path.Relationships) < minimum {
			minimum = len(path.Relationships)
		}
	}
	result := make([]Path, 0, len(paths))
	for _, path := range paths {
		if len(path.Relationships) == minimum {
			result = append(result, path)
		}
	}
	sort.SliceStable(result, func(i, j int) bool { return pathOrderKey(result[i]) < pathOrderKey(result[j]) })
	if !all {
		return result[0]
	}
	values := make([]any, len(result))
	for index := range result {
		values[index] = result[index]
	}
	return values
}

type shortestPredecessor struct {
	from domain.EntityID
	edge *domain.Edge
}

func shortestPathsFrom(evaluator evaluator, graph *memoryGraph, values row, start *domain.Node, target cypher.NodePattern, relationship cypher.RelationshipPattern, minimum, maximum int64) ([]Path, error) {
	if maximum < 0 {
		return nil, nil
	}
	if minimum == 0 {
		ok, err := shortestTargetMatches(evaluator, values, start, target)
		if err != nil {
			return nil, err
		}
		if ok {
			return []Path{{Nodes: []*domain.Node{start}}}, nil
		}
	}
	frontier := []domain.EntityID{start.ID}
	levels := map[domain.EntityID]int64{start.ID: 0}
	predecessors := make(map[domain.EntityID][]shortestPredecessor)
	var targetIDs []domain.EntityID
	var completed []Path
	for depth := int64(1); depth <= maximum && len(frontier) > 0; depth++ {
		if err := evaluator.ctx.Err(); err != nil {
			return nil, err
		}
		nextSet := make(map[domain.EntityID]struct{})
		for _, currentID := range frontier {
			current := graph.nodes[currentID]
			if current == nil {
				continue
			}
			for _, adjacent := range adjacentEdges(graph, current.ID, relationship.Direction) {
				if err := evaluator.paths.take(evaluator.ctx); err != nil {
					return nil, err
				}
				matches, err := shortestRelationshipMatches(evaluator, values, adjacent.edge, relationship)
				if err != nil || !matches {
					if err != nil {
						return nil, err
					}
					continue
				}
				next := graph.nodes[adjacent.next]
				if next == nil {
					continue
				}
				previousLevel, visited := levels[next.ID]
				// A target seen below the declared lower bound must remain
				// eligible at later levels; all other nodes only need their first
				// (therefore shortest) level.
				isTarget, targetErr := shortestTargetMatches(evaluator, values, next, target)
				if targetErr != nil {
					return nil, targetErr
				}
				if next.ID == start.ID && isTarget && depth >= minimum {
					prefixes, reconstructErr := reconstructShortestPaths(evaluator, graph, start.ID, current.ID, predecessors)
					if reconstructErr != nil {
						return nil, reconstructErr
					}
					for _, prefix := range prefixes {
						// Cypher paths are relationship trails. In an undirected
						// traversal the edge used to leave the start is also adjacent
						// when returning from its other endpoint; it must not be
						// reused to manufacture a two-hop cycle.
						if pathUsesRelationship(prefix, adjacent.edge.ID) {
							continue
						}
						if err := evaluator.paths.take(evaluator.ctx); err != nil {
							return nil, err
						}
						nodes := append(append([]*domain.Node(nil), prefix.Nodes...), start)
						edges := append(append([]*domain.Edge(nil), prefix.Relationships...), adjacent.edge)
						completed = append(completed, Path{Nodes: nodes, Relationships: edges})
					}
					continue
				}
				if visited && previousLevel < depth && (!isTarget || depth >= minimum) {
					continue
				}
				if !visited || previousLevel == depth {
					levels[next.ID] = depth
					predecessors[next.ID] = append(predecessors[next.ID], shortestPredecessor{from: current.ID, edge: adjacent.edge})
					nextSet[next.ID] = struct{}{}
				}
				if depth >= minimum && isTarget {
					targetIDs = append(targetIDs, next.ID)
				}
			}
		}
		if len(targetIDs) > 0 || len(completed) > 0 {
			break
		}
		frontier = sortedNodeIDs(nextSet)
	}
	if len(targetIDs) == 0 && len(completed) == 0 {
		return nil, nil
	}
	targetSet := make(map[domain.EntityID]struct{}, len(targetIDs))
	for _, id := range targetIDs {
		targetSet[id] = struct{}{}
	}
	paths := completed
	for _, targetID := range sortedNodeIDs(targetSet) {
		reconstructed, err := reconstructShortestPaths(evaluator, graph, start.ID, targetID, predecessors)
		if err != nil {
			return nil, err
		}
		paths = append(paths, reconstructed...)
	}
	return paths, nil
}

func pathUsesRelationship(path Path, id domain.EntityID) bool {
	for _, edge := range path.Relationships {
		if edge.ID == id {
			return true
		}
	}
	return false
}

func shortestTargetMatches(evaluator evaluator, values row, node *domain.Node, pattern cypher.NodePattern) (bool, error) {
	if pattern.Variable.Name != "" {
		if bound, exists := values[pattern.Variable.Name]; exists {
			wanted, ok := bound.(*domain.Node)
			if !ok || wanted == nil || wanted.ID != node.ID {
				return false, nil
			}
		}
	}
	return nodePatternMatches(evaluator, values, node, pattern)
}

func shortestRelationshipMatches(evaluator evaluator, values row, edge *domain.Edge, pattern cypher.RelationshipPattern) (bool, error) {
	if pattern.Variable.Name != "" {
		if bound, exists := values[pattern.Variable.Name]; exists {
			wanted, ok := bound.(*domain.Edge)
			if !ok || wanted == nil || wanted.ID != edge.ID {
				return false, nil
			}
		}
	}
	return relationshipPatternMatches(evaluator, values, edge, pattern)
}

func reconstructShortestPaths(evaluator evaluator, graph *memoryGraph, start, target domain.EntityID, predecessors map[domain.EntityID][]shortestPredecessor) ([]Path, error) {
	if start == target {
		return []Path{{Nodes: []*domain.Node{graph.nodes[start]}}}, nil
	}
	var result []Path
	var walk func(domain.EntityID, []*domain.Node, []*domain.Edge) error
	walk = func(current domain.EntityID, reverseNodes []*domain.Node, reverseEdges []*domain.Edge) error {
		if err := evaluator.ctx.Err(); err != nil {
			return err
		}
		if current == start {
			if err := evaluator.paths.take(evaluator.ctx); err != nil {
				return err
			}
			nodes := append([]*domain.Node(nil), reverseNodes...)
			edges := append([]*domain.Edge(nil), reverseEdges...)
			reversePointers(nodes)
			reversePointers(edges)
			result = append(result, Path{Nodes: nodes, Relationships: edges})
			return nil
		}
		for _, predecessor := range predecessors[current] {
			from := graph.nodes[predecessor.from]
			if from == nil {
				continue
			}
			if err := walk(predecessor.from, append(reverseNodes, from), append(reverseEdges, predecessor.edge)); err != nil {
				return err
			}
		}
		return nil
	}
	if err := walk(target, []*domain.Node{graph.nodes[target]}, nil); err != nil {
		return nil, err
	}
	return result, nil
}

func reversePointers[T any](values []T) {
	for left, right := 0, len(values)-1; left < right; left, right = left+1, right-1 {
		values[left], values[right] = values[right], values[left]
	}
}

func sortedNodeIDs(values map[domain.EntityID]struct{}) []domain.EntityID {
	result := make([]domain.EntityID, 0, len(values))
	for id := range values {
		result = append(result, id)
	}
	sort.Slice(result, func(i, j int) bool { return result[i] < result[j] })
	return result
}

func pathOrderKey(path Path) string {
	parts := make([]string, len(path.Relationships))
	for index, edge := range path.Relationships {
		parts[index] = string(edge.ID)
	}
	return strings.Join(parts, "\x00")
}

func (e *queryExecution) evaluateExistsSubquery(query *cypher.QueryStatement, values row) (bool, error) {
	if statementMutates(query) {
		return false, fmt.Errorf("EXISTS subqueries cannot mutate the graph")
	}
	previous := e.lastRows
	defer func() { e.lastRows = previous }()
	_, err := e.clauses(query.Clauses, []row{cloneRow(values)})
	if err != nil {
		return false, err
	}
	if len(e.lastRows) > 0 {
		return true, nil
	}
	for _, branch := range query.UnionBranches {
		_, err := e.clauses(branch.Query.Clauses, []row{cloneRow(values)})
		if err != nil {
			return false, err
		}
		if len(e.lastRows) > 0 {
			return true, nil
		}
	}
	return false, nil
}

func extendPattern(graph *memoryGraph, evaluator evaluator, values row, element cypher.PatternElement, relationshipIndex int, current Path) ([]row, error) {
	if relationshipIndex >= len(element.Relationships) {
		values[internalPathKey] = current
		return []row{values}, nil
	}
	relationship := element.Relationships[relationshipIndex]
	targetPattern := element.Nodes[relationshipIndex+1]
	minimum, maximum, err := relationshipBounds(evaluator, values, relationship, len(graph.edges))
	if err != nil {
		return nil, err
	}
	currentNode := current.Nodes[len(current.Nodes)-1]
	var matches []row
	var walk func(*domain.Node, int64, row, Path, map[domain.EntityID]struct{}) error
	walk = func(node *domain.Node, depth int64, currentRow row, path Path, used map[domain.EntityID]struct{}) error {
		if err := evaluator.ctx.Err(); err != nil {
			return err
		}
		if depth >= minimum {
			if ok, err := nodePatternMatches(evaluator, currentRow, node, targetPattern); err != nil {
				return err
			} else if ok {
				next := cloneRow(currentRow)
				if bindValue(next, targetPattern.Variable.Name, node) {
					var relationshipValue any
					if relationship.Length == nil {
						if len(path.Relationships) > 0 {
							relationshipValue = path.Relationships[len(path.Relationships)-1]
						}
					} else {
						relationshipValue = make([]any, len(path.Relationships)-len(current.Relationships))
						for index, edge := range path.Relationships[len(current.Relationships):] {
							relationshipValue.([]any)[index] = edge
						}
					}
					if bindValue(next, relationship.Variable.Name, relationshipValue) {
						extended, err := extendPattern(graph, evaluator, next, element, relationshipIndex+1, path)
						if err != nil {
							return err
						}
						matches = append(matches, extended...)
					}
				}
			}
		}
		if depth == maximum {
			return nil
		}
		for _, adjacent := range adjacentEdges(graph, node.ID, relationship.Direction) {
			if _, exists := used[adjacent.edge.ID]; exists {
				continue
			}
			ok, err := relationshipPatternMatches(evaluator, currentRow, adjacent.edge, relationship)
			if err != nil {
				return err
			}
			if !ok {
				continue
			}
			if err := evaluator.paths.take(evaluator.ctx); err != nil {
				return err
			}
			nextNode := graph.nodes[adjacent.next]
			if nextNode == nil {
				continue
			}
			nextPath := Path{
				Nodes:         append(append([]*domain.Node(nil), path.Nodes...), nextNode),
				Relationships: append(append([]*domain.Edge(nil), path.Relationships...), adjacent.edge),
			}
			nextUsed := make(map[domain.EntityID]struct{}, len(used)+1)
			for id := range used {
				nextUsed[id] = struct{}{}
			}
			nextUsed[adjacent.edge.ID] = struct{}{}
			if err := walk(nextNode, depth+1, currentRow, nextPath, nextUsed); err != nil {
				return err
			}
		}
		return nil
	}
	used := make(map[domain.EntityID]struct{}, len(current.Relationships))
	for _, edge := range current.Relationships {
		used[edge.ID] = struct{}{}
	}
	if err := walk(currentNode, 0, values, current, used); err != nil {
		return nil, err
	}
	return matches, nil
}

func pathForRow(_ cypher.PatternElement, values row, fallback Path) Path {
	if path, ok := values[internalPathKey].(Path); ok {
		delete(values, internalPathKey)
		return path
	}
	return fallback
}

type adjacentEdge struct {
	edge *domain.Edge
	next domain.EntityID
}

func adjacentEdges(graph *memoryGraph, node domain.EntityID, direction cypher.Direction) []adjacentEdge {
	result := make([]adjacentEdge, 0, len(graph.outgoing[node])+len(graph.incoming[node]))
	if direction == cypher.Outgoing || direction == cypher.Undirected || direction == cypher.Bidirectional {
		for _, edge := range graph.outgoing[node] {
			result = append(result, adjacentEdge{edge: edge, next: edge.To})
		}
	}
	if direction == cypher.Incoming || direction == cypher.Undirected || direction == cypher.Bidirectional {
		for _, edge := range graph.incoming[node] {
			result = append(result, adjacentEdge{edge: edge, next: edge.From})
		}
	}
	sort.SliceStable(result, func(i, j int) bool {
		if result[i].edge.ID == result[j].edge.ID {
			return result[i].next < result[j].next
		}
		return result[i].edge.ID < result[j].edge.ID
	})
	// A self-loop occurs in both adjacency lists. An undirected MATCH must see
	// that relationship once, not manufacture two identical rows.
	unique := result[:0]
	for _, candidate := range result {
		if len(unique) > 0 {
			previous := unique[len(unique)-1]
			if previous.edge.ID == candidate.edge.ID && previous.next == candidate.next {
				continue
			}
		}
		unique = append(unique, candidate)
	}
	return unique
}

func candidateNodes(graph *memoryGraph, evaluator evaluator, values row, pattern cypher.NodePattern) ([]*domain.Node, error) {
	expected, err := expectedProperties(evaluator, values, pattern.Properties)
	if err != nil {
		return nil, err
	}
	if pattern.Variable.Name != "" {
		if bound, exists := values[pattern.Variable.Name]; exists {
			node, ok := bound.(*domain.Node)
			if !ok || node == nil {
				return nil, nil
			}
			if !nodeMatchesExpected(node, pattern.Labels, expected) {
				return nil, nil
			}
			return []*domain.Node{node}, nil
		}
	}
	var indexed map[domain.EntityID]*domain.Node
	choose := func(candidates map[domain.EntityID]*domain.Node) {
		if candidates != nil && (indexed == nil || len(candidates) < len(indexed)) {
			indexed = candidates
		}
	}
	for _, label := range pattern.Labels {
		choose(graph.labels[label.Name])
	}
	for key, value := range expected {
		if value == nil {
			return nil, nil
		}
		// Numeric and legacy temporal equality cross Go representations. Leave
		// those values to the residual matcher so representation differences
		// cannot produce a false-negative lookup.
		_, numeric, _ := compareNumbers(value, value)
		_, legacyTime := value.(time.Time)
		_, exactDateTime := value.(temporal.DateTime)
		_, legacyDuration := value.(time.Duration)
		_, exactDuration := value.(temporal.Duration)
		if numeric || legacyTime || exactDateTime || legacyDuration || exactDuration {
			continue
		}
		choose(graph.properties[key][valueKey(value)])
	}
	candidates := graph.nodePointers()
	if indexed != nil {
		candidates = make([]*domain.Node, 0, len(indexed))
		for _, node := range indexed {
			candidates = append(candidates, node)
		}
		sort.Slice(candidates, func(i, j int) bool { return candidates[i].ID < candidates[j].ID })
	}
	result := make([]*domain.Node, 0, len(candidates))
	for _, node := range candidates {
		if err := evaluator.ctx.Err(); err != nil {
			return nil, err
		}
		if nodeMatchesExpected(node, pattern.Labels, expected) {
			result = append(result, node)
		}
	}
	return result, nil
}

func expectedProperties(evaluator evaluator, values row, expression cypher.Expression) (map[string]any, error) {
	if expression == nil {
		return nil, nil
	}
	expected, err := evaluator.expression(expression, values)
	if err != nil {
		return nil, err
	}
	propertyMap, ok := expected.(map[string]any)
	if !ok {
		if typed, typedOK := expected.(domain.Properties); typedOK {
			propertyMap = typed
		} else {
			return nil, evalError(expression, "pattern properties must evaluate to a map")
		}
	}
	return propertyMap, nil
}

func nodeMatchesExpected(node *domain.Node, labels []cypher.Identifier, expected map[string]any) bool {
	for _, label := range labels {
		found := false
		for _, actual := range node.Labels {
			if actual == label.Name {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	for key, expectedValue := range expected {
		actual := property(node, key)
		if actual == nil || expectedValue == nil || !equalValues(actual, expectedValue) {
			return false
		}
	}
	return true
}

func nodePatternMatches(evaluator evaluator, values row, node *domain.Node, pattern cypher.NodePattern) (bool, error) {
	for _, label := range pattern.Labels {
		found := false
		for _, actual := range node.Labels {
			if actual == label.Name {
				found = true
				break
			}
		}
		if !found {
			return false, nil
		}
	}
	return propertiesMatch(evaluator, values, node, pattern.Properties)
}

func relationshipPatternMatches(evaluator evaluator, values row, edge *domain.Edge, pattern cypher.RelationshipPattern) (bool, error) {
	if len(pattern.Types) > 0 {
		found := false
		for _, edgeType := range pattern.Types {
			if edge.Type == edgeType.Name {
				found = true
				break
			}
		}
		if !found {
			return false, nil
		}
	}
	return propertiesMatch(evaluator, values, edge, pattern.Properties)
}

func propertiesMatch(evaluator evaluator, values row, entity any, expression cypher.Expression) (bool, error) {
	if expression == nil {
		return true, nil
	}
	expected, err := evaluator.expression(expression, values)
	if err != nil {
		return false, err
	}
	propertyMap, ok := expected.(map[string]any)
	if !ok {
		if typed, ok := expected.(domain.Properties); ok {
			propertyMap = typed
		} else {
			return false, evalError(expression, "pattern properties must evaluate to a map")
		}
	}
	for key, expectedValue := range propertyMap {
		actual := property(entity, key)
		if actual == nil || expectedValue == nil {
			return false, nil
		}
		if !equalValues(actual, expectedValue) {
			return false, nil
		}
	}
	return true, nil
}

func relationshipBounds(evaluator evaluator, values row, relationship cypher.RelationshipPattern, edgeCount int) (int64, int64, error) {
	if relationship.Length == nil {
		return 1, 1, nil
	}
	minimum, maximum := int64(1), int64(edgeCount)
	if relationship.Length.Lower != nil {
		value, err := evaluator.expression(relationship.Length.Lower, values)
		if err != nil {
			return 0, 0, err
		}
		minimum, _ = integer(value)
		if _, ok := integer(value); !ok || minimum < 0 {
			return 0, 0, evalError(relationship.Length.Lower, "path lower bound must be a non-negative integer")
		}
		if relationship.Length.Exact {
			maximum = minimum
		}
	}
	if relationship.Length.Upper != nil {
		value, err := evaluator.expression(relationship.Length.Upper, values)
		if err != nil {
			return 0, 0, err
		}
		maximum, _ = integer(value)
		if _, ok := integer(value); !ok || maximum < 0 {
			return 0, 0, evalError(relationship.Length.Upper, "path upper bound must be a non-negative integer")
		}
	}
	return minimum, maximum, nil
}

func bindValue(values row, name string, value any) bool {
	if name == "" {
		return true
	}
	current, exists := values[name]
	if !exists {
		values[name] = value
		return true
	}
	return sameBinding(current, value)
}

func sameBinding(left, right any) bool {
	switch left := left.(type) {
	case *domain.Node:
		right, ok := right.(*domain.Node)
		return ok && left != nil && right != nil && left.ID == right.ID
	case *domain.Edge:
		right, ok := right.(*domain.Edge)
		return ok && left != nil && right != nil && left.ID == right.ID
	case Path:
		right, ok := right.(Path)
		return ok && equalPath(left, right)
	default:
		return equalValues(left, right)
	}
}

func equalPath(left, right Path) bool {
	if len(left.Nodes) != len(right.Nodes) || len(left.Relationships) != len(right.Relationships) {
		return false
	}
	for index := range left.Nodes {
		if left.Nodes[index].ID != right.Nodes[index].ID {
			return false
		}
	}
	for index := range left.Relationships {
		if left.Relationships[index].ID != right.Relationships[index].ID {
			return false
		}
	}
	return true
}

func filterRows(evaluator evaluator, input []row, predicate cypher.Expression) ([]row, error) {
	result := make([]row, 0, len(input))
	for _, values := range input {
		if err := evaluator.ctx.Err(); err != nil {
			return nil, err
		}
		value, err := evaluator.expression(predicate, values)
		if err != nil {
			return nil, err
		}
		if value == true {
			result = append(result, values)
			continue
		}
		if value != nil && value != false {
			return nil, evalError(predicate, "WHERE expects a boolean, got %T", value)
		}
	}
	return result, nil
}

func patternVariables(patterns []cypher.PatternPart) []string {
	set := make(map[string]struct{})
	for _, pattern := range patterns {
		if pattern.Variable.Name != "" {
			set[pattern.Variable.Name] = struct{}{}
		}
		for _, node := range pattern.Element.Nodes {
			if node.Variable.Name != "" {
				set[node.Variable.Name] = struct{}{}
			}
		}
		for _, relationship := range pattern.Element.Relationships {
			if relationship.Variable.Name != "" {
				set[relationship.Variable.Name] = struct{}{}
			}
		}
	}
	result := make([]string, 0, len(set))
	for variable := range set {
		result = append(result, variable)
	}
	sort.Strings(result)
	return result
}
