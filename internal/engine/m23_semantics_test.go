package engine

import (
	"context"
	"errors"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/svlocks/sheets/internal/app"
	"github.com/svlocks/sheets/internal/cypher"
	"github.com/svlocks/sheets/internal/domain"
	"github.com/svlocks/sheets/internal/domain/temporal"
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

func TestM23PaginationReportsNonConstantRowDependencies(t *testing.T) {
	for _, query := range []string{
		"MATCH (n) RETURN n SKIP n.count",
		"MATCH (n) RETURN n LIMIT n.count",
	} {
		executor, _ := testEngine(t)
		_, err := executor.Execute(context.Background(), app.ExecuteRequest{Query: query})
		if err == nil || !strings.Contains(err.Error(), "non-constant") || strings.Contains(err.Error(), "is not defined") {
			t.Fatalf("pagination error for %q = %v", query, err)
		}
	}

	// A comprehension-local variable is still a constant expression with
	// respect to the input rows and must not be mistaken for a dependency.
	executor, _ := testEngine(t)
	result := execute(t, executor, "RETURN 1 AS value SKIP size([x IN [1] | x]) - 1", nil)
	if got, want := result.Results[0].Rows, [][]any{{int64(1)}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("constant local pagination = %#v, want %#v", got, want)
	}

	for _, test := range []struct {
		value any
		want  string
		not   string
	}{
		{value: 1.5, want: "expects an integer, got float64", not: "non-negative"},
		{value: int64(-1), want: "non-negative", not: "got int64"},
	} {
		_, err := executor.Execute(context.Background(), app.ExecuteRequest{
			Query: "RETURN 1 AS value SKIP $skip", Params: map[string]any{"skip": test.value},
		})
		if err == nil || !strings.Contains(err.Error(), test.want) || strings.Contains(err.Error(), test.not) {
			t.Fatalf("runtime pagination error for %#v = %v", test.value, err)
		}
	}
}

func TestM23SubstringAndIntegerBoundaryArithmetic(t *testing.T) {
	executor, _ := testEngine(t)
	result := execute(t, executor, `
RETURN substring('abc', 1, 9223372036854775807) AS suffix,
       substring(null, null) AS null_source,
       replace('abc', null, 'x') AS null_replace,
       7 / 3 AS pp, -7 / 3 AS np, 7 / -3 AS pn, -7 / -3 AS nn`, nil)
	want := []any{"bc", nil, nil, int64(2), int64(-2), int64(-2), int64(2)}
	if got := result.Results[0].Rows[0]; !reflect.DeepEqual(got, want) {
		t.Fatalf("boundary arithmetic row = %#v, want %#v", got, want)
	}
	for _, query := range []string{
		"RETURN substring('abc', null)",
		"RETURN substring('abc', -1)",
	} {
		_, err := executor.Execute(context.Background(), app.ExecuteRequest{Query: query})
		if err == nil || !strings.Contains(err.Error(), "offset must be a non-negative integer") {
			t.Fatalf("substring offset error for %q = %v", query, err)
		}
	}

	_, err := executor.Execute(context.Background(), app.ExecuteRequest{Query: "RETURN -9223372036854775808 / -1"})
	if err == nil || !strings.Contains(err.Error(), "integer overflow") {
		t.Fatalf("minimum integer division error = %v", err)
	}
}

func TestM23RangeBoundsAndCancellation(t *testing.T) {
	executor, _ := testEngine(t)
	result := execute(t, executor, `
RETURN range(9223372036854775806, 9223372036854775807) AS high,
       range(-9223372036854775808, -9223372036854775807) AS low`, nil)
	want := []any{
		[]any{int64(9223372036854775806), int64(9223372036854775807)},
		[]any{int64(-9223372036854775808), int64(-9223372036854775807)},
	}
	if got := result.Results[0].Rows[0]; !reflect.DeepEqual(got, want) {
		t.Fatalf("boundary ranges = %#v, want %#v", got, want)
	}
	_, err := executor.Execute(context.Background(), app.ExecuteRequest{
		Query: "RETURN range(-9223372036854775808, 9223372036854775807)",
	})
	if err == nil || !strings.Contains(err.Error(), "too large") {
		t.Fatalf("huge range error = %v", err)
	}
	for _, test := range []struct {
		query string
		want  string
		not   string
	}{
		{query: "RETURN range(true, 1, 1)", want: "start expects an integer, got bool", not: "non-zero"},
		{query: "RETURN range(0, 1, 0)", want: "step must be non-zero", not: "got int64"},
	} {
		_, err := executor.Execute(context.Background(), app.ExecuteRequest{Query: test.query})
		if err == nil || !strings.Contains(err.Error(), test.want) || strings.Contains(err.Error(), test.not) {
			t.Fatalf("range diagnostic for %q = %v", test.query, err)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = integerRange(ctx, &cypher.Literal{}, []any{int64(0), int64(1_000_000)})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled range error = %v", err)
	}
}

func TestM23NestedRandInsideAggregateIsNonConstant(t *testing.T) {
	queries := []string{
		"RETURN count([x IN [1] | rand()])",
		"RETURN count(any(x IN [1] WHERE rand() > 0.5))",
		"RETURN count(reduce(total = 0, x IN [1] | total + rand()))",
		"MATCH (a) RETURN count([(a)-->(b) | rand()])",
	}
	for _, query := range queries {
		executor, _ := testEngine(t)
		_, err := executor.Execute(context.Background(), app.ExecuteRequest{Query: query})
		if err == nil || !strings.Contains(err.Error(), "non-constant") {
			t.Fatalf("nested rand error for %q = %v", query, err)
		}
	}
}

func TestM23TypedNilEntitiesBehaveAsNull(t *testing.T) {
	executor, _ := testEngine(t)
	result := execute(t, executor, `
RETURN id($node), elementId($relationship), labels($node), type($relationship),
       body($node), properties($node), keys($relationship),
       startNode($relationship), endNode($relationship), $node['missing']`, map[string]any{
		"node":         (*domain.Node)(nil),
		"relationship": (*domain.Edge)(nil),
	})
	if got, want := result.Results[0].Rows[0], make([]any, 10); !reflect.DeepEqual(got, want) {
		t.Fatalf("typed nil entity row = %#v, want nulls", got)
	}
}

func TestM23DeletedEntityCannotBeUsedAsAPropertyMap(t *testing.T) {
	for _, query := range []string{
		"CREATE (source {x:1}), (target) WITH source, target DELETE source SET target = source RETURN target",
		"CREATE (source {body:'x'}) WITH source DELETE source RETURN body(source)",
	} {
		executor, _ := testEngine(t)
		_, err := executor.Execute(context.Background(), app.ExecuteRequest{Query: query})
		if err == nil || !strings.Contains(err.Error(), "deleted entity") {
			t.Fatalf("deleted entity error for %q = %v", query, err)
		}
		result := execute(t, executor, "MATCH (n) RETURN count(n)", nil)
		if got := result.Results[0].Rows[0][0]; got != int64(0) {
			t.Fatalf("failed mutation for %q leaked %v nodes", query, got)
		}
	}
}

func TestM23TotalOrderIsAntisymmetricAndTransitive(t *testing.T) {
	date, err := temporal.ParseDate("1984-10-11")
	if err != nil {
		t.Fatal(err)
	}
	localTime, err := temporal.ParseLocalTime("12:31:14")
	if err != nil {
		t.Fatal(err)
	}
	offsetTime, err := temporal.ParseTime("12:31:14+01:00")
	if err != nil {
		t.Fatal(err)
	}
	localDateTime, err := temporal.ParseLocalDateTime("1984-10-11T12:31:14")
	if err != nil {
		t.Fatal(err)
	}
	dateTime, err := temporal.ParseDateTime("1984-10-11T12:31:14Z")
	if err != nil {
		t.Fatal(err)
	}
	duration, err := temporal.ParseDuration("P1M2DT3S")
	if err != nil {
		t.Fatal(err)
	}
	values := []any{
		map[string]any{}, map[string]any{"x": int64(1)}, map[string]any{"x": int64(2)},
		domain.Node{ID: "n1"}, domain.Node{ID: "n2"},
		domain.Edge{ID: "r1"}, domain.Edge{ID: "r2"},
		[]any{}, []any{int64(1)}, []any{int64(1), "x"},
		PathValue{}, PathValue{Nodes: []domain.Node{{ID: "n1"}}},
		time.Unix(0, 0).UTC(), time.Unix(1, 0).UTC(), date, localTime, offsetTime, localDateTime, dateTime,
		time.Second, 2 * time.Second, duration,
		"", "x", false, true, int64(-1), float64(-1), int64(0), math.NaN(), nil,
	}
	sign := func(value int) int {
		switch {
		case value < 0:
			return -1
		case value > 0:
			return 1
		default:
			return 0
		}
	}
	for left := range values {
		for right := range values {
			forward := sign(compareOrderValues(values[left], values[right]))
			reverse := sign(compareOrderValues(values[right], values[left]))
			if forward != -reverse {
				t.Fatalf("order is not antisymmetric for [%d]=%#v and [%d]=%#v: %d / %d", left, values[left], right, values[right], forward, reverse)
			}
			for last := range values {
				if forward <= 0 && compareOrderValues(values[right], values[last]) <= 0 && compareOrderValues(values[left], values[last]) > 0 {
					t.Fatalf("order is not transitive for [%d]=%#v, [%d]=%#v, [%d]=%#v", left, values[left], right, values[right], last, values[last])
				}
			}
		}
	}
}
