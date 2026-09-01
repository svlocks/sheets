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
	} {
		executor, _ := testEngine(t)
		_, err := executor.Execute(context.Background(), app.ExecuteRequest{Query: test.query})
		if err == nil || !strings.Contains(err.Error(), test.want) {
			t.Fatalf("compile error for %q = %v, want %q", test.query, err, test.want)
		}
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
