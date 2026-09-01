package engine

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/svlocks/sheets/internal/app"
	"github.com/svlocks/sheets/internal/cypher"
	"github.com/svlocks/sheets/internal/domain"
	"github.com/svlocks/sheets/internal/store"
)

// Engine parses and atomically executes Cypher against one Store.
type Engine struct {
	store   *store.Store
	cacheMu sync.RWMutex
	cache   *memoryGraph
	planMu  sync.RWMutex
	plan    physicalPlan
}

var _ app.Executor = (*Engine)(nil)
var _ app.StreamExecutor = (*Engine)(nil)

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
	document, mutates, err := prepareRequest(ctx, request)
	if err != nil {
		return app.BatchResult{}, err
	}

	if !mutates {
		if result, plan, ok, err := e.executeDirectCount(ctx, request.Snapshot, document, request.Params); err != nil {
			return app.BatchResult{}, err
		} else if ok {
			e.storePlan(plan)
			return result, nil
		}
		graph, plan, err := e.loadExecutionGraph(ctx, request.Snapshot, document, request.Params)
		if err != nil {
			return app.BatchResult{}, err
		}
		e.storePlan(plan)
		return executeDocument(ctx, document, graph, request.Params, e.listProcedureRevisions)
	}

	var batch app.BatchResult
	var committedGraph *memoryGraph
	writeResult, err := e.store.Write(ctx, store.RevisionMeta{
		Actor: request.Actor, Message: request.Message,
	}, func(transaction *store.WriteTx) error {
		graph, err := e.loadWriteGraph(transaction)
		if err != nil {
			return err
		}
		batch, err = executeDocument(ctx, document, graph, request.Params, e.listProcedureRevisions)
		committedGraph = graph
		return err
	})
	if err != nil {
		return app.BatchResult{}, err
	}
	if writeResult.Changed {
		revision := writeResult.Revision
		batch.Revision = &revision
	}
	if committedGraph != nil {
		committedGraph.writer = nil
		e.storeCache(committedGraph)
	}
	return batch, nil
}

func prepareRequest(ctx context.Context, request app.ExecuteRequest) (*cypher.Document, bool, error) {
	if ctx == nil {
		return nil, false, errors.New("execute query: nil context")
	}
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	if err := request.Validate(); err != nil {
		return nil, false, err
	}
	document, err := cypher.Parse(request.Query)
	if err != nil {
		return nil, false, err
	}
	if len(document.Statements) == 0 {
		return nil, false, errors.New("query has no statements")
	}
	if err := validateDocumentSemantics(document); err != nil {
		return nil, false, err
	}
	mutates := false
	for _, statement := range document.Statements {
		mutates = mutates || statementMutates(statement)
	}
	if request.ReadOnly && mutates {
		return nil, false, domain.ErrReadOnly
	}
	if !request.Snapshot.IsCurrent() && mutates {
		return nil, false, domain.ErrHistoricalWrite
	}
	return document, mutates, nil
}

func executeDocument(
	ctx context.Context,
	document *cypher.Document,
	graph *memoryGraph,
	params map[string]any,
	listRevisions revisionLister,
) (app.BatchResult, error) {
	return executeDocumentWithClock(ctx, document, graph, params, listRevisions, time.Now)
}

func executeDocumentWithClock(
	ctx context.Context,
	document *cypher.Document,
	graph *memoryGraph,
	params map[string]any,
	listRevisions revisionLister,
	clock func() time.Time,
) (app.BatchResult, error) {
	transactionTime := clock()
	batch := app.BatchResult{Results: make([]app.Result, 0, len(document.Statements))}
	for _, statement := range document.Statements {
		if err := ctx.Err(); err != nil {
			return app.BatchResult{}, err
		}
		query, ok := statement.(*cypher.QueryStatement)
		if !ok {
			return app.BatchResult{}, fmt.Errorf("unsupported statement %T", statement)
		}
		if query.Explain {
			batch.Results = append(batch.Results, explainQuery(query))
			continue
		}
		result, err := executeQuery(ctx, document.Source, query, graph, params, listRevisions, queryClock{
			transaction: transactionTime,
			statement:   clock(),
			realtime:    clock,
		})
		if err != nil {
			return app.BatchResult{}, err
		}
		freezeResult(&result)
		batch.Results = append(batch.Results, result)
	}
	return batch, nil
}

func (e *Engine) loadExecutionGraph(
	ctx context.Context,
	snapshot domain.Snapshot,
	document *cypher.Document,
	params map[string]any,
) (*memoryGraph, physicalPlan, error) {
	if documentRequiresGraph(document) {
		iteratorFallback := ""
		if graph, plan, ok, err := e.loadReadWorkingSet(ctx, snapshot, document, params); err != nil {
			return nil, plan, err
		} else if ok {
			return graph, plan, nil
		} else {
			iteratorFallback = plan.Fallback
		}
		if graph, ok, err := e.loadNodeWorkingSet(ctx, snapshot, document, params); err != nil {
			legacyFallback := "legacy single-node working set"
			if iteratorFallback != "" {
				legacyFallback = iteratorFallback + "; " + legacyFallback
			}
			return nil, physicalPlan{Fallback: legacyFallback}, err
		} else if ok {
			return graph, physicalPlan{Operators: []planOperator{{Kind: "NodeIndexScan", Detail: "legacy single-node working set"}}, Fallback: iteratorFallback}, nil
		}
		graph, err := e.loadGraph(ctx, snapshot)
		if iteratorFallback == "" {
			iteratorFallback = "unsupported or correlated graph pipeline"
		} else {
			iteratorFallback += "; full snapshot materialization"
		}
		return graph, physicalPlan{Operators: []planOperator{{Kind: "FullSnapshot", Detail: "complete graph materialization"}}, Fallback: iteratorFallback}, err
	}
	revision, err := e.store.ResolveSnapshot(ctx, snapshot)
	if err != nil {
		return nil, physicalPlan{}, err
	}
	return newMemoryGraph(revision, nil, nil, nil), physicalPlan{Operators: []planOperator{{Kind: "ScalarPipeline", Detail: "graph-free"}}}, nil
}

func (e *Engine) storePlan(plan physicalPlan) {
	e.planMu.Lock()
	e.plan = plan.clone()
	e.planMu.Unlock()
}

// lastPlan is retained for engine-package regression tests. Public callers use
// EXPLAIN, whose rows expose the same selected operator/fallback information.
func (e *Engine) lastPlan() physicalPlan {
	e.planMu.RLock()
	defer e.planMu.RUnlock()
	return e.plan.clone()
}

// Snapshot returns a complete graph state for interactive clients.
func (e *Engine) Snapshot(ctx context.Context, snapshot domain.Snapshot) (GraphSnapshot, error) {
	graph, err := e.loadGraph(ctx, snapshot)
	if err != nil {
		return GraphSnapshot{}, err
	}
	return GraphSnapshot{Revision: graph.revision, Nodes: graph.nodeValues(), Edges: graph.edgeValues()}, nil
}

func (e *Engine) loadGraph(ctx context.Context, snapshot domain.Snapshot) (*memoryGraph, error) {
	if !snapshot.IsCurrent() {
		return loadSnapshot(ctx, e.store, snapshot)
	}
	revision, err := e.store.CurrentRevision(ctx)
	if err != nil {
		return nil, err
	}
	e.cacheMu.RLock()
	cached := e.cache
	if cached != nil && cached.revision == revision {
		e.cacheMu.RUnlock()
		return cached, nil
	}
	e.cacheMu.RUnlock()
	selector := revision
	loaded, err := loadSnapshot(ctx, e.store, domain.Snapshot{Revision: &selector})
	if err != nil {
		return nil, err
	}
	e.storeCache(loaded)
	return loaded, nil
}

func (e *Engine) storeCache(graph *memoryGraph) {
	e.cacheMu.Lock()
	if e.cache == nil || e.cache.revision <= graph.revision {
		e.cache = graph
	}
	e.cacheMu.Unlock()
}

func (e *Engine) loadWriteGraph(transaction *store.WriteTx) (*memoryGraph, error) {
	base := transaction.CurrentRevision()
	e.cacheMu.RLock()
	cached := e.cache
	if cached != nil && cached.revision == base {
		nodes, edges := cached.nodeValues(), cached.edgeValues()
		e.cacheMu.RUnlock()
		return newMemoryGraph(base, nodes, edges, transaction), nil
	}
	e.cacheMu.RUnlock()
	return loadWriteGraph(transaction)
}

// CurrentRevision returns the cheap invalidation token used by the TUI.
func (e *Engine) CurrentRevision(ctx context.Context) (domain.Revision, error) {
	return e.store.CurrentRevision(ctx)
}

// ListRevisions exposes the store's stable revision pagination.
func (e *Engine) ListRevisions(ctx context.Context, page domain.Page) ([]domain.RevisionInfo, domain.PageInfo, error) {
	return e.store.ListRevisions(ctx, page)
}

// ListRevisionPage exposes bounded revision traversal in either stable order.
func (e *Engine) ListRevisionPage(ctx context.Context, page domain.RevisionPage) ([]domain.RevisionInfo, domain.PageInfo, error) {
	return e.store.ListRevisionPage(ctx, page)
}

func (e *Engine) listProcedureRevisions(ctx context.Context, page domain.RevisionPage) ([]domain.RevisionInfo, domain.PageInfo, error) {
	if page.Order == domain.RevisionOrderAscending {
		return e.store.ListRevisions(ctx, domain.Page{Limit: page.Limit, After: page.Cursor})
	}
	return e.store.ListRevisionPage(ctx, page)
}

func statementMutates(statement cypher.Statement) bool {
	query, ok := statement.(*cypher.QueryStatement)
	if !ok {
		return statement.IsMutation()
	}
	if query.Explain {
		return false
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

func explainQuery(query *cypher.QueryStatement) app.Result {
	plan := logicalReadPlan(query)
	rows := make([][]any, 0, len(plan.Operators)+1)
	for _, operator := range plan.Operators {
		rows = append(rows, []any{operator.Kind, operator.Detail, operator.Pushdown, ""})
	}
	if plan.Fallback != "" {
		rows = append(rows, []any{"Fallback", plan.Fallback, "", plan.Fallback})
	}
	if len(rows) == 0 {
		for index, clause := range query.Clauses {
			name := strings.TrimPrefix(fmt.Sprintf("%T", clause), "*cypher.")
			rows = append(rows, []any{fmt.Sprintf("Clause%d", index), name, "", ""})
		}
	}
	return app.Result{Columns: []string{"operator", "detail", "pushdown", "fallback"}, Rows: rows}
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
