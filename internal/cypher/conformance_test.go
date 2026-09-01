package cypher

import (
	"errors"
	"strings"
	"testing"
)

func TestTCKCapabilityCases(t *testing.T) {
	tests := []struct {
		name        string
		query       string
		unsupported string
		wantError   bool
	}{
		{name: "match", query: "MATCH (n) RETURN n"},
		{name: "return", query: "MATCH (n) RETURN n"},
		{name: "comparison", query: "RETURN null = null AS value"},
		{
			name:        "pattern_comprehension",
			query:       "MATCH (n) RETURN [p = (n)-->() | p] AS list",
			unsupported: "pattern comprehension",
		},
		{
			name:        "bidirectional_relationship",
			query:       "CREATE (a)<-[:FOO]->(b)",
			unsupported: "bidirectional relationship pattern",
		},
		{
			name:        "unsupported_function",
			query:       "RETURN abs(-1)",
			unsupported: "function invocation",
		},
		{
			name:        "unsupported_procedure",
			query:       "CALL test.my.proc()",
			unsupported: "procedure invocation",
		},
		{
			name:        "distinct_scalar_function",
			query:       "RETURN size(DISTINCT [1, 1])",
			unsupported: "DISTINCT scalar function",
		},
		{name: "integer_overflow", query: "RETURN 9223372036854775808 AS literal", wantError: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := Parse(test.query)
			if test.unsupported != "" {
				var unsupported *UnsupportedFeatureError
				if !errors.As(err, &unsupported) {
					t.Fatalf("error = %v (%T), want *UnsupportedFeatureError", err, err)
				}
				if unsupported.Feature != test.unsupported || unsupported.Location().Start.Offset < 0 {
					t.Fatalf("unsupported error = %#v", unsupported)
				}
				return
			}
			if test.wantError {
				if err == nil {
					t.Fatal("Parse() succeeded, want rejection")
				}
				var unsupported *UnsupportedFeatureError
				if errors.As(err, &unsupported) {
					t.Fatalf("error = %v, want syntax/value rejection", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestDocumentSplitterUsesGeneratedLexicalSyntax(t *testing.T) {
	source := "RETURN ';' AS semi, 1 AS `a;b`; // comment ; is trivia\n" +
		"RETURN \"x;y\" AS value /* block ; comment */; RETURN 3"
	document, err := Parse(source)
	if err != nil {
		t.Fatal(err)
	}
	if len(document.Statements) != 3 {
		t.Fatalf("statement count = %d, want 3", len(document.Statements))
	}
	second := document.Statements[1].(*QueryStatement)
	if second.Location().Start.Line != 2 || second.Location().Start.Column != 1 {
		t.Fatalf("second statement starts at %#v", second.Location().Start)
	}
}

func TestOfficialUnicodeWhitespaceCommentsAndEscapes(t *testing.T) {
	source := "// leading comment\nRETURN\u00a0'\\u263A' AS `smile``key`, α AS unicode"
	document, err := Parse(source)
	if err != nil {
		t.Fatal(err)
	}
	query := document.Statements[0].(*QueryStatement)
	projection := query.Clauses[0].(*ProjectionClause)
	literal := projection.Items[0].Expression.(*Literal)
	if literal.Value != "☺" || projection.Items[0].Alias.Name != "smile`key" || !projection.Items[0].Alias.BacktickQuoted {
		t.Fatalf("decoded item = %#v / %#v", literal, projection.Items[0].Alias)
	}
	variable := projection.Items[1].Expression.(*Variable)
	if variable.Name.Name != "α" {
		t.Fatalf("Unicode variable = %#v", variable)
	}
	if query.Location().Start.Line != 2 || query.Location().Start.Offset != strings.Index(source, "RETURN") {
		t.Fatalf("query span = %#v", query.Location())
	}
}

func TestRecognizedUnsupportedDoesNotHideLaterStatements(t *testing.T) {
	document, err := Parse("RETURN [(a)-->() | a]; RETURN 2")
	if err == nil {
		t.Fatal("Parse() succeeded")
	}
	var unsupported *UnsupportedFeatureError
	if !errors.As(err, &unsupported) {
		t.Fatalf("error = %T %v, want unsupported feature", err, err)
	}
	if len(document.Statements) != 1 || document.Statements[0].Location().Start.Offset <= unsupported.Location().Start.Offset {
		t.Fatalf("recovered document = %#v", document.Statements)
	}
}

func TestUnsupportedFeatureSpanIsTheExactRecognizedConstruct(t *testing.T) {
	source := "RETURN 1; RETURN [p = (n)-->() | p] AS paths; RETURN 3"
	document, err := Parse(source)
	var unsupported *UnsupportedFeatureError
	if !errors.As(err, &unsupported) {
		t.Fatalf("error = %T %v, want unsupported feature", err, err)
	}
	span := unsupported.Location()
	if got := source[span.Start.Offset:span.End.Offset]; got != "[p = (n)-->() | p]" {
		t.Fatalf("unsupported span = %q (%#v)", got, span)
	}
	if len(document.Statements) != 2 {
		t.Fatalf("recovered statements = %#v", document.Statements)
	}
}

func TestMalformedAmbiguousInputsAreRejected(t *testing.T) {
	for _, source := range []string{
		"RETURN CASE WHEN true THEN 1",
		"MATCH (a)<-[:R]->(b) RETURN a",
		"RETURN [x IN [1, 2] WHERE | x]",
		"RETURN EXISTS { CREATE () }",
		"RETURN 1 /* unterminated",
	} {
		t.Run(source, func(t *testing.T) {
			if _, err := Parse(source); err == nil {
				t.Fatalf("Parse(%q) succeeded", source)
			}
		})
	}
}

func TestExpressionSpansRetainParenthesizedSource(t *testing.T) {
	for _, source := range []string{
		"RETURN (n:Foo)",
		"RETURN (list[1]).missing",
		"RETURN 12 / 4 * (3 - 2 * 4)",
	} {
		document, err := Parse(source)
		if err != nil {
			t.Fatalf("Parse(%q): %v", source, err)
		}
		projection := document.Statements[0].(*QueryStatement).Clauses[0].(*ProjectionClause)
		span := projection.Items[0].Expression.Location()
		got := source[span.Start.Offset:span.End.Offset]
		want := strings.TrimPrefix(source, "RETURN ")
		if got != want {
			t.Fatalf("Parse(%q) expression source = %q, want %q (span %#v)", source, got, want, span)
		}
	}
}

func TestLexicalErrorsDoNotBecomeEmptyStatements(t *testing.T) {
	for _, source := range []string{"\x00", "'", "/* unterminated"} {
		if document, err := Parse(source); err == nil {
			t.Errorf("Parse(%q) = %#v, nil error", source, document)
		}
	}
	document, err := Parse("\x00; RETURN 2")
	if err == nil || len(document.Statements) != 1 {
		t.Fatalf("lexical recovery = %#v, %v", document, err)
	}
	projection := document.Statements[0].(*QueryStatement).Clauses[0].(*ProjectionClause)
	if got := projection.Items[0].Expression.(*Literal).Value; got != int64(2) {
		t.Fatalf("recovered literal = %#v", got)
	}
}

func TestContextualNamesAndReservedParametersRetainTheirText(t *testing.T) {
	source := "MATCH (n:MATCH {return: $skip}) RETURN n.return AS all, $PROFILE AS p, $OFFSET AS o"
	document, err := Parse(source)
	if err != nil {
		t.Fatal(err)
	}
	query := document.Statements[0].(*QueryStatement)
	match := query.Clauses[0].(*MatchClause)
	if got := match.Patterns[0].Element.Nodes[0].Labels[0].Name; got != "MATCH" {
		t.Fatalf("contextual label = %q", got)
	}
	properties := match.Patterns[0].Element.Nodes[0].Properties.(*MapLiteral)
	if properties.Entries[0].Key.Name != "return" || properties.Entries[0].Value.(*Parameter).Name.Name != "skip" {
		t.Fatalf("contextual property/parameter = %#v", properties.Entries[0])
	}
	projection := query.Clauses[1].(*ProjectionClause)
	property := projection.Items[0].Expression.(*PropertyExpression)
	if property.Property.Name != "return" || projection.Items[0].Alias.Name != "all" {
		t.Fatalf("contextual projection = %#v", projection.Items[0])
	}
	if projection.Items[1].Expression.(*Parameter).Name.Name != "PROFILE" ||
		projection.Items[2].Expression.(*Parameter).Name.Name != "OFFSET" {
		t.Fatalf("reserved parameter case was lost: %#v", projection.Items)
	}
}

func TestNestedNotSpansStartAtTheirOwnOperator(t *testing.T) {
	source := "RETURN NOT NOT true"
	document, err := Parse(source)
	if err != nil {
		t.Fatal(err)
	}
	outer := document.Statements[0].(*QueryStatement).Clauses[0].(*ProjectionClause).Items[0].Expression.(*UnaryExpression)
	inner := outer.Expression.(*UnaryExpression)
	if got := source[outer.Span.Start.Offset:outer.Span.End.Offset]; got != "NOT NOT true" {
		t.Fatalf("outer NOT span = %q", got)
	}
	if got := source[inner.Span.Start.Offset:inner.Span.End.Offset]; got != "NOT true" {
		t.Fatalf("inner NOT span = %q", got)
	}
}

func TestFourDigitUppercaseUnicodeEscape(t *testing.T) {
	document, err := Parse(`RETURN '\U263A'`)
	if err != nil {
		t.Fatal(err)
	}
	literal := document.Statements[0].(*QueryStatement).Clauses[0].(*ProjectionClause).Items[0].Expression.(*Literal)
	if literal.Value != "☺" {
		t.Fatalf("literal value = %#v", literal.Value)
	}
}
