package engine

import (
	"context"
	"strings"

	"github.com/svlocks/sheets/internal/cypher"
	"github.com/svlocks/sheets/internal/domain"
	"github.com/svlocks/sheets/internal/store"
)

// documentRequiresGraph is deliberately conservative. It recognizes the
// scalar/list pipeline and sheets.revisions as graph-free; every graph
// pattern, mutation, introspection procedure, or graph-bearing subquery keeps
// using the complete snapshot implementation.
func documentRequiresGraph(document *cypher.Document) bool {
	for _, statement := range document.Statements {
		query, ok := statement.(*cypher.QueryStatement)
		if !ok || queryRequiresGraph(query) {
			return true
		}
	}
	return false
}

func queryRequiresGraph(query *cypher.QueryStatement) bool {
	if query.Explain {
		return false
	}
	if clausesRequireGraph(query.Clauses) {
		return true
	}
	for _, branch := range query.UnionBranches {
		if branch.Query == nil || queryRequiresGraph(branch.Query) {
			return true
		}
	}
	return false
}

func clausesRequireGraph(clauses []cypher.Clause) bool {
	for _, clause := range clauses {
		switch clause := clause.(type) {
		case *cypher.MatchClause, *cypher.CreateClause, *cypher.MergeClause,
			*cypher.SetClause, *cypher.RemoveClause, *cypher.DeleteClause:
			return true
		case *cypher.UnwindClause:
			if expressionRequiresGraph(clause.Expression) {
				return true
			}
		case *cypher.ProjectionClause:
			for _, item := range clause.Items {
				if expressionRequiresGraph(item.Expression) {
					return true
				}
			}
			for _, item := range clause.OrderBy {
				if expressionRequiresGraph(item.Expression) {
					return true
				}
			}
			if expressionRequiresGraph(clause.Where) || expressionRequiresGraph(clause.Skip) || expressionRequiresGraph(clause.Limit) {
				return true
			}
		case *cypher.CallClause:
			if clause.Subquery != nil {
				if queryRequiresGraph(clause.Subquery) {
					return true
				}
			} else if !strings.EqualFold(clause.Procedure.String(), "sheets.revisions") {
				return true
			}
			for _, argument := range clause.Arguments {
				if expressionRequiresGraph(argument) {
					return true
				}
			}
			if expressionRequiresGraph(clause.YieldWhere) {
				return true
			}
		}
	}
	return false
}

func expressionRequiresGraph(expression cypher.Expression) bool {
	switch expression := expression.(type) {
	case nil, *cypher.Literal, *cypher.Variable, *cypher.Parameter:
		return false
	case *cypher.PatternExpression, *cypher.PatternComprehension, *cypher.ExistsSubquery:
		return true
	case *cypher.UnaryExpression:
		return expressionRequiresGraph(expression.Expression)
	case *cypher.BinaryExpression:
		return expressionRequiresGraph(expression.Left) || expressionRequiresGraph(expression.Right)
	case *cypher.IsNullExpression:
		return expressionRequiresGraph(expression.Expression)
	case *cypher.PropertyExpression:
		return expressionRequiresGraph(expression.Expression)
	case *cypher.LabelExpression:
		return expressionRequiresGraph(expression.Expression)
	case *cypher.IndexExpression:
		return expressionRequiresGraph(expression.Expression) || expressionRequiresGraph(expression.Index)
	case *cypher.SliceExpression:
		return expressionRequiresGraph(expression.Expression) || expressionRequiresGraph(expression.Start) || expressionRequiresGraph(expression.End)
	case *cypher.FunctionInvocation:
		for _, argument := range expression.Arguments {
			if expressionRequiresGraph(argument) {
				return true
			}
		}
	case *cypher.ListLiteral:
		for _, element := range expression.Elements {
			if expressionRequiresGraph(element) {
				return true
			}
		}
	case *cypher.MapLiteral:
		for _, entry := range expression.Entries {
			if expressionRequiresGraph(entry.Value) {
				return true
			}
		}
	case *cypher.CaseExpression:
		if expressionRequiresGraph(expression.Operand) || expressionRequiresGraph(expression.Else) {
			return true
		}
		for _, alternative := range expression.Alternatives {
			if expressionRequiresGraph(alternative.When) || expressionRequiresGraph(alternative.Then) {
				return true
			}
		}
	case *cypher.ListComprehension:
		return expressionRequiresGraph(expression.List) || expressionRequiresGraph(expression.Where) || expressionRequiresGraph(expression.Projection)
	case *cypher.ListPredicate:
		return expressionRequiresGraph(expression.List) || expressionRequiresGraph(expression.Where)
	case *cypher.ReduceExpression:
		return expressionRequiresGraph(expression.Initial) || expressionRequiresGraph(expression.List) || expressionRequiresGraph(expression.Expression)
	default:
		return true
	}
	return false
}

// loadNodeWorkingSet pushes exact labels/properties into the temporal store
// for read pipelines whose graph access consists solely of single-node MATCH
// patterns. It is intentionally all-or-nothing: falling back to the complete
// snapshot is preferable to executing against an incomplete graph.
func (e *Engine) loadNodeWorkingSet(
	ctx context.Context,
	snapshot domain.Snapshot,
	document *cypher.Document,
	params map[string]any,
) (*memoryGraph, bool, error) {
	predicates := make([]store.NodePredicate, 0)
	for _, statement := range document.Statements {
		query, ok := statement.(*cypher.QueryStatement)
		if !ok || !collectNodePredicates(query, params, &predicates) {
			return nil, false, nil
		}
	}
	if len(predicates) == 0 {
		return nil, false, nil
	}
	view, err := e.store.View(ctx, snapshot)
	if err != nil {
		return nil, true, err
	}
	byID := make(map[domain.EntityID]domain.Node)
	for _, predicate := range predicates {
		page := domain.Page{Limit: graphPageSize}
		for {
			nodes, info, err := view.ScanNodes(ctx, predicate, page)
			if err != nil {
				return nil, true, err
			}
			for _, node := range nodes {
				byID[node.ID] = node
			}
			if info.Next == "" {
				break
			}
			page.After = info.Next
		}
	}
	nodes := make([]domain.Node, 0, len(byID))
	for _, node := range byID {
		nodes = append(nodes, node)
	}
	return newMemoryGraph(view.Revision(), nodes, nil, nil), true, nil
}

func collectNodePredicates(query *cypher.QueryStatement, params map[string]any, result *[]store.NodePredicate) bool {
	if query == nil || query.Explain {
		return query != nil
	}
	for _, clause := range query.Clauses {
		switch clause := clause.(type) {
		case *cypher.MatchClause:
			if expressionRequiresGraph(clause.Where) {
				return false
			}
			for _, pattern := range clause.Patterns {
				if len(pattern.Element.Nodes) != 1 || len(pattern.Element.Relationships) != 0 {
					return false
				}
				node := pattern.Element.Nodes[0]
				predicate, _ := staticNodePredicate(node, params)
				*result = append(*result, predicate)
			}
		case *cypher.UnwindClause:
			if expressionRequiresGraph(clause.Expression) {
				return false
			}
		case *cypher.ProjectionClause:
			if projectionRequiresGraph(clause) {
				return false
			}
		default:
			return false
		}
	}
	for _, branch := range query.UnionBranches {
		if !collectNodePredicates(branch.Query, params, result) {
			return false
		}
	}
	return true
}

func projectionRequiresGraph(clause *cypher.ProjectionClause) bool {
	for _, item := range clause.Items {
		if expressionRequiresGraph(item.Expression) {
			return true
		}
	}
	for _, item := range clause.OrderBy {
		if expressionRequiresGraph(item.Expression) {
			return true
		}
	}
	return expressionRequiresGraph(clause.Where) || expressionRequiresGraph(clause.Skip) || expressionRequiresGraph(clause.Limit)
}

func staticPatternProperties(expression cypher.Expression, params map[string]any) (domain.Properties, bool) {
	if expression == nil {
		return nil, true
	}
	if !staticExpression(expression) {
		return nil, false
	}
	value, err := newEvaluator(params).expression(expression, row{})
	if err != nil {
		// Preserve the engine's located runtime error instead of changing query
		// behavior in the optimization layer.
		return nil, false
	}
	switch value := value.(type) {
	case map[string]any:
		return domain.Properties(value), true
	case domain.Properties:
		return value, true
	default:
		return nil, false
	}
}

func staticExpression(expression cypher.Expression) bool {
	switch expression := expression.(type) {
	case *cypher.Literal, *cypher.Parameter:
		return true
	case *cypher.UnaryExpression:
		return staticExpression(expression.Expression)
	case *cypher.BinaryExpression:
		return staticExpression(expression.Left) && staticExpression(expression.Right)
	case *cypher.IsNullExpression:
		return staticExpression(expression.Expression)
	case *cypher.IndexExpression:
		return staticExpression(expression.Expression) && staticExpression(expression.Index)
	case *cypher.SliceExpression:
		return staticExpression(expression.Expression) && staticExpression(expression.Start) && staticExpression(expression.End)
	case *cypher.ListLiteral:
		for _, element := range expression.Elements {
			if !staticExpression(element) {
				return false
			}
		}
		return true
	case *cypher.MapLiteral:
		for _, entry := range expression.Entries {
			if !staticExpression(entry.Value) {
				return false
			}
		}
		return true
	default:
		return false
	}
}
