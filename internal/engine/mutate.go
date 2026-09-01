package engine

import (
	"fmt"
	"sort"
	"time"

	"github.com/svlocks/sheets/internal/cypher"
	"github.com/svlocks/sheets/internal/domain"
	"github.com/svlocks/sheets/internal/domain/temporal"
	"github.com/svlocks/sheets/internal/store"
)

func (e *queryExecution) create(input []row, patterns []cypher.PatternPart) ([]row, error) {
	rows := input
	for _, pattern := range patterns {
		result := make([]row, 0, len(rows))
		for _, values := range rows {
			created, err := e.createPattern(values, pattern)
			if err != nil {
				return nil, err
			}
			result = append(result, created)
		}
		rows = result
	}
	return rows, nil
}

func (e *queryExecution) createPattern(values row, pattern cypher.PatternPart) (row, error) {
	next := cloneRow(values)
	if pattern.Variable.Name != "" {
		if _, exists := next[pattern.Variable.Name]; exists {
			return nil, fmt.Errorf("CREATE path variable %s is already declared", pattern.Variable.Name)
		}
	}
	path := Path{}
	for index, nodePattern := range pattern.Element.Nodes {
		var node *domain.Node
		if nodePattern.Variable.Name != "" {
			if bound, exists := next[nodePattern.Variable.Name]; exists {
				var ok bool
				node, ok = bound.(*domain.Node)
				if !ok || node == nil {
					return nil, fmt.Errorf("CREATE variable %s is not a node", nodePattern.Variable.Name)
				}
			}
		}
		if node == nil {
			properties, err := e.evaluateProperties(nodePattern.Properties, next)
			if err != nil {
				return nil, err
			}
			body, err := takeBody(properties)
			if err != nil {
				return nil, err
			}
			dropNullProperties(properties)
			labels := make([]string, len(nodePattern.Labels))
			for labelIndex, label := range nodePattern.Labels {
				labels[labelIndex] = label.Name
			}
			node, err = e.graph.createNode(store.NodeInput{Labels: labels, Properties: properties, Body: body})
			if err != nil {
				return nil, err
			}
			e.summary.NodesCreated++
			e.summary.PropertiesSet += uint64(len(properties))
			e.summary.LabelsAdded += uint64(len(node.Labels))
			if body != "" {
				e.summary.PropertiesSet++
			}
			if !bindValue(next, nodePattern.Variable.Name, node) {
				return nil, fmt.Errorf("CREATE variable %s conflicts with an existing value", nodePattern.Variable.Name)
			}
		} else if len(nodePattern.Labels) > 0 || nodePattern.Properties != nil {
			return nil, fmt.Errorf("CREATE cannot redeclare labels or properties on bound node %s; use SET", nodePattern.Variable.Name)
		}
		path.Nodes = append(path.Nodes, node)
		if index == 0 {
			continue
		}

		relationship := pattern.Element.Relationships[index-1]
		if relationship.Length != nil {
			return nil, fmt.Errorf("CREATE does not allow variable-length relationships")
		}
		if len(relationship.Types) != 1 {
			return nil, fmt.Errorf("CREATE relationships require exactly one type")
		}
		if relationship.Direction == cypher.Undirected || relationship.Direction == cypher.Bidirectional {
			return nil, fmt.Errorf("CREATE relationships must have a direction")
		}
		if relationship.Variable.Name != "" {
			if _, exists := next[relationship.Variable.Name]; exists {
				return nil, fmt.Errorf("CREATE relationship variable %s is already declared", relationship.Variable.Name)
			}
		}
		properties, err := e.evaluateProperties(relationship.Properties, next)
		if err != nil {
			return nil, err
		}
		position, err := takePosition(properties)
		if err != nil {
			return nil, err
		}
		dropNullProperties(properties)
		from, to := path.Nodes[index-1].ID, node.ID
		if relationship.Direction == cypher.Incoming {
			from, to = to, from
		}
		edge, err := e.graph.createEdge(store.EdgeInput{
			From: from, Type: relationship.Types[0].Name, To: to,
			Position: position, Properties: properties,
		})
		if err != nil {
			return nil, err
		}
		e.summary.RelationshipsCreated++
		e.summary.PropertiesSet += uint64(len(properties))
		if position != nil {
			e.summary.PropertiesSet++
		}
		if !bindValue(next, relationship.Variable.Name, edge) {
			return nil, fmt.Errorf("CREATE variable %s conflicts with an existing value", relationship.Variable.Name)
		}
		path.Relationships = append(path.Relationships, edge)
	}
	if !bindValue(next, pattern.Variable.Name, path) {
		return nil, fmt.Errorf("CREATE path variable %s conflicts with an existing value", pattern.Variable.Name)
	}
	return next, nil
}

func (e *queryExecution) merge(input []row, clause *cypher.MergeClause) ([]row, error) {
	result := make([]row, 0, len(input))
	for _, values := range input {
		if err := e.rejectMergeNullProperties(values, clause.Pattern); err != nil {
			return nil, err
		}
		matches, err := matchPattern(e.graph, e.evaluator, []row{values}, clause.Pattern)
		if err != nil {
			return nil, err
		}
		created := len(matches) == 0
		if created {
			createdRow, err := e.createPattern(values, mergeCreatePattern(clause.Pattern))
			if err != nil {
				return nil, err
			}
			matches = []row{createdRow}
		}
		for _, matched := range matches {
			for _, action := range clause.Actions {
				if created && action.Kind != cypher.OnCreate || !created && action.Kind != cypher.OnMatch {
					continue
				}
				if err := e.set([]row{matched}, action.Set); err != nil {
					return nil, err
				}
			}
			result = append(result, matched)
		}
	}
	return result, nil
}

// An undirected MERGE pattern searches in both directions, but when no match
// exists the M23 creation semantics choose the written left-to-right direction.
func mergeCreatePattern(pattern cypher.PatternPart) cypher.PatternPart {
	pattern.Element.Relationships = append([]cypher.RelationshipPattern(nil), pattern.Element.Relationships...)
	for index := range pattern.Element.Relationships {
		if pattern.Element.Relationships[index].Direction == cypher.Undirected {
			pattern.Element.Relationships[index].Direction = cypher.Outgoing
		}
	}
	return pattern
}

func (e *queryExecution) rejectMergeNullProperties(values row, pattern cypher.PatternPart) error {
	for _, node := range pattern.Element.Nodes {
		properties, err := e.evaluateProperties(node.Properties, values)
		if err != nil {
			return err
		}
		for key, value := range properties {
			if value == nil {
				return evalError(node.Properties, "MERGE cannot use null property %q", key)
			}
		}
	}
	for _, relationship := range pattern.Element.Relationships {
		properties, err := e.evaluateProperties(relationship.Properties, values)
		if err != nil {
			return err
		}
		for key, value := range properties {
			if value == nil {
				return evalError(relationship.Properties, "MERGE cannot use null property %q", key)
			}
		}
	}
	return nil
}

func (e *queryExecution) set(rows []row, items []cypher.SetItem) error {
	for _, values := range rows {
		for _, item := range items {
			if len(item.Labels) > 0 {
				entity, err := e.evaluator.expression(item.Target, values)
				if err != nil {
					return err
				}
				if entity == nil {
					continue
				}
				node, ok := entity.(*domain.Node)
				if !ok || node == nil {
					return evalError(item.Target, "labels can only be set on a node")
				}
				labels := append([]string(nil), node.Labels...)
				set := make(map[string]struct{}, len(labels)+len(item.Labels))
				for _, label := range labels {
					set[label] = struct{}{}
				}
				added := uint64(0)
				for _, label := range item.Labels {
					if _, exists := set[label.Name]; !exists {
						labels = append(labels, label.Name)
						set[label.Name] = struct{}{}
						added++
					}
				}
				if added > 0 {
					if _, changed, err := e.graph.updateNode(node.ID, store.NodeUpdate{Labels: &labels}); err != nil {
						return err
					} else if changed {
						e.summary.NodesUpdated++
						e.summary.LabelsAdded += added
					}
				}
				continue
			}
			if err := e.setAssignment(values, item); err != nil {
				return err
			}
		}
	}
	return nil
}

func (e *queryExecution) setAssignment(values row, item cypher.SetItem) error {
	assigned, err := e.evaluator.expression(item.Value, values)
	if err != nil {
		return err
	}
	switch target := item.Target.(type) {
	case *cypher.PropertyExpression:
		entity, err := e.evaluator.expression(target.Expression, values)
		if err != nil {
			return err
		}
		return e.setProperty(entity, target.Property.Name, assigned)
	case *cypher.Variable:
		entity, err := e.evaluator.expression(target, values)
		if err != nil {
			return err
		}
		properties, ok := toProperties(assigned)
		if !ok {
			properties, ok = entityProperties(assigned)
		}
		if !ok {
			return evalError(item.Value, "SET %s expects a map", item.Operator)
		}
		if item.Operator == "+=" {
			current := propertiesOfEntity(entity)
			for key, value := range properties {
				current[key] = value
			}
			properties = current
		}
		return e.replaceProperties(entity, properties)
	default:
		return evalError(item.Target, "SET target is not assignable")
	}
}

func (e *queryExecution) setProperty(entity any, name string, value any) error {
	if value != nil && !validPropertyValue(value) {
		return fmt.Errorf("invalid property type %T: properties cannot contain maps, entities, paths, or nested collections", value)
	}
	switch entity := entity.(type) {
	case *domain.Node:
		if entity == nil {
			return nil
		}
		if name == "body" {
			body, ok := value.(string)
			if value == nil {
				body, ok = "", true
			}
			if !ok {
				return fmt.Errorf("node body must be a string or null")
			}
			_, changed, err := e.graph.updateNode(entity.ID, store.NodeUpdate{Body: &body})
			if changed {
				e.summary.NodesUpdated++
				e.summary.PropertiesSet++
			}
			return err
		}
		if value == nil {
			return e.removeProperty(entity, name)
		}
		properties := clonePropertyMap(entity.Properties)
		if properties == nil {
			properties = make(domain.Properties)
		}
		if equalValues(properties[name], value) {
			return nil
		}
		properties[name] = value
		_, changed, err := e.graph.updateNode(entity.ID, store.NodeUpdate{Properties: &properties})
		if changed {
			e.summary.NodesUpdated++
			e.summary.PropertiesSet++
		}
		return err
	case *domain.Edge:
		if entity == nil {
			return nil
		}
		if name == "position" {
			var position *int64
			if value != nil {
				parsed, ok := integer(value)
				if !ok {
					return fmt.Errorf("relationship position must be an integer or null")
				}
				position = &parsed
			}
			_, changed, err := e.graph.updateEdge(entity.ID, store.EdgeUpdate{SetPosition: true, Position: position})
			if changed {
				e.summary.RelationshipsUpdated++
				e.summary.PropertiesSet++
			}
			return err
		}
		if value == nil {
			return e.removeProperty(entity, name)
		}
		properties := clonePropertyMap(entity.Properties)
		if properties == nil {
			properties = make(domain.Properties)
		}
		if equalValues(properties[name], value) {
			return nil
		}
		properties[name] = value
		_, changed, err := e.graph.updateEdge(entity.ID, store.EdgeUpdate{Properties: &properties})
		if changed {
			e.summary.RelationshipsUpdated++
			e.summary.PropertiesSet++
		}
		return err
	case nil:
		return nil
	default:
		return fmt.Errorf("properties can only be set on nodes and relationships")
	}
}

func (e *queryExecution) replaceProperties(entity any, properties domain.Properties) error {
	bodyValue, hasBody := properties["body"]
	delete(properties, "body")
	positionValue, hasPosition := properties["position"]
	delete(properties, "position")
	dropNullProperties(properties)
	switch entity := entity.(type) {
	case *domain.Node:
		update := store.NodeUpdate{Properties: &properties}
		if hasBody {
			body, ok := bodyValue.(string)
			if bodyValue == nil {
				body, ok = "", true
			}
			if !ok {
				return fmt.Errorf("node body must be a string or null")
			}
			update.Body = &body
		}
		_, changed, err := e.graph.updateNode(entity.ID, update)
		if changed {
			e.summary.NodesUpdated++
			e.summary.PropertiesSet += uint64(len(properties))
			if hasBody {
				e.summary.PropertiesSet++
			}
		}
		return err
	case *domain.Edge:
		update := store.EdgeUpdate{Properties: &properties}
		if hasPosition {
			update.SetPosition = true
			if positionValue != nil {
				position, ok := integer(positionValue)
				if !ok {
					return fmt.Errorf("relationship position must be an integer or null")
				}
				update.Position = &position
			}
		}
		_, changed, err := e.graph.updateEdge(entity.ID, update)
		if changed {
			e.summary.RelationshipsUpdated++
			e.summary.PropertiesSet += uint64(len(properties))
			if hasPosition {
				e.summary.PropertiesSet++
			}
		}
		return err
	case nil:
		return nil
	default:
		return fmt.Errorf("property maps can only be assigned to nodes and relationships")
	}
}

func (e *queryExecution) remove(rows []row, items []cypher.RemoveItem) error {
	for _, values := range rows {
		for _, item := range items {
			if len(item.Labels) > 0 {
				entity, err := e.evaluator.expression(item.Target, values)
				if err != nil {
					return err
				}
				if entity == nil {
					continue
				}
				node, ok := entity.(*domain.Node)
				if !ok || node == nil {
					return evalError(item.Target, "labels can only be removed from a node")
				}
				removeSet := make(map[string]struct{}, len(item.Labels))
				for _, label := range item.Labels {
					removeSet[label.Name] = struct{}{}
				}
				labels := make([]string, 0, len(node.Labels))
				removed := uint64(0)
				for _, label := range node.Labels {
					if _, remove := removeSet[label]; remove {
						removed++
					} else {
						labels = append(labels, label)
					}
				}
				if removed > 0 {
					_, changed, err := e.graph.updateNode(node.ID, store.NodeUpdate{Labels: &labels})
					if err != nil {
						return err
					}
					if changed {
						e.summary.NodesUpdated++
						e.summary.LabelsRemoved += removed
					}
				}
				continue
			}
			target, ok := item.Target.(*cypher.PropertyExpression)
			if !ok {
				return evalError(item.Target, "REMOVE expects a property or label")
			}
			entity, err := e.evaluator.expression(target.Expression, values)
			if err != nil {
				return err
			}
			if err := e.removeProperty(entity, target.Property.Name); err != nil {
				return err
			}
		}
	}
	return nil
}

func (e *queryExecution) removeProperty(entity any, name string) error {
	switch entity := entity.(type) {
	case *domain.Node:
		if entity == nil {
			return nil
		}
		if name == "body" {
			if entity.Body == "" {
				return nil
			}
			empty := ""
			_, changed, err := e.graph.updateNode(entity.ID, store.NodeUpdate{Body: &empty})
			if changed {
				e.summary.NodesUpdated++
				e.summary.PropertiesSet++
			}
			return err
		}
		if _, exists := entity.Properties[name]; !exists {
			return nil
		}
		properties := clonePropertyMap(entity.Properties)
		delete(properties, name)
		_, changed, err := e.graph.updateNode(entity.ID, store.NodeUpdate{Properties: &properties})
		if changed {
			e.summary.NodesUpdated++
			e.summary.PropertiesSet++
		}
		return err
	case *domain.Edge:
		if entity == nil {
			return nil
		}
		if name == "position" {
			if entity.Position == nil {
				return nil
			}
			_, changed, err := e.graph.updateEdge(entity.ID, store.EdgeUpdate{SetPosition: true})
			if changed {
				e.summary.RelationshipsUpdated++
				e.summary.PropertiesSet++
			}
			return err
		}
		if _, exists := entity.Properties[name]; !exists {
			return nil
		}
		properties := clonePropertyMap(entity.Properties)
		delete(properties, name)
		_, changed, err := e.graph.updateEdge(entity.ID, store.EdgeUpdate{Properties: &properties})
		if changed {
			e.summary.RelationshipsUpdated++
			e.summary.PropertiesSet++
		}
		return err
	case nil:
		return nil
	default:
		return fmt.Errorf("properties can only be removed from nodes and relationships")
	}
}

func (e *queryExecution) delete(rows []row, clause *cypher.DeleteClause) error {
	nodes := make(map[domain.EntityID]*domain.Node)
	edges := make(map[domain.EntityID]*domain.Edge)
	for _, values := range rows {
		for _, expression := range clause.Expressions {
			value, err := e.evaluator.expression(expression, values)
			if err != nil {
				return err
			}
			if !collectDeletedEntities(value, nodes, edges) {
				return evalError(expression, "DELETE expects a node, relationship, path, list of entities, or null; got %T", value)
			}
		}
	}
	edgeIDs := sortedEntityIDs(edges)
	for _, id := range edgeIDs {
		if _, exists := e.graph.edges[id]; !exists {
			continue
		}
		if err := e.graph.deleteEdge(id); err != nil {
			return err
		}
		e.summary.RelationshipsDeleted++
	}
	for _, id := range sortedEntityIDs(nodes) {
		if _, exists := e.graph.nodes[id]; !exists {
			continue
		}
		incident := uniqueIncidentCount(e.graph, id)
		if incident > 0 && !clause.Detach {
			return fmt.Errorf("cannot delete node %s with relationships; use DETACH DELETE", id)
		}
		removedEdges, err := e.graph.deleteNode(id)
		if err != nil {
			return err
		}
		e.summary.NodesDeleted++
		e.summary.RelationshipsDeleted += uint64(len(removedEdges))
	}
	return nil
}

func (e *queryExecution) evaluateProperties(expression cypher.Expression, values row) (domain.Properties, error) {
	if expression == nil {
		return make(domain.Properties), nil
	}
	value, err := e.evaluator.expression(expression, values)
	if err != nil {
		return nil, err
	}
	properties, ok := toProperties(value)
	if !ok {
		return nil, evalError(expression, "properties must evaluate to a map")
	}
	return properties, nil
}

func toProperties(value any) (domain.Properties, bool) {
	switch value := value.(type) {
	case map[string]any:
		return clonePropertyMap(domain.Properties(value)), true
	case domain.Properties:
		return clonePropertyMap(value), true
	case nil:
		return make(domain.Properties), true
	default:
		return nil, false
	}
}

func propertiesOfEntity(entity any) domain.Properties {
	properties, _ := entityProperties(entity)
	if properties == nil {
		properties = make(domain.Properties)
	}
	return properties
}

func entityProperties(entity any) (domain.Properties, bool) {
	switch entity := entity.(type) {
	case *domain.Node:
		if entity == nil {
			return make(domain.Properties), true
		}
		return clonePropertyMap(entity.Properties), true
	case domain.Node:
		return clonePropertyMap(entity.Properties), true
	case *domain.Edge:
		if entity == nil {
			return make(domain.Properties), true
		}
		return clonePropertyMap(entity.Properties), true
	case domain.Edge:
		return clonePropertyMap(entity.Properties), true
	default:
		return nil, false
	}
}

func validPropertyValue(value any) bool {
	if value == nil {
		return true
	}
	if _, mapValue := asMap(value); mapValue {
		return false
	}
	switch value.(type) {
	case domain.Node, *domain.Node, domain.Edge, *domain.Edge, Path, PathValue:
		return false
	}
	if items, list := asList(value); list {
		for _, item := range items {
			if item == nil || !validPropertyScalar(item) {
				return false
			}
		}
		return true
	}
	return validPropertyScalar(value)
}

func validPropertyScalar(value any) bool {
	switch value.(type) {
	case string, bool,
		int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64,
		float32, float64, time.Time, time.Duration,
		temporal.Date, temporal.LocalTime, temporal.Time, temporal.LocalDateTime,
		temporal.DateTime, temporal.Duration:
		return true
	default:
		return false
	}
}

func takeBody(properties domain.Properties) (string, error) {
	value, exists := properties["body"]
	if !exists {
		return "", nil
	}
	delete(properties, "body")
	if value == nil {
		return "", nil
	}
	body, ok := value.(string)
	if !ok {
		return "", fmt.Errorf("node body must be a string or null")
	}
	return body, nil
}

func takePosition(properties domain.Properties) (*int64, error) {
	value, exists := properties["position"]
	if !exists {
		return nil, nil
	}
	delete(properties, "position")
	if value == nil {
		return nil, nil
	}
	position, ok := integer(value)
	if !ok {
		return nil, fmt.Errorf("relationship position must be an integer or null")
	}
	return &position, nil
}

func dropNullProperties(properties domain.Properties) {
	for key, value := range properties {
		if value == nil {
			delete(properties, key)
		}
	}
}

func collectDeletedEntities(value any, nodes map[domain.EntityID]*domain.Node, edges map[domain.EntityID]*domain.Edge) bool {
	switch value := value.(type) {
	case nil:
		return true
	case *domain.Node:
		if value != nil {
			nodes[value.ID] = value
		}
		return true
	case *domain.Edge:
		if value != nil {
			edges[value.ID] = value
		}
		return true
	case Path:
		for _, node := range value.Nodes {
			nodes[node.ID] = node
		}
		for _, edge := range value.Relationships {
			edges[edge.ID] = edge
		}
		return true
	case []any:
		for _, item := range value {
			if !collectDeletedEntities(item, nodes, edges) {
				return false
			}
		}
		return true
	}
	return false
}

func sortedEntityIDs[T *domain.Node | *domain.Edge](entities map[domain.EntityID]T) []domain.EntityID {
	ids := make([]domain.EntityID, 0, len(entities))
	for id := range entities {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
}

func uniqueIncidentCount(graph *memoryGraph, id domain.EntityID) int {
	set := make(map[domain.EntityID]struct{}, len(graph.outgoing[id])+len(graph.incoming[id]))
	for _, edge := range graph.outgoing[id] {
		set[edge.ID] = struct{}{}
	}
	for _, edge := range graph.incoming[id] {
		set[edge.ID] = struct{}{}
	}
	return len(set)
}
