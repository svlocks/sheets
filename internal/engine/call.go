package engine

import (
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/svlocks/sheets/internal/cypher"
	"github.com/svlocks/sheets/internal/domain"
)

func (e *queryExecution) call(input []row, clause *cypher.CallClause) ([]row, error) {
	if clause.Subquery != nil {
		return e.callSubquery(input, clause)
	}
	result := make([]row, 0)
	for _, outer := range input {
		procedureRows, err := e.procedure(clause, outer)
		if err != nil {
			return nil, err
		}
		for _, procedureRow := range procedureRows {
			next := cloneRow(outer)
			if len(clause.Yield) == 0 {
				for key, value := range procedureRow {
					next[key] = value
				}
				result = append(result, next)
				continue
			}
			for _, item := range clause.Yield {
				if item.Star {
					for key, value := range procedureRow {
						next[key] = value
					}
					continue
				}
				value, exists := procedureRow[item.Name.Name]
				if !exists {
					return nil, fmt.Errorf("procedure %s does not yield %q", clause.Procedure.String(), item.Name.Name)
				}
				name := item.Name.Name
				if item.Alias.Name != "" {
					name = item.Alias.Name
				}
				next[name] = value
			}
			result = append(result, next)
		}
	}
	if clause.YieldWhere != nil {
		return filterRows(e.evaluator, result, clause.YieldWhere)
	}
	return result, nil
}

func (e *queryExecution) callSubquery(input []row, clause *cypher.CallClause) ([]row, error) {
	result := make([]row, 0)
	for _, outer := range input {
		rows, columns, unit, err := e.executeSubquery(clause.Subquery, outer)
		if err != nil {
			return nil, err
		}
		if unit {
			result = append(result, cloneRow(outer))
			continue
		}
		for _, subqueryRow := range rows {
			merged := cloneRow(outer)
			for _, key := range columns {
				if _, exists := outer[key]; exists {
					return nil, fmt.Errorf("subquery returns variable %q which is already declared in the outer scope", key)
				}
				merged[key] = subqueryRow[key]
			}
			result = append(result, merged)
		}
	}
	return result, nil
}

func (e *queryExecution) executeSubquery(query *cypher.QueryStatement, outer row) ([]row, []string, bool, error) {
	initial := subqueryInitialRows(query.Clauses, outer)
	primary, err := e.clauses(query.Clauses, initial)
	if err != nil {
		return nil, nil, false, err
	}
	unit := !clausesReturnRows(query.Clauses)
	if unit && len(query.UnionBranches) == 0 {
		return nil, nil, true, nil
	}
	columns := primary.Columns
	rows := append([]row(nil), e.lastRows...)
	for _, branch := range query.UnionBranches {
		branchResult, err := e.clauses(branch.Query.Clauses, subqueryInitialRows(branch.Query.Clauses, outer))
		if err != nil {
			return nil, nil, false, err
		}
		if !slices.Equal(columns, branchResult.Columns) {
			return nil, nil, false, fmt.Errorf("UNION branches return different columns: %v and %v", columns, branchResult.Columns)
		}
		rows = append(rows, e.lastRows...)
		if !branch.All {
			rows = distinctMappedRows(rows, columns)
		}
	}
	return rows, columns, false, nil
}

func subqueryInitialRows(clauses []cypher.Clause, outer row) []row {
	// In the supported CALL { ... } form an initial WITH is the explicit
	// importing clause. Without it the subquery begins with an empty scope.
	if len(clauses) > 0 {
		if projection, ok := clauses[0].(*cypher.ProjectionClause); ok && projection.With {
			return []row{cloneRow(outer)}
		}
	}
	return []row{{}}
}

func clausesReturnRows(clauses []cypher.Clause) bool {
	if len(clauses) == 0 {
		return false
	}
	switch clause := clauses[len(clauses)-1].(type) {
	case *cypher.ProjectionClause:
		return !clause.With
	case *cypher.CallClause:
		return clause.Subquery == nil
	default:
		return false
	}
}

func distinctMappedRows(rows []row, columns []string) []row {
	seen := make(map[string]struct{}, len(rows))
	result := make([]row, 0, len(rows))
	for _, values := range rows {
		keyValues := make([]any, len(columns))
		for index, column := range columns {
			keyValues[index] = values[column]
		}
		key := valueKey(keyValues)
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		result = append(result, values)
	}
	return result
}

func (e *queryExecution) procedure(clause *cypher.CallClause, values row) ([]row, error) {
	name := strings.ToLower(clause.Procedure.String())
	if name == "sheets.revisions" {
		return e.revisionRows(clause, values)
	}
	if len(clause.Arguments) != 0 {
		return nil, fmt.Errorf("procedure %s expects no arguments", clause.Procedure.String())
	}
	switch name {
	case "db.labels":
		set := make(map[string]struct{})
		for _, node := range e.graph.nodes {
			for _, label := range node.Labels {
				set[label] = struct{}{}
			}
		}
		return stringRows("label", set), nil
	case "db.relationshiptypes":
		set := make(map[string]struct{})
		for _, edge := range e.graph.edges {
			set[edge.Type] = struct{}{}
		}
		return stringRows("relationshipType", set), nil
	case "db.propertykeys":
		set := make(map[string]struct{})
		for _, node := range e.graph.nodes {
			for key := range node.Properties {
				set[key] = struct{}{}
			}
			if node.Body != "" {
				set["body"] = struct{}{}
			}
		}
		for _, edge := range e.graph.edges {
			for key := range edge.Properties {
				set[key] = struct{}{}
			}
			if edge.Position != nil {
				set["position"] = struct{}{}
			}
		}
		return stringRows("propertyKey", set), nil
	case "sheets.nodes":
		nodes := e.graph.nodePointers()
		result := make([]row, len(nodes))
		for index, node := range nodes {
			result[index] = row{"node": node}
		}
		return result, nil
	case "sheets.edges":
		edges := make([]domain.EntityID, 0, len(e.graph.edges))
		for id := range e.graph.edges {
			edges = append(edges, id)
		}
		sort.Slice(edges, func(i, j int) bool { return edges[i] < edges[j] })
		result := make([]row, len(edges))
		for index, id := range edges {
			result[index] = row{"relationship": e.graph.edges[id]}
		}
		return result, nil
	default:
		return nil, fmt.Errorf("unknown procedure %s", clause.Procedure.String())
	}
}

func (e *queryExecution) revisionRows(clause *cypher.CallClause, values row) ([]row, error) {
	if e.revisions == nil {
		return nil, fmt.Errorf("procedure %s is unavailable", clause.Procedure.String())
	}
	if len(clause.Arguments) > 3 {
		return nil, fmt.Errorf("procedure %s expects at most three arguments: limit, cursor, and order", clause.Procedure.String())
	}
	page := domain.RevisionPage{Order: domain.RevisionOrderAscending}
	if len(clause.Arguments) > 0 {
		value, err := e.evaluator.expression(clause.Arguments[0], values)
		if err != nil {
			return nil, err
		}
		if value != nil {
			limit, ok := integer(value)
			if !ok || limit < 0 || limit > 1000 {
				return nil, evalError(clause.Arguments[0], "revision limit must be an integer between 0 and 1000")
			}
			page.Limit = int(limit)
		}
	}
	if len(clause.Arguments) > 1 {
		value, err := e.evaluator.expression(clause.Arguments[1], values)
		if err != nil {
			return nil, err
		}
		if value != nil {
			after, ok := value.(string)
			if !ok {
				return nil, evalError(clause.Arguments[1], "revision cursor must be a string or null")
			}
			page.Cursor = after
		}
	}
	if len(clause.Arguments) > 2 {
		value, err := e.evaluator.expression(clause.Arguments[2], values)
		if err != nil {
			return nil, err
		}
		if value != nil {
			order, ok := value.(string)
			if !ok {
				return nil, evalError(clause.Arguments[2], "revision order must be ascending or descending")
			}
			switch strings.ToLower(strings.TrimSpace(order)) {
			case "ascending", "asc":
				page.Order = domain.RevisionOrderAscending
			case "descending", "desc":
				page.Order = domain.RevisionOrderDescending
			default:
				return nil, evalError(clause.Arguments[2], "revision order must be ascending or descending")
			}
		}
	}
	infos, pageInfo, err := e.revisions(e.ctx, page)
	if err != nil {
		return nil, fmt.Errorf("procedure %s: %w", clause.Procedure.String(), err)
	}
	result := make([]row, len(infos))
	for index, info := range infos {
		result[index] = row{
			"revision": int64(info.Revision),
			"time":     info.Time,
			"actor":    info.Actor,
			"message":  info.Message,
			"next":     pageInfo.Next,
		}
	}
	return result, nil
}

func stringRows(column string, values map[string]struct{}) []row {
	ordered := make([]string, 0, len(values))
	for value := range values {
		ordered = append(ordered, value)
	}
	sort.Strings(ordered)
	result := make([]row, len(ordered))
	for index, value := range ordered {
		result[index] = row{column: value}
	}
	return result
}
