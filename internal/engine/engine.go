package engine

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/svlocks/sheets/internal/app"
	"github.com/svlocks/sheets/internal/cypher"
	"github.com/svlocks/sheets/internal/domain"
	"github.com/svlocks/sheets/internal/store"
)

// Engine parses and atomically executes Cypher against one Store.
type Engine struct {
	store *store.Store
}

// New creates a graph execution engine. The caller retains ownership of the
// store and must close it when the application is finished.
func New(source *store.Store) (*Engine, error) {
	if source == nil {
		return nil, errors.New("engine store is nil")
	}
	return &Engine{store: source}, nil
}

// Execute implements app.Executor. A request containing any mutation runs all
// of its statements in one Store.Write callback and therefore one revision.
func (e *Engine) Execute(ctx context.Context, request app.ExecuteRequest) (app.BatchResult, error) {
	if err := request.Validate(); err != nil {
		return app.BatchResult{}, err
	}
	document, err := cypher.Parse(request.Query)
	if err != nil {
		return app.BatchResult{}, err
	}
	if len(document.Statements) == 0 {
		return app.BatchResult{}, errors.New("query has no statements")
	}

	mutates := false
	for _, statement := range document.Statements {
		mutates = mutates || statementMutates(statement)
	}
	if request.ReadOnly && mutates {
		return app.BatchResult{}, domain.ErrReadOnly
	}
	if !request.Snapshot.IsCurrent() && mutates {
		return app.BatchResult{}, domain.ErrHistoricalWrite
	}

	if !mutates {
		graph, err := loadSnapshot(ctx, e.store, request.Snapshot)
		if err != nil {
			return app.BatchResult{}, err
		}
		return executeDocument(ctx, document, graph, request.Params)
	}

	var batch app.BatchResult
	writeResult, err := e.store.Write(ctx, store.RevisionMeta{
		Actor: request.Actor, Message: request.Message,
	}, func(transaction *store.WriteTx) error {
		graph, err := loadWriteGraph(transaction)
		if err != nil {
			return err
		}
		batch, err = executeDocument(ctx, document, graph, request.Params)
		return err
	})
	if err != nil {
		return app.BatchResult{}, err
	}
	if writeResult.Changed {
		revision := writeResult.Revision
		batch.Revision = &revision
	}
	return batch, nil
}

func executeDocument(ctx context.Context, document *cypher.Document, graph *memoryGraph, params map[string]any) (app.BatchResult, error) {
	batch := app.BatchResult{Results: make([]app.Result, 0, len(document.Statements))}
	for _, statement := range document.Statements {
		if err := ctx.Err(); err != nil {
			return app.BatchResult{}, err
		}
		query, ok := statement.(*cypher.QueryStatement)
		if !ok {
			return app.BatchResult{}, fmt.Errorf("unsupported statement %T", statement)
		}
		result, err := executeQuery(ctx, document.Source, query, graph, params)
		if err != nil {
			return app.BatchResult{}, err
		}
		freezeResult(&result)
		batch.Results = append(batch.Results, result)
	}
	return batch, nil
}

// Snapshot returns a complete graph state for interactive clients.
func (e *Engine) Snapshot(ctx context.Context, snapshot domain.Snapshot) (GraphSnapshot, error) {
	graph, err := loadSnapshot(ctx, e.store, snapshot)
	if err != nil {
		return GraphSnapshot{}, err
	}
	return GraphSnapshot{Revision: graph.revision, Nodes: graph.nodeValues(), Edges: graph.edgeValues()}, nil
}

// CurrentRevision returns the cheap invalidation token used by the TUI.
func (e *Engine) CurrentRevision(ctx context.Context) (domain.Revision, error) {
	return e.store.CurrentRevision(ctx)
}

// ListRevisions exposes the store's stable revision pagination.
func (e *Engine) ListRevisions(ctx context.Context, page domain.Page) ([]domain.RevisionInfo, domain.PageInfo, error) {
	return e.store.ListRevisions(ctx, page)
}

func statementMutates(statement cypher.Statement) bool {
	query, ok := statement.(*cypher.QueryStatement)
	if !ok {
		return statement.IsMutation()
	}
	if clausesMutate(query.Clauses) {
		return true
	}
	for _, branch := range query.UnionBranches {
		if branch.Query != nil && statementMutates(branch.Query) {
			return true
		}
	}
	return false
}

func clausesMutate(clauses []cypher.Clause) bool {
	for _, clause := range clauses {
		switch clause := clause.(type) {
		case *cypher.CreateClause, *cypher.MergeClause, *cypher.SetClause, *cypher.RemoveClause, *cypher.DeleteClause:
			return true
		case *cypher.CallClause:
			if clause.Subquery != nil {
				if statementMutates(clause.Subquery) {
					return true
				}
				continue
			}
			if !readOnlyProcedure(clause.Procedure.String()) {
				return true
			}
		}
	}
	return false
}

func readOnlyProcedure(name string) bool {
	switch strings.ToLower(name) {
	case "db.labels", "db.relationshiptypes", "db.propertykeys", "sheets.nodes", "sheets.edges", "sheets.revisions":
		return true
	default:
		return false
	}
}

func freezeResult(result *app.Result) {
	for rowIndex := range result.Rows {
		for columnIndex := range result.Rows[rowIndex] {
			result.Rows[rowIndex][columnIndex] = freezeValue(result.Rows[rowIndex][columnIndex])
		}
	}
}

func freezeValue(value any) any {
	switch value := value.(type) {
	case *domain.Node:
		if value == nil {
			return nil
		}
		copy := *value
		copy.Labels = append([]string(nil), value.Labels...)
		copy.Properties = clonePropertyMap(value.Properties)
		return copy
	case *domain.Edge:
		if value == nil {
			return nil
		}
		copy := *value
		if value.Position != nil {
			position := *value.Position
			copy.Position = &position
		}
		copy.Properties = clonePropertyMap(value.Properties)
		return copy
	case []any:
		copy := make([]any, len(value))
		for index := range value {
			copy[index] = freezeValue(value[index])
		}
		return copy
	case map[string]any:
		copy := make(map[string]any, len(value))
		for key, item := range value {
			copy[key] = freezeValue(item)
		}
		return copy
	case Path:
		return value.cloneValues()
	default:
		return value
	}
}

func clonePropertyMap(source domain.Properties) domain.Properties {
	if source == nil {
		return nil
	}
	copy := make(domain.Properties, len(source))
	for key, value := range source {
		copy[key] = freezeValue(value)
	}
	return copy
}
