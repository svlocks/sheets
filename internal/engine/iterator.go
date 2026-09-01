package engine

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/svlocks/sheets/internal/app"
	"github.com/svlocks/sheets/internal/cypher"
	"github.com/svlocks/sheets/internal/domain"
	"github.com/svlocks/sheets/internal/domain/temporal"
	"github.com/svlocks/sheets/internal/store"
)

// The store is deliberately page-oriented. These limits prevent a syntactically
// small, cold query from turning into an unbounded in-memory graph while still
// leaving common point and neighbourhood reads unconstrained in practice.
const (
	defaultIteratorEntities = 200_000
	defaultIteratorBytes    = 64 << 20
	defaultIteratorDepth    = 64
	iteratorPredicateTerms  = 256
)

type planOperator struct {
	Kind     string
	Detail   string
	Pushdown string
}

type physicalPlan struct {
	Operators []planOperator
	Fallback  string
}

func (p physicalPlan) clone() physicalPlan {
	p.Operators = append([]planOperator(nil), p.Operators...)
	return p
}

type iteratorBudget struct {
	entities int
	bytes    int
	depth    int
}

func (b *iteratorBudget) addNode(node domain.Node) error {
	return b.add(1, estimateNodeBytes(node))
}

func (b *iteratorBudget) addEdge(edge domain.Edge) error {
	return b.add(1, estimateEdgeBytes(edge))
}

func (b *iteratorBudget) add(entities, bytes int) error {
	b.entities += entities
	b.bytes += bytes
	if b.entities > defaultIteratorEntities {
		return fmt.Errorf("query working-set entity budget of %d exceeded", defaultIteratorEntities)
	}
	if b.bytes > defaultIteratorBytes {
		return fmt.Errorf("query working-set byte budget of %d exceeded", defaultIteratorBytes)
	}
	return nil
}

func estimateNodeBytes(node domain.Node) int {
	size := len(node.ID) + len(node.Body) + 96
	for _, label := range node.Labels {
		size += len(label)
	}
	return size + estimateProperties(node.Properties)
}

func estimateEdgeBytes(edge domain.Edge) int {
	size := len(edge.ID) + len(edge.From) + len(edge.To) + len(edge.Type) + 96
	return size + estimateProperties(edge.Properties)
}

func estimateProperties(properties domain.Properties) int {
	size := 0
	for key, value := range properties {
		size += len(key) + estimateValue(value)
	}
	return size
}

func estimateValue(value any) int {
	switch value := value.(type) {
	case nil:
		return 1
	case string:
		return len(value)
	case []byte:
		return len(value)
	case []any:
		size := 16
		for _, item := range value {
			size += estimateValue(item)
		}
		return size
	case map[string]any:
		size := 16
		for key, item := range value {
			size += len(key) + estimateValue(item)
		}
		return size
	case domain.Properties:
		return estimateProperties(value)
	default:
		return 16
	}
}

// loadReadWorkingSet builds a bounded, snapshot-bound graph neighbourhood for
// read pipelines. The existing matcher remains the semantic authority: this
// planner only pushes predicates that are exact and leaves every dynamic or
// expression predicate as a residual. Returning ok=false is an explicit,
// observable full-snapshot fallback rather than a partial graph result.
func (e *Engine) loadReadWorkingSet(
	ctx context.Context,
	snapshot domain.Snapshot,
	document *cypher.Document,
	params map[string]any,
) (*memoryGraph, physicalPlan, bool, error) {
	if document == nil || len(document.Statements) != 1 {
		return nil, physicalPlan{Fallback: "multi-statement read document"}, false, nil
	}
	query, ok := document.Statements[0].(*cypher.QueryStatement)
	if !ok || query.Explain || len(query.UnionBranches) != 0 {
		return nil, physicalPlan{Fallback: "EXPLAIN or UNION pipeline"}, false, nil
	}
	if queryContainsPatternExpression(query) {
		return nil, physicalPlan{Fallback: graphExpressionFallbackReason(query)}, false, nil
	}
	if reason := iteratorFallbackReason(query); reason != "" {
		return nil, physicalPlan{Fallback: reason}, false, nil
	}
	patterns := queryPatterns(query)
	if len(patterns) == 0 {
		return nil, physicalPlan{}, false, nil
	}
	view, err := e.store.View(ctx, snapshot)
	if err != nil {
		return nil, physicalPlan{}, true, err
	}
	working := &iteratorWorkingSet{
		ctx: ctx, view: view, params: params, budget: iteratorBudget{depth: defaultIteratorDepth},
		nodes: make(map[domain.EntityID]domain.Node), edges: make(map[domain.EntityID]domain.Edge),
		bindings: make(map[string]map[domain.EntityID]domain.Node),
	}
	plan := physicalPlan{}
	for _, pattern := range patterns {
		if err := working.pattern(pattern, &plan); err != nil {
			return nil, plan, true, err
		}
	}
	nodes := make([]domain.Node, 0, len(working.nodes))
	for _, node := range working.nodes {
		nodes = append(nodes, node)
	}
	edges := make([]domain.Edge, 0, len(working.edges))
	for _, edge := range working.edges {
		edges = append(edges, edge)
	}
	return newMemoryGraph(view.Revision(), nodes, edges, nil), plan, true, nil
}

func queryContainsPatternExpression(query *cypher.QueryStatement) bool {
	for _, clause := range query.Clauses {
		switch clause := clause.(type) {
		case *cypher.MatchClause:
			if expressionRequiresGraph(clause.Where) {
				return true
			}
		case *cypher.UnwindClause:
			if expressionRequiresGraph(clause.Expression) {
				return true
			}
		case *cypher.ProjectionClause:
			if projectionRequiresGraph(clause) {
				return true
			}
		case *cypher.CallClause:
			if clause.Subquery != nil || expressionRequiresGraph(clause.YieldWhere) {
				return true
			}
			for _, argument := range clause.Arguments {
				if expressionRequiresGraph(argument) {
					return true
				}
			}
		}
	}
	return false
}

func queryPatterns(query *cypher.QueryStatement) []cypher.PatternPart {
	var result []cypher.PatternPart
	for _, clause := range query.Clauses {
		match, ok := clause.(*cypher.MatchClause)
		if !ok {
			continue
		}
		result = append(result, match.Patterns...)
	}
	return result
}

func iteratorFallbackReason(query *cypher.QueryStatement) string {
	seenMatch := false
	scopeBoundary := false
	for _, clause := range query.Clauses {
		switch clause := clause.(type) {
		case *cypher.MatchClause:
			if seenMatch && scopeBoundary {
				return "MATCH after WITH requires scope-aware full-snapshot planning"
			}
			seenMatch = true
			for _, pattern := range clause.Patterns {
				for _, relationship := range pattern.Element.Relationships {
					if relationship.Length == nil {
						continue
					}
					if (relationship.Length.Lower != nil && !staticExpression(relationship.Length.Lower)) ||
						(relationship.Length.Upper != nil && !staticExpression(relationship.Length.Upper)) {
						return "dynamic variable-length relationship requires a full snapshot"
					}
					if !relationship.Length.Exact && relationship.Length.Upper == nil {
						return "unbounded variable-length relationship requires a full snapshot"
					}
				}
			}
		case *cypher.ProjectionClause:
			if clause.With && seenMatch {
				scopeBoundary = true
			}
		case *cypher.CallClause:
			if clause.Subquery == nil && procedureRequiresCompleteGraph(clause.Procedure.String()) {
				return "graph-wide procedure requires a full snapshot"
			}
		}
	}
	return ""
}

func procedureRequiresCompleteGraph(name string) bool {
	switch strings.ToLower(name) {
	case "db.labels", "db.relationshiptypes", "db.propertykeys", "sheets.nodes", "sheets.edges":
		return true
	default:
		return false
	}
}

type iteratorWorkingSet struct {
	ctx    context.Context
	view   *store.ReadView
	params map[string]any
	budget iteratorBudget
	nodes  map[domain.EntityID]domain.Node
	edges  map[domain.EntityID]domain.Edge
	// bindings are conservative candidate sets for variables introduced by an
	// earlier pattern. They let a later MATCH (b)-->(c) start from the b values
	// already fetched instead of silently falling back to a full node scan.
	// Row-level equality is still enforced by the matcher.
	bindings map[string]map[domain.EntityID]domain.Node
}

func (w *iteratorWorkingSet) pattern(pattern cypher.PatternPart, plan *physicalPlan) error {
	if err := w.ctx.Err(); err != nil {
		return err
	}
	if len(pattern.Element.Nodes) == 0 {
		return nil
	}
	first := pattern.Element.Nodes[0]
	predicates := buildNodeScanPlan(first, w.params)
	var frontier []domain.Node
	var err error
	if bound := w.bound(first.Variable.Name); len(bound) > 0 {
		if !predicates.empty {
			frontier = filterNodes(bound, predicates.base)
		}
		plan.Operators = append(plan.Operators, planOperator{Kind: "BoundNodeInput", Detail: "root node", Pushdown: predicates.pushdown})
	} else {
		for _, predicate := range predicates.alternatives {
			var candidates []domain.Node
			candidates, err = w.scanNodes(predicate, &physicalPlan{}, "root node", predicates.pushdown)
			if err != nil {
				return err
			}
			frontier = append(frontier, candidates...)
		}
		frontier = uniqueNodes(frontier)
		plan.Operators = append(plan.Operators, planOperator{Kind: "NodeIndexScan", Detail: "root node", Pushdown: predicates.pushdown})
	}
	w.bind(first.Variable.Name, frontier)
	for index, relationship := range pattern.Element.Relationships {
		target := pattern.Element.Nodes[index+1]
		var next []domain.Node
		if relationship.Length == nil {
			next, err = w.expandOne(frontier, relationship, target, plan)
		} else {
			next, err = w.expandVariable(frontier, relationship, target, plan)
		}
		if err != nil {
			return err
		}
		frontier = next
		w.bind(target.Variable.Name, frontier)
		if len(frontier) == 0 {
			break
		}
	}
	return nil
}

func (w *iteratorWorkingSet) bound(name string) []domain.Node {
	if name == "" {
		return nil
	}
	values := w.bindings[name]
	if len(values) == 0 {
		return nil
	}
	result := make([]domain.Node, 0, len(values))
	for _, node := range values {
		result = append(result, node)
	}
	return uniqueNodes(result)
}

func (w *iteratorWorkingSet) bind(name string, nodes []domain.Node) {
	if name == "" || len(nodes) == 0 {
		return
	}
	values := w.bindings[name]
	if values == nil {
		values = make(map[domain.EntityID]domain.Node, len(nodes))
		w.bindings[name] = values
	}
	for _, node := range nodes {
		values[node.ID] = node
	}
}

func staticNodePredicate(pattern cypher.NodePattern, params map[string]any) (store.NodePredicate, string) {
	plan := buildNodeScanPlan(pattern, params)
	return plan.base, plan.pushdown
}

func buildNodePredicate(pattern cypher.NodePattern, params map[string]any) (store.NodePredicate, string, bool) {
	plan := buildNodeScanPlan(pattern, params)
	return plan.base, plan.pushdown, plan.complete && !plan.empty && len(plan.alternatives) == 1
}

type nodeScanPlan struct {
	base         store.NodePredicate
	alternatives []store.NodePredicate
	pushdown     string
	complete     bool
	empty        bool
}

func buildNodeScanPlan(pattern cypher.NodePattern, params map[string]any) nodeScanPlan {
	labels := make([]string, len(pattern.Labels))
	for index, label := range pattern.Labels {
		labels[index] = label.Name
	}
	complete := true
	residual := make([]string, 0)
	if len(labels) > iteratorPredicateTerms {
		labels = labels[:iteratorPredicateTerms]
		complete = false
		residual = append(residual, "labels")
	}
	properties, known := staticPatternProperties(pattern.Properties, params)
	propertyPlan := buildPropertyScanPlan(properties, known, "body")
	complete = complete && propertyPlan.complete
	residual = append(residual, propertyPlan.residual...)
	base := store.NodePredicate{AllLabels: labels, Properties: propertyPlan.base}
	result := nodeScanPlan{base: base, complete: complete, empty: propertyPlan.empty}
	if !result.empty {
		result.alternatives = make([]store.NodePredicate, len(propertyPlan.alternatives))
		for index, properties := range propertyPlan.alternatives {
			result.alternatives[index] = store.NodePredicate{AllLabels: labels, Properties: properties}
		}
	}
	pushed := describeNodePushdown(labels, propertyPlan.base)
	if propertyPlan.numericKey != "" {
		pushed = appendPushdown(pushed, fmt.Sprintf("numeric-property:%s[%d variants]", propertyPlan.numericKey, len(propertyPlan.alternatives)))
	}
	result.pushdown = describePushdown(pushed, residual)
	return result
}

func staticEdgePredicate(pattern cypher.RelationshipPattern, params map[string]any) (store.EdgePredicate, string) {
	plan := buildEdgeScanPlan(pattern, params)
	return plan.base, plan.pushdown
}

type edgeScanPlan struct {
	base         store.EdgePredicate
	alternatives []store.EdgePredicate
	pushdown     string
	empty        bool
}

func buildEdgeScanPlan(pattern cypher.RelationshipPattern, params map[string]any) edgeScanPlan {
	types := make([]string, len(pattern.Types))
	for index, edgeType := range pattern.Types {
		types[index] = edgeType.Name
	}
	residual := make([]string, 0)
	if len(types) > iteratorPredicateTerms {
		// Relationship types are alternatives. Pushing only a subset would be a
		// false negative, so keep the complete type test in the matcher.
		types = nil
		residual = append(residual, "types")
	}
	properties, known := staticPatternProperties(pattern.Properties, params)
	propertyPlan := buildPropertyScanPlan(properties, known, "position")
	residual = append(residual, propertyPlan.residual...)
	base := store.EdgePredicate{Types: types, Properties: propertyPlan.base}
	result := edgeScanPlan{base: base, empty: propertyPlan.empty}
	if !result.empty {
		result.alternatives = make([]store.EdgePredicate, len(propertyPlan.alternatives))
		for index, properties := range propertyPlan.alternatives {
			result.alternatives[index] = store.EdgePredicate{Types: types, Properties: properties}
		}
	}
	pushed := describeEdgePushdown(types, propertyPlan.base)
	if propertyPlan.numericKey != "" {
		pushed = appendPushdown(pushed, fmt.Sprintf("numeric-property:%s[%d variants]", propertyPlan.numericKey, len(propertyPlan.alternatives)))
	}
	result.pushdown = describePushdown(pushed, residual)
	return result
}

type propertyScanPlan struct {
	base         domain.Properties
	alternatives []domain.Properties
	residual     []string
	numericKey   string
	complete     bool
	empty        bool
}

func buildPropertyScanPlan(properties domain.Properties, known bool, special string) propertyScanPlan {
	if !known {
		return propertyScanPlan{alternatives: []domain.Properties{nil}, residual: []string{"dynamic properties"}}
	}
	if len(properties) == 0 {
		return propertyScanPlan{alternatives: []domain.Properties{nil}, complete: true}
	}
	keys := make([]string, 0, len(properties))
	for key := range properties {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	result := propertyScanPlan{base: make(domain.Properties), complete: true}
	var numericValues []any
	for _, key := range keys {
		value := properties[key]
		if key == special {
			continue
		}
		if _, _, numeric := number(value); !numeric {
			continue
		}
		values, empty := numericIndexCandidates(value)
		result.numericKey = key
		numericValues = values
		if empty {
			result.empty = true
			result.alternatives = nil
			result.base = nil
			return result
		}
		break
	}
	propertyLimit := iteratorPredicateTerms
	if result.numericKey != "" {
		propertyLimit--
	}
	for _, key := range keys {
		value := properties[key]
		if key == result.numericKey {
			continue
		}
		if key == special || !storeExactPropertyValue(value) || len(result.base) == propertyLimit {
			result.complete = false
			result.residual = append(result.residual, "property:"+key)
			continue
		}
		result.base[key] = value
	}
	if len(result.base) == 0 {
		result.base = nil
	}
	if result.numericKey == "" {
		result.alternatives = []domain.Properties{result.base}
		return result
	}
	result.alternatives = make([]domain.Properties, 0, len(numericValues))
	for _, value := range numericValues {
		candidate := cloneProperties(result.base)
		candidate[result.numericKey] = value
		result.alternatives = append(result.alternatives, candidate)
	}
	return result
}

func numericIndexCandidates(value any) ([]any, bool) {
	numberValue, integerInput, ok := number(value)
	if !ok {
		return nil, false
	}
	// NaN is unequal to every value, including itself. Avoid a broad scan and
	// let the planner represent the conjunction as an empty candidate set.
	if math.IsNaN(numberValue) {
		return nil, true
	}
	result := make([]any, 0, 3)
	seen := make(map[string]struct{}, 3)
	addInteger := func(candidate int64) {
		key := fmt.Sprintf("i:%d", candidate)
		if _, exists := seen[key]; !exists {
			seen[key] = struct{}{}
			result = append(result, candidate)
		}
	}
	addFloat := func(candidate float64) {
		key := fmt.Sprintf("f:%016x", math.Float64bits(candidate))
		if _, exists := seen[key]; !exists {
			seen[key] = struct{}{}
			result = append(result, candidate)
		}
	}
	addEquivalentFloats := func(candidate float64) {
		if candidate == 0 {
			addFloat(0)
			addFloat(math.Copysign(0, -1))
			return
		}
		addFloat(candidate)
	}
	if integerInput {
		integerValue, integerOK := integer(value)
		if !integerOK {
			return nil, false
		}
		addInteger(integerValue)
		floatValue := float64(integerValue)
		if comparison, numeric, unordered := compareNumbers(integerValue, floatValue); numeric && !unordered && comparison == 0 {
			addEquivalentFloats(floatValue)
		}
		return result, false
	}

	addEquivalentFloats(numberValue)
	if !math.IsInf(numberValue, 0) && numberValue >= -math.Exp2(63) && numberValue < math.Exp2(63) && math.Trunc(numberValue) == numberValue {
		integerValue := int64(numberValue)
		if comparison, numeric, unordered := compareNumbers(integerValue, numberValue); numeric && !unordered && comparison == 0 {
			addInteger(integerValue)
		}
	}
	return result, false
}

func cloneProperties(properties domain.Properties) domain.Properties {
	result := make(domain.Properties, len(properties)+1)
	for key, value := range properties {
		result[key] = value
	}
	return result
}

func appendPushdown(current, next string) string {
	if current == "" {
		return next
	}
	return current + "," + next
}

func storeExactPropertyValue(value any) bool {
	switch value.(type) {
	case bool, string, []byte, temporal.Date, temporal.LocalTime, temporal.Time, temporal.LocalDateTime:
		return true
	default:
		// Numeric and legacy temporal equality cross Go representations, while
		// the store index is representation-exact. Lists and maps can contain
		// those values, so leave them to the residual matcher too.
		return false
	}
}

func describePushdown(pushed string, residual []string) string {
	if len(residual) == 0 {
		return pushed
	}
	if pushed == "" {
		return "residual:" + strings.Join(residual, ",")
	}
	return pushed + "; residual:" + strings.Join(residual, ",")
}

func describeNodePushdown(labels []string, properties domain.Properties) string {
	parts := make([]string, 0, len(labels)+len(properties))
	for _, label := range labels {
		parts = append(parts, "label:"+label)
	}
	for key := range properties {
		parts = append(parts, "property:"+key)
	}
	sort.Strings(parts)
	return strings.Join(parts, ",")
}

func describeEdgePushdown(types []string, properties domain.Properties) string {
	parts := make([]string, 0, len(types)+len(properties))
	for _, edgeType := range types {
		parts = append(parts, "type:"+edgeType)
	}
	for key := range properties {
		parts = append(parts, "property:"+key)
	}
	sort.Strings(parts)
	return strings.Join(parts, ",")
}

func (w *iteratorWorkingSet) scanNodes(predicate store.NodePredicate, plan *physicalPlan, detail, pushdown string) ([]domain.Node, error) {
	result := make([]domain.Node, 0)
	puller := newNodePuller(w.ctx, w.view, predicate)
	for {
		node, ok, err := puller.Next()
		if err != nil {
			return nil, err
		}
		if !ok {
			break
		}
		if _, exists := w.nodes[node.ID]; !exists {
			if err := w.budget.addNode(node); err != nil {
				return nil, err
			}
			w.nodes[node.ID] = node
		}
		result = append(result, node)
	}
	plan.Operators = append(plan.Operators, planOperator{Kind: "NodeIndexScan", Detail: detail, Pushdown: pushdown})
	return uniqueNodes(result), nil
}

func (w *iteratorWorkingSet) expandOne(frontier []domain.Node, relationship cypher.RelationshipPattern, target cypher.NodePattern, plan *physicalPlan) ([]domain.Node, error) {
	edges, err := w.scanAdjacent(frontier, relationship, plan)
	if err != nil {
		return nil, err
	}
	current := nodeIDSet(frontier)
	ids := make([]domain.EntityID, 0, len(edges))
	for _, edge := range edges {
		ids = append(ids, edgeTargets(edge, relationship.Direction, current)...)
	}
	targets, err := w.fetchNodes(ids)
	if err != nil {
		return nil, err
	}
	predicate, _ := staticNodePredicate(target, w.params)
	return filterNodes(targets, predicate), nil
}

func (w *iteratorWorkingSet) expandVariable(frontier []domain.Node, relationship cypher.RelationshipPattern, target cypher.NodePattern, plan *physicalPlan) ([]domain.Node, error) {
	minimum, maximum, err := staticRelationshipBounds(relationship, w.params, w.budget.depth)
	if err != nil {
		return nil, err
	}
	plan.Operators = append(plan.Operators, planOperator{Kind: "VariableLengthExpand", Detail: fmt.Sprintf("depth %d..%d", minimum, maximum), Pushdown: describeEdgePattern(relationship)})
	seen := make(map[domain.EntityID]domain.Node)
	current := uniqueNodes(frontier)
	if minimum == 0 {
		predicate, _ := staticNodePredicate(target, w.params)
		for _, node := range filterNodes(current, predicate) {
			seen[node.ID] = node
		}
	}
	for depth := 1; depth <= maximum && len(current) > 0; depth++ {
		if err := w.ctx.Err(); err != nil {
			return nil, err
		}
		edges, err := w.scanAdjacent(current, relationship, plan)
		if err != nil {
			return nil, err
		}
		currentIDs := nodeIDSet(current)
		ids := make([]domain.EntityID, 0, len(edges))
		for _, edge := range edges {
			ids = append(ids, edgeTargets(edge, relationship.Direction, currentIDs)...)
		}
		next, err := w.fetchNodes(ids)
		if err != nil {
			return nil, err
		}
		if depth >= minimum {
			predicate, _ := staticNodePredicate(target, w.params)
			for _, node := range filterNodes(next, predicate) {
				seen[node.ID] = node
			}
		}
		current = uniqueNodes(next)
	}
	result := make([]domain.Node, 0, len(seen))
	for _, node := range seen {
		result = append(result, node)
	}
	return uniqueNodes(result), nil
}

func (w *iteratorWorkingSet) scanAdjacent(frontier []domain.Node, relationship cypher.RelationshipPattern, plan *physicalPlan) ([]domain.Edge, error) {
	if len(frontier) == 0 {
		return nil, nil
	}
	ids := make([]domain.EntityID, len(frontier))
	for index, node := range frontier {
		ids[index] = node.ID
	}
	predicates := buildEdgeScanPlan(relationship, w.params)
	var result []domain.Edge
	for _, predicate := range predicates.alternatives {
		switch relationship.Direction {
		case cypher.Outgoing:
			edges, err := w.scanEdgesForIDs(predicate, ids, true, "outgoing relationship", predicates.pushdown)
			if err != nil {
				return nil, err
			}
			result = append(result, edges...)
		case cypher.Incoming:
			edges, err := w.scanEdgesForIDs(predicate, ids, false, "incoming relationship", predicates.pushdown)
			if err != nil {
				return nil, err
			}
			result = append(result, edges...)
		case cypher.Undirected:
			edges, err := w.scanEdgesForIDs(predicate, ids, true, "undirected outgoing relationship", predicates.pushdown)
			if err != nil {
				return nil, err
			}
			result = append(result, edges...)
			edges, err = w.scanEdgesForIDs(predicate, ids, false, "undirected incoming relationship", predicates.pushdown)
			if err != nil {
				return nil, err
			}
			result = append(result, edges...)
		}
	}
	plan.Operators = append(plan.Operators, planOperator{Kind: "EdgeIndexScan", Detail: "adjacent relationship", Pushdown: predicates.pushdown})
	return uniqueEdges(result), nil
}

// Store predicates accept a bounded number of endpoint IDs. Split a broad
// frontier deterministically instead of turning a legitimate traversal into
// an implementation-limit error once it passes 100K nodes.
const iteratorEndpointChunk = 50_000

func (w *iteratorWorkingSet) scanEdgesForIDs(predicate store.EdgePredicate, ids []domain.EntityID, from bool, detail, pushdown string) ([]domain.Edge, error) {
	result := make([]domain.Edge, 0)
	for start := 0; start < len(ids); start += iteratorEndpointChunk {
		if err := w.ctx.Err(); err != nil {
			return nil, err
		}
		end := start + iteratorEndpointChunk
		if end > len(ids) {
			end = len(ids)
		}
		part := predicate
		if from {
			part.FromIDs = ids[start:end]
		} else {
			part.ToIDs = ids[start:end]
		}
		edges, err := w.scanEdges(part, detail, pushdown)
		if err != nil {
			return nil, err
		}
		result = append(result, edges...)
	}
	return uniqueEdges(result), nil
}

func (w *iteratorWorkingSet) scanEdges(predicate store.EdgePredicate, detail, pushdown string) ([]domain.Edge, error) {
	result := make([]domain.Edge, 0)
	puller := newEdgePuller(w.ctx, w.view, predicate)
	for {
		edge, ok, err := puller.Next()
		if err != nil {
			return nil, err
		}
		if !ok {
			break
		}
		if _, exists := w.edges[edge.ID]; !exists {
			if err := w.budget.addEdge(edge); err != nil {
				return nil, err
			}
			w.edges[edge.ID] = edge
		}
		result = append(result, edge)
	}
	_ = detail // captured by the parent EdgeIndexScan operator.
	return uniqueEdges(result), nil
}

// nodePuller and edgePuller make the store's opaque keyset pages look like
// pull iterators. Planning and expansion therefore consume one exact ReadView
// incrementally; no code path first asks the store for an all-graph slice.
type nodePuller struct {
	ctx       context.Context
	view      *store.ReadView
	predicate store.NodePredicate
	page      domain.Page
	buffer    []domain.Node
	next      string
	done      bool
}

func newNodePuller(ctx context.Context, view *store.ReadView, predicate store.NodePredicate) *nodePuller {
	return &nodePuller{ctx: ctx, view: view, predicate: predicate, page: domain.Page{Limit: graphPageSize}}
}

func (p *nodePuller) Next() (domain.Node, bool, error) {
	for len(p.buffer) == 0 {
		if p.done {
			return domain.Node{}, false, nil
		}
		if err := p.ctx.Err(); err != nil {
			return domain.Node{}, false, err
		}
		p.page.After = p.next
		nodes, info, err := p.view.ScanNodes(p.ctx, p.predicate, p.page)
		if err != nil {
			return domain.Node{}, false, err
		}
		p.buffer = nodes
		p.next = info.Next
		p.done = p.next == ""
	}
	node := p.buffer[0]
	p.buffer = p.buffer[1:]
	return node, true, nil
}

type edgePuller struct {
	ctx       context.Context
	view      *store.ReadView
	predicate store.EdgePredicate
	page      domain.Page
	buffer    []domain.Edge
	next      string
	done      bool
}

func newEdgePuller(ctx context.Context, view *store.ReadView, predicate store.EdgePredicate) *edgePuller {
	return &edgePuller{ctx: ctx, view: view, predicate: predicate, page: domain.Page{Limit: graphPageSize}}
}

func (p *edgePuller) Next() (domain.Edge, bool, error) {
	for len(p.buffer) == 0 {
		if p.done {
			return domain.Edge{}, false, nil
		}
		if err := p.ctx.Err(); err != nil {
			return domain.Edge{}, false, err
		}
		p.page.After = p.next
		edges, info, err := p.view.ScanEdges(p.ctx, p.predicate, p.page)
		if err != nil {
			return domain.Edge{}, false, err
		}
		p.buffer = edges
		p.next = info.Next
		p.done = p.next == ""
	}
	edge := p.buffer[0]
	p.buffer = p.buffer[1:]
	return edge, true, nil
}

func (w *iteratorWorkingSet) fetchNodes(ids []domain.EntityID) ([]domain.Node, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	ids = uniqueEntityIDs(ids)
	nodes := make([]domain.Node, 0, len(ids))
	for start := 0; start < len(ids); start += iteratorEndpointChunk {
		if err := w.ctx.Err(); err != nil {
			return nil, err
		}
		end := start + iteratorEndpointChunk
		if end > len(ids) {
			end = len(ids)
		}
		part, err := w.view.GetNodes(w.ctx, ids[start:end])
		if err != nil {
			return nil, err
		}
		nodes = append(nodes, part...)
	}
	for _, node := range nodes {
		if _, exists := w.nodes[node.ID]; !exists {
			if err := w.budget.addNode(node); err != nil {
				return nil, err
			}
			w.nodes[node.ID] = node
		}
	}
	return nodes, nil
}

func uniqueEntityIDs(ids []domain.EntityID) []domain.EntityID {
	set := make(map[domain.EntityID]struct{}, len(ids))
	for _, id := range ids {
		set[id] = struct{}{}
	}
	result := make([]domain.EntityID, 0, len(set))
	for id := range set {
		result = append(result, id)
	}
	sort.Slice(result, func(i, j int) bool { return result[i] < result[j] })
	return result
}

func staticRelationshipBounds(pattern cypher.RelationshipPattern, params map[string]any, maxDepth int) (int, int, error) {
	if pattern.Length == nil {
		return 1, 1, nil
	}
	if (pattern.Length.Lower != nil && !staticExpression(pattern.Length.Lower)) ||
		(pattern.Length.Upper != nil && !staticExpression(pattern.Length.Upper)) {
		return 0, 0, fmt.Errorf("query iterator cannot safely bound a dynamic variable-length relationship")
	}
	minimum, maximum := 1, maxDepth
	evaluator := newEvaluator(params)
	if pattern.Length.Lower != nil {
		value, err := evaluator.expression(pattern.Length.Lower, row{})
		if err != nil {
			return 0, 0, err
		}
		integer, ok := integer(value)
		if !ok || integer < 0 {
			return 0, 0, evalError(pattern.Length.Lower, "path lower bound must be a non-negative integer")
		}
		if integer > int64(maxDepth) {
			return 0, 0, fmt.Errorf("query path depth budget of %d exceeded", maxDepth)
		}
		minimum = int(integer)
		if pattern.Length.Exact {
			maximum = minimum
		}
	}
	if pattern.Length.Upper != nil {
		value, err := evaluator.expression(pattern.Length.Upper, row{})
		if err != nil {
			return 0, 0, err
		}
		integer, ok := integer(value)
		if !ok || integer < 0 {
			return 0, 0, evalError(pattern.Length.Upper, "path upper bound must be a non-negative integer")
		}
		if integer > int64(maxDepth) {
			return 0, 0, fmt.Errorf("query path depth budget of %d exceeded", maxDepth)
		}
		maximum = int(integer)
	}
	if maximum > maxDepth {
		return 0, 0, fmt.Errorf("query path depth budget of %d exceeded", maxDepth)
	}
	return minimum, maximum, nil
}

func nodeIDSet(nodes []domain.Node) map[domain.EntityID]struct{} {
	result := make(map[domain.EntityID]struct{}, len(nodes))
	for _, node := range nodes {
		result[node.ID] = struct{}{}
	}
	return result
}

func edgeTargets(edge domain.Edge, direction cypher.Direction, current map[domain.EntityID]struct{}) []domain.EntityID {
	var result []domain.EntityID
	if direction == cypher.Outgoing || direction == cypher.Undirected {
		if _, ok := current[edge.From]; ok {
			result = append(result, edge.To)
		}
	}
	if direction == cypher.Incoming || direction == cypher.Undirected {
		if _, ok := current[edge.To]; ok {
			result = append(result, edge.From)
		}
	}
	return result
}

func filterNodes(nodes []domain.Node, predicate store.NodePredicate) []domain.Node {
	result := make([]domain.Node, 0, len(nodes))
	for _, node := range nodes {
		if nodeMatchesPredicate(node, predicate) {
			result = append(result, node)
		}
	}
	return uniqueNodes(result)
}

func nodeMatchesPredicate(node domain.Node, predicate store.NodePredicate) bool {
	for _, label := range predicate.AllLabels {
		found := false
		for _, actual := range node.Labels {
			if actual == label {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	for key, wanted := range predicate.Properties {
		actual, exists := node.Properties[key]
		if !exists || !equalValues(actual, wanted) {
			return false
		}
	}
	return true
}

func uniqueNodes(nodes []domain.Node) []domain.Node {
	byID := make(map[domain.EntityID]domain.Node, len(nodes))
	for _, node := range nodes {
		byID[node.ID] = node
	}
	ids := make([]domain.EntityID, 0, len(byID))
	for id := range byID {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	result := make([]domain.Node, len(ids))
	for index, id := range ids {
		result[index] = byID[id]
	}
	return result
}

func uniqueEdges(edges []domain.Edge) []domain.Edge {
	byID := make(map[domain.EntityID]domain.Edge, len(edges))
	for _, edge := range edges {
		byID[edge.ID] = edge
	}
	ids := make([]domain.EntityID, 0, len(byID))
	for id := range byID {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	result := make([]domain.Edge, len(ids))
	for index, id := range ids {
		result[index] = byID[id]
	}
	return result
}

func describeEdgePattern(pattern cypher.RelationshipPattern) string {
	_, detail := staticEdgePredicate(pattern, nil)
	return detail
}

func logicalReadPlan(query *cypher.QueryStatement) physicalPlan {
	if query == nil {
		return physicalPlan{Fallback: "nil query"}
	}
	if queryContainsPatternExpression(query) {
		return physicalPlan{Fallback: graphExpressionFallbackReason(query)}
	}
	if reason := iteratorFallbackReason(query); reason != "" {
		return physicalPlan{Fallback: reason}
	}
	if directCountShape(query) {
		match := query.Clauses[0].(*cypher.MatchClause)
		predicatePlan := buildNodeScanPlan(match.Patterns[0].Element.Nodes[0], nil)
		if predicatePlan.complete {
			return physicalPlan{Operators: []planOperator{{Kind: "NodeIndexCount", Detail: "count node match", Pushdown: predicatePlan.pushdown}}}
		}
	}
	plan := physicalPlan{}
	for _, pattern := range queryPatterns(query) {
		if len(pattern.Element.Nodes) == 0 {
			continue
		}
		predicate, pushdown := staticNodePredicate(pattern.Element.Nodes[0], nil)
		_ = predicate
		plan.Operators = append(plan.Operators, planOperator{Kind: "NodeIndexScan", Detail: "root node", Pushdown: pushdown})
		for _, relationship := range pattern.Element.Relationships {
			kind := "EdgeIndexScan"
			if relationship.Length != nil {
				kind = "VariableLengthExpand"
			}
			_, pushdown := staticEdgePredicate(relationship, nil)
			plan.Operators = append(plan.Operators, planOperator{Kind: kind, Detail: "relationship", Pushdown: pushdown})
		}
	}
	if len(plan.Operators) == 0 {
		plan.Fallback = "non-pattern query"
	}
	if len(query.UnionBranches) != 0 {
		plan.Fallback = "UNION pipeline"
	}
	return plan
}

func graphExpressionFallbackReason(query *cypher.QueryStatement) string {
	reason := "graph expression requires a full snapshot"
	var visit func(cypher.Expression)
	visit = func(expression cypher.Expression) {
		if expression == nil || reason != "graph expression requires a full snapshot" {
			return
		}
		switch expression := expression.(type) {
		case *cypher.ExistsSubquery:
			reason = "EXISTS subquery requires a full snapshot"
		case *cypher.PatternExpression:
			reason = "pattern expression requires a full snapshot"
		case *cypher.FunctionInvocation:
			name := strings.ToLower(expression.Name.String())
			if (name == "shortestpath" || name == "allshortestpaths") && len(expression.Arguments) == 1 {
				if pattern, ok := expression.Arguments[0].(*cypher.PatternExpression); ok {
					if shortestPatternUsesBFS(pattern.Pattern) {
						reason = "shortestPath expression requires a full-snapshot BFS"
					} else {
						reason = "shortestPath expression uses the generic trail fallback"
					}
					return
				}
			}
			for _, argument := range expression.Arguments {
				visit(argument)
			}
		case *cypher.UnaryExpression:
			visit(expression.Expression)
		case *cypher.BinaryExpression:
			visit(expression.Left)
			visit(expression.Right)
		case *cypher.IsNullExpression:
			visit(expression.Expression)
		case *cypher.PropertyExpression:
			visit(expression.Expression)
		case *cypher.LabelExpression:
			visit(expression.Expression)
		case *cypher.IndexExpression:
			visit(expression.Expression)
			visit(expression.Index)
		case *cypher.SliceExpression:
			visit(expression.Expression)
			visit(expression.Start)
			visit(expression.End)
		case *cypher.ListLiteral:
			for _, item := range expression.Elements {
				visit(item)
			}
		case *cypher.MapLiteral:
			for _, entry := range expression.Entries {
				visit(entry.Value)
			}
		case *cypher.CaseExpression:
			visit(expression.Operand)
			for _, alternative := range expression.Alternatives {
				visit(alternative.When)
				visit(alternative.Then)
			}
			visit(expression.Else)
		case *cypher.ListComprehension:
			visit(expression.List)
			visit(expression.Where)
			visit(expression.Projection)
		case *cypher.ListPredicate:
			visit(expression.List)
			visit(expression.Where)
		case *cypher.ReduceExpression:
			visit(expression.Initial)
			visit(expression.List)
			visit(expression.Expression)
		}
	}
	for _, clause := range query.Clauses {
		switch clause := clause.(type) {
		case *cypher.MatchClause:
			visit(clause.Where)
		case *cypher.UnwindClause:
			visit(clause.Expression)
		case *cypher.ProjectionClause:
			for _, item := range clause.Items {
				visit(item.Expression)
			}
			visit(clause.Where)
			for _, item := range clause.OrderBy {
				visit(item.Expression)
			}
			visit(clause.Skip)
			visit(clause.Limit)
		case *cypher.CallClause:
			if clause.Subquery != nil {
				reason = "CALL subquery requires a full snapshot"
				return reason
			}
			for _, argument := range clause.Arguments {
				visit(argument)
			}
			visit(clause.YieldWhere)
		}
	}
	return reason
}

func shortestPatternUsesBFS(pattern cypher.PatternElement) bool {
	if len(pattern.Nodes) != 2 || len(pattern.Relationships) != 1 {
		return false
	}
	relationship := pattern.Relationships[0]
	if relationship.Length != nil && relationship.Variable.Name != "" {
		// A named variable-length relationship can be correlated to an outer
		// binding. Runtime must then retain the generic trail matcher, so EXPLAIN
		// must not promise the BFS operator for this shape.
		return false
	}
	length := relationship.Length
	if length == nil || length.Lower == nil {
		return true
	}
	literal, ok := length.Lower.(*cypher.Literal)
	if !ok {
		return false
	}
	value, ok := integer(literal.Value)
	return ok && value <= 1
}

// executeDirectCount avoids constructing even a bounded working set for the
// common `MATCH (n:Label {key:value}) RETURN count(n)` shape. More elaborate
// aggregate pipelines intentionally use the iterator plus residual evaluator.
func (e *Engine) executeDirectCount(
	ctx context.Context,
	snapshot domain.Snapshot,
	document *cypher.Document,
	params map[string]any,
) (app.BatchResult, physicalPlan, bool, error) {
	if document == nil || len(document.Statements) != 1 {
		return app.BatchResult{}, physicalPlan{}, false, nil
	}
	query, ok := document.Statements[0].(*cypher.QueryStatement)
	if !ok || query.Explain || len(query.UnionBranches) != 0 || len(query.Clauses) != 2 {
		return app.BatchResult{}, physicalPlan{}, false, nil
	}
	if !directCountShape(query) {
		return app.BatchResult{}, physicalPlan{}, false, nil
	}
	match := query.Clauses[0].(*cypher.MatchClause)
	pattern := match.Patterns[0]
	projection := query.Clauses[1].(*cypher.ProjectionClause)
	predicatePlan := buildNodeScanPlan(pattern.Element.Nodes[0], params)
	if !predicatePlan.complete {
		return app.BatchResult{}, physicalPlan{}, false, nil
	}
	view, err := e.store.View(ctx, snapshot)
	if err != nil {
		return app.BatchResult{}, physicalPlan{}, true, err
	}
	var count uint64
	for _, predicate := range predicatePlan.alternatives {
		candidateCount, countErr := view.CountNodes(ctx, predicate)
		if countErr != nil {
			return app.BatchResult{}, physicalPlan{}, true, countErr
		}
		count += candidateCount
	}
	column := projection.Items[0].Alias.Name
	if column == "" {
		span := projection.Items[0].Expression.Location()
		column = strings.TrimSpace(document.Source[span.Start.Offset:span.End.Offset])
	}
	plan := physicalPlan{Operators: []planOperator{{Kind: "NodeIndexCount", Detail: "count node match", Pushdown: predicatePlan.pushdown}}}
	return app.BatchResult{Results: []app.Result{{Columns: []string{column}, Rows: [][]any{{int64(count)}}}}}, plan, true, nil
}

func directCountShape(query *cypher.QueryStatement) bool {
	if query == nil || query.Explain || len(query.UnionBranches) != 0 || len(query.Clauses) != 2 {
		return false
	}
	match, ok := query.Clauses[0].(*cypher.MatchClause)
	if !ok || match.Optional || match.Where != nil || len(match.Patterns) != 1 {
		return false
	}
	pattern := match.Patterns[0]
	if len(pattern.Element.Nodes) != 1 || len(pattern.Element.Relationships) != 0 {
		return false
	}
	projection, ok := query.Clauses[1].(*cypher.ProjectionClause)
	if !ok || projection.With || projection.Distinct || projection.Where != nil || len(projection.OrderBy) != 0 || projection.Skip != nil || projection.Limit != nil || len(projection.Items) != 1 || projection.Items[0].Star {
		return false
	}
	invocation, ok := projection.Items[0].Expression.(*cypher.FunctionInvocation)
	return ok && strings.EqualFold(invocation.Name.String(), "count") && !invocation.Distinct &&
		(invocation.Star || (len(invocation.Arguments) == 1 && isPatternVariable(invocation.Arguments[0], pattern.Element.Nodes[0].Variable.Name)))
}

func isPatternVariable(expression cypher.Expression, name string) bool {
	variable, ok := expression.(*cypher.Variable)
	return ok && variable.Name.Name == name
}
