package engine

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/svlocks/sheets/internal/app"
)

func TestM23ScalarCoercionAndOrderingRegressions(t *testing.T) {
	executor, _ := testEngine(t)
	result := execute(t, executor, `
RETURN 7 / 3 AS integerDivision,
       7 / 2.0 AS floatDivision,
       toInteger('2.9') AS decimalString,
       properties(null) AS nullProperties,
       1 STARTS WITH 1 AS nonStringPredicate`, nil)
	want := []any{int64(2), 3.5, int64(2), nil, nil}
	if got := result.Results[0].Rows[0]; !reflect.DeepEqual(got, want) {
		t.Fatalf("scalar row = %#v, want %#v", got, want)
	}

	result = execute(t, executor, `
UNWIND [1, 'a', null, [1, 2], 0.2, 'b'] AS x
RETURN min(x), max(x)`, nil)
	want = []any{[]any{int64(1), int64(2)}, int64(1)}
	if got := result.Results[0].Rows[0]; !reflect.DeepEqual(got, want) {
		t.Fatalf("aggregate ordering row = %#v, want %#v", got, want)
	}
}

func TestM23DynamicEntityLookupAndMergeCreationRegressions(t *testing.T) {
	executor, _ := testEngine(t)
	result := execute(t, executor, `
CREATE (a {id: 2, name: 'A'}), (b {id: 1, name: 'B'})
MERGE (a)-[r:KNOWS]-(b)
  ON CREATE SET r = a
RETURN startNode(r).id, endNode(r).id, r['name']`, nil)
	want := []any{int64(2), int64(1), "A"}
	if got := result.Results[0].Rows[0]; !reflect.DeepEqual(got, want) {
		t.Fatalf("MERGE row = %#v, want %#v", got, want)
	}
}

func TestM23EmptyVariableLengthIntervalsProduceNoMatches(t *testing.T) {
	executor, _ := testEngine(t)
	execute(t, executor, "CREATE (:A {name:'a'}), (:B {name:'b'})", nil)
	result := execute(t, executor, `
MATCH (a:A)
MATCH (a)-[:LIKES*2..1]->(c)
RETURN c`, nil)
	if got := result.Results[0].Rows; len(got) != 0 {
		t.Fatalf("empty interval rows = %#v", got)
	}
	result = execute(t, executor, `
MATCH (a:A), (b:B)
OPTIONAL MATCH (a)-[r*]-(b)
RETURN r`, nil)
	if got, want := result.Results[0].Rows, [][]any{{nil}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("optional empty expansion rows = %#v, want %#v", got, want)
	}
}

func TestM23DeletedEntityAndInvalidPropertyErrors(t *testing.T) {
	for _, query := range []string{
		"CREATE (n {num: 1}) WITH n DELETE n RETURN n.num",
		"CREATE (n) WITH n DELETE n RETURN labels(n)",
		"CREATE ()-[r:R]->() WITH r DELETE r RETURN r.num",
	} {
		executor, _ := testEngine(t)
		_, err := executor.Execute(context.Background(), app.ExecuteRequest{Query: query})
		if err == nil || !strings.Contains(err.Error(), "deleted entity") {
			t.Fatalf("deleted entity error for %q = %v", query, err)
		}
	}

	executor, _ := testEngine(t)
	_, err := executor.Execute(context.Background(), app.ExecuteRequest{
		Query: "CREATE (a) SET a.maplist = [{num: 1}]",
	})
	if err == nil || !strings.Contains(err.Error(), "invalid property type") {
		t.Fatalf("invalid property error = %v", err)
	}
	result := execute(t, executor, "MATCH (n) RETURN count(n)", nil)
	if got := result.Results[0].Rows[0][0]; got != int64(0) {
		t.Fatalf("failed property mutation was not rolled back: count = %#v", got)
	}
}

func TestM23WithPredicateAndDistinctOrderingScopes(t *testing.T) {
	executor, _ := testEngine(t)
	execute(t, executor, "CREATE ({name:'C'}), ({name:'A'}), ({name:'B'})", nil)
	result := execute(t, executor, `
MATCH (n)
WITH DISTINCT n.name AS name
ORDER BY n.name
WHERE n.name <> 'B'
RETURN name`, nil)
	if got, want := result.Results[0].Rows, [][]any{{"A"}, {"C"}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("WITH input scope rows = %#v, want %#v", got, want)
	}

	result = execute(t, executor, `
MATCH (n)
WITH n.name AS name
WHERE name = 'A' OR n.name = 'C'
RETURN name ORDER BY name`, nil)
	if got, want := result.Results[0].Rows, [][]any{{"A"}, {"C"}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("WITH combined scope rows = %#v, want %#v", got, want)
	}
}

func TestM23CompileTimeProjectionAndPatternErrors(t *testing.T) {
	for _, test := range []struct {
		query string
		want  string
	}{
		{"MATCH (a)-[r]->()-[r]->(a) RETURN r", "relationship variable"},
		{"MATCH (a) WITH a, count(*) RETURN a", "must be aliased"},
		{"MATCH (n) WHERE (n) RETURN n", "expects a boolean"},
		{"MATCH (a) RETURN foo(a)", "unknown function foo"},
		{"CREATE (a)<-[:R]->(b)", "must have a direction"},
		{"MATCH (n) WHERE EXISTS { MATCH (n)-->(m) SET m.x = 1 } RETURN n", "EXISTS subquery cannot contain updating clauses"},
		{"MATCH (a) RETURN [(a)-->(b) WHERE 1 | b]", "expects a boolean"},
		{"MATCH p = (a)-->(b) RETURN [p = (a)-->(b) | p]", "already bound"},
	} {
		executor, _ := testEngine(t)
		_, err := executor.Execute(context.Background(), app.ExecuteRequest{Query: test.query})
		if err == nil || !strings.Contains(err.Error(), test.want) {
			t.Fatalf("compile error for %q = %v, want %q", test.query, err, test.want)
		}
	}
}

func TestM23BidirectionalMatchTraversesBothDirections(t *testing.T) {
	executor, _ := testEngine(t)
	execute(t, executor, "CREATE (a:A)-[:R]->(b:B), (b)-[:R]->(a)", nil)
	result := execute(t, executor, "MATCH (a:A)<-[:R]->(b:B) RETURN a, b", nil)
	if len(result.Results[0].Rows) != 2 {
		t.Fatalf("bidirectional relationship rows = %#v, want both directed edges", result.Results[0].Rows)
	}
}

func TestM23ImmediateWithLimitBoundsUnwind(t *testing.T) {
	executor, _ := testEngine(t)
	result := execute(t, executor, `
UNWIND range(1, 200000) AS i
WITH i LIMIT 2
RETURN sum(i)`, nil)
	if got := result.Results[0].Rows[0][0]; got != int64(3) {
		t.Fatalf("bounded UNWIND sum = %#v, want 3", got)
	}
}

func TestM23CommonScalarFunctions(t *testing.T) {
	executor, _ := testEngine(t)
	result := execute(t, executor, `
RETURN split('a😀b', '😀') AS parts,
	   reverse([1, 2, 3]) AS reversedList,
	   abs(-3) AS integerAbs,
       abs(-3.5) AS floatAbs,
       ceil(1.2) AS ceiling,
       sqrt(12.96) AS squareRoot,
       sign(-0.5) AS negativeSign,
       sign(0) AS zeroSign,
       sign(3) AS positiveSign`, nil)
	want := []any{
		[]any{"a", "b"}, []any{int64(3), int64(2), int64(1)}, int64(3), 3.5, 2.0, 3.6,
		int64(-1), int64(0), int64(1),
	}
	if got := result.Results[0].Rows[0]; !reflect.DeepEqual(got, want) {
		t.Fatalf("scalar functions = %#v, want %#v", got, want)
	}

	result = execute(t, executor, `
RETURN split(null, ',') AS splitValue,
       abs(null) AS absValue,
       ceil(null) AS ceilValue,
       sqrt(null) AS sqrtValue,
       sign(null) AS signValue`, nil)
	if got, want := result.Results[0].Rows[0], []any{nil, nil, nil, nil, nil}; !reflect.DeepEqual(got, want) {
		t.Fatalf("null scalar functions = %#v, want %#v", got, want)
	}

	result = execute(t, executor, "UNWIND range(1, 64) AS i RETURN rand() AS value", nil)
	seen := make(map[float64]struct{}, len(result.Results[0].Rows))
	for _, values := range result.Results[0].Rows {
		value, ok := values[0].(float64)
		if !ok || value < 0 || value >= 1 {
			t.Fatalf("rand() value = %#v", values[0])
		}
		seen[value] = struct{}{}
	}
	if len(seen) < 2 {
		t.Fatalf("rand() did not vary across rows: %#v", seen)
	}
}

func TestM23ScalarFunctionErrors(t *testing.T) {
	executor, _ := testEngine(t)
	for _, test := range []struct {
		query string
		want  string
	}{
		{"RETURN abs(-9223372036854775808)", "out of integer range"},
		{"RETURN split(1, ',')", "split expects strings"},
		{"RETURN count(rand())", "non-constant"},
	} {
		_, err := executor.Execute(context.Background(), app.ExecuteRequest{Query: test.query})
		if err == nil || !strings.Contains(err.Error(), test.want) {
			t.Fatalf("error for %q = %v, want %q", test.query, err, test.want)
		}
	}
}
