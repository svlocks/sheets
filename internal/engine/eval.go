// Package engine executes parsed Cypher against sheets's graph store.
package engine

import (
	"context"
	"errors"
	"fmt"
	"math"
	"math/big"
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
	ctx      context.Context
	params   map[string]any
	now      func() time.Time
	group    []row
	graph    *memoryGraph
	pattern  func(cypher.PatternElement, row) ([]Path, error)
	subquery func(*cypher.QueryStatement, row) (bool, error)
	shortest func(cypher.PatternElement, row, bool) (any, error)
	paths    *pathExpansionBudget
	rows     *rowBudget
}

func newEvaluator(params map[string]any) evaluator {
	return evaluator{ctx: context.Background(), params: params, now: time.Now}
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
		if value == nil {
			return nil, nil
		}
		return hasLabels(value, expression.Labels), nil
	case *cypher.IndexExpression:
		return e.index(expression, values)
	case *cypher.SliceExpression:
		return e.slice(expression, values)
	case *cypher.FunctionInvocation:
		name := strings.ToLower(expression.Name.String())
		if (name == "shortestpath" || name == "allshortestpaths") && len(expression.Arguments) == 1 {
			if pattern, ok := expression.Arguments[0].(*cypher.PatternExpression); ok && e.shortest != nil {
				return e.shortest(pattern.Pattern, values, name == "allshortestpaths")
			}
		}
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
	case *cypher.PatternExpression:
		if e.pattern == nil {
			return nil, evalError(expression, "graph expression requires a match context")
		}
		paths, err := e.pattern(expression.Pattern, values)
		if err != nil {
			return nil, err
		}
		result := make([]any, len(paths))
		for index := range paths {
			result[index] = paths[index]
		}
		return result, nil
	case *cypher.ExistsSubquery:
		if e.subquery == nil {
			return nil, evalError(expression, "EXISTS subquery requires a match context")
		}
		return e.subquery(expression.Subquery, values)
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
		return cypherEqual(left, right), nil
	case "<>", "!=":
		equal := cypherEqual(left, right)
		if equal == nil {
			return nil, nil
		}
		return !equal.(bool), nil
	case "<", "<=", ">", ">=":
		if left == nil || right == nil {
			return nil, nil
		}
		comparison, comparable, unordered := compareCypherValues(left, right)
		if unordered {
			return false, nil
		}
		if !comparable {
			return nil, nil
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
	case "NOT IN":
		contained, err := containsValue(expression, right, left)
		if err != nil || contained == nil {
			return contained, err
		}
		return !contained.(bool), nil
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
	if operator == "%" && rightFloat == 0 || operator == "/" && rightFloat == 0 && leftInteger && rightInteger {
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
		if value == nil {
			return nil, nil
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
		if value == nil {
			return nil, nil
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
			matches = cypherEqual(operand, condition) == true
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
		if err := e.ctx.Err(); err != nil {
			return nil, err
		}
		next := cloneRow(values)
		next[expression.Variable.Name] = item
		if expression.Where != nil {
			predicate, err := e.expression(expression.Where, next)
			if err != nil {
				return nil, err
			}
			if predicate != nil {
				if _, ok := predicate.(bool); !ok {
					return nil, evalError(expression.Where, "list comprehension predicate must be boolean")
				}
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
		if err := e.ctx.Err(); err != nil {
			return nil, err
		}
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
		if err := e.ctx.Err(); err != nil {
			return nil, err
		}
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
		if e.group == nil {
			return nil, evalError(expression, "aggregate function %s is not valid in this expression context", name)
		}
		return e.aggregate(expression, values)
	}
	arguments := make([]any, len(expression.Arguments))
	for index, argument := range expression.Arguments {
		if err := e.ctx.Err(); err != nil {
			return nil, err
		}
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
			if err := e.ctx.Err(); err != nil {
				return nil, err
			}
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
	case "nodes", "relationships":
		if err := require(1); err != nil {
			return nil, err
		}
		path, ok := arguments[0].(Path)
		if !ok {
			if arguments[0] == nil {
				return nil, nil
			}
			return nil, evalError(expression, "%s expects a path", name)
		}
		if name == "nodes" {
			result := make([]any, len(path.Nodes))
			for index, node := range path.Nodes {
				result[index] = node
			}
			return result, nil
		}
		result := make([]any, len(path.Relationships))
		for index, edge := range path.Relationships {
			result[index] = edge
		}
		return result, nil
	case "startnode", "endnode":
		if err := require(1); err != nil {
			return nil, err
		}
		edge, ok := arguments[0].(*domain.Edge)
		if !ok || edge == nil {
			if arguments[0] == nil {
				return nil, nil
			}
			return nil, evalError(expression, "%s expects a relationship", name)
		}
		if e.graph == nil {
			return nil, evalError(expression, "%s requires a graph context", name)
		}
		// A zero-length node pattern constrained by the endpoint gives the
		// canonical pointer from the current graph through the match callback.
		endpoint := edge.From
		if name == "endnode" {
			endpoint = edge.To
		}
		return e.graph.nodes[endpoint], nil
	case "exists":
		if err := require(1); err != nil {
			return nil, err
		}
		if paths, ok := arguments[0].([]any); ok {
			return len(paths) > 0, nil
		}
		return arguments[0] != nil, nil
	case "shortestpath", "allshortestpaths":
		if err := require(1); err != nil {
			return nil, err
		}
		items, ok := asList(arguments[0])
		if !ok || len(items) == 0 {
			return nil, nil
		}
		paths := make([]Path, 0, len(items))
		minimum := math.MaxInt
		for _, item := range items {
			if err := e.ctx.Err(); err != nil {
				return nil, err
			}
			path, ok := item.(Path)
			if !ok {
				return nil, evalError(expression, "%s expects a pattern", name)
			}
			length := len(path.Relationships)
			if length < minimum {
				minimum = length
				paths = paths[:0]
			}
			if length == minimum {
				paths = append(paths, path)
			}
		}
		if name == "shortestpath" {
			return paths[0], nil
		}
		result := make([]any, len(paths))
		for index := range paths {
			result[index] = paths[index]
		}
		return result, nil
	case "size", "length":
		if err := require(1); err != nil {
			return nil, err
		}
		switch value := arguments[0].(type) {
		case string:
			return int64(utf8.RuneCountInString(value)), nil
		case Path:
			return int64(len(value.Relationships)), nil
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
	case "datetime", "localdatetime", "date", "time", "localtime":
		return temporalValue(expression, name, arguments, e.now())
	case "duration":
		return durationValue(expression, arguments)
	case "randomuuid":
		if err := require(0); err != nil {
			return nil, err
		}
		return randomUUID()
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
	case domain.Properties:
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
	case domain.Properties:
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
	if comparison, numeric, unordered := compareNumbers(left, right); numeric {
		return !unordered && comparison == 0
	}
	if leftTime, ok := left.(time.Time); ok {
		rightTime, ok := right.(time.Time)
		return ok && leftTime.Equal(rightTime)
	}
	return reflect.DeepEqual(left, right)
}

func compareValues(left, right any) (int, bool) {
	if comparison, numeric, unordered := compareNumbers(left, right); numeric {
		return comparison, !unordered
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
	if collection == nil {
		return nil, nil
	}
	items, ok := asList(collection)
	if !ok {
		return nil, evalError(expression, "IN expects a list on its right side")
	}
	foundNull := false
	for _, item := range items {
		equal := cypherEqual(item, needle)
		if equal == nil {
			foundNull = true
			continue
		}
		if equal == true {
			return true, nil
		}
	}
	if foundNull {
		return nil, nil
	}
	return false, nil
}

// cypherEqual implements Cypher's three-valued structural equality. It is
// deliberately separate from equalValues, which is used for internal change
// detection and grouping where null values must form a stable equivalence
// class.
func cypherEqual(left, right any) any {
	if left == nil || right == nil {
		return nil
	}
	leftNumber, _, leftNumeric := number(left)
	rightNumber, _, rightNumeric := number(right)
	if leftNumeric || rightNumeric {
		if !leftNumeric || !rightNumeric || math.IsNaN(leftNumber) || math.IsNaN(rightNumber) {
			return false
		}
		comparison, _, _ := compareNumbers(left, right)
		return comparison == 0
	}
	switch left := left.(type) {
	case string:
		right, ok := right.(string)
		return ok && left == right
	case bool:
		right, ok := right.(bool)
		return ok && left == right
	case time.Time:
		right, ok := right.(time.Time)
		return ok && left.Equal(right)
	case time.Duration:
		right, ok := right.(time.Duration)
		return ok && left == right
	case *domain.Node:
		switch right := right.(type) {
		case *domain.Node:
			return left != nil && right != nil && left.ID == right.ID
		case domain.Node:
			return left != nil && left.ID == right.ID
		default:
			return false
		}
	case domain.Node:
		switch right := right.(type) {
		case *domain.Node:
			return right != nil && left.ID == right.ID
		case domain.Node:
			return left.ID == right.ID
		default:
			return false
		}
	case *domain.Edge:
		switch right := right.(type) {
		case *domain.Edge:
			return left != nil && right != nil && left.ID == right.ID
		case domain.Edge:
			return left != nil && left.ID == right.ID
		default:
			return false
		}
	case domain.Edge:
		switch right := right.(type) {
		case *domain.Edge:
			return right != nil && left.ID == right.ID
		case domain.Edge:
			return left.ID == right.ID
		default:
			return false
		}
	case Path:
		right, ok := right.(Path)
		return ok && equalPath(left, right)
	}
	leftList, leftIsList := asList(left)
	rightList, rightIsList := asList(right)
	if leftIsList || rightIsList {
		if !leftIsList || !rightIsList || len(leftList) != len(rightList) {
			return false
		}
		unknown := false
		for index := range leftList {
			equal := cypherEqual(leftList[index], rightList[index])
			if equal == false {
				return false
			}
			unknown = unknown || equal == nil
		}
		if unknown {
			return nil
		}
		return true
	}
	leftMap, leftIsMap := asMap(left)
	rightMap, rightIsMap := asMap(right)
	if leftIsMap || rightIsMap {
		if !leftIsMap || !rightIsMap || len(leftMap) != len(rightMap) {
			return false
		}
		for key := range leftMap {
			if _, exists := rightMap[key]; !exists {
				return false
			}
		}
		unknown := false
		for key, leftValue := range leftMap {
			equal := cypherEqual(leftValue, rightMap[key])
			if equal == false {
				return false
			}
			unknown = unknown || equal == nil
		}
		if unknown {
			return nil
		}
		return true
	}
	return reflect.DeepEqual(left, right)
}

func asMap(value any) (map[string]any, bool) {
	switch value := value.(type) {
	case map[string]any:
		return value, true
	case domain.Properties:
		return map[string]any(value), true
	default:
		return nil, false
	}
}

// compareCypherValues is the partial ordering used by comparison operators.
// comparable is false for different or non-orderable types. unordered is true
// for NaN, for which every ordering predicate is false rather than null.
func compareCypherValues(left, right any) (comparison int, comparable, unordered bool) {
	_, _, leftNumeric := number(left)
	_, _, rightNumeric := number(right)
	if leftNumeric || rightNumeric {
		if !leftNumeric || !rightNumeric {
			return 0, false, false
		}
		return compareNumbers(left, right)
	}
	switch left := left.(type) {
	case string:
		right, ok := right.(string)
		if !ok {
			return 0, false, false
		}
		return strings.Compare(left, right), true, false
	case bool:
		right, ok := right.(bool)
		if !ok {
			return 0, false, false
		}
		return compareBool(left, right), true, false
	case time.Time:
		right, ok := right.(time.Time)
		if !ok {
			return 0, false, false
		}
		return left.Compare(right), true, false
	case time.Duration:
		right, ok := right.(time.Duration)
		if !ok {
			return 0, false, false
		}
		return compare(int64(left), int64(right)), true, false
	}
	leftList, leftOK := asList(left)
	rightList, rightOK := asList(right)
	if leftOK || rightOK {
		if !leftOK || !rightOK {
			return 0, false, false
		}
		for index := 0; index < min(len(leftList), len(rightList)); index++ {
			if equal := cypherEqual(leftList[index], rightList[index]); equal == true {
				continue
			}
			if leftList[index] == nil || rightList[index] == nil {
				return 0, false, false
			}
			comparison, comparable, unordered := compareCypherValues(leftList[index], rightList[index])
			if unordered || !comparable {
				return comparison, comparable, unordered
			}
			if comparison != 0 {
				return comparison, true, false
			}
		}
		return compare(len(leftList), len(rightList)), true, false
	}
	return 0, false, false
}

func compareNumbers(left, right any) (comparison int, numeric, unordered bool) {
	leftFloat, _, leftOK := number(left)
	rightFloat, _, rightOK := number(right)
	if !leftOK || !rightOK {
		return 0, false, false
	}
	if math.IsNaN(leftFloat) || math.IsNaN(rightFloat) {
		return 0, true, true
	}
	leftInteger, leftIsInteger := integer(left)
	rightInteger, rightIsInteger := integer(right)
	if leftIsInteger && rightIsInteger {
		return compare(leftInteger, rightInteger), true, false
	}
	if !leftIsInteger && !rightIsInteger {
		return compare(leftFloat, rightFloat), true, false
	}
	if math.IsInf(leftFloat, 0) || math.IsInf(rightFloat, 0) {
		return compare(leftFloat, rightFloat), true, false
	}
	if leftIsInteger {
		integerValue := new(big.Rat).SetInt64(leftInteger)
		floatValue := new(big.Rat).SetFloat64(rightFloat)
		return integerValue.Cmp(floatValue), true, false
	}
	integerValue := new(big.Rat).SetInt64(rightInteger)
	floatValue := new(big.Rat).SetFloat64(leftFloat)
	return floatValue.Cmp(integerValue), true, false
}

// compareOrderValues implements the total value hierarchy used by ORDER BY.
// Unlike comparison operators, ORDER BY defines an order across types and
// places null last (therefore first when the caller reverses for DESC).
func compareOrderValues(left, right any) int {
	if left == nil || isNilEntity(left) {
		if right == nil || isNilEntity(right) {
			return 0
		}
		return 1
	}
	if right == nil || isNilEntity(right) {
		return -1
	}
	leftRank, rightRank := orderRank(left), orderRank(right)
	if leftRank != rightRank {
		return compare(leftRank, rightRank)
	}
	switch leftRank {
	case 0: // maps
		leftMap, _ := asMap(left)
		rightMap, _ := asMap(right)
		if len(leftMap) != len(rightMap) {
			return compare(len(leftMap), len(rightMap))
		}
		leftKeys, rightKeys := sortedMapKeys(leftMap), sortedMapKeys(rightMap)
		for index := range leftKeys {
			if keyComparison := strings.Compare(leftKeys[index], rightKeys[index]); keyComparison != 0 {
				return keyComparison
			}
		}
		for _, key := range leftKeys {
			if valueComparison := compareOrderValues(leftMap[key], rightMap[key]); valueComparison != 0 {
				return valueComparison
			}
		}
		return 0
	case 1: // nodes
		return strings.Compare(string(nodeIdentity(left)), string(nodeIdentity(right)))
	case 2: // relationships
		return strings.Compare(string(edgeIdentity(left)), string(edgeIdentity(right)))
	case 3: // lists
		leftList, _ := asList(left)
		rightList, _ := asList(right)
		for index := 0; index < min(len(leftList), len(rightList)); index++ {
			if valueComparison := compareOrderValues(leftList[index], rightList[index]); valueComparison != 0 {
				return valueComparison
			}
		}
		return compare(len(leftList), len(rightList))
	case 4: // paths
		return strings.Compare(valueKey(left), valueKey(right))
	case 5: // temporal values
		leftTime, leftOK := left.(time.Time)
		rightTime, rightOK := right.(time.Time)
		if leftOK && rightOK {
			return leftTime.Compare(rightTime)
		}
	case 6: // durations
		leftDuration, leftOK := left.(time.Duration)
		rightDuration, rightOK := right.(time.Duration)
		if leftOK && rightOK {
			return compare(int64(leftDuration), int64(rightDuration))
		}
	case 7:
		return strings.Compare(left.(string), right.(string))
	case 8:
		return compareBool(left.(bool), right.(bool))
	case 9:
		comparison, _, unordered := compareNumbers(left, right)
		if unordered {
			leftNaN, rightNaN := isNaN(left), isNaN(right)
			if leftNaN == rightNaN {
				return 0
			}
			if leftNaN {
				return 1
			}
			return -1
		}
		return comparison
	}
	return strings.Compare(valueKey(left), valueKey(right))
}

func orderRank(value any) int {
	if _, ok := asMap(value); ok {
		return 0
	}
	switch value.(type) {
	case domain.Node, *domain.Node:
		return 1
	case domain.Edge, *domain.Edge:
		return 2
	}
	if _, ok := asList(value); ok {
		return 3
	}
	switch value.(type) {
	case Path, PathValue:
		return 4
	case time.Time:
		return 5
	case time.Duration:
		return 6
	case string:
		return 7
	case bool:
		return 8
	}
	if _, _, ok := number(value); ok {
		return 9
	}
	return 11
}

func isNilEntity(value any) bool {
	switch value := value.(type) {
	case *domain.Node:
		return value == nil
	case *domain.Edge:
		return value == nil
	default:
		return false
	}
}

func nodeIdentity(value any) domain.EntityID {
	switch value := value.(type) {
	case domain.Node:
		return value.ID
	case *domain.Node:
		if value != nil {
			return value.ID
		}
	}
	return ""
}

func edgeIdentity(value any) domain.EntityID {
	switch value := value.(type) {
	case domain.Edge:
		return value.ID
	case *domain.Edge:
		if value != nil {
			return value.ID
		}
	}
	return ""
}

func sortedMapKeys(value map[string]any) []string {
	keys := make([]string, 0, len(value))
	for key := range value {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func isNaN(value any) bool {
	number, _, ok := number(value)
	return ok && math.IsNaN(number)
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
	// Compute cardinality outside int64 arithmetic. Both subtraction across
	// opposite-signed endpoints and negating MinInt64 can overflow, and a
	// boundary-controlled loop can wrap after appending MaxInt64/MinInt64.
	// Iterating the proven count also avoids incrementing after the final item.
	distance := new(big.Int).Sub(big.NewInt(end), big.NewInt(start))
	distance.Abs(distance)
	stepMagnitude := new(big.Int).Abs(big.NewInt(step))
	countValue := new(big.Int).Quo(distance, stepMagnitude)
	countValue.Add(countValue, big.NewInt(1))
	if !countValue.IsInt64() || countValue.Int64() > 1_000_000 {
		return nil, evalError(expression, "range result is too large")
	}
	count := countValue.Int64()
	result := make([]any, 0, count)
	current := start
	for index := int64(0); index < count; index++ {
		result = append(result, current)
		if index+1 < count {
			current += step
		}
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
