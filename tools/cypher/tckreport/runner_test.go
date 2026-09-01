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
