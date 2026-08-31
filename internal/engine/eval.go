// Package engine executes parsed Cypher against sheets's graph store.
package engine

import (
	"errors"
	"fmt"
	"math"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/svlocks/sheets/internal/cypher"
	"github.com/svlocks/sheets/internal/domain"
)

type row map[string]any

type evaluator struct {
	params map[string]any
	now    func() time.Time
}

func newEvaluator(params map[string]any) evaluator {
	return evaluator{params: params, now: time.Now}
}

// evaluationError adds a Cypher source location to a runtime expression error.
type evaluationError struct {
	Position cypher.Position
	Message  string
}

func (e *evaluationError) Error() string {
	return fmt.Sprintf("line %d, column %d: %s", e.Position.Line, e.Position.Column, e.Message)
}

func evalError(expression cypher.Expression, format string, args ...any) error {
	return &evaluationError{
		Position: expression.Location().Start,
		Message:  fmt.Sprintf(format, args...),
	}
}

func (e evaluator) expression(expression cypher.Expression, values row) (any, error) {
	switch expression := expression.(type) {
	case nil:
		return nil, nil
	case *cypher.Literal:
		return expression.Value, nil
	case *cypher.Variable:
		value, ok := values[expression.Name.Name]
		if !ok {
			return nil, evalError(expression, "variable %q is not defined", expression.Name.Name)
		}
		return value, nil
	case *cypher.Parameter:
		value, ok := e.params[expression.Name.Name]
		if !ok {
			return nil, evalError(expression, "parameter $%s was not supplied", expression.Name.Name)
		}
		return value, nil
	case *cypher.UnaryExpression:
		return e.unary(expression, values)
	case *cypher.BinaryExpression:
		return e.binary(expression, values)
	case *cypher.IsNullExpression:
		value, err := e.expression(expression.Expression, values)
		if err != nil {
			return nil, err
		}
		result := value == nil
		if expression.Not {
			result = !result
		}
		return result, nil
	case *cypher.PropertyExpression:
		value, err := e.expression(expression.Expression, values)
		if err != nil {
			return nil, err
		}
		return property(value, expression.Property.Name), nil
	case *cypher.LabelExpression:
		value, err := e.expression(expression.Expression, values)
		if err != nil {
			return nil, err
		}
		return hasLabels(value, expression.Labels), nil
	case *cypher.IndexExpression:
		return e.index(expression, values)
	case *cypher.SliceExpression:
		return e.slice(expression, values)
	case *cypher.FunctionInvocation:
		return e.function(expression, values)
	case *cypher.ListLiteral:
		items := make([]any, len(expression.Elements))
		for index, element := range expression.Elements {
			value, err := e.expression(element, values)
			if err != nil {
				return nil, err
			}
			items[index] = value
		}
		return items, nil
	case *cypher.MapLiteral:
		result := make(map[string]any, len(expression.Entries))
		for _, entry := range expression.Entries {
			value, err := e.expression(entry.Value, values)
			if err != nil {
				return nil, err
			}
			result[entry.Key.Name] = value
		}
		return result, nil
	case *cypher.CaseExpression:
		return e.caseExpression(expression, values)
	case *cypher.ListComprehension:
		return e.listComprehension(expression, values)
	case *cypher.ListPredicate:
		return e.listPredicate(expression, values)
	case *cypher.ReduceExpression:
		return e.reduceExpression(expression, values)
	case *cypher.PatternExpression, *cypher.ExistsSubquery:
		return nil, evalError(expression, "graph expressions require a match context")
	default:
		return nil, evalError(expression, "unsupported expression %T", expression)
	}
}

func (e evaluator) unary(expression *cypher.UnaryExpression, values row) (any, error) {
	value, err := e.expression(expression.Expression, values)
	if err != nil || value == nil {
		return value, err
	}
	switch strings.ToUpper(expression.Operator) {
	case "NOT":
		boolean, ok := value.(bool)
		if !ok {
			return nil, evalError(expression, "NOT expects a boolean, got %T", value)
		}
		return !boolean, nil
	case "+":
		if isNumber(value) {
			return value, nil
		}
	case "-":
		switch value := value.(type) {
		case int:
			if int64(value) == math.MinInt64 {
				return nil, evalError(expression, "integer overflow")
			}
			return -int64(value), nil
		case int64:
			if value == math.MinInt64 {
				return nil, evalError(expression, "integer overflow")
			}
			return -value, nil
		case float64:
			return -value, nil
		case float32:
			return -float64(value), nil
		}
	}
	return nil, evalError(expression, "%s expects a number, got %T", expression.Operator, value)
}

func (e evaluator) binary(expression *cypher.BinaryExpression, values row) (any, error) {
	operator := strings.ToUpper(strings.TrimSpace(expression.Operator))
	left, err := e.expression(expression.Left, values)
	if err != nil {
		return nil, err
	}

	// AND and OR can determine their result without evaluating the other side.
	if operator == "AND" {
		if left == false {
			return false, nil
		}
	}
	if operator == "OR" {
		if left == true {
			return true, nil
		}
	}
	right, err := e.expression(expression.Right, values)
	if err != nil {
		return nil, err
	}

	switch operator {
	case "AND", "OR", "XOR":
		return booleanBinary(expression, operator, left, right)
	case "=":
		if left == nil || right == nil {
			return nil, nil
		}
		return equalValues(left, right), nil
	case "<>", "!=":
		if left == nil || right == nil {
			return nil, nil
		}
		return !equalValues(left, right), nil
	case "<", "<=", ">", ">=":
		if left == nil || right == nil {
			return nil, nil
		}
		comparison, ok := compareValues(left, right)
		if !ok {
			return nil, evalError(expression, "cannot compare %T and %T", left, right)
		}
		switch operator {
		case "<":
			return comparison < 0, nil
		case "<=":
			return comparison <= 0, nil
		case ">":
			return comparison > 0, nil
		default:
			return comparison >= 0, nil
		}
	case "IN":
		return containsValue(expression, right, left)
	case "STARTS WITH", "ENDS WITH", "CONTAINS":
		if left == nil || right == nil {
			return nil, nil
		}
		leftString, leftOK := left.(string)
		rightString, rightOK := right.(string)
		if !leftOK || !rightOK {
			return nil, evalError(expression, "%s expects strings", operator)
		}
		switch operator {
		case "STARTS WITH":
			return strings.HasPrefix(leftString, rightString), nil
		case "ENDS WITH":
			return strings.HasSuffix(leftString, rightString), nil
		default:
			return strings.Contains(leftString, rightString), nil
		}
	case "=~":
		if left == nil || right == nil {
			return nil, nil
		}
		text, textOK := left.(string)
		pattern, patternOK := right.(string)
		if !textOK || !patternOK {
			return nil, evalError(expression, "=~ expects strings")
		}
		compiled, err := regexp.Compile(pattern)
		if err != nil {
			return nil, evalError(expression, "invalid regular expression: %v", err)
		}
		return compiled.MatchString(text), nil
	case "+":
		return addValues(expression, left, right)
	case "-", "*", "/", "%", "^":
		return numericBinary(expression, operator, left, right)
	default:
		return nil, evalError(expression, "unsupported operator %q", expression.Operator)
	}
}

func booleanBinary(expression cypher.Expression, operator string, left, right any) (any, error) {
	if left != nil {
		if _, ok := left.(bool); !ok {
			return nil, evalError(expression, "%s expects booleans", operator)
		}
	}
	if right != nil {
		if _, ok := right.(bool); !ok {
			return nil, evalError(expression, "%s expects booleans", operator)
		}
	}
	if left == nil || right == nil {
		switch operator {
		case "AND":
			if left == false || right == false {
				return false, nil
			}
		case "OR":
			if left == true || right == true {
				return true, nil
			}
		}
		return nil, nil
	}
	l, r := left.(bool), right.(bool)
	switch operator {
	case "AND":
		return l && r, nil
	case "OR":
		return l || r, nil
	default:
		return l != r, nil
	}
}

func addValues(expression cypher.Expression, left, right any) (any, error) {
	if left == nil || right == nil {
		return nil, nil
	}
	if leftString, ok := left.(string); ok {
		rightString, ok := right.(string)
		if !ok {
			return nil, evalError(expression, "+ cannot combine string and %T", right)
		}
		return leftString + rightString, nil
	}
	if leftList, ok := asList(left); ok {
		if rightList, ok := asList(right); ok {
			result := make([]any, 0, len(leftList)+len(rightList))
			result = append(result, leftList...)
			result = append(result, rightList...)
			return result, nil
		}
		return append(append([]any(nil), leftList...), right), nil
	}
	if rightList, ok := asList(right); ok {
		return append([]any{left}, rightList...), nil
	}
	return numericBinary(expression, "+", left, right)
}

func numericBinary(expression cypher.Expression, operator string, left, right any) (any, error) {
	if left == nil || right == nil {
		return nil, nil
	}
	leftFloat, leftInteger, leftOK := number(left)
	rightFloat, rightInteger, rightOK := number(right)
	if !leftOK || !rightOK {
		return nil, evalError(expression, "%s expects numbers, got %T and %T", operator, left, right)
	}
	if operator == "/" && rightFloat == 0 || operator == "%" && rightFloat == 0 {
		return nil, evalError(expression, "division by zero")
	}
	if leftInteger && rightInteger && operator != "/" && operator != "^" {
		l, _ := integer(left)
		r, _ := integer(right)
		switch operator {
		case "+":
			if r > 0 && l > math.MaxInt64-r || r < 0 && l < math.MinInt64-r {
				return nil, evalError(expression, "integer overflow")
			}
			return l + r, nil
		case "-":
			if r < 0 && l > math.MaxInt64+r || r > 0 && l < math.MinInt64+r {
				return nil, evalError(expression, "integer overflow")
			}
			return l - r, nil
		case "*":
			if l != 0 && (l == math.MinInt64 && r == -1 || r == math.MinInt64 && l == -1 || l*r/r != l) {
				return nil, evalError(expression, "integer overflow")
			}
			return l * r, nil
		case "%":
			if l == math.MinInt64 && r == -1 {
				return int64(0), nil
			}
			return l % r, nil
		}
	}
	switch operator {
	case "+":
		return leftFloat + rightFloat, nil
	case "-":
		return leftFloat - rightFloat, nil
	case "*":
		return leftFloat * rightFloat, nil
	case "/":
		return leftFloat / rightFloat, nil
	case "%":
		return math.Mod(leftFloat, rightFloat), nil
	case "^":
		return math.Pow(leftFloat, rightFloat), nil
	default:
		return nil, evalError(expression, "unsupported numeric operator %q", operator)
	}
}

func (e evaluator) index(expression *cypher.IndexExpression, values row) (any, error) {
	collection, err := e.expression(expression.Expression, values)
	if err != nil || collection == nil {
		return collection, err
	}
	index, err := e.expression(expression.Index, values)
	if err != nil || index == nil {
		return index, err
	}
	if items, ok := asList(collection); ok {
		position, ok := integer(index)
		if !ok {
			return nil, evalError(expression, "list index must be an integer")
		}
		position = normalizedIndex(position, int64(len(items)))
		if position < 0 || position >= int64(len(items)) {
			return nil, nil
		}
		return items[position], nil
	}
	if values, ok := collection.(map[string]any); ok {
		key, ok := index.(string)
		if !ok {
			return nil, evalError(expression, "map index must be a string")
		}
		return values[key], nil
	}
	return nil, evalError(expression, "cannot index %T", collection)
}

func (e evaluator) slice(expression *cypher.SliceExpression, values row) (any, error) {
	collection, err := e.expression(expression.Expression, values)
	if err != nil || collection == nil {
		return collection, err
	}
	items, ok := asList(collection)
	if !ok {
		return nil, evalError(expression, "slice expects a list")
	}
	start, end := int64(0), int64(len(items))
	if expression.Start != nil {
		value, err := e.expression(expression.Start, values)
		if err != nil {
			return nil, err
		}
		start, ok = integer(value)
		if !ok {
			return nil, evalError(expression, "slice start must be an integer")
		}
	}
	if expression.End != nil {
		value, err := e.expression(expression.End, values)
		if err != nil {
			return nil, err
		}
		end, ok = integer(value)
		if !ok {
			return nil, evalError(expression, "slice end must be an integer")
		}
	}
	length := int64(len(items))
	start, end = normalizedIndex(start, length), normalizedIndex(end, length)
	start = max(0, min(start, length))
	end = max(0, min(end, length))
	if start > end {
		return []any{}, nil
	}
	return append([]any(nil), items[start:end]...), nil
}

func (e evaluator) caseExpression(expression *cypher.CaseExpression, values row) (any, error) {
	var operand any
	var err error
	if expression.Operand != nil {
		operand, err = e.expression(expression.Operand, values)
		if err != nil {
			return nil, err
		}
	}
	for _, alternative := range expression.Alternatives {
		condition, err := e.expression(alternative.When, values)
		if err != nil {
			return nil, err
		}
		matches := condition == true
		if expression.Operand != nil {
			matches = operand != nil && condition != nil && equalValues(operand, condition)
		}
		if matches {
			return e.expression(alternative.Then, values)
		}
	}
	return e.expression(expression.Else, values)
}

func (e evaluator) listComprehension(expression *cypher.ListComprehension, values row) (any, error) {
	collection, err := e.expression(expression.List, values)
	if err != nil || collection == nil {
		return collection, err
	}
	items, ok := asList(collection)
	if !ok {
		return nil, evalError(expression, "list comprehension expects a list")
	}
	result := make([]any, 0, len(items))
	for _, item := range items {
		next := cloneRow(values)
		next[expression.Variable.Name] = item
		if expression.Where != nil {
			predicate, err := e.expression(expression.Where, next)
			if err != nil {
				return nil, err
			}
			if predicate != true {
				continue
			}
		}
		value := item
		if expression.Projection != nil {
			value, err = e.expression(expression.Projection, next)
			if err != nil {
				return nil, err
			}
		}
		result = append(result, value)
	}
	return result, nil
}

func (e evaluator) listPredicate(expression *cypher.ListPredicate, values row) (any, error) {
	collection, err := e.expression(expression.List, values)
	if err != nil || collection == nil {
		return collection, err
	}
	items, ok := asList(collection)
	if !ok {
		return nil, evalError(expression, "%s expects a list", expression.Operator)
	}
	trueCount := 0
	falseCount := 0
	hasNull := false
	for _, item := range items {
		next := cloneRow(values)
		next[expression.Variable.Name] = item
		predicate, err := e.expression(expression.Where, next)
		if err != nil {
			return nil, err
		}
		if predicate == nil {
			hasNull = true
			continue
		}
		matched, ok := predicate.(bool)
		if !ok {
			return nil, evalError(expression, "%s predicate must be boolean", expression.Operator)
		}
		if matched {
			trueCount++
		} else {
			falseCount++
		}
	}
	switch strings.ToLower(expression.Operator) {
	case "any":
		if trueCount > 0 {
			return true, nil
		}
		if hasNull {
			return nil, nil
		}
		return false, nil
	case "all":
		if falseCount > 0 {
			return false, nil
		}
		if hasNull {
			return nil, nil
		}
		return true, nil
	case "none":
		if trueCount > 0 {
			return false, nil
		}
		if hasNull {
			return nil, nil
		}
		return true, nil
	case "single":
		if trueCount > 1 {
			return false, nil
		}
		if hasNull {
			return nil, nil
		}
		return trueCount == 1, nil
	default:
		return nil, evalError(expression, "unknown list predicate %q", expression.Operator)
	}
}

func (e evaluator) reduceExpression(expression *cypher.ReduceExpression, values row) (any, error) {
	accumulator, err := e.expression(expression.Initial, values)
	if err != nil {
		return nil, err
	}
	collection, err := e.expression(expression.List, values)
	if err != nil || collection == nil {
		return collection, err
	}
	items, ok := asList(collection)
	if !ok {
		return nil, evalError(expression, "reduce expects a list")
	}
	for _, item := range items {
		next := cloneRow(values)
		next[expression.Accumulator.Name] = accumulator
		next[expression.Variable.Name] = item
		accumulator, err = e.expression(expression.Expression, next)
		if err != nil {
			return nil, err
		}
	}
	return accumulator, nil
}

func (e evaluator) function(expression *cypher.FunctionInvocation, values row) (any, error) {
	name := strings.ToLower(expression.Name.String())
	if isAggregate(name) {
		return nil, evalError(expression, "aggregate function %s is not valid in this expression context", name)
	}
	arguments := make([]any, len(expression.Arguments))
	for index, argument := range expression.Arguments {
		value, err := e.expression(argument, values)
		if err != nil {
			return nil, err
		}
		arguments[index] = value
	}
	require := func(count int) error {
		if len(arguments) != count {
			return evalError(expression, "%s expects %d argument(s), got %d", name, count, len(arguments))
		}
		return nil
	}
	switch name {
	case "coalesce":
		for _, argument := range arguments {
			if argument != nil {
				return argument, nil
			}
		}
		return nil, nil
	case "id", "elementid":
		if err := require(1); err != nil {
			return nil, err
		}
		switch value := arguments[0].(type) {
		case domain.Node:
			return string(value.ID), nil
		case *domain.Node:
			return string(value.ID), nil
		case domain.Edge:
			return string(value.ID), nil
		case *domain.Edge:
			return string(value.ID), nil
		case nil:
			return nil, nil
		default:
			return nil, evalError(expression, "%s expects a node or relationship", name)
		}
	case "labels":
		if err := require(1); err != nil {
			return nil, err
		}
		var labels []string
		switch value := arguments[0].(type) {
		case domain.Node:
			labels = value.Labels
		case *domain.Node:
			labels = value.Labels
		case nil:
			return nil, nil
		default:
			return nil, evalError(expression, "labels expects a node")
		}
		result := make([]any, len(labels))
		for index := range labels {
			result[index] = labels[index]
		}
		return result, nil
	case "type":
		if err := require(1); err != nil {
			return nil, err
		}
		switch value := arguments[0].(type) {
		case domain.Edge:
			return value.Type, nil
		case *domain.Edge:
			return value.Type, nil
		case nil:
			return nil, nil
		default:
			return nil, evalError(expression, "type expects a relationship")
		}
	case "properties":
		if err := require(1); err != nil {
			return nil, err
		}
		return properties(arguments[0]), nil
	case "keys":
		if err := require(1); err != nil {
			return nil, err
		}
		propertyMap := properties(arguments[0])
		if propertyMap == nil {
			return nil, nil
		}
		keys := make([]string, 0, len(propertyMap))
		for key := range propertyMap {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		result := make([]any, len(keys))
		for index := range keys {
			result[index] = keys[index]
		}
		return result, nil
	case "body":
		if err := require(1); err != nil {
			return nil, err
		}
		switch value := arguments[0].(type) {
		case domain.Node:
			return value.Body, nil
		case *domain.Node:
			return value.Body, nil
		case nil:
			return nil, nil
		default:
			return nil, evalError(expression, "body expects a node")
		}
	case "size", "length":
		if err := require(1); err != nil {
			return nil, err
		}
		switch value := arguments[0].(type) {
		case string:
			return int64(utf8.RuneCountInString(value)), nil
		default:
			if items, ok := asList(value); ok {
				return int64(len(items)), nil
			}
			if value == nil {
				return nil, nil
			}
			return nil, evalError(expression, "%s expects a string or list", name)
		}
	case "head", "last", "tail":
		if err := require(1); err != nil {
			return nil, err
		}
		if arguments[0] == nil {
			return nil, nil
		}
		items, ok := asList(arguments[0])
		if !ok {
			return nil, evalError(expression, "%s expects a list", name)
		}
		if name == "tail" {
			if len(items) == 0 {
				return []any{}, nil
			}
			return append([]any(nil), items[1:]...), nil
		}
		if len(items) == 0 {
			return nil, nil
		}
		if name == "head" {
			return items[0], nil
		}
		return items[len(items)-1], nil
	case "tostring", "tointeger", "tofloat", "toboolean":
		if err := require(1); err != nil {
			return nil, err
		}
		return convertValue(expression, name, arguments[0])
	case "tolower", "lower", "toupper", "upper", "trim", "ltrim", "rtrim", "reverse":
		if err := require(1); err != nil {
			return nil, err
		}
		if arguments[0] == nil {
			return nil, nil
		}
		text, ok := arguments[0].(string)
		if !ok {
			return nil, evalError(expression, "%s expects a string", name)
		}
		switch name {
		case "tolower", "lower":
			return strings.ToLower(text), nil
		case "toupper", "upper":
			return strings.ToUpper(text), nil
		case "trim":
			return strings.TrimSpace(text), nil
		case "ltrim":
			return strings.TrimLeftFunc(text, func(r rune) bool { return r == ' ' || r == '\t' || r == '\r' || r == '\n' }), nil
		case "rtrim":
			return strings.TrimRightFunc(text, func(r rune) bool { return r == ' ' || r == '\t' || r == '\r' || r == '\n' }), nil
		default:
			return reverseString(text), nil
		}
	case "replace":
		if len(arguments) != 3 {
			return nil, evalError(expression, "replace expects 3 arguments")
		}
		text, okText := arguments[0].(string)
		old, okOld := arguments[1].(string)
		newValue, okNew := arguments[2].(string)
		if !okText || !okOld || !okNew {
			return nil, evalError(expression, "replace expects strings")
		}
		return strings.ReplaceAll(text, old, newValue), nil
	case "substring":
		return substring(expression, arguments)
	case "range":
		return integerRange(expression, arguments)
	case "timestamp":
		if err := require(0); err != nil {
			return nil, err
		}
		return e.now().UnixMilli(), nil
	default:
		return nil, evalError(expression, "unknown function %s", expression.Name.String())
	}
}

func isAggregate(name string) bool {
	switch name {
	case "count", "collect", "sum", "avg", "min", "max", "stdev", "stdevp", "percentilecont", "percentiledisc":
		return true
	default:
		return false
	}
}

func property(value any, key string) any {
	switch value := value.(type) {
	case domain.Node:
		if key == "body" {
			return value.Body
		}
		return value.Properties[key]
	case *domain.Node:
		if value == nil {
			return nil
		}
		return property(*value, key)
	case domain.Edge:
		if key == "position" {
			if value.Position == nil {
				return nil
			}
			return *value.Position
		}
		return value.Properties[key]
	case *domain.Edge:
		if value == nil {
			return nil
		}
		return property(*value, key)
	case map[string]any:
		return value[key]
	default:
		return nil
	}
}

func properties(value any) map[string]any {
	var source map[string]any
	switch value := value.(type) {
	case domain.Node:
		source = value.Properties
	case *domain.Node:
		if value != nil {
			source = value.Properties
		}
	case domain.Edge:
		source = value.Properties
	case *domain.Edge:
		if value != nil {
			source = value.Properties
		}
	case map[string]any:
		source = value
	}
	if source == nil {
		return nil
	}
	result := make(map[string]any, len(source))
	for key, item := range source {
		result[key] = item
	}
	return result
}

func hasLabels(value any, labels []cypher.Identifier) bool {
	var actual []string
	switch value := value.(type) {
	case domain.Node:
		actual = value.Labels
	case *domain.Node:
		if value != nil {
			actual = value.Labels
		}
	default:
		return false
	}
	set := make(map[string]struct{}, len(actual))
	for _, label := range actual {
		set[label] = struct{}{}
	}
	for _, label := range labels {
		if _, exists := set[label.Name]; !exists {
			return false
		}
	}
	return true
}

func asList(value any) ([]any, bool) {
	if value == nil {
		return nil, false
	}
	if list, ok := value.([]any); ok {
		return list, true
	}
	reflected := reflect.ValueOf(value)
	if reflected.Kind() != reflect.Array && reflected.Kind() != reflect.Slice {
		return nil, false
	}
	result := make([]any, reflected.Len())
	for index := range result {
		result[index] = reflected.Index(index).Interface()
	}
	return result, true
}

func equalValues(left, right any) bool {
	leftNumber, leftInteger, leftOK := number(left)
	rightNumber, rightInteger, rightOK := number(right)
	if leftOK && rightOK {
		if leftInteger && rightInteger {
			leftExact, _ := integer(left)
			rightExact, _ := integer(right)
			return leftExact == rightExact
		}
		return leftNumber == rightNumber
	}
	return reflect.DeepEqual(left, right)
}

func compareValues(left, right any) (int, bool) {
	leftNumber, leftInteger, leftOK := number(left)
	rightNumber, rightInteger, rightOK := number(right)
	if leftOK && rightOK {
		if leftInteger && rightInteger {
			leftExact, _ := integer(left)
			rightExact, _ := integer(right)
			return compare(leftExact, rightExact), true
		}
		return compare(leftNumber, rightNumber), true
	}
	switch left := left.(type) {
	case string:
		right, ok := right.(string)
		if !ok {
			return 0, false
		}
		return strings.Compare(left, right), true
	case bool:
		right, ok := right.(bool)
		if !ok {
			return 0, false
		}
		return compareBool(left, right), true
	case time.Time:
		right, ok := right.(time.Time)
		if !ok {
			return 0, false
		}
		return left.Compare(right), true
	default:
		return 0, false
	}
}

func containsValue(expression cypher.Expression, collection, needle any) (any, error) {
	if collection == nil || needle == nil {
		return nil, nil
	}
	items, ok := asList(collection)
	if !ok {
		return nil, evalError(expression, "IN expects a list on its right side")
	}
	foundNull := false
	for _, item := range items {
		if item == nil {
			foundNull = true
			continue
		}
		if equalValues(item, needle) {
			return true, nil
		}
	}
	if foundNull {
		return nil, nil
	}
	return false, nil
}

func number(value any) (float64, bool, bool) {
	switch value := value.(type) {
	case int:
		return float64(value), true, true
	case int8:
		return float64(value), true, true
	case int16:
		return float64(value), true, true
	case int32:
		return float64(value), true, true
	case int64:
		return float64(value), true, true
	case uint:
		return float64(value), true, true
	case uint8:
		return float64(value), true, true
	case uint16:
		return float64(value), true, true
	case uint32:
		return float64(value), true, true
	case uint64:
		if value > math.MaxInt64 {
			return 0, false, false
		}
		return float64(value), true, true
	case float32:
		return float64(value), false, true
	case float64:
		return value, false, true
	default:
		return 0, false, false
	}
}

func integer(value any) (int64, bool) {
	switch value := value.(type) {
	case int:
		return int64(value), true
	case int8:
		return int64(value), true
	case int16:
		return int64(value), true
	case int32:
		return int64(value), true
	case int64:
		return value, true
	case uint:
		if uint64(value) <= math.MaxInt64 {
			return int64(value), true
		}
	case uint8:
		return int64(value), true
	case uint16:
		return int64(value), true
	case uint32:
		return int64(value), true
	case uint64:
		if value <= math.MaxInt64 {
			return int64(value), true
		}
	}
	return 0, false
}

func isNumber(value any) bool {
	_, _, ok := number(value)
	return ok
}

func normalizedIndex(index, length int64) int64 {
	if index < 0 {
		return length + index
	}
	return index
}

func convertValue(expression cypher.Expression, name string, value any) (any, error) {
	if value == nil {
		return nil, nil
	}
	switch name {
	case "tostring":
		switch value := value.(type) {
		case string:
			return value, nil
		case bool:
			return strconv.FormatBool(value), nil
		case int64:
			return strconv.FormatInt(value, 10), nil
		case float64:
			return strconv.FormatFloat(value, 'g', -1, 64), nil
		default:
			if integer, ok := integer(value); ok {
				return strconv.FormatInt(integer, 10), nil
			}
			if number, _, ok := number(value); ok {
				return strconv.FormatFloat(number, 'g', -1, 64), nil
			}
			return nil, nil
		}
	case "tointeger":
		switch value := value.(type) {
		case string:
			parsed, err := strconv.ParseInt(value, 10, 64)
			if err != nil {
				return nil, nil
			}
			return parsed, nil
		case bool:
			if value {
				return int64(1), nil
			}
			return int64(0), nil
		default:
			if integer, ok := integer(value); ok {
				return integer, nil
			}
			number, _, ok := number(value)
			if !ok || number > math.MaxInt64 || number < math.MinInt64 {
				return nil, nil
			}
			return int64(number), nil
		}
	case "tofloat":
		if value, _, ok := number(value); ok {
			return value, nil
		}
		if text, ok := value.(string); ok {
			parsed, err := strconv.ParseFloat(text, 64)
			if err == nil {
				return parsed, nil
			}
		}
		return nil, nil
	case "toboolean":
		if value, ok := value.(bool); ok {
			return value, nil
		}
		if text, ok := value.(string); ok {
			parsed, err := strconv.ParseBool(strings.ToLower(text))
			if err == nil {
				return parsed, nil
			}
		}
		return nil, nil
	default:
		return nil, evalError(expression, "unknown conversion %s", name)
	}
}

func substring(expression cypher.Expression, arguments []any) (any, error) {
	if len(arguments) != 2 && len(arguments) != 3 {
		return nil, evalError(expression, "substring expects 2 or 3 arguments")
	}
	if arguments[0] == nil || arguments[1] == nil {
		return nil, nil
	}
	text, ok := arguments[0].(string)
	if !ok {
		return nil, evalError(expression, "substring expects a string first argument")
	}
	start, ok := integer(arguments[1])
	if !ok {
		return nil, evalError(expression, "substring offset must be an integer")
	}
	runes := []rune(text)
	start = normalizedIndex(start, int64(len(runes)))
	start = max(0, min(start, int64(len(runes))))
	end := int64(len(runes))
	if len(arguments) == 3 {
		length, ok := integer(arguments[2])
		if !ok || length < 0 {
			return nil, evalError(expression, "substring length must be a non-negative integer")
		}
		end = min(end, start+length)
	}
	return string(runes[start:end]), nil
}

func integerRange(expression cypher.Expression, arguments []any) (any, error) {
	if len(arguments) != 2 && len(arguments) != 3 {
		return nil, evalError(expression, "range expects 2 or 3 arguments")
	}
	start, startOK := integer(arguments[0])
	end, endOK := integer(arguments[1])
	step := int64(1)
	if len(arguments) == 3 {
		step, _ = integer(arguments[2])
	}
	if !startOK || !endOK || step == 0 {
		return nil, evalError(expression, "range expects integers and a non-zero step")
	}
	if (step > 0 && start > end) || (step < 0 && start < end) {
		return []any{}, nil
	}
	distance := end - start
	count := distance/step + 1
	if count < 0 || count > 1_000_000 {
		return nil, evalError(expression, "range result is too large")
	}
	result := make([]any, 0, count)
	for current := start; ; current += step {
		if step > 0 && current > end || step < 0 && current < end {
			break
		}
		result = append(result, current)
	}
	return result, nil
}

func reverseString(value string) string {
	runes := []rune(value)
	for left, right := 0, len(runes)-1; left < right; left, right = left+1, right-1 {
		runes[left], runes[right] = runes[right], runes[left]
	}
	return string(runes)
}

func cloneRow(source row) row {
	result := make(row, len(source)+1)
	for key, value := range source {
		result[key] = value
	}
	return result
}

func compare[T ~int | ~int64 | ~float64](left, right T) int {
	if left < right {
		return -1
	}
	if left > right {
		return 1
	}
	return 0
}

func compareBool(left, right bool) int {
	if left == right {
		return 0
	}
	if !left {
		return -1
	}
	return 1
}

var errUnsupportedAggregation = errors.New("unsupported aggregation")
