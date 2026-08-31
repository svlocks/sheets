package engine

import (
	"fmt"
	"sort"
	"strings"

	"github.com/svlocks/sheets/internal/cypher"
	"github.com/svlocks/sheets/internal/domain"
)

func (e *queryExecution) call(input []row, clause *cypher.CallClause) ([]row, error) {
	if clause.Subquery != nil {
		return e.callSubquery(input, clause)
	}
	procedureRows, err := e.procedure(clause)
	if err != nil {
		return nil, err
	}
	result := make([]row, 0, len(input)*max(1, len(procedureRows)))
	for _, outer := range input {
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
		_, err := e.clauses(clause.Subquery.Clauses, []row{cloneRow(outer)})
		if err != nil {
			return nil, err
		}
		for _, subqueryRow := range e.lastRows {
			merged := cloneRow(outer)
			for key, value := range subqueryRow {
				merged[key] = value
			}
			result = append(result, merged)
		}
	}
	return result, nil
}

func (e *queryExecution) procedure(clause *cypher.CallClause) ([]row, error) {
	if len(clause.Arguments) != 0 {
		return nil, fmt.Errorf("procedure %s expects no arguments", clause.Procedure.String())
	}
	switch strings.ToLower(clause.Procedure.String()) {
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
