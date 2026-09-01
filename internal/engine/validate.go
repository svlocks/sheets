package engine

import (
	"fmt"
	"sort"
	"strings"

	"github.com/svlocks/sheets/internal/cypher"
)

type variableScope map[string]struct{}

func validateDocumentSemantics(document *cypher.Document) error {
	for _, statement := range document.Statements {
		query, ok := statement.(*cypher.QueryStatement)
		if !ok {
			continue
		}
		_, _, err := validateQuerySemantics(document.Source, query, func([]cypher.Clause) variableScope {
			return variableScope{}
		})
		if err != nil {
			return err
		}
	}
	return nil
}

type scopeInitializer func([]cypher.Clause) variableScope

func validateQuerySemantics(source string, query *cypher.QueryStatement, initialize scopeInitializer) (variableScope, []string, error) {
	scope, columns, err := validateClauses(source, query.Clauses, initialize(query.Clauses))
	if err != nil {
		return nil, nil, err
	}
	for _, branch := range query.UnionBranches {
		_, branchColumns, err := validateClauses(source, branch.Query.Clauses, initialize(branch.Query.Clauses))
		if err != nil {
			return nil, nil, err
		}
		if !equalStrings(columns, branchColumns) {
			return nil, nil, semanticError(branch.Query, "UNION branches return different columns: %v and %v", columns, branchColumns)
		}
	}
	return scope, columns, nil
}

func validateClauses(source string, clauses []cypher.Clause, scope variableScope) (variableScope, []string, error) {
	current := cloneScope(scope)
	var resultColumns []string
	for _, raw := range clauses {
		switch clause := raw.(type) {
		case *cypher.MatchClause:
			next := cloneScope(current)
			addPatternVariables(next, clause.Patterns)
			if err := validatePatterns(source, clause.Patterns, next, false); err != nil {
				return nil, nil, err
			}
			if err := validateNonAggregateExpression(source, clause.Where, next, "MATCH WHERE"); err != nil {
				return nil, nil, err
			}
			current = next
		case *cypher.UnwindClause:
			if err := validateNonAggregateExpression(source, clause.Expression, current, "UNWIND"); err != nil {
				return nil, nil, err
			}
			current[clause.Alias.Name] = struct{}{}
		case *cypher.ProjectionClause:
			output, columns, err := validateProjection(source, clause, current)
			if err != nil {
				return nil, nil, err
			}
			resultColumns = columns
			current = output
		case *cypher.CreateClause:
			if err := validateMutationPatterns(clause.Patterns, true); err != nil {
				return nil, nil, err
			}
			next := cloneScope(current)
			addPatternVariables(next, clause.Patterns)
			if err := validatePatterns(source, clause.Patterns, next, false); err != nil {
				return nil, nil, err
			}
			current = next
		case *cypher.MergeClause:
			if err := validateMutationPatterns([]cypher.PatternPart{clause.Pattern}, false); err != nil {
				return nil, nil, err
			}
			next := cloneScope(current)
			addPatternVariables(next, []cypher.PatternPart{clause.Pattern})
			if err := validatePatterns(source, []cypher.PatternPart{clause.Pattern}, next, false); err != nil {
				return nil, nil, err
			}
			for _, action := range clause.Actions {
				for _, item := range action.Set {
					if err := validateSetItem(source, item, next); err != nil {
						return nil, nil, err
					}
				}
			}
			current = next
		case *cypher.SetClause:
			for _, item := range clause.Items {
				if err := validateSetItem(source, item, current); err != nil {
					return nil, nil, err
				}
			}
		case *cypher.RemoveClause:
			for _, item := range clause.Items {
				if err := validateNonAggregateExpression(source, item.Target, current, "REMOVE"); err != nil {
					return nil, nil, err
				}
			}
		case *cypher.DeleteClause:
			for _, expression := range clause.Expressions {
				if err := validateNonAggregateExpression(source, expression, current, "DELETE"); err != nil {
					return nil, nil, err
				}
			}
		case *cypher.CallClause:
			next, columns, err := validateCall(source, clause, current)
			if err != nil {
				return nil, nil, err
			}
			current = next
			resultColumns = columns
		}
	}
	return current, resultColumns, nil
}

func validateMutationPatterns(patterns []cypher.PatternPart, create bool) error {
	for _, pattern := range patterns {
		for _, relationship := range pattern.Element.Relationships {
			if relationship.Length != nil {
				return semanticSpanError(relationship.Span, "CREATE and MERGE do not allow variable-length relationships")
			}
			if len(relationship.Types) != 1 {
				return semanticSpanError(relationship.Span, "CREATE and MERGE relationships require exactly one type")
			}
			if create && relationship.Direction == cypher.Undirected {
				return semanticSpanError(relationship.Span, "CREATE relationships must have a direction")
			}
		}
	}
	return nil
}

func validateProjection(source string, clause *cypher.ProjectionClause, input variableScope) (variableScope, []string, error) {
	aggregates := projectionAggregates(clause.Items)
	hasStar := false
	for _, item := range clause.Items {
		if item.Star {
			hasStar = true
			continue
		}
		if err := validateExpression(source, item.Expression, input); err != nil {
			return nil, nil, err
		}
		if err := validateAggregateShape(item.Expression, false); err != nil {
			return nil, nil, err
		}
		if containsAggregate(item.Expression) && hasVariableOutsideAggregate(item.Expression, false) {
			return nil, nil, semanticError(item.Expression, "variables outside an aggregate in the same projection expression are not supported; project the grouping key separately")
		}
	}
	output := variableScope{}
	if hasStar {
		for name := range input {
			output[name] = struct{}{}
		}
	}
	columns := projectionStaticColumns(source, clause.Items, input)
	seenColumns := make(map[string]struct{}, len(columns))
	for _, column := range columns {
		if _, exists := seenColumns[column]; exists {
			return nil, nil, semanticError(clause, "projection declares column %q more than once", column)
		}
		seenColumns[column] = struct{}{}
	}
	for _, item := range clause.Items {
		if item.Star {
			continue
		}
		if item.Alias.Name != "" {
			output[item.Alias.Name] = struct{}{}
		} else if variable, ok := item.Expression.(*cypher.Variable); ok {
			output[variable.Name.Name] = struct{}{}
		}
	}

	if err := validateNonAggregateExpression(source, clause.Where, output, "WITH WHERE"); err != nil {
		return nil, nil, err
	}
	orderScope := cloneScope(output)
	if !clause.Distinct {
		for name := range input {
			orderScope[name] = struct{}{}
		}
	}
	for _, item := range clause.OrderBy {
		if err := validateExpression(source, item.Expression, orderScope); err != nil {
			return nil, nil, err
		}
		if err := validateAggregateShape(item.Expression, false); err != nil {
			return nil, nil, err
		}
		if containsAggregate(item.Expression) && !aggregates {
			return nil, nil, semanticError(item.Expression, "ORDER BY cannot introduce aggregation absent from the projection")
		}
	}
	if err := validateStaticPagination(source, clause.Skip, "SKIP/OFFSET"); err != nil {
		return nil, nil, err
	}
	if err := validateStaticPagination(source, clause.Limit, "LIMIT"); err != nil {
		return nil, nil, err
	}
	return output, columns, nil
}

func validateCall(source string, clause *cypher.CallClause, input variableScope) (variableScope, []string, error) {
	if clause.Subquery != nil {
		outer := cloneScope(input)
		initialize := func(clauses []cypher.Clause) variableScope {
			if len(clauses) > 0 {
				if projection, ok := clauses[0].(*cypher.ProjectionClause); ok && projection.With {
					return cloneScope(outer)
				}
			}
			return variableScope{}
		}
		output, columns, err := validateQuerySemantics(source, clause.Subquery, initialize)
		if err != nil {
			return nil, nil, err
		}
		next := cloneScope(input)
		if clausesReturnRows(clause.Subquery.Clauses) {
			for name := range output {
				if _, exists := next[name]; exists {
					return nil, nil, semanticError(clause, "subquery returns variable %q which is already declared in the outer scope", name)
				}
				next[name] = struct{}{}
			}
		}
		return next, columns, nil
	}

	for _, argument := range clause.Arguments {
		if err := validateNonAggregateExpression(source, argument, input, "procedure argument"); err != nil {
			return nil, nil, err
		}
	}
	available := procedureColumns(clause.Procedure.String())
	availableSet := make(map[string]struct{}, len(available))
	for _, name := range available {
		availableSet[name] = struct{}{}
	}
	yields := available
	if len(clause.Yield) > 0 && !clause.Yield[0].Star {
		yields = make([]string, 0, len(clause.Yield))
		for _, item := range clause.Yield {
			if len(availableSet) > 0 {
				if _, exists := availableSet[item.Name.Name]; !exists {
					return nil, nil, semanticError(clause, "procedure %s does not yield %q", clause.Procedure.String(), item.Name.Name)
				}
			}
			name := item.Name.Name
			if item.Alias.Name != "" {
				name = item.Alias.Name
			}
			yields = append(yields, name)
		}
	}
	next := cloneScope(input)
	for _, name := range yields {
		if _, exists := next[name]; exists {
			return nil, nil, semanticError(clause, "procedure yield variable %q is already declared", name)
		}
		next[name] = struct{}{}
	}
	if err := validateNonAggregateExpression(source, clause.YieldWhere, next, "YIELD WHERE"); err != nil {
		return nil, nil, err
	}
	return next, sortedScope(next), nil
}

func validatePatterns(source string, patterns []cypher.PatternPart, scope variableScope, allowAggregate bool) error {
	for _, part := range patterns {
		for _, node := range part.Element.Nodes {
			if allowAggregate {
				if err := validateExpression(source, node.Properties, scope); err != nil {
					return err
				}
			} else if err := validateNonAggregateExpression(source, node.Properties, scope, "pattern properties"); err != nil {
				return err
			}
		}
		for _, relationship := range part.Element.Relationships {
			if err := validateNonAggregateExpression(source, relationship.Properties, scope, "pattern properties"); err != nil {
				return err
			}
			if relationship.Length != nil {
				if err := validateNonAggregateExpression(source, relationship.Length.Lower, scope, "relationship length"); err != nil {
					return err
				}
				if err := validateNonAggregateExpression(source, relationship.Length.Upper, scope, "relationship length"); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func validateSetItem(source string, item cypher.SetItem, scope variableScope) error {
	if err := validateNonAggregateExpression(source, item.Target, scope, "SET"); err != nil {
		return err
	}
	return validateNonAggregateExpression(source, item.Value, scope, "SET")
}

func validateStaticPagination(source string, expression cypher.Expression, name string) error {
	if expression == nil {
		return nil
	}
	return validateNonAggregateExpression(source, expression, variableScope{}, name)
}

func validateNonAggregateExpression(source string, expression cypher.Expression, scope variableScope, context string) error {
	if expression == nil {
		return nil
	}
	if err := validateExpression(source, expression, scope); err != nil {
		return err
	}
	if containsAggregate(expression) {
		return semanticError(expression, "%s does not allow aggregate functions", context)
	}
	return nil
}

func validateExpression(source string, expression cypher.Expression, scope variableScope) error { //nolint:gocyclo
	switch expression := expression.(type) {
	case nil, *cypher.Literal, *cypher.Parameter:
		return nil
	case *cypher.Variable:
		if _, exists := scope[expression.Name.Name]; !exists {
			return semanticError(expression, "variable %q is not defined", expression.Name.Name)
		}
	case *cypher.UnaryExpression:
		return validateExpression(source, expression.Expression, scope)
	case *cypher.BinaryExpression:
		if err := validateExpression(source, expression.Left, scope); err != nil {
			return err
		}
		return validateExpression(source, expression.Right, scope)
	case *cypher.IsNullExpression:
		return validateExpression(source, expression.Expression, scope)
	case *cypher.PropertyExpression:
		return validateExpression(source, expression.Expression, scope)
	case *cypher.LabelExpression:
		return validateExpression(source, expression.Expression, scope)
	case *cypher.IndexExpression:
		if err := validateExpression(source, expression.Expression, scope); err != nil {
			return err
		}
		return validateExpression(source, expression.Index, scope)
	case *cypher.SliceExpression:
		for _, item := range []cypher.Expression{expression.Expression, expression.Start, expression.End} {
			if err := validateExpression(source, item, scope); err != nil {
				return err
			}
		}
	case *cypher.FunctionInvocation:
		for _, argument := range expression.Arguments {
			if err := validateExpression(source, argument, scope); err != nil {
				return err
			}
		}
	case *cypher.ListLiteral:
		for _, item := range expression.Elements {
			if err := validateExpression(source, item, scope); err != nil {
				return err
			}
		}
	case *cypher.MapLiteral:
		for _, entry := range expression.Entries {
			if err := validateExpression(source, entry.Value, scope); err != nil {
				return err
			}
		}
	case *cypher.CaseExpression:
		if err := validateExpression(source, expression.Operand, scope); err != nil {
			return err
		}
		for _, alternative := range expression.Alternatives {
			if err := validateExpression(source, alternative.When, scope); err != nil {
				return err
			}
			if err := validateExpression(source, alternative.Then, scope); err != nil {
				return err
			}
		}
		return validateExpression(source, expression.Else, scope)
	case *cypher.ListComprehension:
		if err := validateExpression(source, expression.List, scope); err != nil {
			return err
		}
		inner := cloneScope(scope)
		inner[expression.Variable.Name] = struct{}{}
		if err := validateExpression(source, expression.Where, inner); err != nil {
			return err
		}
		return validateExpression(source, expression.Projection, inner)
	case *cypher.ListPredicate:
		if err := validateExpression(source, expression.List, scope); err != nil {
			return err
		}
		inner := cloneScope(scope)
		inner[expression.Variable.Name] = struct{}{}
		return validateExpression(source, expression.Where, inner)
	case *cypher.ReduceExpression:
		if err := validateExpression(source, expression.Initial, scope); err != nil {
			return err
		}
		if err := validateExpression(source, expression.List, scope); err != nil {
			return err
		}
		inner := cloneScope(scope)
		inner[expression.Accumulator.Name] = struct{}{}
		inner[expression.Variable.Name] = struct{}{}
		return validateExpression(source, expression.Expression, inner)
	case *cypher.PatternExpression:
		inner := cloneScope(scope)
		addPatternElementVariables(inner, expression.Pattern)
		return validatePatterns(source, []cypher.PatternPart{{Element: expression.Pattern}}, inner, false)
	case *cypher.ExistsSubquery:
		outer := cloneScope(scope)
		_, _, err := validateQuerySemantics(source, expression.Subquery, func([]cypher.Clause) variableScope {
			return cloneScope(outer)
		})
		return err
	default:
		return semanticError(expression, "unsupported expression %T", expression)
	}
	return nil
}

func validateAggregateShape(expression cypher.Expression, insideAggregate bool) error {
	switch expression := expression.(type) {
	case *cypher.FunctionInvocation:
		aggregate := isAggregate(strings.ToLower(expression.Name.String()))
		if aggregate && insideAggregate {
			return semanticError(expression, "aggregate functions cannot be nested")
		}
		for _, argument := range expression.Arguments {
			if err := validateAggregateShape(argument, insideAggregate || aggregate); err != nil {
				return err
			}
		}
	case *cypher.UnaryExpression:
		return validateAggregateShape(expression.Expression, insideAggregate)
	case *cypher.BinaryExpression:
		if err := validateAggregateShape(expression.Left, insideAggregate); err != nil {
			return err
		}
		return validateAggregateShape(expression.Right, insideAggregate)
	case *cypher.IsNullExpression:
		return validateAggregateShape(expression.Expression, insideAggregate)
	case *cypher.PropertyExpression:
		return validateAggregateShape(expression.Expression, insideAggregate)
	case *cypher.LabelExpression:
		return validateAggregateShape(expression.Expression, insideAggregate)
	case *cypher.IndexExpression:
		if err := validateAggregateShape(expression.Expression, insideAggregate); err != nil {
			return err
		}
		return validateAggregateShape(expression.Index, insideAggregate)
	case *cypher.SliceExpression:
		for _, item := range []cypher.Expression{expression.Expression, expression.Start, expression.End} {
			if err := validateAggregateShape(item, insideAggregate); err != nil {
				return err
			}
		}
	case *cypher.ListLiteral:
		for _, item := range expression.Elements {
			if err := validateAggregateShape(item, insideAggregate); err != nil {
				return err
			}
		}
	case *cypher.MapLiteral:
		for _, entry := range expression.Entries {
			if err := validateAggregateShape(entry.Value, insideAggregate); err != nil {
				return err
			}
		}
	case *cypher.CaseExpression:
		for _, item := range []cypher.Expression{expression.Operand, expression.Else} {
			if err := validateAggregateShape(item, insideAggregate); err != nil {
				return err
			}
		}
		for _, alternative := range expression.Alternatives {
			if err := validateAggregateShape(alternative.When, insideAggregate); err != nil {
				return err
			}
			if err := validateAggregateShape(alternative.Then, insideAggregate); err != nil {
				return err
			}
		}
	}
	return nil
}

func hasVariableOutsideAggregate(expression cypher.Expression, insideAggregate bool) bool { //nolint:gocyclo
	switch expression := expression.(type) {
	case *cypher.Variable:
		return !insideAggregate
	case *cypher.FunctionInvocation:
		nested := insideAggregate || isAggregate(strings.ToLower(expression.Name.String()))
		for _, argument := range expression.Arguments {
			if hasVariableOutsideAggregate(argument, nested) {
				return true
			}
		}
	case *cypher.UnaryExpression:
		return hasVariableOutsideAggregate(expression.Expression, insideAggregate)
	case *cypher.BinaryExpression:
		return hasVariableOutsideAggregate(expression.Left, insideAggregate) || hasVariableOutsideAggregate(expression.Right, insideAggregate)
	case *cypher.IsNullExpression:
		return hasVariableOutsideAggregate(expression.Expression, insideAggregate)
	case *cypher.PropertyExpression:
		return hasVariableOutsideAggregate(expression.Expression, insideAggregate)
	case *cypher.LabelExpression:
		return hasVariableOutsideAggregate(expression.Expression, insideAggregate)
	case *cypher.IndexExpression:
		return hasVariableOutsideAggregate(expression.Expression, insideAggregate) || hasVariableOutsideAggregate(expression.Index, insideAggregate)
	case *cypher.SliceExpression:
		return hasVariableOutsideAggregate(expression.Expression, insideAggregate) || hasVariableOutsideAggregate(expression.Start, insideAggregate) || hasVariableOutsideAggregate(expression.End, insideAggregate)
	case *cypher.ListLiteral:
		for _, item := range expression.Elements {
			if hasVariableOutsideAggregate(item, insideAggregate) {
				return true
			}
		}
	case *cypher.MapLiteral:
		for _, entry := range expression.Entries {
			if hasVariableOutsideAggregate(entry.Value, insideAggregate) {
				return true
			}
		}
	case *cypher.CaseExpression:
		if hasVariableOutsideAggregate(expression.Operand, insideAggregate) || hasVariableOutsideAggregate(expression.Else, insideAggregate) {
			return true
		}
		for _, alternative := range expression.Alternatives {
			if hasVariableOutsideAggregate(alternative.When, insideAggregate) || hasVariableOutsideAggregate(alternative.Then, insideAggregate) {
				return true
			}
		}
	}
	return false
}

func addPatternVariables(scope variableScope, patterns []cypher.PatternPart) {
	for _, pattern := range patterns {
		if pattern.Variable.Name != "" {
			scope[pattern.Variable.Name] = struct{}{}
		}
		addPatternElementVariables(scope, pattern.Element)
	}
}

func addPatternElementVariables(scope variableScope, element cypher.PatternElement) {
	for _, node := range element.Nodes {
		if node.Variable.Name != "" {
			scope[node.Variable.Name] = struct{}{}
		}
	}
	for _, relationship := range element.Relationships {
		if relationship.Variable.Name != "" {
			scope[relationship.Variable.Name] = struct{}{}
		}
	}
}

func projectionStaticColumns(source string, items []cypher.ProjectionItem, input variableScope) []string {
	columns := make([]string, 0, len(items)+len(input))
	for _, item := range items {
		switch {
		case item.Star:
			columns = append(columns, sortedScope(input)...)
		case item.Alias.Name != "":
			columns = append(columns, item.Alias.Name)
		case item.Expression != nil:
			span := item.Expression.Location()
			if span.Start.Offset >= 0 && span.End.Offset <= len(source) && span.End.Offset > span.Start.Offset {
				columns = append(columns, strings.TrimSpace(source[span.Start.Offset:span.End.Offset]))
			} else {
				columns = append(columns, fmt.Sprintf("column_%d", len(columns)+1))
			}
		}
	}
	return columns
}

func semanticError(node cypher.Node, format string, args ...any) error {
	return &evaluationError{Position: node.Location().Start, Message: fmt.Sprintf(format, args...)}
}

func semanticSpanError(span cypher.Span, format string, args ...any) error {
	return &evaluationError{Position: span.Start, Message: fmt.Sprintf(format, args...)}
}

func cloneScope(scope variableScope) variableScope {
	result := make(variableScope, len(scope))
	for name := range scope {
		result[name] = struct{}{}
	}
	return result
}

func sortedScope(scope variableScope) []string {
	result := make([]string, 0, len(scope))
	for name := range scope {
		result = append(result, name)
	}
	sort.Strings(result)
	return result
}

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}
