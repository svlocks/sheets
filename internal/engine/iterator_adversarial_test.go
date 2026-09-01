package engine

import (
	"context"
	"fmt"
	"math"
	"math/rand"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/svlocks/sheets/internal/app"
	"github.com/svlocks/sheets/internal/cypher"
	"github.com/svlocks/sheets/internal/domain"
	"github.com/svlocks/sheets/internal/store"
)

func forcedFullSnapshotResult(ctx context.Context, executor *Engine, request app.ExecuteRequest) (app.BatchResult, error) {
	if err := request.Validate(); err != nil {
		return app.BatchResult{}, err
	}
	document, err := cypher.Parse(request.Query)
	if err != nil {
		return app.BatchResult{}, err
	}
	if len(document.Statements) == 0 {
		return app.BatchResult{}, fmt.Errorf("query has no statements")
	}
	if err := validateDocumentSemantics(document); err != nil {
		return app.BatchResult{}, err
	}
	graph, err := loadSnapshot(ctx, executor.store, request.Snapshot)
	if err != nil {
		return app.BatchResult{}, err
	}
	return executeDocument(ctx, document, graph, request.Params, executor.store.ListRevisions)
}

func assertIteratorEqualsFullSnapshot(t *testing.T, executor *Engine, request app.ExecuteRequest) app.BatchResult {
	t.Helper()
	got, err := executor.Execute(context.Background(), request)
	if err != nil {
		t.Fatalf("iterator Execute(%q): %v", request.Query, err)
	}
	want, err := forcedFullSnapshotResult(context.Background(), executor, request)
	if err != nil {
		t.Fatalf("full snapshot Execute(%q): %v", request.Query, err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("iterator/full mismatch for %q\n got: %#v\nwant: %#v", request.Query, got, want)
	}
	return got
}

func TestIteratorDifferentialUnsafeStoragePredicates(t *testing.T) {
	executor, _ := testEngine(t)
	execute(t, executor, `
CREATE (:Number {name:'float', value:1.0}),
       (:Number {name:'integer', value:1}),
	       (:Document {name:'body', body:'stored outside the property map'}),
	       (:Moment {name:'instant', at:datetime('2024-01-01T00:00:00Z')}),
	       (:Endpoint {name:'from'})-[:CHILD {position:7}]->(:Endpoint {name:'to'}),
	       (:NumericSource)-[:WEIGHTED {weight:1}]->(:NumericTarget),
	       (:NumericSource)-[:WEIGHTED {weight:1.0}]->(:NumericTarget)`, nil)

	equivalentInstant := time.Date(2023, 12, 31, 19, 0, 0, 0, time.FixedZone("query-zone", -5*60*60))
	tests := []struct {
		name   string
		query  string
		params map[string]any
		rows   int
	}{
		{
			name:   "numeric cross type",
			query:  "MATCH (n:Number {value:$value}) RETURN n.name AS name ORDER BY name",
			params: map[string]any{"value": int64(1)},
			rows:   2,
		},
		{
			name:  "node body",
			query: "MATCH (n:Document {body:'stored outside the property map'}) RETURN n.name AS name",
			rows:  1,
		},
		{
			name:  "relationship position",
			query: "MATCH ()-[r:CHILD {position:7}]->() RETURN count(r) AS total",
			rows:  1,
		},
		{
			name:   "temporal instant equality",
			query:  "MATCH (n:Moment {at:$at}) RETURN n.name AS name",
			params: map[string]any{"at": equivalentInstant},
			rows:   1,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result := assertIteratorEqualsFullSnapshot(t, executor, app.ExecuteRequest{Query: test.query, Params: test.params})
			if len(result.Results) != 1 || len(result.Results[0].Rows) != test.rows {
				t.Fatalf("rows = %#v", result.Results)
			}
			if plan := executor.lastPlan(); plan.Fallback != "" {
				t.Fatalf("safe residual unexpectedly fell back: %#v", plan)
			}
		})
	}

	numericCount := assertIteratorEqualsFullSnapshot(t, executor, app.ExecuteRequest{
		Query:  "MATCH (n:Number {value:$value}) RETURN count(n) AS total",
		Params: map[string]any{"value": int64(1)},
	})
	if got := numericCount.Results[0].Rows; len(got) != 1 || got[0][0] != int64(2) {
		t.Fatalf("numeric count = %#v", got)
	}
	if plan := executor.lastPlan(); !hasPlanOperator(plan, "NodeIndexCount") || !strings.Contains(plan.Operators[0].Pushdown, "numeric-property:value[2 variants]") {
		t.Fatalf("numeric count did not use sound representation alternatives: %#v", plan)
	}

	bodyCount := assertIteratorEqualsFullSnapshot(t, executor, app.ExecuteRequest{
		Query: "MATCH (n:Document {body:'stored outside the property map'}) RETURN count(n) AS total",
	})
	if got := bodyCount.Results[0].Rows; len(got) != 1 || got[0][0] != int64(1) {
		t.Fatalf("body count = %#v", got)
	}
	if plan := executor.lastPlan(); hasPlanOperator(plan, "NodeIndexCount") {
		t.Fatalf("body predicate used direct count: %#v", plan)
	}

	safeCount := assertIteratorEqualsFullSnapshot(t, executor, app.ExecuteRequest{
		Query: "MATCH (n:Number {name:'integer'}) RETURN count(n) AS total",
	})
	if got := safeCount.Results[0].Rows; len(got) != 1 || got[0][0] != int64(1) {
		t.Fatalf("safe count = %#v", got)
	}
	if plan := executor.lastPlan(); !hasPlanOperator(plan, "NodeIndexCount") {
		t.Fatalf("exact string predicate did not use direct count: %#v", plan)
	}

	numericEdges := assertIteratorEqualsFullSnapshot(t, executor, app.ExecuteRequest{
		Query:  "MATCH ()-[r:WEIGHTED {weight:$weight}]->() RETURN count(r) AS total",
		Params: map[string]any{"weight": int64(1)},
	})
	if got := numericEdges.Results[0].Rows; len(got) != 1 || got[0][0] != int64(2) {
		t.Fatalf("numeric relationship count = %#v", got)
	}
	edgePlan := executor.lastPlan()
	if len(edgePlan.Operators) < 2 || !strings.Contains(edgePlan.Operators[1].Pushdown, "numeric-property:weight[2 variants]") {
		t.Fatalf("numeric relationship alternatives missing: %#v", edgePlan)
	}
}

func TestIteratorNumericAlternativeBoundaries(t *testing.T) {
	executor, database := testEngine(t)
	values := []struct {
		name  string
		value any
	}{
		{name: "int-zero", value: int64(0)},
		{name: "float-positive-zero", value: float64(0)},
		{name: "float-negative-zero", value: math.Copysign(0, -1)},
		{name: "float-fraction", value: 1.5},
		{name: "int-two53", value: int64(1 << 53)},
		{name: "float-two53", value: float64(1 << 53)},
		{name: "int-two53-plus-one", value: int64(1<<53 + 1)},
		{name: "int-max", value: int64(math.MaxInt64)},
		{name: "int-min", value: int64(math.MinInt64)},
		{name: "float-min-int", value: -math.Exp2(63)},
		{name: "positive-infinity", value: math.Inf(1)},
		{name: "negative-infinity", value: math.Inf(-1)},
		{name: "nan", value: math.NaN()},
	}
	_, err := database.Write(context.Background(), store.RevisionMeta{}, func(tx *store.WriteTx) error {
		for _, item := range values {
			if _, createErr := tx.CreateNode(store.NodeInput{
				Labels:     []string{"NumericBoundary"},
				Properties: domain.Properties{"name": item.name, "value": item.value},
			}); createErr != nil {
				return createErr
			}
		}
		pairs := []domain.Properties{
			{"name": "int-int", "x": int64(1), "y": int64(2)},
			{"name": "float-float", "x": float64(1), "y": float64(2)},
			{"name": "wrong-residual", "x": int64(1), "y": int64(3)},
		}
		for _, properties := range pairs {
			if _, createErr := tx.CreateNode(store.NodeInput{Labels: []string{"NumericPair"}, Properties: properties}); createErr != nil {
				return createErr
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name  string
		value any
		want  int64
	}{
		{name: "integer zero includes both float signs", value: int64(0), want: 3},
		{name: "negative zero includes integer and positive zero", value: math.Copysign(0, -1), want: 3},
		{name: "nonintegral float", value: 1.5, want: 1},
		{name: "exact two53 integer", value: int64(1 << 53), want: 2},
		{name: "integer above exact float boundary", value: int64(1<<53 + 1), want: 1},
		{name: "integral float at two53", value: float64(1 << 53), want: 2},
		{name: "maximum integer has no equal float", value: int64(math.MaxInt64), want: 1},
		{name: "minimum integer has an equal float", value: int64(math.MinInt64), want: 2},
		{name: "minimum integral float includes integer", value: -math.Exp2(63), want: 2},
		{name: "positive infinity", value: math.Inf(1), want: 1},
		{name: "negative infinity", value: math.Inf(-1), want: 1},
		{name: "nan matches nothing", value: math.NaN(), want: 0},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := app.ExecuteRequest{
				Query:  "MATCH (n:NumericBoundary {value:$value}) RETURN count(n) AS total",
				Params: map[string]any{"value": test.value},
			}
			result := assertIteratorEqualsFullSnapshot(t, executor, request)
			if got := result.Results[0].Rows; len(got) != 1 || got[0][0] != test.want {
				t.Fatalf("numeric boundary count = %#v, want %d", got, test.want)
			}
			if plan := executor.lastPlan(); !hasPlanOperator(plan, "NodeIndexCount") {
				t.Fatalf("numeric boundary did not use index count: %#v", plan)
			}
		})
	}

	multiple := assertIteratorEqualsFullSnapshot(t, executor, app.ExecuteRequest{
		Query:  "MATCH (n:NumericPair {x:$x, y:$y}) RETURN count(n) AS total",
		Params: map[string]any{"x": float64(1), "y": float64(2)},
	})
	if got := multiple.Results[0].Rows; len(got) != 1 || got[0][0] != int64(2) {
		t.Fatalf("multiple numeric properties = %#v", got)
	}
	plan := executor.lastPlan()
	if hasPlanOperator(plan, "NodeIndexCount") || len(plan.Operators) == 0 ||
		!strings.Contains(plan.Operators[0].Pushdown, "numeric-property:x") ||
		!strings.Contains(plan.Operators[0].Pushdown, "residual:property:y") {
		t.Fatalf("multiple numeric property plan = %#v", plan)
	}
}

func TestIteratorDifferentialDepthZeroAndVisibleFallbacks(t *testing.T) {
	executor, _ := testEngine(t)
	execute(t, executor, `
CREATE (a:A {name:'a'})-[:S]->(:C {name:'c'}),
       (:A {name:'noise'})-[:R]->(:B {name:'noise-b'}),
       (:B {name:'wanted'})-[:R]->(:C {name:'wanted-c'}),
       (:Other {name:'other'})`, nil)

	zeroLength := app.ExecuteRequest{Query: `
MATCH (a:A {name:'a'})-[:R*0..1]->(same)-[:S]->(c:C)
RETURN a.name AS start, same.name AS same, c.name AS finish`}
	result := assertIteratorEqualsFullSnapshot(t, executor, zeroLength)
	if got := result.Results[0].Rows; len(got) != 1 || !reflect.DeepEqual(got[0], []any{"a", "a", "c"}) {
		t.Fatalf("zero-length continuation = %#v", got)
	}
	if plan := executor.lastPlan(); plan.Fallback != "" {
		t.Fatalf("bounded zero-length query unexpectedly fell back: %#v", plan)
	}

	withScope := app.ExecuteRequest{Query: `
MATCH (a:A {name:'a'})
WITH 1 AS marker
MATCH (a:B {name:'wanted'})-[:R]->(c:C)
RETURN marker AS marker, a.name AS name, c.name AS child`}
	result = assertIteratorEqualsFullSnapshot(t, executor, withScope)
	if got := result.Results[0].Rows; len(got) != 1 || !reflect.DeepEqual(got[0], []any{int64(1), "wanted", "wanted-c"}) {
		t.Fatalf("WITH scope result = %#v", got)
	}
	if plan := executor.lastPlan(); !strings.Contains(plan.Fallback, "MATCH after WITH") || !hasPlanOperator(plan, "FullSnapshot") {
		t.Fatalf("WITH scope fallback was not explicit: %#v", plan)
	}

	labels := app.ExecuteRequest{Query: `
MATCH (:A {name:'a'})
CALL db.labels() YIELD label
RETURN label ORDER BY label`}
	result = assertIteratorEqualsFullSnapshot(t, executor, labels)
	if got := result.Results[0].Rows; len(got) != 4 {
		t.Fatalf("db.labels rows = %#v", got)
	}
	if plan := executor.lastPlan(); !strings.Contains(plan.Fallback, "graph-wide procedure") || !hasPlanOperator(plan, "FullSnapshot") {
		t.Fatalf("graph-wide procedure fallback was not explicit: %#v", plan)
	}
}

func TestIteratorUnboundedTraversalDoesNotSilentlyStopAtDepthBudget(t *testing.T) {
	executor, database := testEngine(t)
	_, err := database.Write(context.Background(), store.RevisionMeta{}, func(tx *store.WriteTx) error {
		nodes := make([]domain.Node, 66)
		for index := range nodes {
			created, createErr := tx.CreateNode(store.NodeInput{
				Labels:     []string{"Chain"},
				Properties: domain.Properties{"name": fmt.Sprintf("n%03d", index)},
			})
			if createErr != nil {
				return createErr
			}
			nodes[index] = created
			if index == 0 {
				continue
			}
			if _, createErr = tx.CreateEdge(store.EdgeInput{From: nodes[index-1].ID, Type: "R", To: nodes[index].ID}); createErr != nil {
				return createErr
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	request := app.ExecuteRequest{Query: `
MATCH (:Chain {name:'n000'})-[:R*1..]->(finish:Chain {name:'n065'})
RETURN finish.name AS name`}
	result := assertIteratorEqualsFullSnapshot(t, executor, request)
	if got := result.Results[0].Rows; len(got) != 1 || got[0][0] != "n065" {
		t.Fatalf("unbounded traversal = %#v", got)
	}
	if plan := executor.lastPlan(); !strings.Contains(plan.Fallback, "unbounded variable-length") || !hasPlanOperator(plan, "FullSnapshot") {
		t.Fatalf("unbounded fallback was not explicit: %#v", plan)
	}
	explain := execute(t, executor, "EXPLAIN "+request.Query, nil)
	if !hasFallback(explain.Results[0].Rows, "unbounded variable-length") {
		t.Fatalf("EXPLAIN omitted unbounded fallback: %#v", explain.Results[0].Rows)
	}
}

func TestShortestPathRelationshipTrailAndSameDepthTargets(t *testing.T) {
	t.Run("one undirected edge cannot be reused", func(t *testing.T) {
		executor, _ := testEngine(t)
		execute(t, executor, "CREATE (a:A {name:'a'})-[:R]->(:B {name:'b'})", nil)
		result := execute(t, executor, `
MATCH (a:A {name:'a'})
RETURN shortestPath((a)-[:R*1..]-(a)) AS path`, nil)
		if got := result.Results[0].Rows; len(got) != 1 || got[0][0] != nil {
			t.Fatalf("single-edge undirected cycle = %#v", got)
		}
	})

	t.Run("parallel edges form two trails", func(t *testing.T) {
		executor, _ := testEngine(t)
		execute(t, executor, `
CREATE (a:A {name:'a'}), (b:B {name:'b'}),
       (a)-[:R]->(b), (a)-[:R]->(b)`, nil)
		result := execute(t, executor, `
MATCH (a:A {name:'a'})
RETURN size(allShortestPaths((a)-[:R*1..]-(a))) AS total`, nil)
		if got := result.Results[0].Rows; len(got) != 1 || got[0][0] != int64(2) {
			t.Fatalf("parallel-edge cycles = %#v", got)
		}
	})

	t.Run("cycle and distinct target at the same depth", func(t *testing.T) {
		executor, _ := testEngine(t)
		execute(t, executor, `
CREATE (s:Goal {name:'start'}), (x:Middle {name:'x'}),
       (y:Middle {name:'y'}), (t:Goal {name:'target'}),
       (s)-[:R]->(x), (x)-[:R]->(s),
       (s)-[:R]->(y), (y)-[:R]->(t)`, nil)
		result := execute(t, executor, `
MATCH (s:Goal {name:'start'})
RETURN size(allShortestPaths((s)-[:R*1..]->(:Goal))) AS total`, nil)
		if got := result.Results[0].Rows; len(got) != 1 || got[0][0] != int64(2) {
			t.Fatalf("same-depth shortest paths = %#v", got)
		}
	})
}

func TestShortestPathGlobalTieOrderAndReconstructionBudget(t *testing.T) {
	t.Run("single shortest path uses global deterministic edge order", func(t *testing.T) {
		nodes := []domain.Node{
			{ID: "start", Labels: []string{"Start"}},
			{ID: "a-target", Labels: []string{"Goal"}},
			{ID: "z-target", Labels: []string{"Goal"}},
		}
		edges := []domain.Edge{
			{ID: "z-edge", From: "start", Type: "R", To: "a-target"},
			{ID: "a-edge", From: "start", Type: "R", To: "z-target"},
		}
		graph := newMemoryGraph(1, nodes, edges, nil)
		evaluator := newEvaluator(nil)
		evaluator.paths = &pathExpansionBudget{limit: 100}
		paths, err := shortestPathsFrom(
			evaluator,
			graph,
			row{},
			graph.nodes["start"],
			cypher.NodePattern{Labels: []cypher.Identifier{{Name: "Goal"}}},
			cypher.RelationshipPattern{Direction: cypher.Outgoing, Types: []cypher.Identifier{{Name: "R"}}},
			1,
			1,
		)
		if err != nil {
			t.Fatal(err)
		}
		selected, ok := selectShortestPaths(paths, false).(Path)
		if !ok || len(selected.Relationships) != 1 || selected.Relationships[0].ID != "a-edge" {
			t.Fatalf("selected path = %#v from %#v", selected, paths)
		}
	})

	t.Run("predecessor reconstruction is budgeted", func(t *testing.T) {
		nodes := []domain.Node{{ID: "start", Labels: []string{"Start"}}}
		previous := []domain.EntityID{"start"}
		var edges []domain.Edge
		for layer := 0; layer < 8; layer++ {
			current := []domain.EntityID{
				domain.EntityID(fmt.Sprintf("layer-%02d-a", layer)),
				domain.EntityID(fmt.Sprintf("layer-%02d-b", layer)),
			}
			for _, id := range current {
				nodes = append(nodes, domain.Node{ID: id, Labels: []string{"Middle"}})
			}
			for _, from := range previous {
				for _, to := range current {
					edges = append(edges, domain.Edge{
						ID:   domain.EntityID(fmt.Sprintf("edge-%03d", len(edges))),
						From: from,
						Type: "R",
						To:   to,
					})
				}
			}
			previous = current
		}
		nodes = append(nodes, domain.Node{ID: "target", Labels: []string{"Goal"}})
		for _, from := range previous {
			edges = append(edges, domain.Edge{
				ID:   domain.EntityID(fmt.Sprintf("edge-%03d", len(edges))),
				From: from,
				Type: "R",
				To:   "target",
			})
		}
		graph := newMemoryGraph(1, nodes, edges, nil)
		budget := &pathExpansionBudget{limit: 100}
		evaluator := newEvaluator(nil)
		evaluator.paths = budget
		_, err := shortestPathsFrom(
			evaluator,
			graph,
			row{},
			graph.nodes["start"],
			cypher.NodePattern{Labels: []cypher.Identifier{{Name: "Goal"}}},
			cypher.RelationshipPattern{Direction: cypher.Outgoing, Types: []cypher.Identifier{{Name: "R"}}},
			1,
			9,
		)
		if err == nil || !strings.Contains(err.Error(), "path expansion limit") {
			t.Fatalf("reconstruction budget error = %v (used %d)", err, budget.used)
		}
	})
}

func TestIteratorChunksLargeEndpointFetchesAndBoundsPredicates(t *testing.T) {
	_, database := testEngine(t)
	view, err := database.View(context.Background(), domain.Snapshot{})
	if err != nil {
		t.Fatal(err)
	}
	working := iteratorWorkingSet{
		ctx: context.Background(), view: view,
		budget: iteratorBudget{depth: defaultIteratorDepth},
		nodes:  make(map[domain.EntityID]domain.Node),
		edges:  make(map[domain.EntityID]domain.Edge),
	}
	ids := make([]domain.EntityID, 100_001)
	for index := range ids {
		ids[index] = domain.EntityID(fmt.Sprintf("01890f00-0000-7000-8000-%012x", index))
	}
	nodes, err := working.fetchNodes(ids)
	if err != nil {
		t.Fatalf("chunked endpoint fetch: %v", err)
	}
	if len(nodes) != 0 {
		t.Fatalf("unexpected endpoint rows = %d", len(nodes))
	}

	pattern := cypher.NodePattern{}
	for index := 0; index < iteratorPredicateTerms+10; index++ {
		pattern.Labels = append(pattern.Labels, cypher.Identifier{Name: fmt.Sprintf("Label%d", index)})
	}
	predicate, pushdown, complete := buildNodePredicate(pattern, nil)
	if complete || len(predicate.AllLabels) != iteratorPredicateTerms || !strings.Contains(pushdown, "residual:labels") {
		t.Fatalf("bounded node predicate = %#v, %q, complete=%v", predicate, pushdown, complete)
	}

	relationship := cypher.RelationshipPattern{}
	for index := 0; index < iteratorPredicateTerms+10; index++ {
		relationship.Types = append(relationship.Types, cypher.Identifier{Name: fmt.Sprintf("Type%d", index)})
	}
	edgePredicate, edgePushdown := staticEdgePredicate(relationship, nil)
	if len(edgePredicate.Types) != 0 || !strings.Contains(edgePushdown, "residual:types") {
		t.Fatalf("bounded edge predicate = %#v, %q", edgePredicate, edgePushdown)
	}
}

func TestIteratorPagedHighFanoutDifferential(t *testing.T) {
	executor, database := testEngine(t)
	const leaves = 2_200
	_, err := database.Write(context.Background(), store.RevisionMeta{}, func(tx *store.WriteTx) error {
		root, createErr := tx.CreateNode(store.NodeInput{
			Labels: []string{"Root"}, Properties: domain.Properties{"name": "root"},
		})
		if createErr != nil {
			return createErr
		}
		for index := 0; index < leaves; index++ {
			leaf, leafErr := tx.CreateNode(store.NodeInput{
				Labels: []string{"Leaf"},
				Properties: domain.Properties{
					"name":    fmt.Sprintf("leaf-%04d", index),
					"ordinal": int64(index),
				},
			})
			if leafErr != nil {
				return leafErr
			}
			if _, leafErr = tx.CreateEdge(store.EdgeInput{From: root.ID, Type: "R", To: leaf.ID}); leafErr != nil {
				return leafErr
			}
		}
		_, createErr = tx.CreateEdge(store.EdgeInput{From: root.ID, Type: "LOOP", To: root.ID})
		return createErr
	})
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name  string
		query string
		want  int64
	}{
		{
			name:  "node pull pages",
			query: "MATCH (n:Leaf) RETURN count(n.ordinal) AS total",
			want:  leaves,
		},
		{
			name:  "outgoing edge pull pages",
			query: "MATCH (:Root {name:'root'})-[r:R]->(:Leaf) RETURN count(r) AS total",
			want:  leaves,
		},
		{
			name:  "incoming endpoint set",
			query: "MATCH (:Leaf)<-[r:R]-(:Root {name:'root'}) RETURN count(r) AS total",
			want:  leaves,
		},
		{
			name:  "undirected edge pull pages",
			query: "MATCH (:Root {name:'root'})-[r:R]-(:Leaf) RETURN count(r) AS total",
			want:  leaves,
		},
		{
			name:  "undirected self loop is unique",
			query: "MATCH (n:Root {name:'root'})-[r:LOOP]-(n) RETURN count(r) AS total",
			want:  1,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result := assertIteratorEqualsFullSnapshot(t, executor, app.ExecuteRequest{Query: test.query})
			if got := result.Results[0].Rows; len(got) != 1 || got[0][0] != test.want {
				t.Fatalf("paged result = %#v, want %d", got, test.want)
			}
			if plan := executor.lastPlan(); plan.Fallback != "" {
				t.Fatalf("paged iterator unexpectedly fell back: %#v", plan)
			}
		})
	}
}

func TestIteratorRandomizedDifferentialSmallGraphs(t *testing.T) {
	queries := []struct {
		name   string
		query  string
		params map[string]any
	}{
		{
			name:   "safe point predicate",
			query:  "MATCH (n:N {name:$name}) RETURN n.name AS name",
			params: map[string]any{"name": "n00"},
		},
		{
			name:   "numeric cross-type node predicate",
			query:  "MATCH (n:N {score:$score}) RETURN count(n) AS total",
			params: map[string]any{"score": int64(1)},
		},
		{
			name:  "outgoing scan",
			query: "MATCH (:N {name:'n00'})-[r:R]->(:N) RETURN count(r) AS total",
		},
		{
			name:  "incoming scan",
			query: "MATCH (:N {name:'n00'})<-[r:S]-(:N) RETURN count(r) AS total",
		},
		{
			name:  "undirected alternatives",
			query: "MATCH (:N {name:'n00'})-[r:R|:S]-(:N) RETURN count(r) AS total",
		},
		{
			name:  "repeated node variable and self loops",
			query: "MATCH (n:N)-[r:R]->(n) RETURN count(r) AS total",
		},
		{
			name:  "fixed multi hop",
			query: "MATCH (:N {name:'n00'})-[:R]->(middle:N)-[:S]->(finish:N) RETURN count(finish) AS total",
		},
		{
			name:  "bounded variable length including zero",
			query: "MATCH (:N {name:'n00'})-[:R*0..3]->(finish:N) RETURN count(finish) AS total",
		},
		{
			name:  "optional expansion",
			query: "MATCH (n:N {name:'n00'}) OPTIONAL MATCH (n)-[:T]->(other:N) RETURN n.name AS name, count(other) AS total",
		},
		{
			name:  "two bound matches",
			query: "MATCH (:N {name:'n00'})-[first:R]->(middle:N) MATCH (middle)-[second:S]->(finish:N) RETURN count(second) AS total",
		},
		{
			name:  "endpoint labels and properties",
			query: "MATCH (:Alpha)-[r:R]->(:Beta {bucket:'b1'}) RETURN count(r) AS total",
		},
		{
			name:   "numeric relationship residual",
			query:  "MATCH (:N)-[r:R {weight:$weight}]->(:N) RETURN count(r) AS total",
			params: map[string]any{"weight": int64(1)},
		},
		{
			name:  "correlated relationship residual",
			query: "MATCH (a:N)-[r:R {kind:a.edgeKind}]->(b:N {bucket:a.bucket}) RETURN count(r) AS total",
		},
		{
			name:  "where with projection and aggregation",
			query: "MATCH (n:N) WHERE n.score >= 1 WITH n.bucket AS bucket, count(n) AS total WHERE total > 0 RETURN bucket, total ORDER BY bucket",
		},
		{
			name:  "distinct order skip limit",
			query: "MATCH (n:N) RETURN DISTINCT n.bucket AS bucket ORDER BY bucket SKIP 1 LIMIT 2",
		},
	}

	for seed := int64(1); seed <= 10; seed++ {
		t.Run(fmt.Sprintf("seed_%02d", seed), func(t *testing.T) {
			executor, database := testEngine(t)
			random := rand.New(rand.NewSource(seed))
			_, err := database.Write(context.Background(), store.RevisionMeta{}, func(tx *store.WriteTx) error {
				nodes := make([]domain.Node, 12)
				for index := range nodes {
					labels := []string{"N"}
					if index%2 == 0 {
						labels = append(labels, "Alpha")
					} else {
						labels = append(labels, "Beta")
					}
					var score any = int64(index % 3)
					if index%2 == 0 {
						score = float64(index % 3)
					}
					body := ""
					if index%5 == 0 {
						body = fmt.Sprintf("body-%02d", index)
					}
					created, createErr := tx.CreateNode(store.NodeInput{
						Labels: labels,
						Properties: domain.Properties{
							"name":     fmt.Sprintf("n%02d", index),
							"bucket":   fmt.Sprintf("b%d", index%3),
							"score":    score,
							"edgeKind": fmt.Sprintf("k%d", index%2),
						},
						Body: body,
					})
					if createErr != nil {
						return createErr
					}
					nodes[index] = created
				}
				for index := 0; index < 42; index++ {
					from := random.Intn(len(nodes))
					to := random.Intn(len(nodes))
					edgeType := []string{"R", "S", "T"}[random.Intn(3)]
					var weight any = int64(index % 3)
					if index%2 == 0 {
						weight = float64(index % 3)
					}
					_, createErr := tx.CreateEdge(store.EdgeInput{
						From: nodes[from].ID,
						Type: edgeType,
						To:   nodes[to].ID,
						Properties: domain.Properties{
							"kind":   fmt.Sprintf("k%d", from%2),
							"weight": weight,
						},
					})
					if createErr != nil {
						return createErr
					}
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}

			for _, query := range queries {
				t.Run(query.name, func(t *testing.T) {
					assertIteratorEqualsFullSnapshot(t, executor, app.ExecuteRequest{Query: query.query, Params: query.params})
					if plan := executor.lastPlan(); plan.Fallback != "" {
						t.Fatalf("eligible randomized query fell back: %#v", plan)
					}
				})
			}
		})
	}
}

func TestEngineHugePaginationRangeAndMutationReadYourWrites(t *testing.T) {
	executor, _ := testEngine(t)

	result := execute(t, executor, "RETURN 1 AS value SKIP $skip LIMIT $limit", map[string]any{
		"skip":  int64(math.MaxInt64),
		"limit": int64(math.MaxInt64),
	})
	if got := result.Results[0].Rows; len(got) != 0 {
		t.Fatalf("huge SKIP rows = %#v", got)
	}
	result = execute(t, executor, "RETURN 1 AS value LIMIT $limit", map[string]any{"limit": int64(math.MaxInt64)})
	if got := result.Results[0].Rows; len(got) != 1 || got[0][0] != int64(1) {
		t.Fatalf("huge LIMIT rows = %#v", got)
	}
	_, err := executor.Execute(context.Background(), app.ExecuteRequest{
		Query:  "RETURN range($start, $finish) AS values",
		Params: map[string]any{"start": int64(math.MinInt64), "finish": int64(math.MaxInt64)},
	})
	if err == nil || !strings.Contains(err.Error(), "range result is too large") {
		t.Fatalf("huge range error = %v", err)
	}

	mutation := execute(t, executor, `
CREATE (n:Txn {name:'before'})
SET n.name = 'after'
WITH n
MATCH (same:Txn {name:'after'})
RETURN n.name AS created, count(same) AS visible`, nil)
	if got := mutation.Results[0].Rows; len(got) != 1 || !reflect.DeepEqual(got[0], []any{"after", int64(1)}) {
		t.Fatalf("mutation read-your-writes = %#v", got)
	}

	batch := execute(t, executor, `
CREATE (:Txn {name:'batch'});
MATCH (n:Txn {name:'batch'}) RETURN count(n) AS visible`, nil)
	if len(batch.Results) != 2 || len(batch.Results[1].Rows) != 1 || batch.Results[1].Rows[0][0] != int64(1) {
		t.Fatalf("document read-your-writes = %#v", batch)
	}

	budget := iteratorBudget{bytes: defaultIteratorBytes - 1}
	if err := budget.addNode(domain.Node{ID: "node", Body: "xx"}); err == nil || !strings.Contains(err.Error(), "working-set byte budget") {
		t.Fatalf("working-set byte budget error = %v", err)
	}
}
