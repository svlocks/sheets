package main

import (
	"strings"
	"testing"
)

func TestParseFeatureExpandsOutlinesAndBackground(t *testing.T) {
	feature := `Feature: example
  Background:
    Given an empty graph
    And having executed:
      """
      CREATE (:Seed)
      """

  Scenario Outline: [1] values
    When executing query:
      """
      RETURN <value> AS value
      """
    Then the result should be, in any order:
      | value   |
      | <value> |
    And no side effects

    Examples:
      | value |
      # A comment may occur inside the Examples table.
      | 1     |
      | 2     |
`
	document, err := parseFeature("expressions/Example.feature", strings.NewReader(feature))
	if err != nil {
		t.Fatal(err)
	}
	if len(document.Definitions) != 1 || len(document.Instances) != 2 {
		t.Fatalf("definitions/instances = %d/%d", len(document.Definitions), len(document.Instances))
	}
	first := document.Instances[0]
	if first.ID != "expressions/Example.feature::[1] values::example[001]" {
		t.Fatalf("first ID = %q", first.ID)
	}
	if len(first.Steps) != 5 || first.Steps[1].Doc != "CREATE (:Seed)" || first.Steps[2].Doc != "RETURN 1 AS value" {
		t.Fatalf("first steps = %#v", first.Steps)
	}
	if got := document.Instances[1].Steps[3].Table[1][0]; got != "2" {
		t.Fatalf("second expected value = %q", got)
	}
}

func TestParseTableRowUsesGherkinEscapesOnly(t *testing.T) {
	row, err := parseTableRow(`| '\'' | 'a\\\\b\|c' |`)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{`'\''`, `'a\\b|c'`}
	if len(row) != len(want) {
		t.Fatalf("row = %#v", row)
	}
	for index := range want {
		if row[index] != want[index] {
			t.Fatalf("row[%d] = %q, want %q", index, row[index], want[index])
		}
	}
}

func TestExampleSubstitutionIsSinglePass(t *testing.T) {
	values := map[string]string{"a": "<b>", "b": "replacement"}
	if got := substituteExamples("<a> <b>", values); got != "<b> replacement" {
		t.Fatalf("substitution = %q", got)
	}
	if got := substituteExamples("RETURN <a> < <b>", values); got != "RETURN <b> < replacement" {
		t.Fatalf("substitution after literal less-than = %q", got)
	}
}

func TestScenarioTagsAreNotConsumedByPreviousBody(t *testing.T) {
	feature := `Feature: tags
  Scenario: first
    Given any graph
    When executing query:
      """
      RETURN 1
      """
    Then the result should be empty

  @ignore @skipStyleCheck
  Scenario: second
    Given any graph
    When executing query:
      """
      RETURN 2
      """
    Then the result should be empty
`
	document, err := parseFeature("Tags.feature", strings.NewReader(feature))
	if err != nil {
		t.Fatal(err)
	}
	if got := document.Instances[1].Tags; len(got) != 2 || got[0] != "@ignore" {
		t.Fatalf("second tags = %#v", got)
	}
}
