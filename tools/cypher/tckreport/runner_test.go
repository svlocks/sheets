package main

import (
	"errors"
	"testing"

	"github.com/svlocks/sheets/internal/domain"
	"github.com/svlocks/sheets/internal/engine"
)

func TestGraphEffectsCountCreateAndDeleteInsteadOfNetting(t *testing.T) {
	before := engine.GraphSnapshot{Nodes: []domain.Node{{ID: "old", Labels: []string{"Old"}, Properties: domain.Properties{"x": int64(1)}}}}
	after := engine.GraphSnapshot{Nodes: []domain.Node{{ID: "new", Labels: []string{"New"}, Properties: domain.Properties{"y": int64(2)}}}}
	effects := nonzeroEffects(graphEffects(before, after))
	want := map[string]int64{
		"+nodes": 1, "-nodes": 1,
		"+labels": 1, "-labels": 1,
		"+properties": 1, "-properties": 1,
	}
	if formatEffects(effects) != formatEffects(want) {
		t.Fatalf("effects = %s, want %s", formatEffects(effects), formatEffects(want))
	}
}

func TestNoSideEffectsDistinguishesTemporalValuesFromRenderedStrings(t *testing.T) {
	instance := scenarioInstance{
		ID: "temporal-type-change",
		Steps: []tckStep{
			{Text: "an empty graph", Line: 1},
			{Text: "having executed:", Doc: "CREATE (:Val {value: date('1984-10-11')})", Line: 2},
			{Text: "executing query:", Doc: "MATCH (n:Val) SET n.value = '1984-10-11'", Line: 3},
			{Text: "no side effects", Line: 4},
		},
	}
	result := runScenario(instance, nil, true)
	if result.Status != statusSemanticFailure || result.Error != "unexpected side effects" {
		t.Fatalf("result = %#v", result)
	}
	if result.Actual != "{+properties=1,-properties=1}" {
		t.Fatalf("actual effects = %q", result.Actual)
	}
}

func TestNamedGraphFixtureIsExecuted(t *testing.T) {
	instance := scenarioInstance{
		ID: "fixture",
		Steps: []tckStep{
			{Text: "the binary-tree-1 graph", Line: 1},
			{Text: "executing query:", Doc: "MATCH (n:Seed) RETURN n.name AS name", Line: 2},
			{Text: "the result should be, in any order:", Table: [][]string{{"name"}, {"'seed'"}}, Line: 3},
			{Text: "no side effects", Line: 4},
		},
	}
	result := runScenario(instance, map[string]string{
		"the binary-tree-1 graph": "CREATE (:Seed {name: 'seed'})",
	}, true)
	if result.Status != statusPass {
		t.Fatalf("result = %#v", result)
	}
}

func TestExpectedErrorPhaseIsVerified(t *testing.T) {
	tests := []struct {
		name        string
		query       string
		expectation string
		phase       string
	}{
		{
			name:        "compile",
			query:       "RETURN missing",
			expectation: "a SyntaxError should be raised at compile time: UndefinedVariable",
			phase:       "compile time",
		},
		{
			name:        "runtime",
			query:       "CREATE (:Leaked) RETURN 1[0]",
			expectation: "a TypeError should be raised at runtime: InvalidArgumentType",
			phase:       "runtime",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			instance := scenarioInstance{
				ID: test.name,
				Steps: []tckStep{
					{Text: "an empty graph", Line: 1},
					{Text: "executing query:", Doc: test.query, Line: 2},
					{Text: test.expectation, Line: 3},
				},
			}
			result := runScenario(instance, nil, true)
			if result.Status != statusPass || result.ErrorPhase != test.phase {
				t.Fatalf("result = %#v", result)
			}
		})
	}
}

func TestScenarioSummaryIncludesTerminalFrontendClassifications(t *testing.T) {
	result := &report{
		ScenarioInstances: 5,
		Scenarios: []scenarioResult{
			{ID: "pass", Status: statusPass, FrontendStatus: frontendBound},
			{ID: "semantic", Status: statusSemanticFailure, FrontendStatus: frontendBound},
			{ID: "harness", Status: statusHarnessUnsupported, FrontendStatus: frontendBound},
			{ID: "unsupported", Status: statusTypedUnsupported, FrontendStatus: frontendTypedUnsupported},
			{ID: "rejected", Status: statusParseRejected, FrontendStatus: frontendParseRejected},
		},
	}
	if err := summarizeScenarios(result); err != nil {
		t.Fatal(err)
	}
	if result.Bound != 3 || result.TypedUnsupported != 1 || result.ParseRejected != 1 ||
		result.Passed != 1 || result.SemanticFailures != 1 || result.HarnessUnsupported != 1 ||
		result.SilentSkips != 0 {
		t.Fatalf("summary = %#v", result)
	}
}

func TestErrorEvidenceRedactsGeneratedEntityIDs(t *testing.T) {
	err := errors.New("cannot delete node 01a05ae1-a679-7a3a-b9b5-376105766073 with relationships")
	want := "cannot delete node <entity-id> with relationships"
	if got := stableError(err); got != want {
		t.Fatalf("stableError = %q, want %q", got, want)
	}
}

func TestExpectedErrorCategoryDoesNotAcceptGenericContextMessage(t *testing.T) {
	expectation := "a SyntaxError should be raised at compile time: UnexpectedSyntax"
	if matched, supported := matchExpectedError(expectation, errors.New("aggregate functions are not allowed in this context")); matched || !supported {
		t.Fatalf("unrelated context error matched: matched=%v supported=%v", matched, supported)
	}
	if matched, supported := matchExpectedError(expectation, errors.New("pattern expression is not allowed in this context")); !matched || !supported {
		t.Fatalf("pattern placement error did not match: matched=%v supported=%v", matched, supported)
	}

	expectation = "a SyntaxError should be raised at compile time: NonConstantExpression"
	if matched, supported := matchExpectedError(expectation, errors.New(`variable "n" is not defined`)); matched || !supported {
		t.Fatalf("undefined-variable error matched non-constant expression: matched=%v supported=%v", matched, supported)
	}
	if matched, supported := matchExpectedError(expectation, errors.New("non-constant expression is not allowed")); !matched || !supported {
		t.Fatalf("non-constant error did not match: matched=%v supported=%v", matched, supported)
	}

	for _, test := range []struct {
		code   string
		actual string
	}{
		{"InvalidArgumentType", "LIMIT must be a non-negative integer"},
		{"NegativeIntegerArgument", "LIMIT expects an integer, got float64"},
		{"InvalidArgumentType", "range expects integers and a non-zero step"},
		{"NumberOutOfRange", "range expects integers and a non-zero step"},
		{"InvalidArgumentValue", "property access expects a node, relationship, or map; got integer"},
		{"InvalidParameterUse", `parameter "value" was not supplied`},
		{"InvalidAggregation", "aggregate functions cannot be nested"},
		{"DeleteConnectedNode", "type expects a relationship"},
	} {
		expectation := "an Error should be raised at runtime: " + test.code
		if matched, supported := matchExpectedError(expectation, errors.New(test.actual)); matched || !supported {
			t.Errorf("%s accepted %q: matched=%v supported=%v", test.code, test.actual, matched, supported)
		}
	}
}

func TestEscapedDottedFunctionBindsThenRaisesUnknownFunction(t *testing.T) {
	instance := scenarioInstance{
		ID: "escaped-dotted-function",
		Steps: []tckStep{
			{Text: "an empty graph", Line: 1},
			{Text: "executing query:", Doc: "RETURN `date.truncate`('year', date('1984-10-11'))", Line: 2},
			{Text: "a SyntaxError should be raised at compile time: UnknownFunction", Line: 3},
		},
	}
	result := runScenario(instance, nil, true)
	if result.Status != statusPass || result.FrontendStatus != frontendBound || result.ErrorPhase != "compile time" {
		t.Fatalf("result = %#v", result)
	}
}
