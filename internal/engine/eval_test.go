package engine

import (
	"errors"
	"math"
	"reflect"
	"testing"

	"github.com/svlocks/sheets/internal/cypher"
	"github.com/svlocks/sheets/internal/domain"
)

var testSpan = cypher.Span{
	Start: cypher.Position{Line: 2, Column: 4},
	End:   cypher.Position{Line: 2, Column: 5},
}

func literal(value any) *cypher.Literal {
	kind := cypher.IntegerLiteral
	switch value.(type) {
	case nil:
		kind = cypher.NullLiteral
	case bool:
		kind = cypher.BooleanLiteral
	case float64:
		kind = cypher.FloatLiteral
	case string:
		kind = cypher.StringLiteral
	}
	return &cypher.Literal{Span: testSpan, Kind: kind, Value: value}
}

func variable(name string) *cypher.Variable {
	return &cypher.Variable{Span: testSpan, Name: cypher.Identifier{Name: name}}
}

func binary(left cypher.Expression, operator string, right cypher.Expression) *cypher.BinaryExpression {
	return &cypher.BinaryExpression{Span: testSpan, Left: left, Operator: operator, Right: right}
}

func function(name string, arguments ...cypher.Expression) *cypher.FunctionInvocation {
	return &cypher.FunctionInvocation{
		Span:      testSpan,
		Name:      cypher.QualifiedName{Parts: []cypher.Identifier{{Name: name}}},
		Arguments: arguments,
	}
}

func TestEvaluatorArithmeticAndNullLogic(t *testing.T) {
	evaluator := newEvaluator(nil)
	tests := []struct {
		name       string
		expression cypher.Expression
		want       any
	}{
		{"integer arithmetic", binary(literal(int64(6)), "*", literal(int64(7))), int64(42)},
		{"float arithmetic", binary(literal(int64(3)), "/", literal(int64(2))), 1.5},
		{"numeric equality", binary(literal(int64(3)), "=", literal(float64(3))), true},
		{"large integer inequality", binary(literal(int64(math.MaxInt64)), "=", literal(int64(math.MaxInt64-1))), false},
		{"null equality", binary(literal(nil), "=", literal(nil)), nil},
		{"null and false", binary(literal(nil), "AND", literal(false)), false},
		{"null or true", binary(literal(nil), "OR", literal(true)), true},
		{"membership", binary(literal(int64(2)), "IN", &cypher.ListLiteral{Span: testSpan, Elements: []cypher.Expression{literal(int64(1)), literal(int64(2))}}), true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := evaluator.expression(test.expression, nil)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, test.want) {
				t.Fatalf("value = %#v (%T), want %#v (%T)", got, got, test.want, test.want)
			}
		})
	}
}

func TestEvaluatorRejectsIntegerOverflow(t *testing.T) {
	for _, expression := range []cypher.Expression{
		binary(literal(int64(math.MaxInt64)), "+", literal(int64(1))),
		binary(literal(int64(math.MinInt64)), "-", literal(int64(1))),
		binary(literal(int64(math.MaxInt64)), "*", literal(int64(2))),
		&cypher.UnaryExpression{Span: testSpan, Operator: "-", Expression: literal(int64(math.MinInt64))},
	} {
		if _, err := newEvaluator(nil).expression(expression, nil); err == nil {
			t.Fatalf("overflow expression %#v succeeded", expression)
		}
	}
}

func TestEvaluatorBooleanShortCircuit(t *testing.T) {
	evaluator := newEvaluator(nil)
	for _, expression := range []cypher.Expression{
		binary(literal(false), "AND", variable("missing")),
		binary(literal(true), "OR", variable("missing")),
	} {
		if _, err := evaluator.expression(expression, nil); err != nil {
			t.Fatalf("short-circuited expression returned %v", err)
		}
	}
}

func TestEvaluatorGraphPropertiesAndFunctions(t *testing.T) {
	evaluator := newEvaluator(nil)
	node := domain.Node{
		ID:         "019945ee-ea00-7be6-a100-000000000001",
		Labels:     []string{"Task", "Ready"},
		Properties: domain.Properties{"title": "Pay invoices", "priority": int64(3)},
		Body:       "# Details",
	}
	values := row{"n": node}

	tests := []struct {
		expression cypher.Expression
		want       any
	}{
		{&cypher.PropertyExpression{Span: testSpan, Expression: variable("n"), Property: cypher.Identifier{Name: "title"}}, "Pay invoices"},
		{&cypher.PropertyExpression{Span: testSpan, Expression: variable("n"), Property: cypher.Identifier{Name: "body"}}, "# Details"},
		{function("elementId", variable("n")), string(node.ID)},
		{function("labels", variable("n")), []any{"Task", "Ready"}},
		{function("body", variable("n")), "# Details"},
		{function("keys", variable("n")), []any{"priority", "title"}},
	}
	for _, test := range tests {
		got, err := evaluator.expression(test.expression, values)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, test.want) {
			t.Fatalf("value = %#v, want %#v", got, test.want)
		}
	}
}

func TestEvaluatorListComprehension(t *testing.T) {
	expression := &cypher.ListComprehension{
		Span:     testSpan,
		Variable: cypher.Identifier{Name: "x"},
		List: &cypher.ListLiteral{Span: testSpan, Elements: []cypher.Expression{
			literal(int64(1)), literal(int64(2)), literal(int64(3)),
		}},
		Where:      binary(variable("x"), ">", literal(int64(1))),
		Projection: binary(variable("x"), "*", literal(int64(10))),
	}
	got, err := newEvaluator(nil).expression(expression, row{"outer": true})
	if err != nil {
		t.Fatal(err)
	}
	if want := []any{int64(20), int64(30)}; !reflect.DeepEqual(got, want) {
		t.Fatalf("value = %#v, want %#v", got, want)
	}
}

func TestEvaluatorStringListAndConversionFunctions(t *testing.T) {
	evaluator := newEvaluator(nil)
	tests := []struct {
		expression cypher.Expression
		want       any
	}{
		{function("substring", literal("héllo"), literal(int64(1)), literal(int64(3))), "éll"},
		{function("reverse", literal("abc")), "cba"},
		{function("range", literal(int64(3)), literal(int64(-1)), literal(int64(-2))), []any{int64(3), int64(1), int64(-1)}},
		{function("toInteger", literal("42")), int64(42)},
		{function("coalesce", literal(nil), literal("ready")), "ready"},
	}
	for _, test := range tests {
		got, err := evaluator.expression(test.expression, nil)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, test.want) {
			t.Fatalf("value = %#v, want %#v", got, test.want)
		}
	}
}

func TestEvaluatorReportsLocatedErrors(t *testing.T) {
	_, err := newEvaluator(nil).expression(variable("missing"), nil)
	var located *evaluationError
	if !errors.As(err, &located) {
		t.Fatalf("error %v is not an evaluationError", err)
	}
	if located.Position.Line != 2 || located.Position.Column != 4 {
		t.Fatalf("position = %#v", located.Position)
	}
}
