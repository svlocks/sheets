package engine

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/svlocks/sheets/internal/app"
	"github.com/svlocks/sheets/internal/cypher"
)

type queryExecution struct {
	ctx       context.Context
	source    string
	graph     *memoryGraph
	evaluator evaluator
	summary   app.Summary
	lastRows  []row
}

func executeQuery(ctx context.Context, source string, query *cypher.QueryStatement, graph *memoryGraph, params map[string]any) (app.Result, error) {
	execution := &queryExecution{
		ctx: ctx, source: source, graph: graph, evaluator: newEvaluator(params),
	}
	execution.evaluator.graph = graph
	execution.evaluator.pattern = execution.evaluatePattern
	execution.evaluator.subquery = execution.evaluateExistsSubquery
	primary, err := execution.clauses(query.Clauses, []row{{}})
	if err != nil {
		return app.Result{}, err
	}
	if len(query.UnionBranches) == 0 {
		primary.Summary = execution.summary
		return primary, nil
	}

	combined := primary
	for _, branch := range query.UnionBranches {
		branchResult, err := execution.clauses(branch.Query.Clauses, []row{{}})
		if err != nil {
			return app.Result{}, err
		}
		if len(branchResult.Columns) != len(combined.Columns) {
			return app.Result{}, fmt.Errorf("UNION branches return different column counts")
		}
		combined.Rows = append(combined.Rows, branchResult.Rows...)
		if !branch.All {
			combined.Rows = distinctResultRows(combined.Rows)
		}
	}
	combined.Summary = execution.summary
	return combined, nil
}

func (e *queryExecution) clauses(clauses []cypher.Clause, rows []row) (app.Result, error) {
	result := app.Result{}
	for clauseIndex, clause := range clauses {
		if err := e.ctx.Err(); err != nil {
			return app.Result{}, err
		}
		var err error
		switch clause := clause.(type) {
		case *cypher.MatchClause:
			rows, err = matchClauseRows(e.graph, e.evaluator, rows, clause)
		case *cypher.UnwindClause:
			rows, err = e.unwind(rows, clause)
		case *cypher.ProjectionClause:
			rows, result, err = e.project(rows, clause)
		case *cypher.CreateClause:
			rows, err = e.create(rows, clause.Patterns)
		case *cypher.MergeClause:
			rows, err = e.merge(rows, clause)
		case *cypher.SetClause:
			err = e.set(rows, clause.Items)
		case *cypher.RemoveClause:
			err = e.remove(rows, clause.Items)
		case *cypher.DeleteClause:
			err = e.delete(rows, clause)
		case *cypher.CallClause:
			rows, err = e.call(rows, clause)
			if err == nil && clauseIndex == len(clauses)-1 {
				result = rowsResult(rows)
			}
		default:
			err = fmt.Errorf("unsupported clause %T", clause)
		}
		if err != nil {
			return app.Result{}, err
		}
	}
	e.lastRows = rows
	return result, nil
}

func rowsResult(rows []row) app.Result {
	set := make(map[string]struct{})
	for _, values := range rows {
		for key := range values {
			if key != internalPathKey && key != expressionPathKey {
				set[key] = struct{}{}
			}
		}
	}
	columns := make([]string, 0, len(set))
	for key := range set {
		columns = append(columns, key)
	}
	sort.Strings(columns)
	table := make([][]any, len(rows))
	for rowIndex, values := range rows {
		table[rowIndex] = make([]any, len(columns))
		for columnIndex, column := range columns {
			table[rowIndex][columnIndex] = values[column]
		}
	}
	return app.Result{Columns: columns, Rows: table}
}

func (e *queryExecution) unwind(input []row, clause *cypher.UnwindClause) ([]row, error) {
	result := make([]row, 0)
	for _, values := range input {
		value, err := e.evaluator.expression(clause.Expression, values)
		if err != nil {
			return nil, err
		}
		if value == nil {
			continue
		}
		items, ok := asList(value)
		if !ok {
			return nil, evalError(clause.Expression, "UNWIND expects a list, got %T", value)
		}
		for _, item := range items {
			next := cloneRow(values)
			next[clause.Alias.Name] = item
			result = append(result, next)
		}
	}
	return result, nil
}

func (e *queryExecution) project(input []row, clause *cypher.ProjectionClause) ([]row, app.Result, error) {
	columns := projectionColumns(e.source, clause.Items, input)
	var projected []row
	var tableRows [][]any
	if projectionAggregates(clause.Items) {
		groups, err := e.groupRows(input, clause.Items)
		if err != nil {
			return nil, app.Result{}, err
		}
		for _, group := range groups {
			representative := row{}
			if len(group) > 0 {
				representative = group[0]
			}
			groupEvaluator := e.evaluator
			groupEvaluator.group = group
			values, mapped, err := evaluateProjection(groupEvaluator, representative, clause.Items, columns)
			if err != nil {
				return nil, app.Result{}, err
			}
			projected = append(projected, mapped)
			tableRows = append(tableRows, values)
		}
	} else {
		for _, values := range input {
			projectedValues, mapped, err := evaluateProjection(e.evaluator, values, clause.Items, columns)
			if err != nil {
				return nil, app.Result{}, err
			}
			projected = append(projected, mapped)
			tableRows = append(tableRows, projectedValues)
		}
	}

	var err error
	if clause.Where != nil {
		filteredRows := make([]row, 0, len(projected))
		filteredTable := make([][]any, 0, len(tableRows))
		for index, values := range projected {
			keep, filterErr := e.evaluator.expression(clause.Where, values)
			if filterErr != nil {
				return nil, app.Result{}, filterErr
			}
			if keep == true {
				filteredRows = append(filteredRows, values)
				filteredTable = append(filteredTable, tableRows[index])
			} else if keep != nil && keep != false {
				return nil, app.Result{}, evalError(clause.Where, "WHERE expects a boolean")
			}
		}
		projected, tableRows = filteredRows, filteredTable
	}
	if clause.Distinct {
		projected, tableRows = distinctProjected(projected, tableRows)
	}
	if len(clause.OrderBy) > 0 {
		if err = e.sortProjection(projected, tableRows, clause.OrderBy); err != nil {
			return nil, app.Result{}, err
		}
	}
	projected, tableRows, err = e.paginateProjection(projected, tableRows, clause.Skip, clause.Limit)
	if err != nil {
		return nil, app.Result{}, err
	}

	result := app.Result{}
	if !clause.With {
		result.Columns = columns
		result.Rows = tableRows
	}
	return projected, result, nil
}

func projectionColumns(source string, items []cypher.ProjectionItem, input []row) []string {
	columns := make([]string, 0, len(items))
	for _, item := range items {
		if item.Star {
			set := make(map[string]struct{})
			for _, values := range input {
				for key := range values {
					if key != internalPathKey {
						set[key] = struct{}{}
					}
				}
			}
			stars := make([]string, 0, len(set))
			for key := range set {
				stars = append(stars, key)
			}
			sort.Strings(stars)
			columns = append(columns, stars...)
			continue
		}
		if item.Alias.Name != "" {
			columns = append(columns, item.Alias.Name)
			continue
		}
		if variable, ok := item.Expression.(*cypher.Variable); ok {
			columns = append(columns, variable.Name.Name)
			continue
		}
		span := item.Expression.Location()
		if span.Start.Offset >= 0 && span.End.Offset <= len(source) && span.End.Offset > span.Start.Offset {
			columns = append(columns, strings.TrimSpace(source[span.Start.Offset:span.End.Offset]))
		} else {
			columns = append(columns, fmt.Sprintf("column_%d", len(columns)+1))
		}
	}
	return columns
}

func evaluateProjection(evaluator evaluator, source row, items []cypher.ProjectionItem, columns []string) ([]any, row, error) {
	values := make([]any, 0, len(columns))
	mapped := make(row, len(columns))
	columnIndex := 0
	for _, item := range items {
		if item.Star {
			starNames := make([]string, 0, len(source))
			for name := range source {
				if name != internalPathKey {
					starNames = append(starNames, name)
				}
			}
			sort.Strings(starNames)
			for _, name := range starNames {
				values = append(values, source[name])
				mapped[name] = source[name]
				columnIndex++
			}
			continue
		}
		value, err := evaluator.expression(item.Expression, source)
		if err != nil {
			return nil, nil, err
		}
		name := columns[columnIndex]
		values = append(values, value)
		mapped[name] = value
		columnIndex++
	}
	return values, mapped, nil
}

func (e *queryExecution) groupRows(input []row, items []cypher.ProjectionItem) ([][]row, error) {
	groupExpressions := make([]cypher.Expression, 0)
	for _, item := range items {
		if !item.Star && !containsAggregate(item.Expression) {
			groupExpressions = append(groupExpressions, item.Expression)
		}
	}
	if len(groupExpressions) == 0 {
		return [][]row{input}, nil
	}
	type group struct {
		key  string
		rows []row
	}
	groups := make([]group, 0)
	index := make(map[string]int)
	for _, values := range input {
		keyValues := make([]any, len(groupExpressions))
		for expressionIndex, expression := range groupExpressions {
			value, err := e.evaluator.expression(expression, values)
			if err != nil {
				return nil, err
			}
			keyValues[expressionIndex] = value
		}
		key := valueKey(keyValues)
		position, exists := index[key]
		if !exists {
			position = len(groups)
			index[key] = position
			groups = append(groups, group{key: key})
		}
		groups[position].rows = append(groups[position].rows, values)
	}
	result := make([][]row, len(groups))
	for index := range groups {
		result[index] = groups[index].rows
	}
	return result, nil
}

func projectionAggregates(items []cypher.ProjectionItem) bool {
	for _, item := range items {
		if !item.Star && containsAggregate(item.Expression) {
			return true
		}
	}
	return false
}

func containsAggregate(expression cypher.Expression) bool {
	switch expression := expression.(type) {
	case *cypher.FunctionInvocation:
		if isAggregate(strings.ToLower(expression.Name.String())) {
			return true
		}
		for _, argument := range expression.Arguments {
			if containsAggregate(argument) {
				return true
			}
		}
	case *cypher.UnaryExpression:
		return containsAggregate(expression.Expression)
	case *cypher.BinaryExpression:
		return containsAggregate(expression.Left) || containsAggregate(expression.Right)
	case *cypher.IsNullExpression:
		return containsAggregate(expression.Expression)
	case *cypher.PropertyExpression:
		return containsAggregate(expression.Expression)
	case *cypher.LabelExpression:
		return containsAggregate(expression.Expression)
	case *cypher.IndexExpression:
		return containsAggregate(expression.Expression) || containsAggregate(expression.Index)
	case *cypher.SliceExpression:
		return containsAggregate(expression.Expression) || containsAggregate(expression.Start) || containsAggregate(expression.End)
	case *cypher.ListLiteral:
		for _, item := range expression.Elements {
			if containsAggregate(item) {
				return true
			}
		}
	case *cypher.MapLiteral:
		for _, entry := range expression.Entries {
			if containsAggregate(entry.Value) {
				return true
			}
		}
	case *cypher.CaseExpression:
		if containsAggregate(expression.Operand) || containsAggregate(expression.Else) {
			return true
		}
		for _, alternative := range expression.Alternatives {
			if containsAggregate(alternative.When) || containsAggregate(alternative.Then) {
				return true
			}
		}
	}
	return false
}

func distinctProjected(rows []row, table [][]any) ([]row, [][]any) {
	seen := make(map[string]struct{}, len(table))
	resultRows := make([]row, 0, len(rows))
	resultTable := make([][]any, 0, len(table))
	for index, values := range table {
		key := valueKey(values)
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		resultRows = append(resultRows, rows[index])
		resultTable = append(resultTable, values)
	}
	return resultRows, resultTable
}

func distinctResultRows(rows [][]any) [][]any {
	seen := make(map[string]struct{}, len(rows))
	result := make([][]any, 0, len(rows))
	for _, values := range rows {
		key := valueKey(values)
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		result = append(result, values)
	}
	return result
}

func (e *queryExecution) sortProjection(rows []row, table [][]any, items []cypher.SortItem) error {
	type sortable struct {
		row    row
		values []any
		keys   []any
	}
	sortableRows := make([]sortable, len(rows))
	for index := range rows {
		sortableRows[index] = sortable{row: rows[index], values: table[index], keys: make([]any, len(items))}
		for itemIndex, item := range items {
			value, err := e.evaluator.expression(item.Expression, rows[index])
			if err != nil {
				return err
			}
			sortableRows[index].keys[itemIndex] = value
		}
	}
	sort.SliceStable(sortableRows, func(left, right int) bool {
		for index, item := range items {
			leftValue, rightValue := sortableRows[left].keys[index], sortableRows[right].keys[index]
			if equalValues(leftValue, rightValue) {
				continue
			}
			if leftValue == nil {
				return false
			}
			if rightValue == nil {
				return true
			}
			comparison, ok := compareValues(leftValue, rightValue)
			if !ok {
				comparison = strings.Compare(valueKey(leftValue), valueKey(rightValue))
			}
			if item.Descending {
				return comparison > 0
			}
			return comparison < 0
		}
		return false
	})
	for index := range sortableRows {
		rows[index], table[index] = sortableRows[index].row, sortableRows[index].values
	}
	return nil
}

func (e *queryExecution) paginateProjection(rows []row, table [][]any, skipExpression, limitExpression cypher.Expression) ([]row, [][]any, error) {
	skip, limit := int64(0), int64(len(rows))
	if skipExpression != nil {
		value, err := e.evaluator.expression(skipExpression, row{})
		if err != nil {
			return nil, nil, err
		}
		var ok bool
		skip, ok = integer(value)
		if !ok || skip < 0 {
			return nil, nil, evalError(skipExpression, "SKIP must be a non-negative integer")
		}
	}
	if limitExpression != nil {
		value, err := e.evaluator.expression(limitExpression, row{})
		if err != nil {
			return nil, nil, err
		}
		var ok bool
		limit, ok = integer(value)
		if !ok || limit < 0 {
			return nil, nil, evalError(limitExpression, "LIMIT must be a non-negative integer")
		}
	}
	start := min(skip, int64(len(rows)))
	end := min(int64(len(rows)), start+limit)
	return rows[start:end], table[start:end], nil
}
