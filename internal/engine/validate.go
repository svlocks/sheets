package engine

import (
	"fmt"
	"sort"
	"strings"

	"github.com/svlocks/sheets/internal/cypher"
)

type variableKind uint8

const (
	variableUnknown variableKind = iota
	variableNull
	variableBoolean
	variableInteger
	variableFloat
	variableString
	variableList
	variableMap
	variableNode
	variableRelationship
	variablePath
)

type variableScope map[string]variableKind

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
	if len(query.UnionBranches) > 1 {
		all := query.UnionBranches[0].All
		for _, branch := range query.UnionBranches[1:] {
			if branch.All != all {
				return nil, nil, semanticSpanError(branch.Span, "cannot mix UNION and UNION ALL in the same query")
			}
		}
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
			if err := bindPatternVariables(next, clause.Patterns); err != nil {
				return nil, nil, err
			}
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
			current[clause.Alias.Name] = listElementKind(clause.Expression, current)
		case *cypher.ProjectionClause:
			output, columns, err := validateProjection(source, clause, current)
			if err != nil {
				return nil, nil, err
			}
			resultColumns = columns
			current = output
		case *cypher.CreateClause:
			if err := validateMutationBindings(current, clause.Patterns); err != nil {
				return nil, nil, err
			}
			if err := validateMutationPatterns(clause.Patterns, true); err != nil {
				return nil, nil, err
			}
			next := cloneScope(current)
			if err := bindPatternVariables(next, clause.Patterns); err != nil {
				return nil, nil, err
			}
			if err := validatePatterns(source, clause.Patterns, next, false); err != nil {
				return nil, nil, err
			}
			current = next
		case *cypher.MergeClause:
			if err := validateMutationBindings(current, []cypher.PatternPart{clause.Pattern}); err != nil {
				return nil, nil, err
			}
			if err := validateMutationPatterns([]cypher.PatternPart{clause.Pattern}, false); err != nil {
				return nil, nil, err
			}
			next := cloneScope(current)
			if err := bindPatternVariables(next, []cypher.PatternPart{clause.Pattern}); err != nil {
				return nil, nil, err
			}
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
				kind := expressionKind(expression, current)
				if kind != variableUnknown && kind != variableNull && kind != variableNode && kind != variableRelationship && kind != variablePath && kind != variableList {
					return nil, nil, semanticError(expression, "DELETE expects a node, relationship, path, list of entities, or null; got %s", variableKindName(kind))
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

func validateMutationBindings(input variableScope, patterns []cypher.PatternPart) error {
	bound := cloneScope(input)
	for _, pattern := range patterns {
		created := false
		var existing cypher.Identifier
		for _, node := range pattern.Element.Nodes {
			if node.Variable.Name == "" {
				created = true
				continue
			}
			if _, exists := bound[node.Variable.Name]; exists {
				existing = node.Variable
				if len(node.Labels) != 0 || node.Properties != nil {
					return semanticSpanError(node.Variable.Span, "node variable %q is already bound; bound nodes cannot declare labels or properties in CREATE or MERGE", node.Variable.Name)
				}
				continue
			}
			bound[node.Variable.Name] = variableNode
			created = true
		}
		for _, relationship := range pattern.Element.Relationships {
			if relationship.Variable.Name != "" {
				if _, exists := bound[relationship.Variable.Name]; exists {
					return semanticSpanError(relationship.Variable.Span, "relationship variable %q is already bound and cannot be rebound in CREATE or MERGE", relationship.Variable.Name)
				}
				bound[relationship.Variable.Name] = variableRelationship
			}
			// A relationship pattern always creates (or merges) an entity, even
			// when it is anonymous.
			created = true
		}
		if !created && existing.Name != "" {
			return semanticSpanError(existing.Span, "node variable %q is already bound; CREATE or MERGE must introduce an entity", existing.Name)
		}
	}
	return nil
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
			if !clause.With && len(input) == 0 {
				return nil, nil, semanticSpanError(item.Span, "RETURN * requires at least one variable in scope")
			}
			hasStar = true
			continue
		}
		if err := validateExpression(source, item.Expression, input); err != nil {
			return nil, nil, err
		}
		if err := validatePatternPlacement(item.Expression, false); err != nil {
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
		for name, kind := range input {
			output[name] = kind
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
			output[item.Alias.Name] = expressionKind(item.Expression, input)
		} else if variable, ok := item.Expression.(*cypher.Variable); ok {
			output[variable.Name.Name] = input[variable.Name.Name]
		}
	}

	if err := validateNonAggregateExpression(source, clause.Where, output, "WITH WHERE"); err != nil {
		return nil, nil, err
	}
	orderScope := cloneScope(output)
	if !clause.Distinct {
		for name, kind := range input {
			orderScope[name] = kind
		}
	}
	for _, item := range clause.OrderBy {
		if err := validateExpression(source, item.Expression, orderScope); err != nil {
			return nil, nil, err
		}
		if aggregates {
			if err := validateAggregateOrderExpression(source, item.Expression, clause.Items); err != nil {
				return nil, nil, err
			}
		}
		if err := validatePatternPlacement(item.Expression, false); err != nil {
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

type aggregateOrderScope struct {
	star             bool
	aliases          map[string]struct{}
	groupingAtoms    map[string]struct{}
	aggregates       map[string]struct{}
	complexVariables map[string]struct{}
}

func validateAggregateOrderExpression(source string, expression cypher.Expression, items []cypher.ProjectionItem) error {
	scope := aggregateOrderScope{
		aliases:          make(map[string]struct{}),
		groupingAtoms:    make(map[string]struct{}),
		aggregates:       make(map[string]struct{}),
		complexVariables: make(map[string]struct{}),
	}
	for _, item := range items {
		if item.Star {
			scope.star = true
			continue
		}
		if item.Alias.Name != "" {
			scope.aliases[item.Alias.Name] = struct{}{}
		}
		collectAggregateExpressionKeys(source, item.Expression, scope.aggregates)
		if containsAggregate(item.Expression) {
			continue
		}
		switch item.Expression.(type) {
		case *cypher.Variable, *cypher.PropertyExpression:
			scope.groupingAtoms[expressionSourceKey(source, item.Expression)] = struct{}{}
		default:
			collectExpressionVariables(item.Expression, scope.complexVariables)
		}
	}
	return validateAggregateOrderPart(source, expression, scope, false)
}

func validateAggregateOrderPart(source string, expression cypher.Expression, scope aggregateOrderScope, insideAggregate bool) error { //nolint:gocyclo
	switch expression := expression.(type) {
	case nil, *cypher.Literal, *cypher.Parameter:
		return nil
	case *cypher.Variable:
		if scope.star {
			return nil
		}
		if _, exists := scope.aliases[expression.Name.Name]; exists {
			return nil
		}
		if _, exists := scope.groupingAtoms[expressionSourceKey(source, expression)]; exists {
			return nil
		}
		if _, complex := scope.complexVariables[expression.Name.Name]; complex && !insideAggregate {
			return semanticError(expression, "variables outside an aggregate must be projected as simple grouping keys")
		}
		return semanticError(expression, "variable %q is not defined in the aggregate projection", expression.Name.Name)
	case *cypher.PropertyExpression:
		if scope.star {
			return nil
		}
		if _, exists := scope.groupingAtoms[expressionSourceKey(source, expression)]; exists {
			return nil
		}
		if variable, ok := expression.Expression.(*cypher.Variable); ok {
			if _, complex := scope.complexVariables[variable.Name.Name]; complex && !insideAggregate {
				return semanticError(expression, "variables outside an aggregate must be projected as simple grouping keys")
			}
			return semanticError(expression, "variable %q is not defined in the aggregate projection", variable.Name.Name)
		}
		return validateAggregateOrderPart(source, expression.Expression, scope, insideAggregate)
	case *cypher.FunctionInvocation:
		aggregate := isAggregate(strings.ToLower(expression.Name.String()))
		if aggregate {
			if _, exists := scope.aggregates[expressionSourceKey(source, expression)]; exists {
				return nil
			}
		}
		for _, argument := range expression.Arguments {
			if err := validateAggregateOrderPart(source, argument, scope, insideAggregate || aggregate); err != nil {
				return err
			}
		}
	case *cypher.UnaryExpression:
		return validateAggregateOrderPart(source, expression.Expression, scope, insideAggregate)
	case *cypher.BinaryExpression:
		if err := validateAggregateOrderPart(source, expression.Left, scope, insideAggregate); err != nil {
			return err
		}
		return validateAggregateOrderPart(source, expression.Right, scope, insideAggregate)
	case *cypher.IsNullExpression:
		return validateAggregateOrderPart(source, expression.Expression, scope, insideAggregate)
	case *cypher.LabelExpression:
		return validateAggregateOrderPart(source, expression.Expression, scope, insideAggregate)
	case *cypher.IndexExpression:
		if err := validateAggregateOrderPart(source, expression.Expression, scope, insideAggregate); err != nil {
			return err
		}
		return validateAggregateOrderPart(source, expression.Index, scope, insideAggregate)
	case *cypher.SliceExpression:
		for _, child := range []cypher.Expression{expression.Expression, expression.Start, expression.End} {
			if err := validateAggregateOrderPart(source, child, scope, insideAggregate); err != nil {
				return err
			}
		}
	case *cypher.ListLiteral:
		for _, child := range expression.Elements {
			if err := validateAggregateOrderPart(source, child, scope, insideAggregate); err != nil {
				return err
			}
		}
	case *cypher.MapLiteral:
		for _, entry := range expression.Entries {
			if err := validateAggregateOrderPart(source, entry.Value, scope, insideAggregate); err != nil {
				return err
			}
		}
	case *cypher.CaseExpression:
		for _, child := range []cypher.Expression{expression.Operand, expression.Else} {
			if err := validateAggregateOrderPart(source, child, scope, insideAggregate); err != nil {
				return err
			}
		}
		for _, alternative := range expression.Alternatives {
			if err := validateAggregateOrderPart(source, alternative.When, scope, insideAggregate); err != nil {
				return err
			}
			if err := validateAggregateOrderPart(source, alternative.Then, scope, insideAggregate); err != nil {
				return err
			}
		}
	}
	return nil
}

func collectAggregateExpressionKeys(source string, expression cypher.Expression, result map[string]struct{}) {
	visitExpression(expression, func(candidate cypher.Expression) bool {
		function, ok := candidate.(*cypher.FunctionInvocation)
		if ok && isAggregate(strings.ToLower(function.Name.String())) {
			result[expressionSourceKey(source, candidate)] = struct{}{}
		}
		return true
	})
}

func collectExpressionVariables(expression cypher.Expression, result map[string]struct{}) {
	visitExpression(expression, func(candidate cypher.Expression) bool {
		if variable, ok := candidate.(*cypher.Variable); ok {
			result[variable.Name.Name] = struct{}{}
		}
		return true
	})
}

func expressionSourceKey(source string, expression cypher.Expression) string {
	span := expression.Location()
	if span.Start.Offset >= 0 && span.End.Offset <= len(source) && span.End.Offset > span.Start.Offset {
		return strings.Join(strings.Fields(source[span.Start.Offset:span.End.Offset]), " ")
	}
	return fmt.Sprintf("%T@%d:%d", expression, span.Start.Offset, span.End.Offset)
}

func visitExpression(expression cypher.Expression, visit func(cypher.Expression) bool) { //nolint:gocyclo
	if expression == nil || !visit(expression) {
		return
	}
	switch expression := expression.(type) {
	case *cypher.UnaryExpression:
		visitExpression(expression.Expression, visit)
	case *cypher.BinaryExpression:
		visitExpression(expression.Left, visit)
		visitExpression(expression.Right, visit)
	case *cypher.IsNullExpression:
		visitExpression(expression.Expression, visit)
	case *cypher.PropertyExpression:
		visitExpression(expression.Expression, visit)
	case *cypher.LabelExpression:
		visitExpression(expression.Expression, visit)
	case *cypher.IndexExpression:
		visitExpression(expression.Expression, visit)
		visitExpression(expression.Index, visit)
	case *cypher.SliceExpression:
		visitExpression(expression.Expression, visit)
		visitExpression(expression.Start, visit)
		visitExpression(expression.End, visit)
	case *cypher.FunctionInvocation:
		for _, argument := range expression.Arguments {
			visitExpression(argument, visit)
		}
	case *cypher.ListLiteral:
		for _, child := range expression.Elements {
			visitExpression(child, visit)
		}
	case *cypher.MapLiteral:
		for _, entry := range expression.Entries {
			visitExpression(entry.Value, visit)
		}
	case *cypher.CaseExpression:
		visitExpression(expression.Operand, visit)
		for _, alternative := range expression.Alternatives {
			visitExpression(alternative.When, visit)
			visitExpression(alternative.Then, visit)
		}
		visitExpression(expression.Else, visit)
	case *cypher.ListComprehension:
		visitExpression(expression.List, visit)
		visitExpression(expression.Where, visit)
		visitExpression(expression.Projection, visit)
	case *cypher.ListPredicate:
		visitExpression(expression.List, visit)
		visitExpression(expression.Where, visit)
	case *cypher.ReduceExpression:
		visitExpression(expression.Initial, visit)
		visitExpression(expression.List, visit)
		visitExpression(expression.Expression, visit)
	}
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
			for name, kind := range output {
				if _, exists := next[name]; exists {
					return nil, nil, semanticError(clause, "subquery returns variable %q which is already declared in the outer scope", name)
				}
				next[name] = kind
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
		next[name] = variableUnknown
	}
	if err := validateNonAggregateExpression(source, clause.YieldWhere, next, "YIELD WHERE"); err != nil {
		return nil, nil, err
	}
	return next, sortedScope(next), nil
}

func validatePatterns(source string, patterns []cypher.PatternPart, scope variableScope, allowAggregate bool) error {
	for _, part := range patterns {
		for _, node := range part.Element.Nodes {
			if _, parameter := node.Properties.(*cypher.Parameter); parameter {
				return semanticError(node.Properties, "parameter cannot be used directly as a node pattern predicate; use a map literal")
			}
			if allowAggregate {
				if err := validateExpression(source, node.Properties, scope); err != nil {
					return err
				}
			} else if err := validateNonAggregateExpression(source, node.Properties, scope, "pattern properties"); err != nil {
				return err
			}
		}
		for _, relationship := range part.Element.Relationships {
			if _, parameter := relationship.Properties.(*cypher.Parameter); parameter {
				return semanticError(relationship.Properties, "parameter cannot be used directly as a relationship pattern predicate; use a map literal")
			}
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
	if err := validateNonAggregateExpression(source, expression, variableScope{}, name); err != nil {
		return err
	}
	kind := expressionKind(expression, variableScope{})
	if kind != variableUnknown && kind != variableNull && kind != variableInteger {
		return semanticError(expression, "%s expects an integer, got %s", name, variableKindName(kind))
	}
	if value, known := staticInteger(expression); known && value < 0 {
		return semanticError(expression, "%s expects a non-negative integer", name)
	}
	return nil
}

func validateNonAggregateExpression(source string, expression cypher.Expression, scope variableScope, context string) error {
	if expression == nil {
		return nil
	}
	if err := validateExpression(source, expression, scope); err != nil {
		return err
	}
	allowPattern := context == "MATCH WHERE" || context == "WITH WHERE" || context == "YIELD WHERE"
	if err := validatePatternPlacement(expression, allowPattern); err != nil {
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
		if err := validateExpression(source, expression.Expression, scope); err != nil {
			return err
		}
		kind := expressionKind(expression.Expression, scope)
		if strings.EqualFold(expression.Operator, "NOT") && isKnownNonNullKind(kind) && kind != variableBoolean {
			return semanticError(expression, "NOT expects a boolean, got %s", variableKindName(kind))
		}
		if (expression.Operator == "+" || expression.Operator == "-") && isKnownNonNullKind(kind) && !isNumericKind(kind) {
			return semanticError(expression, "%s expects a number, got %s", expression.Operator, variableKindName(kind))
		}
	case *cypher.BinaryExpression:
		if err := validateExpression(source, expression.Left, scope); err != nil {
			return err
		}
		if err := validateExpression(source, expression.Right, scope); err != nil {
			return err
		}
		if err := validateBinaryStaticTypes(expression, scope); err != nil {
			return err
		}
	case *cypher.IsNullExpression:
		return validateExpression(source, expression.Expression, scope)
	case *cypher.PropertyExpression:
		if err := validateExpression(source, expression.Expression, scope); err != nil {
			return err
		}
		kind := expressionKind(expression.Expression, scope)
		if isKnownNonNullKind(kind) && kind != variableNode && kind != variableRelationship && kind != variableMap {
			return semanticError(expression, "property access expects a node, relationship, or map; got %s", variableKindName(kind))
		}
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
		name := strings.ToLower(expression.Name.String())
		for _, argument := range expression.Arguments {
			if pattern, ok := argument.(*cypher.PatternExpression); ok && (name == "shortestpath" || name == "allshortestpaths") {
				inner := cloneScope(scope)
				if err := bindPatternVariables(inner, []cypher.PatternPart{{Element: pattern.Pattern}}); err != nil {
					return err
				}
				if err := validatePatterns(source, []cypher.PatternPart{{Element: pattern.Pattern}}, inner, false); err != nil {
					return err
				}
				continue
			}
			if err := validateExpression(source, argument, scope); err != nil {
				return err
			}
		}
		if err := validateFunctionStaticTypes(expression, scope); err != nil {
			return err
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
		inner[expression.Variable.Name] = listElementKind(expression.List, scope)
		if err := validateExpression(source, expression.Where, inner); err != nil {
			return err
		}
		return validateExpression(source, expression.Projection, inner)
	case *cypher.ListPredicate:
		if err := validateExpression(source, expression.List, scope); err != nil {
			return err
		}
		inner := cloneScope(scope)
		inner[expression.Variable.Name] = listElementKind(expression.List, scope)
		return validateExpression(source, expression.Where, inner)
	case *cypher.ReduceExpression:
		if err := validateExpression(source, expression.Initial, scope); err != nil {
			return err
		}
		if err := validateExpression(source, expression.List, scope); err != nil {
			return err
		}
		inner := cloneScope(scope)
		inner[expression.Accumulator.Name] = expressionKind(expression.Initial, scope)
		inner[expression.Variable.Name] = listElementKind(expression.List, scope)
		return validateExpression(source, expression.Expression, inner)
	case *cypher.PatternExpression:
		if len(expression.Pattern.Relationships) == 0 {
			return semanticError(expression, "pattern predicate must contain at least one relationship")
		}
		if err := requirePatternVariables(scope, expression.Pattern); err != nil {
			return err
		}
		return validatePatterns(source, []cypher.PatternPart{{Element: expression.Pattern}}, scope, false)
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

func validateBinaryStaticTypes(expression *cypher.BinaryExpression, scope variableScope) error {
	operator := strings.ToUpper(strings.TrimSpace(expression.Operator))
	left := expressionKind(expression.Left, scope)
	right := expressionKind(expression.Right, scope)
	switch operator {
	case "AND", "OR", "XOR":
		if isKnownNonNullKind(left) && left != variableBoolean {
			return semanticError(expression.Left, "%s expects booleans, got %s", operator, variableKindName(left))
		}
		if isKnownNonNullKind(right) && right != variableBoolean {
			return semanticError(expression.Right, "%s expects booleans, got %s", operator, variableKindName(right))
		}
	case "IN", "NOT IN":
		if isKnownNonNullKind(right) && right != variableList {
			return semanticError(expression.Right, "%s expects a list on its right side, got %s", operator, variableKindName(right))
		}
	case "STARTS WITH", "ENDS WITH", "CONTAINS", "=~":
		if isKnownNonNullKind(left) && left != variableString {
			return semanticError(expression.Left, "%s expects strings, got %s", operator, variableKindName(left))
		}
		if isKnownNonNullKind(right) && right != variableString {
			return semanticError(expression.Right, "%s expects strings, got %s", operator, variableKindName(right))
		}
	case "-", "*", "/", "%", "^":
		if isKnownNonNullKind(left) && !isNumericKind(left) {
			return semanticError(expression.Left, "%s expects numbers, got %s", operator, variableKindName(left))
		}
		if isKnownNonNullKind(right) && !isNumericKind(right) {
			return semanticError(expression.Right, "%s expects numbers, got %s", operator, variableKindName(right))
		}
	}
	return nil
}

func validateFunctionStaticTypes(expression *cypher.FunctionInvocation, scope variableScope) error {
	if len(expression.Arguments) != 1 {
		return nil
	}
	containsPattern := false
	visitExpression(expression.Arguments[0], func(candidate cypher.Expression) bool {
		if _, ok := candidate.(*cypher.PatternExpression); ok {
			containsPattern = true
			return false
		}
		return true
	})
	if containsPattern {
		// Placement validation owns the more specific UnexpectedSyntax
		// classification for pattern expressions used as ordinary arguments.
		return nil
	}
	kind := expressionKind(expression.Arguments[0], scope)
	if !isKnownNonNullKind(kind) {
		return nil
	}
	name := strings.ToLower(expression.Name.String())
	allowed := func(kinds ...variableKind) bool {
		for _, allowedKind := range kinds {
			if kind == allowedKind {
				return true
			}
		}
		return false
	}
	switch name {
	case "labels":
		if !allowed(variableNode) {
			return semanticError(expression, "labels expects a node, got %s", variableKindName(kind))
		}
	case "type":
		if !allowed(variableRelationship) {
			return semanticError(expression, "type expects a relationship, got %s", variableKindName(kind))
		}
	case "properties", "keys":
		if !allowed(variableNode, variableRelationship, variableMap) {
			return semanticError(expression, "%s expects a node, relationship, or map; got %s", name, variableKindName(kind))
		}
	case "size":
		if !allowed(variableList, variableString) {
			return semanticError(expression, "size expects a string or list, got %s", variableKindName(kind))
		}
	case "length":
		if !allowed(variablePath, variableList, variableString) {
			return semanticError(expression, "length expects a path, string, or list; got %s", variableKindName(kind))
		}
	case "nodes", "relationships":
		if !allowed(variablePath) {
			return semanticError(expression, "%s expects a path, got %s", name, variableKindName(kind))
		}
	case "startnode", "endnode":
		if !allowed(variableRelationship) {
			return semanticError(expression, "%s expects a relationship, got %s", name, variableKindName(kind))
		}
	}
	return nil
}

func isKnownNonNullKind(kind variableKind) bool {
	return kind != variableUnknown && kind != variableNull
}

func isNumericKind(kind variableKind) bool {
	return kind == variableInteger || kind == variableFloat
}

func staticInteger(expression cypher.Expression) (int64, bool) {
	switch expression := expression.(type) {
	case *cypher.Literal:
		value, ok := expression.Value.(int64)
		return value, ok
	case *cypher.UnaryExpression:
		value, ok := staticInteger(expression.Expression)
		if !ok {
			return 0, false
		}
		switch expression.Operator {
		case "+":
			return value, true
		case "-":
			if value == -value && value != 0 {
				return 0, false
			}
			return -value, true
		}
	}
	return 0, false
}

func requirePatternVariables(scope variableScope, pattern cypher.PatternElement) error {
	for _, node := range pattern.Nodes {
		if err := requirePatternVariable(scope, node.Variable, variableNode); err != nil {
			return err
		}
	}
	for _, relationship := range pattern.Relationships {
		kind := variableRelationship
		if relationship.Length != nil {
			kind = variableList
		}
		if err := requirePatternVariable(scope, relationship.Variable, kind); err != nil {
			return err
		}
	}
	return nil
}

func requirePatternVariable(scope variableScope, identifier cypher.Identifier, expected variableKind) error {
	if identifier.Name == "" {
		return nil
	}
	kind, exists := scope[identifier.Name]
	if !exists {
		return semanticSpanError(identifier.Span, "variable %q is not defined", identifier.Name)
	}
	if kind != variableUnknown && kind != variableNull && kind != expected {
		return semanticSpanError(identifier.Span, "variable %q has a type conflict: previously %s, used as %s", identifier.Name, variableKindName(kind), variableKindName(expected))
	}
	return nil
}

func validatePatternPlacement(expression cypher.Expression, allowed bool) error { //nolint:gocyclo
	switch expression := expression.(type) {
	case nil, *cypher.Literal, *cypher.Variable, *cypher.Parameter:
		return nil
	case *cypher.PatternExpression:
		if !allowed {
			return semanticError(expression, "pattern expression is not allowed in this context")
		}
	case *cypher.UnaryExpression:
		return validatePatternPlacement(expression.Expression, allowed)
	case *cypher.BinaryExpression:
		if err := validatePatternPlacement(expression.Left, allowed); err != nil {
			return err
		}
		return validatePatternPlacement(expression.Right, allowed)
	case *cypher.IsNullExpression:
		return validatePatternPlacement(expression.Expression, allowed)
	case *cypher.PropertyExpression:
		return validatePatternPlacement(expression.Expression, allowed)
	case *cypher.LabelExpression:
		return validatePatternPlacement(expression.Expression, allowed)
	case *cypher.IndexExpression:
		if err := validatePatternPlacement(expression.Expression, allowed); err != nil {
			return err
		}
		return validatePatternPlacement(expression.Index, allowed)
	case *cypher.SliceExpression:
		for _, child := range []cypher.Expression{expression.Expression, expression.Start, expression.End} {
			if err := validatePatternPlacement(child, allowed); err != nil {
				return err
			}
		}
	case *cypher.FunctionInvocation:
		name := strings.ToLower(expression.Name.String())
		argumentAllowsPattern := name == "exists" || name == "shortestpath" || name == "allshortestpaths"
		for _, argument := range expression.Arguments {
			if err := validatePatternPlacement(argument, argumentAllowsPattern); err != nil {
				return err
			}
		}
	case *cypher.ListLiteral:
		for _, child := range expression.Elements {
			if err := validatePatternPlacement(child, allowed); err != nil {
				return err
			}
		}
	case *cypher.MapLiteral:
		for _, entry := range expression.Entries {
			if err := validatePatternPlacement(entry.Value, allowed); err != nil {
				return err
			}
		}
	case *cypher.CaseExpression:
		if err := validatePatternPlacement(expression.Operand, allowed); err != nil {
			return err
		}
		for _, alternative := range expression.Alternatives {
			if err := validatePatternPlacement(alternative.When, allowed); err != nil {
				return err
			}
			if err := validatePatternPlacement(alternative.Then, allowed); err != nil {
				return err
			}
		}
		return validatePatternPlacement(expression.Else, allowed)
	case *cypher.ListComprehension:
		for _, child := range []cypher.Expression{expression.List, expression.Where, expression.Projection} {
			if err := validatePatternPlacement(child, allowed); err != nil {
				return err
			}
		}
	case *cypher.ListPredicate:
		if err := validatePatternPlacement(expression.List, allowed); err != nil {
			return err
		}
		return validatePatternPlacement(expression.Where, allowed)
	case *cypher.ReduceExpression:
		for _, child := range []cypher.Expression{expression.Initial, expression.List, expression.Expression} {
			if err := validatePatternPlacement(child, allowed); err != nil {
				return err
			}
		}
	case *cypher.ExistsSubquery:
		return nil
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

func bindPatternVariables(scope variableScope, patterns []cypher.PatternPart) error {
	for _, pattern := range patterns {
		for _, node := range pattern.Element.Nodes {
			if err := bindPatternVariable(scope, node.Variable, variableNode); err != nil {
				return err
			}
		}
		for _, relationship := range pattern.Element.Relationships {
			kind := variableRelationship
			if relationship.Length != nil {
				kind = variableList
			}
			if err := bindPatternVariable(scope, relationship.Variable, kind); err != nil {
				return err
			}
		}
		if pattern.Variable.Name != "" {
			if _, exists := scope[pattern.Variable.Name]; exists {
				return semanticSpanError(pattern.Variable.Span, "path variable %q is already bound", pattern.Variable.Name)
			}
			scope[pattern.Variable.Name] = variablePath
		}
	}
	return nil
}

func bindPatternVariable(scope variableScope, identifier cypher.Identifier, kind variableKind) error {
	if identifier.Name == "" {
		return nil
	}
	existing, exists := scope[identifier.Name]
	if !exists || existing == variableUnknown || existing == variableNull {
		scope[identifier.Name] = kind
		return nil
	}
	if existing != kind {
		return semanticSpanError(identifier.Span, "variable %q has a type conflict: previously %s, used as %s", identifier.Name, variableKindName(existing), variableKindName(kind))
	}
	return nil
}

func variableKindName(kind variableKind) string {
	switch kind {
	case variableNull:
		return "null"
	case variableBoolean:
		return "boolean"
	case variableInteger:
		return "integer"
	case variableFloat:
		return "float"
	case variableString:
		return "string"
	case variableList:
		return "list"
	case variableMap:
		return "map"
	case variableNode:
		return "node"
	case variableRelationship:
		return "relationship"
	case variablePath:
		return "path"
	default:
		return "value"
	}
}

func expressionKind(expression cypher.Expression, scope variableScope) variableKind { //nolint:gocyclo
	switch expression := expression.(type) {
	case nil:
		return variableUnknown
	case *cypher.Literal:
		switch expression.Kind {
		case cypher.NullLiteral:
			return variableNull
		case cypher.BooleanLiteral:
			return variableBoolean
		case cypher.IntegerLiteral:
			return variableInteger
		case cypher.FloatLiteral:
			return variableFloat
		case cypher.StringLiteral:
			return variableString
		}
	case *cypher.Variable:
		return scope[expression.Name.Name]
	case *cypher.UnaryExpression:
		if strings.EqualFold(expression.Operator, "NOT") {
			return variableBoolean
		}
		return expressionKind(expression.Expression, scope)
	case *cypher.BinaryExpression:
		switch strings.ToUpper(strings.TrimSpace(expression.Operator)) {
		case "AND", "OR", "XOR", "=", "<>", "!=", "<", "<=", ">", ">=", "IN", "NOT IN", "STARTS WITH", "ENDS WITH", "CONTAINS", "=~":
			return variableBoolean
		case "+", "-", "*", "/", "%", "^":
			left, right := expressionKind(expression.Left, scope), expressionKind(expression.Right, scope)
			if left == variableString && strings.TrimSpace(expression.Operator) == "+" {
				return variableString
			}
			if left == variableList || right == variableList {
				return variableList
			}
			if left == variableFloat || right == variableFloat || strings.TrimSpace(expression.Operator) == "^" {
				return variableFloat
			}
			if left == variableInteger && right == variableInteger {
				return variableInteger
			}
		}
	case *cypher.IsNullExpression, *cypher.LabelExpression, *cypher.ExistsSubquery, *cypher.PatternExpression:
		return variableBoolean
	case *cypher.PropertyExpression, *cypher.IndexExpression, *cypher.Parameter:
		return variableUnknown
	case *cypher.SliceExpression, *cypher.ListLiteral, *cypher.ListComprehension:
		return variableList
	case *cypher.MapLiteral:
		return variableMap
	case *cypher.ListPredicate:
		return variableBoolean
	case *cypher.ReduceExpression:
		return expressionKind(expression.Initial, scope)
	case *cypher.CaseExpression:
		kind := expressionKind(expression.Else, scope)
		for _, alternative := range expression.Alternatives {
			candidate := expressionKind(alternative.Then, scope)
			if kind == variableUnknown || kind == variableNull {
				kind = candidate
			} else if candidate != variableNull && candidate != kind {
				return variableUnknown
			}
		}
		return kind
	case *cypher.FunctionInvocation:
		return functionResultKind(strings.ToLower(expression.Name.String()))
	}
	return variableUnknown
}

func functionResultKind(name string) variableKind {
	switch name {
	case "labels", "keys", "nodes", "relationships", "range", "collect":
		return variableList
	case "properties":
		return variableMap
	case "type", "tostring", "tostringornull", "trim", "ltrim", "rtrim", "tolower", "lower", "toupper", "upper", "replace", "substring", "left", "right":
		return variableString
	case "toboolean", "tobooleanornull", "exists":
		return variableBoolean
	case "tointeger", "tointegerornull", "size", "length", "id", "count":
		return variableInteger
	case "tofloat", "tofloatornull", "avg", "stdev", "stdevp":
		return variableFloat
	}
	return variableUnknown
}

func listElementKind(expression cypher.Expression, scope variableScope) variableKind {
	switch expression := expression.(type) {
	case *cypher.ListLiteral:
		kind := variableUnknown
		for _, element := range expression.Elements {
			candidate := expressionKind(element, scope)
			if candidate == variableNull {
				continue
			}
			if kind == variableUnknown {
				kind = candidate
			} else if candidate != kind {
				return variableUnknown
			}
		}
		return kind
	case *cypher.ListComprehension:
		if expression.Projection != nil {
			inner := cloneScope(scope)
			inner[expression.Variable.Name] = listElementKind(expression.List, scope)
			return expressionKind(expression.Projection, inner)
		}
		return listElementKind(expression.List, scope)
	}
	return variableUnknown
}

func addPatternVariables(scope variableScope, patterns []cypher.PatternPart) {
	for _, pattern := range patterns {
		if pattern.Variable.Name != "" {
			scope[pattern.Variable.Name] = variablePath
		}
		addPatternElementVariables(scope, pattern.Element)
	}
}

func addPatternElementVariables(scope variableScope, element cypher.PatternElement) {
	for _, node := range element.Nodes {
		if node.Variable.Name != "" {
			scope[node.Variable.Name] = variableNode
		}
	}
	for _, relationship := range element.Relationships {
		if relationship.Variable.Name != "" {
			if relationship.Length != nil {
				scope[relationship.Variable.Name] = variableList
			} else {
				scope[relationship.Variable.Name] = variableRelationship
			}
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
	for name, kind := range scope {
		result[name] = kind
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
