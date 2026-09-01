package main

import (
	"context"
	"errors"
	"fmt"
	"math"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/svlocks/sheets/internal/app"
	"github.com/svlocks/sheets/internal/cypher"
	"github.com/svlocks/sheets/internal/domain"
	"github.com/svlocks/sheets/internal/engine"
	"github.com/svlocks/sheets/internal/store"
)

const scenarioTimeout = 10 * time.Second

type queryObservation struct {
	result  app.Result
	err     error
	phase   string
	effects map[string]int64
	query   string
}

func runScenario(instance scenarioInstance, namedGraphs map[string]string, execute bool) scenarioResult {
	result := scenarioResult{
		ID: instance.ID, DefinitionID: instance.DefinitionID, Example: instance.Example,
		Status: statusPass, FrontendStatus: frontendBound,
	}
	for _, step := range instance.Steps {
		query, isQuery := scenarioStepQuery(step, namedGraphs)
		if !isQuery {
			continue
		}
		result.Queries = append(result.Queries, query)
		if result.Query == "" && strings.HasPrefix(step.Text, "executing query:") {
			result.Query = step.Doc
		}
		_, parseErr := cypher.Parse(query)
		if parseErr == nil {
			continue
		}
		var unsupported *cypher.UnsupportedFeatureError
		if errors.As(parseErr, &unsupported) {
			result.Status = statusTypedUnsupported
			result.FrontendStatus = frontendTypedUnsupported
			result.Stage = step.Text
			result.Error = parseErr.Error()
			result.Expected = expectedErrorStep(instance.Steps)
			return result
		}
		result.Status = statusParseRejected
		result.FrontendStatus = frontendParseRejected
		result.Stage = step.Text
		result.Error = parseErr.Error()
		result.Expected = expectedErrorStep(instance.Steps)
		return result
	}
	if result.Query == "" {
		result.Status = statusHarnessUnsupported
		result.Error = "scenario has no When executing query step"
		return result
	}
	if !execute {
		return result
	}
	for _, tag := range instance.Tags {
		if tag == "@ignore" {
			result.Status = statusHarnessUnsupported
			result.Error = "scenario is tagged @ignore by the pinned TCK"
			return result
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), scenarioTimeout)
	defer cancel()
	database, err := store.Open(ctx, ":memory:")
	if err != nil {
		result.Status = statusHarnessUnsupported
		result.Error = "open temporary store: " + err.Error()
		return result
	}
	defer func() { _ = database.Close() }()
	executor, err := engine.New(database)
	if err != nil {
		result.Status = statusHarnessUnsupported
		result.Error = "create engine: " + err.Error()
		return result
	}

	params := make(map[string]any)
	var current *queryObservation
	for _, step := range instance.Steps {
		result.Stage = fmt.Sprintf("line %d: %s", step.Line, step.Text)
		switch {
		case step.Text == "an empty graph", step.Text == "any graph":
			continue
		case strings.HasPrefix(step.Text, "the ") && strings.HasSuffix(step.Text, " graph"):
			fixture, exists := namedGraphs[step.Text]
			if !exists {
				result.Status = statusHarnessUnsupported
				result.Error = "unknown named graph fixture: " + step.Text
				return result
			}
			observation, executeErr := observeQuery(ctx, executor, fixture, params, false)
			if executeErr != nil {
				result.Status = statusHarnessUnsupported
				result.Error = executeErr.Error()
				return result
			}
			if observation.err != nil {
				result.Status = statusSemanticFailure
				result.Error = "named graph fixture query failed: " + stableError(observation.err)
				result.Actual = stableError(observation.err)
				return result
			}
		case step.Text == "parameters are:":
			parsed, parameterErr := parseParameters(step.Table)
			if parameterErr != nil {
				result.Status = statusHarnessUnsupported
				result.Error = parameterErr.Error()
				return result
			}
			params = parsed
		case strings.HasPrefix(step.Text, "there exists a procedure "):
			result.Status = statusHarnessUnsupported
			result.Error = "temporary procedure catalogs are not representable by sheets"
			return result
		case step.Text == "having executed:":
			observation, executeErr := observeQuery(ctx, executor, step.Doc, params, false)
			if executeErr != nil {
				result.Status = statusHarnessUnsupported
				result.Error = executeErr.Error()
				return result
			}
			if observation.err != nil {
				result.Status = statusSemanticFailure
				result.Error = "fixture query failed: " + stableError(observation.err)
				result.Actual = stableError(observation.err)
				return result
			}
		case step.Text == "executing query:", step.Text == "executing control query:":
			observation, executeErr := observeQuery(ctx, executor, step.Doc, params, true)
			if executeErr != nil {
				result.Status = statusHarnessUnsupported
				result.Error = executeErr.Error()
				return result
			}
			current = &observation
			if current.err != nil {
				result.ErrorPhase = current.phase
			}
		case step.Text == "the result should be empty":
			if current == nil {
				return harnessStepFailure(result, "result assertion has no preceding query")
			}
			if current.err != nil {
				return semanticStepFailure(result, "query returned an error", "empty result", stableError(current.err))
			}
			if len(current.result.Rows) != 0 {
				actual, normalizeErr := normalizedRows(current.result.Rows, false)
				if normalizeErr != nil {
					return harnessStepFailure(result, normalizeErr.Error())
				}
				return semanticStepFailure(result, "result was not empty", "[]", actual)
			}
		case strings.HasPrefix(step.Text, "the result should be"):
			if current == nil {
				return harnessStepFailure(result, "result assertion has no preceding query")
			}
			if current.err != nil {
				return semanticStepFailure(result, "query returned an error", formatTable(step.Table), stableError(current.err))
			}
			ordered := strings.Contains(step.Text, "in order") && !strings.Contains(step.Text, "any order")
			ignoreListOrder := strings.Contains(step.Text, "ignoring element order for lists")
			expected, actual, comparisonErr := compareResult(current.result, step.Table, ordered, ignoreListOrder)
			if comparisonErr != nil {
				if errors.Is(comparisonErr, errHarnessValue) {
					return harnessStepFailure(result, comparisonErr.Error())
				}
				return semanticStepFailure(result, comparisonErr.Error(), expected, actual)
			}
		case step.Text == "no side effects":
			if current == nil {
				return harnessStepFailure(result, "side-effect assertion has no preceding query")
			}
			if current.err != nil {
				return semanticStepFailure(result, "query returned an error", "no side effects", stableError(current.err))
			}
			if actual := nonzeroEffects(current.effects); len(actual) != 0 {
				return semanticStepFailure(result, "unexpected side effects", "{}", formatEffects(actual))
			}
		case step.Text == "the side effects should be:":
			if current == nil {
				return harnessStepFailure(result, "side-effect assertion has no preceding query")
			}
			if current.err != nil {
				return semanticStepFailure(result, "query returned an error", formatTable(step.Table), stableError(current.err))
			}
			expected, parseErr := parseEffects(step.Table)
			if parseErr != nil {
				return harnessStepFailure(result, parseErr.Error())
			}
			actual := nonzeroEffects(current.effects)
			if !reflect.DeepEqual(expected, actual) {
				return semanticStepFailure(result, "side effects differ", formatEffects(expected), formatEffects(actual))
			}
		case isErrorExpectation(step.Text):
			if current == nil {
				return harnessStepFailure(result, "error assertion has no preceding query")
			}
			if current.err == nil {
				return semanticStepFailure(result, "expected error was not raised", step.Text, "query succeeded")
			}
			result.Expected = step.Text
			result.Actual = stableError(current.err)
			expectedPhase := expectedErrorPhase(step.Text)
			result.ExpectedPhase = expectedPhase
			if current.phase != expectedPhase {
				result.Status = statusSemanticFailure
				result.Error = "error was raised at the wrong phase"
				return result
			}
			matched, supported := matchExpectedError(step.Text, current.err)
			if !supported {
				result.Expected = step.Text
				result.Actual = stableError(current.err)
				return harnessStepFailure(result, "error-code normalization is not implemented for this expectation")
			}
			if !matched {
				return semanticStepFailure(result, "raised error does not match expected category", step.Text, stableError(current.err))
			}
			if actual := nonzeroEffects(current.effects); len(actual) != 0 {
				return semanticStepFailure(result, "expected error left side effects", "{}", formatEffects(actual))
			}
		default:
			return harnessStepFailure(result, "unrecognized TCK step: "+step.Text)
		}
	}
	result.Stage = ""
	return result
}

func scenarioStepQuery(step tckStep, namedGraphs map[string]string) (string, bool) {
	if isExecutableStep(step.Text) {
		return step.Doc, true
	}
	if strings.HasPrefix(step.Text, "the ") && strings.HasSuffix(step.Text, " graph") {
		query, exists := namedGraphs[step.Text]
		return query, exists
	}
	return "", false
}

func isExecutableStep(text string) bool {
	return text == "having executed:" || text == "executing query:" || text == "executing control query:"
}

func expectedErrorStep(steps []tckStep) string {
	for _, step := range steps {
		if isErrorExpectation(step.Text) {
			return step.Text
		}
	}
	return ""
}

func isErrorExpectation(text string) bool {
	return strings.Contains(text, " should be raised at ")
}

func expectedErrorPhase(text string) string {
	if strings.Contains(text, " at compile time:") {
		return "compile time"
	}
	return "runtime"
}

func harnessStepFailure(result scenarioResult, message string) scenarioResult {
	result.Status = statusHarnessUnsupported
	result.Error = message
	return result
}

func semanticStepFailure(result scenarioResult, message, expected, actual string) scenarioResult {
	result.Status = statusSemanticFailure
	result.Error = message
	result.Expected = expected
	result.Actual = actual
	return result
}

var entityIDPattern = regexp.MustCompile(`\b[[:xdigit:]]{8}-[[:xdigit:]]{4}-[[:xdigit:]]{4}-[[:xdigit:]]{4}-[[:xdigit:]]{12}\b`)

func stableError(err error) string {
	return entityIDPattern.ReplaceAllString(err.Error(), "<entity-id>")
}

func observeQuery(ctx context.Context, executor *engine.Engine, query string, params map[string]any, classifyPhase bool) (queryObservation, error) {
	before, err := executor.Snapshot(ctx, domain.Snapshot{})
	if err != nil {
		return queryObservation{}, fmt.Errorf("snapshot before query: %w", err)
	}
	var (
		batch    app.BatchResult
		queryErr error
		phase    string
	)
	if classifyPhase {
		document, parseErr := cypher.Parse(query)
		if parseErr != nil || len(document.Statements) != 1 {
			return queryObservation{}, fmt.Errorf("cannot classify error phase for a non-single parsed query")
		}
		_, queryErr = executor.Execute(ctx, app.ExecuteRequest{Query: "EXPLAIN " + query, Params: params})
		if queryErr != nil {
			phase = "compile time"
		}
	}
	if queryErr == nil {
		batch, queryErr = executor.Execute(ctx, app.ExecuteRequest{Query: query, Params: params})
		if queryErr != nil && classifyPhase {
			phase = "runtime"
		}
	}
	after, err := executor.Snapshot(ctx, domain.Snapshot{})
	if err != nil {
		return queryObservation{}, fmt.Errorf("snapshot after query: %w", err)
	}
	observation := queryObservation{err: queryErr, phase: phase, effects: graphEffects(before, after), query: query}
	if len(batch.Results) > 0 {
		observation.result = batch.Results[len(batch.Results)-1]
	}
	return observation, nil
}

func parseParameters(table [][]string) (map[string]any, error) {
	params := make(map[string]any, len(table))
	for _, row := range table {
		if len(row) != 2 {
			return nil, fmt.Errorf("parameter row has %d cells, want 2", len(row))
		}
		normalized, err := parseExpectedValue(row[1])
		if err != nil {
			return nil, fmt.Errorf("parameter %s: %w", row[0], err)
		}
		value, err := normalized.native()
		if err != nil {
			return nil, fmt.Errorf("parameter %s: %w", row[0], err)
		}
		params[row[0]] = value
	}
	return params, nil
}

func (v normalizedValue) native() (any, error) {
	switch v.kind {
	case "null":
		return nil, nil
	case "bool":
		return strconv.ParseBool(v.scalar)
	case "int":
		return strconv.ParseInt(v.scalar, 10, 64)
	case "float":
		switch v.scalar {
		case "NaN":
			return mathNaN(), nil
		case "+Infinity":
			return mathInf(1), nil
		case "-Infinity":
			return mathInf(-1), nil
		default:
			return strconv.ParseFloat(v.scalar, 64)
		}
	case "string":
		return strconv.Unquote(v.scalar)
	case "list":
		result := make([]any, len(v.items))
		for index, item := range v.items {
			value, err := item.native()
			if err != nil {
				return nil, err
			}
			result[index] = value
		}
		return result, nil
	case "map":
		result := make(map[string]any, len(v.properties))
		for key, item := range v.properties {
			value, err := item.native()
			if err != nil {
				return nil, err
			}
			result[key] = value
		}
		return result, nil
	default:
		return nil, fmt.Errorf("%s values cannot be used as parameters", v.kind)
	}
}

// Wrapped to keep the normalization code's special float cases explicit.
func mathNaN() float64         { return math.NaN() }
func mathInf(sign int) float64 { return math.Inf(sign) }

var errHarnessValue = errors.New("TCK value is not representable by the comparison harness")

func compareResult(result app.Result, table [][]string, ordered, ignoreListOrder bool) (string, string, error) {
	if len(table) == 0 {
		return "", "", fmt.Errorf("%w: result assertion has no table", errHarnessValue)
	}
	headings := table[0]
	if !reflect.DeepEqual(headings, result.Columns) {
		return formatTable(table), fmt.Sprintf("columns=%v", result.Columns), fmt.Errorf("result columns differ")
	}
	expectedRows := make([]string, 0, len(table)-1)
	for rowIndex, row := range table[1:] {
		if len(row) != len(headings) {
			return "", "", fmt.Errorf("%w: expected row %d has %d cells, want %d", errHarnessValue, rowIndex+1, len(row), len(headings))
		}
		cells := make([]string, len(row))
		for cellIndex, raw := range row {
			value, err := parseExpectedValue(raw)
			if err != nil {
				return "", "", fmt.Errorf("%w: expected cell %d/%d %q: %v", errHarnessValue, rowIndex+1, cellIndex+1, raw, err)
			}
			cells[cellIndex] = value.key(ignoreListOrder)
		}
		expectedRows = append(expectedRows, strings.Join(cells, "\x1f"))
	}
	actualRows, err := normalizedRowKeys(result.Rows, ignoreListOrder)
	if err != nil {
		return "", "", fmt.Errorf("%w: %v", errHarnessValue, err)
	}
	if !ordered {
		sort.Strings(expectedRows)
		sort.Strings(actualRows)
	}
	expected := "[" + strings.Join(expectedRows, ";") + "]"
	actual := "[" + strings.Join(actualRows, ";") + "]"
	if !reflect.DeepEqual(expectedRows, actualRows) {
		return expected, actual, fmt.Errorf("result rows differ")
	}
	return expected, actual, nil
}

func normalizedRows(rows [][]any, ignoreListOrder bool) (string, error) {
	keys, err := normalizedRowKeys(rows, ignoreListOrder)
	if err != nil {
		return "", err
	}
	return "[" + strings.Join(keys, ";") + "]", nil
}

func normalizedRowKeys(rows [][]any, ignoreListOrder bool) ([]string, error) {
	result := make([]string, len(rows))
	for rowIndex, row := range rows {
		cells := make([]string, len(row))
		for cellIndex, raw := range row {
			value, err := normalizeActual(raw)
			if err != nil {
				return nil, fmt.Errorf("actual cell %d/%d: %w", rowIndex+1, cellIndex+1, err)
			}
			cells[cellIndex] = value.key(ignoreListOrder)
		}
		result[rowIndex] = strings.Join(cells, "\x1f")
	}
	return result, nil
}

func graphEffects(before, after engine.GraphSnapshot) map[string]int64 {
	effects := make(map[string]int64)
	beforeNodes := make(map[domain.EntityID]struct{}, len(before.Nodes))
	afterNodes := make(map[domain.EntityID]struct{}, len(after.Nodes))
	for _, node := range before.Nodes {
		beforeNodes[node.ID] = struct{}{}
	}
	for _, node := range after.Nodes {
		afterNodes[node.ID] = struct{}{}
		if _, exists := beforeNodes[node.ID]; !exists {
			effects["+nodes"]++
		}
	}
	for id := range beforeNodes {
		if _, exists := afterNodes[id]; !exists {
			effects["-nodes"]++
		}
	}
	beforeRelationships := make(map[domain.EntityID]struct{}, len(before.Edges))
	afterRelationships := make(map[domain.EntityID]struct{}, len(after.Edges))
	for _, edge := range before.Edges {
		beforeRelationships[edge.ID] = struct{}{}
	}
	for _, edge := range after.Edges {
		afterRelationships[edge.ID] = struct{}{}
		if _, exists := beforeRelationships[edge.ID]; !exists {
			effects["+relationships"]++
		}
	}
	for id := range beforeRelationships {
		if _, exists := afterRelationships[id]; !exists {
			effects["-relationships"]++
		}
	}
	beforeLabels, afterLabels := graphLabels(before), graphLabels(after)
	for label := range afterLabels {
		if _, exists := beforeLabels[label]; !exists {
			effects["+labels"]++
		}
	}
	for label := range beforeLabels {
		if _, exists := afterLabels[label]; !exists {
			effects["-labels"]++
		}
	}
	beforeProperties := graphProperties(before)
	afterProperties := graphProperties(after)
	for entity, properties := range afterProperties {
		old := beforeProperties[entity]
		for key, value := range properties {
			oldValue, exists := old[key]
			if !exists {
				effects["+properties"]++
			} else if oldValue != value {
				effects["-properties"]++
				effects["+properties"]++
			}
		}
	}
	for entity, properties := range beforeProperties {
		current := afterProperties[entity]
		for key := range properties {
			if _, exists := current[key]; !exists {
				effects["-properties"]++
			}
		}
	}
	return effects
}

func graphLabels(snapshot engine.GraphSnapshot) map[string]struct{} {
	labels := make(map[string]struct{})
	for _, node := range snapshot.Nodes {
		for _, label := range node.Labels {
			labels[label] = struct{}{}
		}
	}
	return labels
}

func graphProperties(snapshot engine.GraphSnapshot) map[string]map[string]string {
	result := make(map[string]map[string]string, len(snapshot.Nodes)+len(snapshot.Edges))
	for _, node := range snapshot.Nodes {
		properties := make(map[string]string, len(node.Properties)+1)
		for key, value := range node.Properties {
			normalized, err := normalizeActual(value)
			if err == nil {
				properties[key] = normalized.key(false)
			}
		}
		if node.Body != "" {
			properties["body"] = normalizedValue{kind: "string", scalar: strconv.Quote(node.Body)}.key(false)
		}
		result["n:"+string(node.ID)] = properties
	}
	for _, edge := range snapshot.Edges {
		properties := make(map[string]string, len(edge.Properties)+1)
		for key, value := range edge.Properties {
			normalized, err := normalizeActual(value)
			if err == nil {
				properties[key] = normalized.key(false)
			}
		}
		if edge.Position != nil {
			properties["position"] = normalizedValue{kind: "int", scalar: strconv.FormatInt(*edge.Position, 10)}.key(false)
		}
		result["r:"+string(edge.ID)] = properties
	}
	return result
}

func parseEffects(table [][]string) (map[string]int64, error) {
	effects := make(map[string]int64, len(table))
	for _, row := range table {
		if len(row) != 2 {
			return nil, fmt.Errorf("side-effect row has %d cells, want 2", len(row))
		}
		value, err := strconv.ParseInt(row[1], 10, 64)
		if err != nil || value < 0 {
			return nil, fmt.Errorf("invalid side-effect count %q", row[1])
		}
		effects[row[0]] = value
	}
	return nonzeroEffects(effects), nil
}

func nonzeroEffects(effects map[string]int64) map[string]int64 {
	result := make(map[string]int64)
	for key, value := range effects {
		if value != 0 {
			result[key] = value
		}
	}
	return result
}

func formatEffects(effects map[string]int64) string {
	keys := make([]string, 0, len(effects))
	for key := range effects {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		parts = append(parts, fmt.Sprintf("%s=%d", key, effects[key]))
	}
	return "{" + strings.Join(parts, ",") + "}"
}

func formatTable(table [][]string) string {
	rows := make([]string, len(table))
	for index, row := range table {
		rows[index] = "[" + strings.Join(row, ",") + "]"
	}
	return "[" + strings.Join(rows, ",") + "]"
}

func matchExpectedError(expectation string, actual error) (matched, supported bool) {
	code := expectation
	if colon := strings.LastIndex(expectation, ":"); colon >= 0 {
		code = strings.TrimSpace(expectation[colon+1:])
	}
	if code == "*" {
		return true, true
	}
	patterns, exists := map[string][]string{
		"MissingParameter":               {"was not supplied"},
		"DifferentColumnsInUnion":        {"different columns"},
		"DeleteConnectedNode":            {"detach", "relationship"},
		"NegativeIntegerArgument":        {"non-negative", "negative"},
		"InvalidArgumentType":            {"expects", "must be", "got ", "cannot index"},
		"InvalidArgumentValue":           {"invalid", "out of range", "must be", "expects"},
		"NumberOutOfRange":               {"out of range", "exceeds", "between", "non-zero"},
		"UndefinedVariable":              {"is not defined", "undefined variable"},
		"VariableTypeConflict":           {"type conflict"},
		"VariableAlreadyBound":           {"already bound", "cannot redeclare"},
		"ProcedureNotFound":              {"unsupported procedure"},
		"NoSingleRelationshipType":       {"single relationship type", "exactly one type"},
		"CreatingVarLength":              {"variable-length", "variable length"},
		"RequiresDirectedRelationship":   {"directed relationship", "must have a direction"},
		"AmbiguousAggregationExpression": {"variables outside an aggregate"},
		"ColumnNameConflict":             {"declares column", "more than once"},
		"InvalidAggregation":             {"aggregate", "aggregation"},
		"InvalidDelete":                  {"delete expects"},
		"InvalidParameterUse":            {"parameter", "was not supplied"},
		"UnexpectedSyntax":               {"pattern expression is not allowed in this context"},
		"InvalidClauseComposition":       {"cannot mix union"},
		"NoVariablesInScope":             {"requires at least one variable"},
		"NestedAggregation":              {"cannot be nested"},
		"NonConstantExpression":          {"is not defined"},
		"MapElementAccessByNonString":    {"map index must be a string"},
		"MergeReadOwnWrites":             {"merge cannot use null property"},
	}[code]
	if !exists {
		return false, false
	}
	message := strings.ToLower(actual.Error())
	for _, pattern := range patterns {
		if strings.Contains(message, strings.ToLower(pattern)) {
			return true, true
		}
	}
	return false, true
}
