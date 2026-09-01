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
			if err := validateRelationshipUniqueness(clause.Patterns); err != nil {
				return nil, nil, err
			}
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
			if relationship.Direction == cypher.Bidirectional {
				return semanticSpanError(relationship.Span, "CREATE and MERGE relationships must have a direction")
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
	aggregateScope := aggregateScopeForProjection(source, clause.Items, false)
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
		if containsAggregate(item.Expression) {
			if err := validateAggregateExpressionPart(source, item.Expression, aggregateScope, false, true, nil); err != nil {
				return nil, nil, err
			}
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

	whereScope := cloneScope(output)
	if !aggregates {
		for name, kind := range input {
			whereScope[name] = kind
		}
	}
	if err := validateNonAggregateExpression(source, clause.Where, whereScope, "WITH WHERE"); err != nil {
		return nil, nil, err
	}
	orderScope := cloneScope(output)
	if !clause.Distinct {
		for name, kind := range input {
			orderScope[name] = kind
		}
	}
	for _, item := range clause.OrderBy {
		var err error
		if clause.Distinct && !aggregates {
			err = validateDistinctOrderExpression(source, item.Expression, clause.Items, input, output)
		} else {
			err = validateExpression(source, item.Expression, orderScope)
		}
		if err != nil {
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
	if clause.With {
		for _, item := range clause.Items {
			if item.Star || item.Alias.Name != "" {
				continue
			}
			if _, variable := item.Expression.(*cypher.Variable); !variable {
				return nil, nil, semanticError(item.Expression, "WITH expressions must be aliased")
			}
		}
	}
	return output, columns, nil
}

func validateDistinctOrderExpression(
	source string,
	expression cypher.Expression,
	items []cypher.ProjectionItem,
	input, output variableScope,
) error {
	if err := validateExpression(source, expression, output); err == nil {
		return nil
	}
	key := expressionSourceKey(source, expression)
	for _, item := range items {
		if item.Star || expressionSourceKey(source, item.Expression) != key {
			continue
		}
		return validateExpression(source, expression, input)
	}
	return validateExpression(source, expression, output)
}

func validateRelationshipUniqueness(patterns []cypher.PatternPart) error {
	for _, pattern := range patterns {
		seen := make(map[string]struct{}, len(pattern.Element.Relationships))
		for _, relationship := range pattern.Element.Relationships {
			name := relationship.Variable.Name
			if name == "" {
				continue
			}
			if _, exists := seen[name]; exists {
				return semanticSpanError(relationship.Variable.Span, "relationship variable %q cannot be reused in the same pattern", name)
			}
			seen[name] = struct{}{}
		}
	}
	return nil
}

type aggregateOrderScope struct {
	star             bool
	aliases          map[string]struct{}
	groupingAtoms    map[string]struct{}
	aggregates       map[string]struct{}
	complexVariables map[string]struct{}
}

func validateAggregateOrderExpression(source string, expression cypher.Expression, items []cypher.ProjectionItem) error {
	scope := aggregateScopeForProjection(source, items, true)
	return validateAggregateExpressionPart(source, expression, scope, false, false, nil)
}

func aggregateScopeForProjection(source string, items []cypher.ProjectionItem, includeAliases bool) aggregateOrderScope {
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
		if includeAliases && item.Alias.Name != "" {
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
	return scope
}

func validateAggregateExpressionPart( //nolint:gocyclo
	source string,
	expression cypher.Expression,
	scope aggregateOrderScope,
	insideAggregate bool,
	projection bool,
	locals map[string]struct{},
) error {
	switch expression := expression.(type) {
	case nil, *cypher.Literal, *cypher.Parameter:
		return nil
	case *cypher.Variable:
		if _, local := locals[expression.Name.Name]; local {
			return nil
		}
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
		if projection && !insideAggregate {
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
			if _, local := locals[variable.Name.Name]; local {
				return nil
			}
			if _, complex := scope.complexVariables[variable.Name.Name]; complex && !insideAggregate {
				return semanticError(expression, "variables outside an aggregate must be projected as simple grouping keys")
			}
			if projection && !insideAggregate {
				return semanticError(expression, "variables outside an aggregate must be projected as simple grouping keys")
			}
			return semanticError(expression, "variable %q is not defined in the aggregate projection", variable.Name.Name)
		}
		return validateAggregateExpressionPart(source, expression.Expression, scope, insideAggregate, projection, locals)
	case *cypher.FunctionInvocation:
		aggregate := isAggregate(strings.ToLower(expression.Name.String()))
		if aggregate {
			if _, exists := scope.aggregates[expressionSourceKey(source, expression)]; exists {
				return nil
			}
		}
		for _, argument := range expression.Arguments {
			if err := validateAggregateExpressionPart(source, argument, scope, insideAggregate || aggregate, projection, locals); err != nil {
				return err
			}
		}
	case *cypher.UnaryExpression:
		return validateAggregateExpressionPart(source, expression.Expression, scope, insideAggregate, projection, locals)
	case *cypher.BinaryExpression:
		if err := validateAggregateExpressionPart(source, expression.Left, scope, insideAggregate, projection, locals); err != nil {
			return err
		}
		return validateAggregateExpressionPart(source, expression.Right, scope, insideAggregate, projection, locals)
	case *cypher.IsNullExpression:
		return validateAggregateExpressionPart(source, expression.Expression, scope, insideAggregate, projection, locals)
	case *cypher.LabelExpression:
		return validateAggregateExpressionPart(source, expression.Expression, scope, insideAggregate, projection, locals)
	case *cypher.IndexExpression:
		if err := validateAggregateExpressionPart(source, expression.Expression, scope, insideAggregate, projection, locals); err != nil {
			return err
		}
		return validateAggregateExpressionPart(source, expression.Index, scope, insideAggregate, projection, locals)
	case *cypher.SliceExpression:
		for _, child := range []cypher.Expression{expression.Expression, expression.Start, expression.End} {
			if err := validateAggregateExpressionPart(source, child, scope, insideAggregate, projection, locals); err != nil {
				return err
			}
		}
	case *cypher.ListLiteral:
		for _, child := range expression.Elements {
			if err := validateAggregateExpressionPart(source, child, scope, insideAggregate, projection, locals); err != nil {
				return err
			}
		}
	case *cypher.MapLiteral:
		for _, entry := range expression.Entries {
			if err := validateAggregateExpressionPart(source, entry.Value, scope, insideAggregate, projection, locals); err != nil {
				return err
			}
		}
	case *cypher.CaseExpression:
		for _, child := range []cypher.Expression{expression.Operand, expression.Else} {
			if err := validateAggregateExpressionPart(source, child, scope, insideAggregate, projection, locals); err != nil {
				return err
			}
		}
		for _, alternative := range expression.Alternatives {
			if err := validateAggregateExpressionPart(source, alternative.When, scope, insideAggregate, projection, locals); err != nil {
				return err
			}
			if err := validateAggregateExpressionPart(source, alternative.Then, scope, insideAggregate, projection, locals); err != nil {
				return err
			}
		}
	case *cypher.ListComprehension:
		if err := validateAggregateExpressionPart(source, expression.List, scope, insideAggregate, projection, locals); err != nil {
			return err
		}
		inner := cloneNames(locals)
		inner[expression.Variable.Name] = struct{}{}
		if err := validateAggregateExpressionPart(source, expression.Where, scope, insideAggregate, projection, inner); err != nil {
			return err
		}
		return validateAggregateExpressionPart(source, expression.Projection, scope, insideAggregate, projection, inner)
	case *cypher.PatternComprehension:
		if projection && !insideAggregate {
			return semanticError(expression, "expressions outside an aggregate must be projected as simple grouping keys")
		}
		inner := cloneNames(locals)
		addPatternExpressionLocalNames(inner, expression)
		if err := validateAggregateExpressionPart(source, expression.Where, scope, insideAggregate, projection, inner); err != nil {
			return err
		}
		return validateAggregateExpressionPart(source, expression.Projection, scope, insideAggregate, projection, inner)
	case *cypher.ListPredicate:
		if err := validateAggregateExpressionPart(source, expression.List, scope, insideAggregate, projection, locals); err != nil {
			return err
		}
		inner := cloneNames(locals)
		inner[expression.Variable.Name] = struct{}{}
		return validateAggregateExpressionPart(source, expression.Where, scope, insideAggregate, projection, inner)
	case *cypher.ReduceExpression:
		if err := validateAggregateExpressionPart(source, expression.Initial, scope, insideAggregate, projection, locals); err != nil {
			return err
		}
		if err := validateAggregateExpressionPart(source, expression.List, scope, insideAggregate, projection, locals); err != nil {
			return err
		}
		inner := cloneNames(locals)
		inner[expression.Accumulator.Name] = struct{}{}
		inner[expression.Variable.Name] = struct{}{}
		return validateAggregateExpressionPart(source, expression.Expression, scope, insideAggregate, projection, inner)
	case *cypher.ExistsSubquery, *cypher.PatternExpression:
		if projection && !insideAggregate {
			return semanticError(expression, "expressions outside an aggregate must be projected as simple grouping keys")
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
	case *cypher.PatternComprehension:
		visitPatternElementExpressions(expression.Pattern, visit)
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

func visitPatternElementExpressions(element cypher.PatternElement, visit func(cypher.Expression) bool) {
	for _, node := range element.Nodes {
		visitExpression(node.Properties, visit)
	}
	for _, relationship := range element.Relationships {
		visitExpression(relationship.Properties, visit)
		if relationship.Length != nil {
			visitExpression(relationship.Length.Lower, visit)
			visitExpression(relationship.Length.Upper, visit)
		}
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
	allowPattern := context == "MATCH WHERE" || context == "WITH WHERE" || context == "YIELD WHERE" || context == "pattern comprehension WHERE"
	if err := validatePatternPlacement(expression, allowPattern); err != nil {
		return err
	}
	if containsAggregate(expression) {
		return semanticError(expression, "%s does not allow aggregate functions", context)
	}
	if strings.HasSuffix(context, "WHERE") {
		kind := expressionKind(expression, scope)
		if isKnownNonNullKind(kind) && kind != variableBoolean {
			return semanticError(expression, "%s expects a boolean, got %s", context, variableKindName(kind))
		}
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
		if !cypher.IsSupportedFunction(name) {
			return semanticError(expression, "unknown function %s", expression.Name.String())
		}
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
	case *cypher.PatternComprehension:
		if len(expression.Pattern.Relationships) == 0 {
			return semanticError(expression, "pattern comprehension must contain at least one relationship")
		}
		part := cypher.PatternPart{Variable: expression.Variable, Element: expression.Pattern}
		if err := validateRelationshipUniqueness([]cypher.PatternPart{part}); err != nil {
			return err
		}
		inner := cloneScope(scope)
		if err := bindPatternVariables(inner, []cypher.PatternPart{part}); err != nil {
			return err
		}
		if err := validatePatterns(source, []cypher.PatternPart{part}, inner, false); err != nil {
			return err
		}
		if err := validateNonAggregateExpression(source, expression.Where, inner, "pattern comprehension WHERE"); err != nil {
			return err
		}
		return validateNonAggregateExpression(source, expression.Projection, inner, "pattern comprehension projection")
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
		if statementMutates(expression.Subquery) {
			return semanticError(expression, "EXISTS subquery cannot contain updating clauses")
		}
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
	case "=~":
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
	case *cypher.PatternComprehension:
		if err := validatePatternPlacement(expression.Where, true); err != nil {
			return err
		}
		return validatePatternPlacement(expression.Projection, false)
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
		name := strings.ToLower(expression.Name.String())
		aggregate := isAggregate(name)
		if aggregate && insideAggregate {
			return semanticError(expression, "aggregate functions cannot be nested")
		}
		if insideAggregate && name == "rand" {
			return semanticError(expression, "non-constant function rand is not allowed inside an aggregate")
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
	case *cypher.ListComprehension:
		if err := validateAggregateShape(expression.List, insideAggregate); err != nil {
			return err
		}
		for _, inner := range []cypher.Expression{expression.Where, expression.Projection} {
			if containsAggregate(inner) {
				return semanticError(inner, "aggregate functions are not allowed inside a list comprehension")
			}
		}
	case *cypher.PatternComprehension:
		for _, inner := range []cypher.Expression{expression.Where, expression.Projection} {
			if containsAggregate(inner) {
				return semanticError(inner, "aggregate functions are not allowed inside a pattern comprehension")
			}
		}
	case *cypher.ListPredicate:
		if err := validateAggregateShape(expression.List, insideAggregate); err != nil {
			return err
		}
		if containsAggregate(expression.Where) {
			return semanticError(expression.Where, "aggregate functions are not allowed inside a list predicate")
		}
	case *cypher.ReduceExpression:
		for _, outer := range []cypher.Expression{expression.Initial, expression.List} {
			if err := validateAggregateShape(outer, insideAggregate); err != nil {
				return err
			}
		}
		if containsAggregate(expression.Expression) {
			return semanticError(expression.Expression, "aggregate functions are not allowed inside reduce")
		}
	}
	return nil
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
	case *cypher.SliceExpression, *cypher.ListLiteral, *cypher.ListComprehension, *cypher.PatternComprehension:
		return variableList
	case *cypher.MapLiteral:
		return variableMap
	case *cypher.ListPredicate:
		return variableBoolean
	case *cypher.ReduceExpression:
		initial := expressionKind(expression.Initial, scope)
		inner := cloneScope(scope)
		inner[expression.Accumulator.Name] = initial
		inner[expression.Variable.Name] = listElementKind(expression.List, scope)
		return mergeExpressionKinds(initial, expressionKind(expression.Expression, inner))
	case *cypher.CaseExpression:
		kind := variableNull
		if expression.Else != nil {
			kind = expressionKind(expression.Else, scope)
		}
		for _, alternative := range expression.Alternatives {
			kind = mergeExpressionKinds(kind, expressionKind(alternative.Then, scope))
		}
		return kind
	case *cypher.FunctionInvocation:
		return functionResultKind(strings.ToLower(expression.Name.String()))
	}
	return variableUnknown
}

func functionResultKind(name string) variableKind {
	switch name {
	case "labels", "keys", "nodes", "relationships", "range", "collect", "split":
		return variableList
	case "properties":
		return variableMap
	case "type", "tostring", "tostringornull", "trim", "ltrim", "rtrim", "tolower", "lower", "toupper", "upper", "replace", "substring", "left", "right":
		return variableString
	case "toboolean", "tobooleanornull", "exists":
		return variableBoolean
	case "tointeger", "tointegerornull", "size", "length", "id", "count", "sign":
		return variableInteger
	case "tofloat", "tofloatornull", "avg", "stdev", "stdevp", "ceil", "sqrt", "rand":
		return variableFloat
	}
	return variableUnknown
}

func listElementKind(expression cypher.Expression, scope variableScope) variableKind {
	switch expression := expression.(type) {
	case *cypher.ListLiteral:
		kind := variableUnknown
		set := false
		for _, element := range expression.Elements {
			candidate := expressionKind(element, scope)
			if candidate == variableNull {
				continue
			}
			if candidate == variableUnknown {
				return variableUnknown
			}
			if !set {
				kind = candidate
				set = true
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
	case *cypher.PatternComprehension:
		inner := cloneScope(scope)
		part := cypher.PatternPart{Variable: expression.Variable, Element: expression.Pattern}
		if err := bindPatternVariables(inner, []cypher.PatternPart{part}); err != nil {
			return variableUnknown
		}
		return expressionKind(expression.Projection, inner)
	}
	return variableUnknown
}

func mergeExpressionKinds(left, right variableKind) variableKind {
	if left == variableUnknown || right == variableUnknown {
		return variableUnknown
	}
	if left == variableNull {
		return right
	}
	if right == variableNull || left == right {
		return left
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

func addPatternExpressionLocalNames(names map[string]struct{}, expression *cypher.PatternComprehension) {
	if expression.Variable.Name != "" {
		names[expression.Variable.Name] = struct{}{}
	}
	for _, node := range expression.Pattern.Nodes {
		if node.Variable.Name != "" {
			names[node.Variable.Name] = struct{}{}
		}
	}
	for _, relationship := range expression.Pattern.Relationships {
		if relationship.Variable.Name != "" {
			names[relationship.Variable.Name] = struct{}{}
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

func cloneNames(names map[string]struct{}) map[string]struct{} {
	result := make(map[string]struct{}, len(names)+1)
	for name := range names {
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
