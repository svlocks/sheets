package engine

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/svlocks/sheets/internal/app"
	"github.com/svlocks/sheets/internal/cypher"
	"github.com/svlocks/sheets/internal/domain"
)

// ExecuteStream executes a read request and synchronously emits its results.
// Mutations are rejected before graph execution: publishing a row before a
// commit would violate atomic visibility, while publishing after a commit
// would make a downstream writer failure look like a rolled-back command.
func (e *Engine) ExecuteStream(ctx context.Context, request app.ExecuteRequest, emit app.ResultEmitter) error {
	if emit == nil {
		return errors.New("result emitter is nil")
	}
	document, mutates, err := prepareRequest(ctx, request)
	if err != nil {
		return err
	}
	if mutates {
		return app.ErrStreamingMutation
	}

	if result, plan, ok, err := e.executeDirectCount(ctx, request.Snapshot, document, request.Params); err != nil {
		return err
	} else if ok {
		e.storePlan(plan)
		return emitBatch(ctx, result, emit)
	}
	graph, plan, err := e.loadExecutionGraph(ctx, request.Snapshot, document, request.Params)
	if err != nil {
		return err
	}
	e.storePlan(plan)
	return executeDocumentStream(ctx, document, graph, request.Params, e.listProcedureRevisions, emit)
}

func executeDocumentStream(
	ctx context.Context,
	document *cypher.Document,
	graph *memoryGraph,
	params map[string]any,
	listRevisions revisionLister,
	emit app.ResultEmitter,
) error {
	return executeDocumentStreamWithClock(ctx, document, graph, params, listRevisions, emit, time.Now)
}

func executeDocumentStreamWithClock(
	ctx context.Context,
	document *cypher.Document,
	graph *memoryGraph,
	params map[string]any,
	listRevisions revisionLister,
	emit app.ResultEmitter,
	clock func() time.Time,
) error {
	transactionTime := clock()
	for statementIndex, statement := range document.Statements {
		if err := ctx.Err(); err != nil {
			return err
		}
		query, ok := statement.(*cypher.QueryStatement)
		if !ok {
			return fmt.Errorf("unsupported statement %T", statement)
		}
		if query.Explain {
			if err := emitResult(ctx, statementIndex, explainQuery(query), emit); err != nil {
				return err
			}
			continue
		}
		output := &queryResultEmitter{ctx: ctx, statement: statementIndex, emit: emit}
		result, err := executeQueryWithEmitter(ctx, document.Source, query, graph, params, listRevisions, queryClock{
			transaction: transactionTime,
			statement:   clock(),
			realtime:    clock,
		}, output)
		if err != nil {
			return err
		}
		if !output.started {
			if err := emitResult(ctx, statementIndex, result, emit); err != nil {
				return err
			}
			continue
		}
		if err := output.end(result.Summary, result.Page); err != nil {
			return err
		}
	}
	return nil
}

func emitBatch(ctx context.Context, batch app.BatchResult, emit app.ResultEmitter) error {
	for statement, result := range batch.Results {
		if err := emitResult(ctx, statement, result, emit); err != nil {
			return err
		}
	}
	return nil
}

func emitResult(ctx context.Context, statement int, result app.Result, emit app.ResultEmitter) error {
	output := queryResultEmitter{ctx: ctx, statement: statement, emit: emit}
	if err := output.start(result.Columns); err != nil {
		return err
	}
	for _, values := range result.Rows {
		if err := output.row(values); err != nil {
			return err
		}
	}
	return output.end(result.Summary, result.Page)
}

// queryResultEmitter is intentionally synchronous. The evaluator cannot
// outrun a slow writer, and a writer failure stops graph work immediately.
type queryResultEmitter struct {
	ctx       context.Context
	statement int
	emit      app.ResultEmitter
	started   bool
}

func (e *queryResultEmitter) start(columns []string) error {
	if e.started {
		return errors.New("result stream started more than once")
	}
	if err := e.ctx.Err(); err != nil {
		return err
	}
	e.started = true
	return e.emit(app.ResultEvent{
		Kind: app.ResultStart, Statement: e.statement, Columns: append([]string(nil), columns...),
	})
}

func (e *queryResultEmitter) row(values []any) error {
	if !e.started {
		return errors.New("result stream row emitted before start")
	}
	if err := e.ctx.Err(); err != nil {
		return err
	}
	detached := make([]any, len(values))
	for index, value := range values {
		detached[index] = freezeValue(value)
	}
	return e.emit(app.ResultEvent{Kind: app.ResultRow, Statement: e.statement, Values: detached})
}

func (e *queryResultEmitter) end(summary app.Summary, page *domain.PageInfo) error {
	if !e.started {
		return errors.New("result stream ended before start")
	}
	if err := e.ctx.Err(); err != nil {
		return err
	}
	var detachedPage *domain.PageInfo
	if page != nil {
		copy := *page
		detachedPage = &copy
	}
	return e.emit(app.ResultEvent{
		Kind: app.ResultEnd, Statement: e.statement, Summary: summary, Page: detachedPage,
	})
}
