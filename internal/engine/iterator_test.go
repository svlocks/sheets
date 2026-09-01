package engine

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/svlocks/sheets/internal/app"
	"github.com/svlocks/sheets/internal/cypher"
	"github.com/svlocks/sheets/internal/domain"
)

func TestIteratorPlansBoundMultiHopAndOptionalRead(t *testing.T) {
	executor, _ := testEngine(t)
	execute(t, executor, `
CREATE (root:Task {title:'root'})-[:CHILD]->(middle:Task {title:'middle'})-[:CHILD]->(leaf:Task {title:'leaf'}),
       (:Task {title:'noise'})-[:CHILD]->(:Task {title:'unrelated'})`, nil)

	result := execute(t, executor, `
MATCH (root:Task {title:$title})-[:CHILD]->(middle)
MATCH (middle)-[:CHILD]->(leaf:Task)
RETURN leaf.title AS title`, map[string]any{"title": "root"})
	if got := result.Results[0].Rows; len(got) != 1 || got[0][0] != "leaf" {
		t.Fatalf("multi-hop result = %#v", got)
	}
	plan := executor.lastPlan()
	if plan.Fallback != "" {
		t.Fatalf("multi-hop unexpectedly fell back: %#v", plan)
	}
	if !hasPlanOperator(plan, "BoundNodeInput") || !hasPlanOperator(plan, "EdgeIndexScan") {
		t.Fatalf("multi-hop plan lacks bound expansion/index scan: %#v", plan)
	}

	optional := execute(t, executor, `
MATCH (root:Task {title:'root'})
OPTIONAL MATCH (root)-[:CHILD]->(child:Task {title:'missing'})
RETURN root.title AS root, child AS child`, nil)
	if got := optional.Results[0].Rows; len(got) != 1 || got[0][0] != "root" || got[0][1] != nil {
		t.Fatalf("optional iterator result = %#v", got)
	}
	if plan = executor.lastPlan(); plan.Fallback != "" || !hasPlanOperator(plan, "EdgeIndexScan") {
		t.Fatalf("optional plan = %#v", plan)
	}
}

func TestIteratorHistoricalSnapshotAndDirectCount(t *testing.T) {
	executor, database := testEngine(t)
	created := execute(t, executor, "CREATE (:Task {title:'before'}), (:Task {title:'other'})", nil)
	if created.Revision == nil {
		t.Fatal("create did not produce a revision")
	}
	other, err := New(database)
	if err != nil {
		t.Fatal(err)
	}
	execute(t, other, "MATCH (n:Task {title:'before'}) SET n.title = 'after'", nil)

	result, err := executor.Execute(context.Background(), app.ExecuteRequest{
		Query:    "MATCH (n:Task {title:'before'}) RETURN count(n) AS total",
		Snapshot: domain.Snapshot{Revision: created.Revision},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := result.Results[0].Rows; len(got) != 1 || got[0][0] != int64(1) {
		t.Fatalf("historical count = %#v", got)
	}
	plan := executor.lastPlan()
	if len(plan.Operators) != 1 || plan.Operators[0].Kind != "NodeIndexCount" || plan.Fallback != "" {
		t.Fatalf("direct-count plan = %#v", plan)
	}

	current := execute(t, executor, "MATCH (n:Task {title:'before'}) RETURN count(n) AS total", nil)
	if got := current.Results[0].Rows; len(got) != 1 || got[0][0] != int64(0) {
		t.Fatalf("current count after external revision = %#v", got)
	}
}

func TestIteratorMatchesFullSnapshotForResidualPipelines(t *testing.T) {
	executor, _ := testEngine(t)
	execute(t, executor, `
CREATE (root:Task {title:'root', kind:'wanted'})-[:CHILD {kind:'wanted'}]->(middle:Task {title:'middle'})-[:CHILD]->(leaf:Task {title:'leaf'}),
       (root)-[:CHILD {kind:'ignored'}]->(:Task {title:'ignored'}),
       (:Task {title:'noise'})-[:CHILD]->(:Task {title:'unrelated'})`, nil)
	query := `
MATCH (root:Task {title:$title})-[first:CHILD {kind:root.kind}]->(middle)
OPTIONAL MATCH (middle)-[:CHILD]->(leaf:Task)
WITH root, first, middle, leaf
WHERE middle.title <> 'absent'
RETURN root.title AS root, first.kind AS kind, leaf.title AS leaf
ORDER BY leaf LIMIT 10`
	params := map[string]any{"title": "root"}
	document, err := cypher.Parse(query)
	if err != nil {
		t.Fatal(err)
	}
	full, err := executor.loadGraph(context.Background(), domain.Snapshot{})
	if err != nil {
		t.Fatal(err)
	}
	want, err := executeDocument(context.Background(), document, full, params, executor.store.ListRevisions)
	if err != nil {
		t.Fatal(err)
	}
	got := execute(t, executor, query, params)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("iterator result\n got: %#v\nwant: %#v", got, want)
	}
	if plan := executor.lastPlan(); plan.Fallback != "" {
		t.Fatalf("residual pipeline unexpectedly fell back: %#v", plan)
	}
}

func TestShortestPathBFSChoosesGlobalMinimumAndSelfLoop(t *testing.T) {
	executor, _ := testEngine(t)
	execute(t, executor, `
CREATE (slow:Start)-[:R]->(:Middle)-[:R]->(target:Target),
       (fast:Start)-[:R]->(target),
       (loop:Loop)-[:R]->(loop)`, nil)

	result := execute(t, executor, `
RETURN length(shortestPath((start:Start)-[:R*]->(target:Target))) AS distance,
       size(allShortestPaths((start:Start)-[:R*]->(target:Target))) AS count`, nil)
	if got := result.Results[0].Rows; len(got) != 1 || got[0][0] != int64(1) || got[0][1] != int64(1) {
		t.Fatalf("global shortest result = %#v", got)
	}
	if plan := executor.lastPlan(); !strings.Contains(plan.Fallback, "full-snapshot BFS") {
		t.Fatalf("BFS expression plan was not observable: %#v", plan)
	}
	lowerBounded := execute(t, executor, `
RETURN length(shortestPath((start:Start)-[:R*2..]->(target:Target))) AS distance`, nil)
	if got := lowerBounded.Results[0].Rows; len(got) != 1 || got[0][0] != int64(2) {
		t.Fatalf("lower-bounded shortest result = %#v", got)
	}
	if plan := executor.lastPlan(); !strings.Contains(plan.Fallback, "generic trail fallback") {
		t.Fatalf("generic shortest fallback was not observable: %#v", plan)
	}

	loop := execute(t, executor, `
MATCH (node:Loop)
RETURN length(shortestPath((node)-[:R*1..]->(node))) AS distance`, nil)
	if got := loop.Results[0].Rows; len(got) != 1 || got[0][0] != int64(1) {
		t.Fatalf("self-loop shortest result = %#v", got)
	}
}

func TestIteratorExplainAndCancellation(t *testing.T) {
	executor, _ := testEngine(t)
	execute(t, executor, "CREATE (:Task {title:'root'})-[:CHILD]->(:Task {title:'leaf'})", nil)
	explained := execute(t, executor, "EXPLAIN MATCH (:Task {title:'root'})-[:CHILD]->(child) RETURN child", nil)
	if len(explained.Results[0].Columns) != 4 || !hasResultOperator(explained.Results[0].Rows, "NodeIndexScan") || !hasResultOperator(explained.Results[0].Rows, "EdgeIndexScan") {
		t.Fatalf("EXPLAIN did not expose index plan: %#v", explained.Results[0])
	}
	shortestExplain := execute(t, executor, "EXPLAIN RETURN shortestPath((start:Task)-[:CHILD*2..]->(target:Task))", nil)
	if !hasFallback(shortestExplain.Results[0].Rows, "generic trail fallback") {
		t.Fatalf("EXPLAIN did not expose shortest-path fallback: %#v", shortestExplain.Results[0])
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := executor.Execute(ctx, app.ExecuteRequest{Query: "MATCH (n:Task)-[:CHILD]->(child) RETURN child"})
	if !errors.Is(err, context.Canceled) && !strings.Contains(strings.ToLower(err.Error()), "canceled") {
		t.Fatalf("canceled iterator error = %v", err)
	}
}

func TestIteratorBudgetsFailClearly(t *testing.T) {
	working := iteratorBudget{entities: defaultIteratorEntities}
	if err := working.addNode(domain.Node{ID: "node"}); err == nil || !strings.Contains(err.Error(), "working-set entity budget") {
		t.Fatalf("working-set budget error = %v", err)
	}
	rows := rowBudget{limit: 1}
	if err := rows.take(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	if err := rows.take(context.Background(), 1); err == nil || !strings.Contains(err.Error(), "row budget") {
		t.Fatalf("row budget error = %v", err)
	}
	paths := pathExpansionBudget{limit: 1}
	if err := paths.take(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := paths.take(context.Background()); err == nil || !strings.Contains(err.Error(), "path expansion limit") {
		t.Fatalf("path-expansion budget error = %v", err)
	}

	executor, _ := testEngine(t)
	_, err := executor.Execute(context.Background(), app.ExecuteRequest{Query: "MATCH (:Task)-[:CHILD*65]->(child) RETURN child"})
	if err == nil || !strings.Contains(err.Error(), "path depth budget") {
		t.Fatalf("path-depth budget error = %v", err)
	}
}

func hasPlanOperator(plan physicalPlan, kind string) bool {
	for _, operator := range plan.Operators {
		if operator.Kind == kind {
			return true
		}
	}
	return false
}

func hasResultOperator(rows [][]any, kind string) bool {
	for _, row := range rows {
		if len(row) > 0 && row[0] == kind {
			return true
		}
	}
	return false
}

func hasFallback(rows [][]any, wanted string) bool {
	for _, row := range rows {
		for _, value := range row {
			if text, ok := value.(string); ok && strings.Contains(text, wanted) {
				return true
			}
		}
	}
	return false
}
