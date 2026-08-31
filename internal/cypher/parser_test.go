package cypher

import (
	"errors"
	"strings"
	"testing"
)

func TestParseReadPipeline(t *testing.T) {
	source := `MATCH p = (a:Person {name: $name})-[r:KNOWS*1..3]->(b:Person)
WHERE a.age >= $minimumAge AND b.name STARTS WITH 'A'
WITH DISTINCT a, count(DISTINCT b) AS degree
WHERE degree > 0
RETURN a.name AS name, degree ORDER BY degree DESC SKIP $skip LIMIT 10`

	document, err := Parse(source)
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if len(document.Statements) != 1 {
		t.Fatalf("statement count = %d, want 1", len(document.Statements))
	}
	statement := document.Statements[0]
	if statement.IsMutation() || !statement.IsReadOnly() || !statement.Info().ReadOnly {
		t.Fatal("read pipeline was classified as mutation")
	}
	query := statement.(*QueryStatement)
	if len(query.Clauses) != 3 {
		t.Fatalf("clause count = %d, want 3", len(query.Clauses))
	}
	match := query.Clauses[0].(*MatchClause)
	if len(match.Patterns) != 1 || match.Patterns[0].Variable.Name != "p" {
		t.Fatalf("unexpected path pattern: %#v", match.Patterns)
	}
	relationship := match.Patterns[0].Element.Relationships[0]
	if relationship.Direction != Outgoing || relationship.Length == nil || relationship.Length.Lower == nil || relationship.Length.Upper == nil {
		t.Fatalf("variable length relationship not retained: %#v", relationship)
	}
	with := query.Clauses[1].(*ProjectionClause)
	if !with.With || !with.Distinct || with.Where == nil || len(with.Items) != 2 {
		t.Fatalf("unexpected WITH: %#v", with)
	}
	ret := query.Clauses[2].(*ProjectionClause)
	if ret.With || len(ret.OrderBy) != 1 || !ret.OrderBy[0].Descending || ret.Skip == nil || ret.Limit == nil {
		t.Fatalf("unexpected RETURN modifiers: %#v", ret)
	}
}

func TestParseMutatingClausesAndMergeActions(t *testing.T) {
	source := `CREATE (n:Task {id: $id, tags: ['a', 'b']});
MATCH (n:Task {id: $id})
MERGE (n)-[r:ASSIGNED_TO]->(u:User {id: $userID})
ON CREATE SET r.createdAt = datetime(), n:Active
ON MATCH SET r.count = coalesce(r.count, 0) + 1
SET n += $properties, n.title = 'updated'
REMOVE n.obsolete, n:Deprecated
DETACH DELETE r`

	document, err := Parse(source)
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if len(document.Statements) != 2 {
		t.Fatalf("statement count = %d, want 2", len(document.Statements))
	}
	for i, statement := range document.Statements {
		if !statement.IsMutation() || !statement.Info().Mutates {
			t.Errorf("statement %d was not classified as mutating", i)
		}
	}
	query := document.Statements[1].(*QueryStatement)
	merge, ok := query.Clauses[1].(*MergeClause)
	if !ok || len(merge.Actions) != 2 || merge.Actions[0].Kind != OnCreate || merge.Actions[1].Kind != OnMatch {
		t.Fatalf("unexpected MERGE AST: %#v", query.Clauses[1])
	}
	set := query.Clauses[2].(*SetClause)
	if len(set.Items) != 2 || set.Items[0].Operator != "+=" || set.Items[1].Operator != "=" {
		t.Fatalf("unexpected SET AST: %#v", set)
	}
	remove := query.Clauses[3].(*RemoveClause)
	if len(remove.Items) != 2 || len(remove.Items[1].Labels) != 1 {
		t.Fatalf("unexpected REMOVE AST: %#v", remove)
	}
	deleteClause := query.Clauses[4].(*DeleteClause)
	if !deleteClause.Detach {
		t.Fatal("DETACH DELETE lost detach flag")
	}
}

func TestExpressionsAndProcedureCalls(t *testing.T) {
	source := "UNWIND [x IN $values WHERE x IS NOT NULL | x * 2] AS value " +
		"WITH value, CASE WHEN value IN [2, 4] THEN {kind: 'small', value: value} ELSE {kind: 'large'} END AS result " +
		"CALL db.labels() YIELD label AS label WHERE label CONTAINS 'Task' " +
		"RETURN result, label, any(x IN $values WHERE x = value) AS present, reduce(total = 0, x IN $values | total + x) AS total"
	document, err := Parse(source)
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	query := document.Statements[0].(*QueryStatement)
	if len(query.Clauses) != 4 {
		t.Fatalf("clause count = %d, want 4", len(query.Clauses))
	}
	unwind := query.Clauses[0].(*UnwindClause)
	if _, ok := unwind.Expression.(*ListComprehension); !ok {
		t.Fatalf("UNWIND expression = %T, want *ListComprehension", unwind.Expression)
	}
	with := query.Clauses[1].(*ProjectionClause)
	if _, ok := with.Items[1].Expression.(*CaseExpression); !ok {
		t.Fatalf("CASE expression = %T, want *CaseExpression", with.Items[1].Expression)
	}
	call := query.Clauses[2].(*CallClause)
	if call.Procedure.String() != "db.labels" || len(call.Yield) != 1 || call.YieldWhere == nil {
		t.Fatalf("unexpected CALL AST: %#v", call)
	}
	if !document.Statements[0].IsMutation() {
		t.Fatal("CALL should be conservatively classified as mutating")
	}
	ret := query.Clauses[3].(*ProjectionClause)
	if _, ok := ret.Items[2].Expression.(*ListPredicate); !ok {
		t.Fatalf("list predicate = %T, want *ListPredicate", ret.Items[2].Expression)
	}
	if _, ok := ret.Items[3].Expression.(*ReduceExpression); !ok {
		t.Fatalf("reduce expression = %T, want *ReduceExpression", ret.Items[3].Expression)
	}
}

func FuzzParseNeverPanics(f *testing.F) {
	for _, source := range []string{
		"MATCH (n:Task)-[:CHILD*0..]->(m) RETURN n, m",
		"CREATE (n {title:$title}) SET n.status = 'ready' RETURN n",
		"CALL db.labels() YIELD label RETURN label",
		"RETURN [x IN $values WHERE x <> null | x * 2]",
		"MATCH (n) WHERE EXISTS { MATCH (n)-[:BLOCKED_BY]->() } RETURN n",
		"'unterminated",
		"\x00\xff",
	} {
		f.Add(source)
	}
	f.Fuzz(func(t *testing.T, source string) {
		_, _ = Parse(source)
	})
}

func TestCallAndExistsSubqueriesAndPatterns(t *testing.T) {
	source := `CALL {
  MATCH (a)-[:KNOWS]->(b)
  WHERE EXISTS { MATCH (b)-[:KNOWS]->(:Person) }
  RETURN a, b
}
WITH a, shortestPath((a)-[:KNOWS*..5]->(b)) AS path
RETURN path`
	document, err := Parse(source)
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	query := document.Statements[0].(*QueryStatement)
	call := query.Clauses[0].(*CallClause)
	if call.Subquery == nil || len(call.Subquery.Clauses) != 2 {
		t.Fatalf("subquery missing or malformed: %#v", call)
	}
	with := query.Clauses[1].(*ProjectionClause)
	function, ok := with.Items[1].Expression.(*FunctionInvocation)
	if !ok || len(function.Arguments) != 1 {
		t.Fatalf("shortestPath AST = %#v", with.Items[1].Expression)
	}
	if _, ok := function.Arguments[0].(*PatternExpression); !ok {
		t.Fatalf("shortestPath argument = %T, want *PatternExpression", function.Arguments[0])
	}
}

func TestMultipleStatementsAndPositions(t *testing.T) {
	document, err := Parse("RETURN 1;\nCREATE (:Task {title: 'x'}); ; RETURN $value")
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if len(document.Statements) != 3 {
		t.Fatalf("statement count = %d, want 3", len(document.Statements))
	}
	if document.Statements[0].Location().Start.Line != 1 || document.Statements[1].Location().Start.Line != 2 {
		t.Fatalf("bad spans: %#v / %#v", document.Statements[0].Location(), document.Statements[1].Location())
	}
	if document.Statements[0].IsMutation() || !document.Statements[1].IsMutation() || document.Statements[2].IsMutation() {
		t.Fatal("incorrect statement mutation classification")
	}
}

func TestUnionAndRelationshipTypeAlternatives(t *testing.T) {
	document, err := Parse("MATCH (a)-[:KNOWS|:FOLLOWS*2]->(b) RETURN a UNION ALL MATCH (n:Person) RETURN n")
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	query := document.Statements[0].(*QueryStatement)
	if len(query.UnionBranches) != 1 || !query.UnionBranches[0].All {
		t.Fatalf("UNION ALL missing: %#v", query.UnionBranches)
	}
	relationship := query.Clauses[0].(*MatchClause).Patterns[0].Element.Relationships[0]
	if len(relationship.Types) != 2 || relationship.Length == nil || !relationship.Length.Exact {
		t.Fatalf("relationship alternatives/length missing: %#v", relationship)
	}
}

func TestParseErrorsCarrySourcePositionAndRecover(t *testing.T) {
	document, err := Parse("MATCH (n RETURN n; CREATE (:Task)")
	if err == nil {
		t.Fatal("Parse() error = nil, want syntax error")
	}
	var parseError *ParseError
	if !errors.As(err, &parseError) {
		t.Fatalf("error type = %T, want *ParseError or wrapped one", err)
	}
	if parseError.Position.Line != 1 || parseError.Position.Column < 1 || !strings.Contains(parseError.Message, "expected") {
		t.Fatalf("unexpected parse error: %#v", parseError)
	}
	if len(document.Statements) != 1 || !document.Statements[0].IsMutation() {
		t.Fatalf("parser did not recover after a bad statement: %#v", document.Statements)
	}
}

func TestReturnMustBeFinal(t *testing.T) {
	_, err := Parse("RETURN 1 MATCH (n) RETURN n")
	if err == nil || !strings.Contains(err.Error(), "RETURN must be the final clause") {
		t.Fatalf("error = %v, want final RETURN validation", err)
	}
}

func TestNotBindsAroundComparison(t *testing.T) {
	document, err := Parse("MATCH (n) WHERE NOT n.active = true RETURN n")
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	where := document.Statements[0].(*QueryStatement).Clauses[0].(*MatchClause).Where
	not, ok := where.(*UnaryExpression)
	if !ok || not.Operator != "NOT" {
		t.Fatalf("WHERE AST = %#v, want NOT expression", where)
	}
	if _, ok := not.Expression.(*BinaryExpression); !ok {
		t.Fatalf("NOT operand = %T, want *BinaryExpression", not.Expression)
	}
}
