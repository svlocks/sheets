package engine

import (
	"context"
	"errors"
	"math"
	"reflect"
	"strings"
	"testing"

	"github.com/svlocks/sheets/internal/app"
	"github.com/svlocks/sheets/internal/cypher"
	"github.com/svlocks/sheets/internal/domain"
)

// These cases are distilled from the openCypher 9 M23 TCK comparison and
// UNION features. They are local regression fixtures, not a claim that the
// complete TCK is supported.
func TestOpenCypherComparisonFixtures(t *testing.T) {
	engine, _ := testEngine(t)
	tests := []struct {
		query string
		want  any
	}{
		{"RETURN [1, 2] = [1] AS value", false},
		{"RETURN [null] = [1] AS value", nil},
		{"RETURN {a: null} = {a: null} AS value", nil},
		{"RETURN null IN [] AS value", false},
		{"RETURN null IN [1] AS value", nil},
		{"RETURN 0.0 / 0.0 = 0.0 / 0.0 AS value", false},
		{"RETURN 1 < 'one' AS value", nil},
		{"RETURN [1, 2] < [1, 3] AS value", true},
		{"RETURN 1 < 2 < 3 AS value", true},
		{"RETURN 1 NOT IN [2, 3] AS value", true},
		{"RETURN 9007199254740993 = 9007199254740992.0 AS value", false},
	}
	for _, test := range tests {
		t.Run(test.query, func(t *testing.T) {
			result := execute(t, engine, test.query, nil)
			got := result.Results[0].Rows[0][0]
			if number, ok := got.(float64); ok && math.IsNaN(number) {
				t.Fatalf("value unexpectedly remained NaN")
			}
			if !reflect.DeepEqual(got, test.want) {
				t.Fatalf("value = %#v (%T), want %#v (%T)", got, got, test.want, test.want)
			}
		})
	}
}

func TestRangeAtIntegerBoundariesCannotWrap(t *testing.T) {
	engine, _ := testEngine(t)
	for _, test := range []struct {
		query string
		want  []any
	}{
		{
			query: "RETURN range(9223372036854775806, 9223372036854775807) AS value",
			want:  []any{int64(9223372036854775806), int64(9223372036854775807)},
		},
		{
			query: "RETURN range(0, -9223372036854775808, -9223372036854775808) AS value",
			want:  []any{int64(0), int64(-9223372036854775807 - 1)},
		},
	} {
		result := execute(t, engine, test.query, nil)
		if got := result.Results[0].Rows[0][0]; !reflect.DeepEqual(got, test.want) {
			t.Fatalf("%s = %#v, want %#v", test.query, got, test.want)
		}
	}

	_, err := engine.Execute(context.Background(), app.ExecuteRequest{
		Query: "RETURN range(-9223372036854775808, 9223372036854775807) AS value",
	})
	if err == nil || !strings.Contains(err.Error(), "range result is too large") {
		t.Fatalf("oversize cross-boundary range error = %v", err)
	}
}

func TestOpenCypherOrderingFixtures(t *testing.T) {
	engine, _ := testEngine(t)
	result := execute(t, engine, "UNWIND [3, null, 1] AS x RETURN x ORDER BY x DESC", nil)
	if want := [][]any{{nil}, {int64(3)}, {int64(1)}}; !reflect.DeepEqual(result.Results[0].Rows, want) {
		t.Fatalf("descending null order = %#v, want %#v", result.Results[0].Rows, want)
	}
	result = execute(t, engine, "UNWIND [[1, 3], [1], [1, 2]] AS x RETURN x ORDER BY x", nil)
	want := [][]any{{[]any{int64(1)}}, {[]any{int64(1), int64(2)}}, {[]any{int64(1), int64(3)}}}
	if !reflect.DeepEqual(result.Results[0].Rows, want) {
		t.Fatalf("list order = %#v, want %#v", result.Results[0].Rows, want)
	}

	execute(t, engine, "CREATE (:Number {num: 3}), (:Number {num: 1}), (:Number {num: 2})", nil)
	result = execute(t, engine, "MATCH (n:Number) RETURN n.num AS prop ORDER BY n.num", nil)
	if want := [][]any{{int64(1)}, {int64(2)}, {int64(3)}}; !reflect.DeepEqual(result.Results[0].Rows, want) {
		t.Fatalf("source-scope order = %#v, want %#v", result.Results[0].Rows, want)
	}
}

func TestWithFiltersBeforeEvaluatingOrderKeys(t *testing.T) {
	engine, _ := testEngine(t)
	result := execute(t, engine, "UNWIND [0, 1] AS x WITH x AS y ORDER BY 1 / x WHERE y = 1 RETURN y", nil)
	if want := [][]any{{int64(1)}}; !reflect.DeepEqual(result.Results[0].Rows, want) {
		t.Fatalf("rows = %#v, want %#v", result.Results[0].Rows, want)
	}
}

func TestUnionColumnContractAndMultipleBranches(t *testing.T) {
	engine, _ := testEngine(t)
	result := execute(t, engine, "RETURN 1 AS x UNION ALL RETURN 2 AS x UNION ALL RETURN 3 AS x", nil)
	if want := [][]any{{int64(1)}, {int64(2)}, {int64(3)}}; !reflect.DeepEqual(result.Results[0].Rows, want) {
		t.Fatalf("UNION rows = %#v, want %#v", result.Results[0].Rows, want)
	}
	_, err := engine.Execute(context.Background(), app.ExecuteRequest{Query: "RETURN 1 AS x UNION RETURN 2 AS y"})
	if err == nil || !strings.Contains(err.Error(), "different columns") {
		t.Fatalf("column mismatch error = %v", err)
	}
}

func TestRelationshipTrailsCannotReuseAcrossPatternSegments(t *testing.T) {
	engine, _ := testEngine(t)
	execute(t, engine, "CREATE (a:Only)-[:LINK]->(b:Only)", nil)
	result := execute(t, engine, "MATCH (a:Only)-[first:LINK]-(b)-[second:LINK]-(a) RETURN first", nil)
	if len(result.Results[0].Rows) != 0 {
		t.Fatalf("one relationship was reused in a trail: %#v", result.Results[0].Rows)
	}
}

func TestUndirectedSelfLoopIsNotDuplicated(t *testing.T) {
	engine, _ := testEngine(t)
	execute(t, engine, "CREATE (a:Loop) WITH a CREATE (a)-[:LOOP]->(a)", nil)
	result := execute(t, engine, "MATCH (a:Loop)-[edge:LOOP]-(a) RETURN count(edge)", nil)
	if got := result.Results[0].Rows[0][0]; got != int64(1) {
		t.Fatalf("self-loop count = %v, want 1", got)
	}
}

func TestPathExpansionBudgetAndCancellation(t *testing.T) {
	nodes := []domain.Node{{ID: "a"}, {ID: "b"}, {ID: "c"}}
	edges := []domain.Edge{
		{ID: "ab", From: "a", To: "b", Type: "LINK"},
		{ID: "ac", From: "a", To: "c", Type: "LINK"},
	}
	graph := newMemoryGraph(1, nodes, edges, nil)
	document, err := cypher.Parse("MATCH (a)-[:LINK*1..2]->(b) RETURN b")
	if err != nil {
		t.Fatal(err)
	}
	pattern := document.Statements[0].(*cypher.QueryStatement).Clauses[0].(*cypher.MatchClause).Patterns[0]
	evaluator := newEvaluator(nil)
	evaluator.paths = &pathExpansionBudget{limit: 1}
	_, err = matchPattern(graph, evaluator, []row{{}}, pattern)
	if err == nil || !strings.Contains(err.Error(), "path expansion limit") {
		t.Fatalf("budget error = %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	evaluator = newEvaluator(nil)
	evaluator.ctx = ctx
	_, err = matchPattern(graph, evaluator, []row{{}}, pattern)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation error = %v", err)
	}
}

func TestCallSubqueryScopeUnitAndUnionSemantics(t *testing.T) {
	engine, _ := testEngine(t)
	result := execute(t, engine, `
UNWIND [1] AS outer
CALL { WITH outer RETURN outer + 1 AS inner }
RETURN outer, inner`, nil)
	if want := [][]any{{int64(1), int64(2)}}; !reflect.DeepEqual(result.Results[0].Rows, want) {
		t.Fatalf("correlated subquery = %#v, want %#v", result.Results[0].Rows, want)
	}

	result = execute(t, engine, "CALL { RETURN 1 AS x UNION ALL RETURN 2 AS x UNION ALL RETURN 3 AS x } RETURN x", nil)
	if want := [][]any{{int64(1)}, {int64(2)}, {int64(3)}}; !reflect.DeepEqual(result.Results[0].Rows, want) {
		t.Fatalf("subquery UNION = %#v, want %#v", result.Results[0].Rows, want)
	}

	result = execute(t, engine, "UNWIND [1] AS outer CALL { CREATE (:SubqueryLocal) } RETURN outer", nil)
	if want := [][]any{{int64(1)}}; !reflect.DeepEqual(result.Results[0].Rows, want) {
		t.Fatalf("unit subquery = %#v, want %#v", result.Results[0].Rows, want)
	}

	for _, query := range []string{
		"UNWIND [1] AS outer CALL { RETURN outer AS leaked } RETURN leaked",
		"UNWIND [1] AS outer CALL { WITH outer RETURN outer } RETURN outer",
	} {
		if _, err := engine.Execute(context.Background(), app.ExecuteRequest{Query: query}); err == nil {
			t.Fatalf("invalid subquery scope succeeded: %s", query)
		}
	}
}

func TestEmptyProcedureRetainsItsSchema(t *testing.T) {
	engine, _ := testEngine(t)
	result := execute(t, engine, "CALL db.labels()", nil)
	if want := []string{"label"}; !reflect.DeepEqual(result.Results[0].Columns, want) || len(result.Results[0].Rows) != 0 {
		t.Fatalf("empty procedure result = %#v", result.Results[0])
	}
}

func TestSemanticErrorsDoNotDependOnRows(t *testing.T) {
	engine, _ := testEngine(t)
	for _, query := range []string{
		"MATCH (:DefinitelyMissing) RETURN missing",
		"UNWIND [] AS value RETURN missing",
		"MATCH (n) WHERE count(n) > 0 RETURN n",
		"RETURN sum(count(*))",
		"UNWIND [1] AS x RETURN x + count(*)",
		"RETURN 1 AS duplicate, 2 AS duplicate",
		"CALL db.labels() YIELD missing RETURN missing",
		"RETURN 1 AS value ORDER BY count(*)",
		"UNWIND [1] AS x RETURN x SKIP x",
	} {
		if _, err := engine.Execute(context.Background(), app.ExecuteRequest{Query: query}); err == nil {
			t.Errorf("semantic-invalid query succeeded: %s", query)
		}
	}
}

func TestStarParticipatesInAggregateGrouping(t *testing.T) {
	engine, _ := testEngine(t)
	result := execute(t, engine, "UNWIND [1, 1, 2] AS x RETURN *, count(*) AS occurrences ORDER BY x", nil)
	want := [][]any{{int64(1), int64(2)}, {int64(2), int64(1)}}
	if !reflect.DeepEqual(result.Results[0].Rows, want) {
		t.Fatalf("rows = %#v, want %#v", result.Results[0].Rows, want)
	}
}

func TestZeroRowPipelinesRetainStaticColumns(t *testing.T) {
	engine, _ := testEngine(t)
	for _, query := range []string{
		"MATCH (n:DefinitelyMissing) RETURN *",
		"MATCH (n:DefinitelyMissing) WITH * RETURN *",
	} {
		result := execute(t, engine, query, nil)
		if want := []string{"n"}; !reflect.DeepEqual(result.Results[0].Columns, want) || len(result.Results[0].Rows) != 0 {
			t.Errorf("%s result = %#v", query, result.Results[0])
		}
	}
	result := execute(t, engine, "CALL db.labels() YIELD label RETURN *", nil)
	if want := []string{"label"}; !reflect.DeepEqual(result.Results[0].Columns, want) || len(result.Results[0].Rows) != 0 {
		t.Fatalf("empty CALL pipeline result = %#v", result.Results[0])
	}
}

func TestMutationShapesFailBeforeDataDependentExecution(t *testing.T) {
	engine, _ := testEngine(t)
	for _, query := range []string{
		"UNWIND [] AS ignored CREATE ()-[:LINK]-()",
		"UNWIND [] AS ignored CREATE ()-[:ONE|:TWO]->()",
		"UNWIND [] AS ignored CREATE ()-[:LINK*1..2]->()",
		"UNWIND [] AS ignored MERGE ()-[:ONE|:TWO]->()",
		"DELETE 1",
		"MERGE (:Task {id: null})",
	} {
		if _, err := engine.Execute(context.Background(), app.ExecuteRequest{Query: query}); err == nil {
			t.Errorf("invalid mutation succeeded: %s", query)
		}
	}
}

func TestNullMutationTargetsAreNoOps(t *testing.T) {
	engine, _ := testEngine(t)
	result := execute(t, engine, "OPTIONAL MATCH (n:DefinitelyMissing) SET n:Seen REMOVE n:Old RETURN n", nil)
	if result.Revision != nil || len(result.Results[0].Rows) != 1 || result.Results[0].Rows[0][0] != nil {
		t.Fatalf("null-target mutation result = %#v", result)
	}
}

func TestCreateDoesNotSilentlyIgnoreDecorationsOnBoundNodes(t *testing.T) {
	engine, _ := testEngine(t)
	execute(t, engine, "CREATE (:Task)", nil)
	_, err := engine.Execute(context.Background(), app.ExecuteRequest{Query: "MATCH (n:Task) CREATE (n:NewLabel)"})
	if err == nil || !strings.Contains(err.Error(), "bound node") {
		t.Fatalf("bound decoration error = %v", err)
	}
}
