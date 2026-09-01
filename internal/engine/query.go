package engine

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/svlocks/sheets/internal/app"
	"github.com/svlocks/sheets/internal/cypher"
	"github.com/svlocks/sheets/internal/domain"
)

type revisionLister func(context.Context, domain.RevisionPage) ([]domain.RevisionInfo, domain.PageInfo, error)

type queryClock struct {
	transaction time.Time
	statement   time.Time
	realtime    func() time.Time
}

type queryExecution struct {
	ctx       context.Context
	source    string
	graph     *memoryGraph
	evaluator evaluator
	summary   app.Summary
	lastRows  []row
	revisions revisionLister
}

const defaultQueryRows int64 = 100_000

type rowBudget struct {
	limit int64
	used  int64
}

func (b *rowBudget) take(ctx context.Context, count int) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if b == nil || b.limit <= 0 || count <= 0 {
		return nil
	}
	b.used += int64(count)
	if b.used > b.limit {
		return fmt.Errorf("query row budget of %d exceeded", b.limit)
	}
	return nil
}

// A variable-length pattern can enumerate exponentially many relationship
// trails even though each individual trail is finite. Keep one budget across
// the complete query so a small collection of patterns cannot evade the cap.
const maxPathExpansions int64 = 1_000_000

func executeQuery(
	ctx context.Context,
	source string,
	query *cypher.QueryStatement,
	graph *memoryGraph,
	params map[string]any,
	listRevisions revisionLister,
	clock queryClock,
) (app.Result, error) {
	execution := &queryExecution{
		ctx: ctx, source: source, graph: graph, evaluator: newEvaluatorWithClock(params, clock), revisions: listRevisions,
	}
	execution.evaluator.ctx = ctx
	execution.evaluator.paths = &pathExpansionBudget{limit: maxPathExpansions}
	execution.evaluator.rows = &rowBudget{limit: defaultQueryRows}
	execution.evaluator.graph = graph
	execution.evaluator.pattern = execution.evaluatePattern
	execution.evaluator.patternRows = execution.evaluatePatternRows
	execution.evaluator.subquery = execution.evaluateExistsSubquery
	execution.evaluator.shortest = execution.evaluateShortestPattern
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
		if !slices.Equal(branchResult.Columns, combined.Columns) {
			return app.Result{}, fmt.Errorf("UNION branches return different columns: %v and %v", combined.Columns, branchResult.Columns)
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
	known := scopeFromRows(rows)
	for clauseIndex, clause := range clauses {
		if err := e.ctx.Err(); err != nil {
			return app.Result{}, err
		}
		var err error
		switch clause := clause.(type) {
		case *cypher.MatchClause:
			rows, err = matchClauseRows(e.graph, e.evaluator, rows, clause)
			addPatternVariables(known, clause.Patterns)
		case *cypher.UnwindClause:
			limit := int64(-1)
			if clauseIndex+1 < len(clauses) {
				limit, err = e.safeImmediateProjectionLimit(clauses[clauseIndex+1])
			}
			if err == nil {
				rows, err = e.unwind(rows, clause, limit)
			}
			known[clause.Alias.Name] = variableUnknown
		case *cypher.ProjectionClause:
			inputKnown := known
			rows, result, err = e.project(rows, clause, known)
			if clause.With {
				known = projectionOutputScope(e.source, clause.Items, inputKnown, rows)
			} else {
				known = scopeFromColumns(result.Columns)
			}
		case *cypher.CreateClause:
			rows, err = e.create(rows, clause.Patterns)
			addPatternVariables(known, clause.Patterns)
		case *cypher.MergeClause:
			rows, err = e.merge(rows, clause)
			addPatternVariables(known, []cypher.PatternPart{clause.Pattern})
		case *cypher.SetClause:
			err = e.set(rows, clause.Items)
		case *cypher.RemoveClause:
			err = e.remove(rows, clause.Items)
		case *cypher.DeleteClause:
			err = e.delete(rows, clause)
		case *cypher.CallClause:
			var nextKnown variableScope
			nextKnown, _, err = validateCall(e.source, clause, known)
			if err != nil {
				break
			}
			rows, err = e.call(rows, clause)
			known = nextKnown
			if err == nil && clauseIndex == len(clauses)-1 {
				result = callRowsResult(rows, clause, sortedScope(known))
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

func callRowsResult(rows []row, clause *cypher.CallClause, known []string) app.Result {
	result := rowsResult(rows)
	if len(result.Columns) != 0 {
		return result
	}
	if len(known) > 0 {
		result.Columns = append([]string(nil), known...)
		return result
	}
	if clause.Subquery != nil {
		return result
	}
	if len(clause.Yield) > 0 {
		for _, item := range clause.Yield {
			if item.Star {
				result.Columns = procedureColumns(clause.Procedure.String())
				return result
			}
			name := item.Name.Name
			if item.Alias.Name != "" {
				name = item.Alias.Name
			}
			result.Columns = append(result.Columns, name)
		}
		return result
	}
	result.Columns = procedureColumns(clause.Procedure.String())
	return result
}

func procedureColumns(name string) []string {
	switch strings.ToLower(name) {
	case "db.labels":
		return []string{"label"}
	case "db.relationshiptypes":
		return []string{"relationshipType"}
	case "db.propertykeys":
		return []string{"propertyKey"}
	case "sheets.nodes":
		return []string{"node"}
	case "sheets.edges":
		return []string{"relationship"}
	case "sheets.revisions":
		return []string{"revision", "time", "actor", "message", "next"}
	default:
		return nil
	}
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

func (e *queryExecution) unwind(input []row, clause *cypher.UnwindClause, limit int64) ([]row, error) {
	result := make([]row, 0)
	for _, values := range input {
		if err := e.ctx.Err(); err != nil {
			return nil, err
		}
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
			if limit >= 0 && int64(len(result)) >= limit {
				return result, nil
			}
			if err := e.evaluator.rows.take(e.ctx, 1); err != nil {
				return nil, err
			}
			next := cloneRow(values)
			next[clause.Alias.Name] = item
			result = append(result, next)
		}
	}
	return result, nil
}

func (e *queryExecution) safeImmediateProjectionLimit(clause cypher.Clause) (int64, error) {
	projection, ok := clause.(*cypher.ProjectionClause)
	if !ok || !projection.With || projection.Limit == nil || projection.Skip != nil ||
		projection.Distinct || projection.Where != nil || len(projection.OrderBy) != 0 ||
		projectionAggregates(projection.Items) {
		return -1, nil
	}
	value, err := e.evaluator.expression(projection.Limit, row{})
	if err != nil {
		return 0, err
	}
	limit, ok := integer(value)
	if !ok {
		return 0, evalError(projection.Limit, "LIMIT expects an integer, got %T", value)
	}
	if limit < 0 {
		return 0, evalError(projection.Limit, "LIMIT must be a non-negative integer")
	}
	return limit, nil
}

func (e *queryExecution) project(input []row, clause *cypher.ProjectionClause, known variableScope) ([]row, app.Result, error) {
	columns := projectionColumns(e.source, clause.Items, input, known)
	var projected []row
	var tableRows [][]any
	var sortScopes []row
	var filterScopes []row
	var sortEvaluators []evaluator
	if projectionAggregates(clause.Items) {
		groups, err := e.groupRows(input, clause.Items)
		if err != nil {
			return nil, app.Result{}, err
		}
		for _, group := range groups {
			if err := e.ctx.Err(); err != nil {
				return nil, app.Result{}, err
			}
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
			sortScopes = append(sortScopes, projectionSortScope(representative, mapped, clause))
			filterScopes = append(filterScopes, mapped)
			sortEvaluators = append(sortEvaluators, groupEvaluator)
		}
	} else {
		for _, values := range input {
			if err := e.ctx.Err(); err != nil {
				return nil, app.Result{}, err
			}
			if err := e.evaluator.rows.take(e.ctx, 1); err != nil {
				return nil, app.Result{}, err
			}
			projectedValues, mapped, err := evaluateProjection(e.evaluator, values, clause.Items, columns)
			if err != nil {
				return nil, app.Result{}, err
			}
			projected = append(projected, mapped)
			tableRows = append(tableRows, projectedValues)
			sortScopes = append(sortScopes, projectionSortScope(values, mapped, clause))
			filterScopes = append(filterScopes, projectionFilterScope(values, mapped))
			sortEvaluators = append(sortEvaluators, e.evaluator)
		}
	}

	var err error
	if clause.Where != nil {
		filteredRows := make([]row, 0, len(projected))
		filteredTable := make([][]any, 0, len(tableRows))
		filteredScopes := make([]row, 0, len(sortScopes))
		filteredEvaluators := make([]evaluator, 0, len(sortEvaluators))
		for index, values := range projected {
			if err := e.ctx.Err(); err != nil {
				return nil, app.Result{}, err
			}
			keep, filterErr := e.evaluator.expression(clause.Where, filterScopes[index])
			if filterErr != nil {
				return nil, app.Result{}, filterErr
			}
			if keep == true {
				filteredRows = append(filteredRows, values)
				filteredTable = append(filteredTable, tableRows[index])
				filteredScopes = append(filteredScopes, sortScopes[index])
				filteredEvaluators = append(filteredEvaluators, sortEvaluators[index])
			} else if keep != nil && keep != false {
				return nil, app.Result{}, evalError(clause.Where, "WHERE expects a boolean")
			}
		}
		projected, tableRows, sortScopes, sortEvaluators = filteredRows, filteredTable, filteredScopes, filteredEvaluators
	}
	if clause.Distinct {
		projected, tableRows, sortScopes, sortEvaluators, err = distinctProjected(e.ctx, projected, tableRows, sortScopes, sortEvaluators)
		if err != nil {
			return nil, app.Result{}, err
		}
	}
	if len(clause.OrderBy) > 0 {
		sortKeys := make([][]any, len(projected))
		for index := range projected {
			if err := e.ctx.Err(); err != nil {
				return nil, app.Result{}, err
			}
			keys, sortErr := evaluateSortKeys(sortEvaluators[index], sortScopes[index], clause.OrderBy)
			if sortErr != nil {
				return nil, app.Result{}, sortErr
			}
			sortKeys[index] = keys
		}
		if err := sortProjection(e.ctx, projected, tableRows, sortKeys, clause.OrderBy); err != nil {
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

func projectionSortScope(source, projected row, clause *cypher.ProjectionClause) row {
	// Validation limits DISTINCT sort expressions to projected expressions,
	// while evaluation still needs their original input bindings.
	scope := cloneRow(source)
	for name, value := range projected {
		scope[name] = value
	}
	return scope
}

func projectionFilterScope(source, projected row) row {
	// WITH WHERE is evaluated before the projection becomes the downstream
	// scope, and can therefore see both incoming bindings and new aliases.
	scope := cloneRow(source)
	for name, value := range projected {
		scope[name] = value
	}
	return scope
}

func evaluateSortKeys(evaluator evaluator, scope row, items []cypher.SortItem) ([]any, error) {
	keys := make([]any, len(items))
	for index, item := range items {
		if err := evaluator.ctx.Err(); err != nil {
			return nil, err
		}
		value, err := evaluator.expression(item.Expression, scope)
		if err != nil {
			return nil, err
		}
		keys[index] = value
	}
	return keys, nil
}

func projectionColumns(source string, items []cypher.ProjectionItem, input []row, known variableScope) []string {
	columns := make([]string, 0, len(items))
	for _, item := range items {
		if item.Star {
			set := make(map[string]struct{}, len(known))
			for key := range known {
				set[key] = struct{}{}
			}
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

func scopeFromRows(rows []row) variableScope {
	result := variableScope{}
	for _, values := range rows {
		for name := range values {
			if name != internalPathKey && name != expressionPathKey {
				result[name] = variableUnknown
			}
		}
	}
	return result
}

func scopeFromColumns(columns []string) variableScope {
	result := make(variableScope, len(columns))
	for _, column := range columns {
		result[column] = variableUnknown
	}
	return result
}

func projectionOutputScope(source string, items []cypher.ProjectionItem, input variableScope, rows []row) variableScope {
	// result.Columns is intentionally empty for WITH, so reconstruct its
	// static schema and include any concrete keys as a consistency fallback.
	result := scopeFromColumns(projectionStaticColumns(source, items, input))
	for name := range scopeFromRows(rows) {
		if _, exists := result[name]; !exists {
			result[name] = variableUnknown
		}
	}
	return result
}

func evaluateProjection(evaluator evaluator, source row, items []cypher.ProjectionItem, columns []string) ([]any, row, error) {
	values := make([]any, 0, len(columns))
	mapped := make(row, len(columns))
	columnIndex := 0
	for _, item := range items {
		if err := evaluator.ctx.Err(); err != nil {
			return nil, nil, err
		}
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
	groupCompleteRow := false
	for _, item := range items {
		if item.Star {
			groupCompleteRow = true
		} else if !containsAggregate(item.Expression) {
			groupExpressions = append(groupExpressions, item.Expression)
		}
	}
	if len(groupExpressions) == 0 && !groupCompleteRow {
		return [][]row{input}, nil
	}
	type group struct {
		key  string
		rows []row
	}
	groups := make([]group, 0)
	index := make(map[string]int)
	for _, values := range input {
		if err := e.ctx.Err(); err != nil {
			return nil, err
		}
		keyValues := make([]any, 0, len(groupExpressions)+1)
		if groupCompleteRow {
			keyValues = append(keyValues, values)
		}
		for _, expression := range groupExpressions {
			value, err := e.evaluator.expression(expression, values)
			if err != nil {
				return nil, err
			}
			keyValues = append(keyValues, value)
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
	case *cypher.ListComprehension:
		return containsAggregate(expression.List) ||
			containsAggregate(expression.Where) ||
			containsAggregate(expression.Projection)
	case *cypher.PatternComprehension:
		return containsAggregate(expression.Where) || containsAggregate(expression.Projection)
	case *cypher.ListPredicate:
		return containsAggregate(expression.List) || containsAggregate(expression.Where)
	case *cypher.ReduceExpression:
		return containsAggregate(expression.Initial) ||
			containsAggregate(expression.List) ||
			containsAggregate(expression.Expression)
	}
	return false
}

func distinctProjected(
	ctx context.Context,
	rows []row,
	table [][]any,
	scopes []row,
	evaluators []evaluator,
) ([]row, [][]any, []row, []evaluator, error) {
	seen := make(map[string]struct{}, len(table))
	resultRows := make([]row, 0, len(rows))
	resultTable := make([][]any, 0, len(table))
	resultScopes := make([]row, 0, len(scopes))
	resultEvaluators := make([]evaluator, 0, len(evaluators))
	for index, values := range table {
		if err := ctx.Err(); err != nil {
			return nil, nil, nil, nil, err
		}
		key := valueKey(values)
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		resultRows = append(resultRows, rows[index])
		resultTable = append(resultTable, values)
		resultScopes = append(resultScopes, scopes[index])
		resultEvaluators = append(resultEvaluators, evaluators[index])
	}
	return resultRows, resultTable, resultScopes, resultEvaluators, nil
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

func sortProjection(ctx context.Context, rows []row, table, keys [][]any, items []cypher.SortItem) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	type sortable struct {
		row    row
		values []any
		keys   []any
	}
	sortableRows := make([]sortable, len(rows))
	for index := range rows {
		sortableRows[index] = sortable{row: rows[index], values: table[index], keys: keys[index]}
	}
	sort.SliceStable(sortableRows, func(left, right int) bool {
		for index, item := range items {
			leftValue, rightValue := sortableRows[left].keys[index], sortableRows[right].keys[index]
			comparison := compareOrderValues(leftValue, rightValue)
			if comparison == 0 {
				continue
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
	return ctx.Err()
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
		if !ok {
			return nil, nil, evalError(skipExpression, "SKIP expects an integer, got %T", value)
		}
		if skip < 0 {
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
		if !ok {
			return nil, nil, evalError(limitExpression, "LIMIT expects an integer, got %T", value)
		}
		if limit < 0 {
			return nil, nil, evalError(limitExpression, "LIMIT must be a non-negative integer")
		}
	}
	length := int64(len(rows))
	start := min(skip, length)
	end := length
	if limit < length-start {
		end = start + limit
	}
	return rows[start:end], table[start:end], nil
}
