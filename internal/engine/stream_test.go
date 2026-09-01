package engine

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/svlocks/sheets/internal/app"
	"github.com/svlocks/sheets/internal/cypher"
	"github.com/svlocks/sheets/internal/domain"
)

type resultCollector struct {
	batch  app.BatchResult
	active int
	events []app.ResultEventKind
}

func (c *resultCollector) emit(event app.ResultEvent) error {
	c.events = append(c.events, event.Kind)
	switch event.Kind {
	case app.ResultStart:
		if c.active != 0 || event.Statement != len(c.batch.Results) {
			return errors.New("invalid result start")
		}
		c.batch.Results = append(c.batch.Results, app.Result{Columns: append([]string(nil), event.Columns...)})
		c.active = event.Statement + 1
	case app.ResultRow:
		if c.active != event.Statement+1 {
			return errors.New("invalid result row")
		}
		result := &c.batch.Results[event.Statement]
		result.Rows = append(result.Rows, append([]any(nil), event.Values...))
	case app.ResultEnd:
		if c.active != event.Statement+1 {
			return errors.New("invalid result end")
		}
		result := &c.batch.Results[event.Statement]
		result.Summary = event.Summary
		if event.Page != nil {
			page := *event.Page
			result.Page = &page
		}
		c.active = 0
	default:
		return errors.New("unknown result event")
	}
	return nil
}

func TestExecuteStreamMatchesMaterializedReadSemantics(t *testing.T) {
	executor, _ := testEngine(t)
	execute(t, executor, "UNWIND [3, 1, 2, 1] AS rank CREATE (:Task {rank:rank})", nil)

	for _, query := range []string{
		"UNWIND range(1, 20) AS value RETURN value, value * value AS square SKIP 3 LIMIT 5",
		"MATCH (task:Task) RETURN DISTINCT task.rank AS rank ORDER BY rank DESC LIMIT 2",
		"MATCH (task:Task) RETURN count(task) AS total",
		"RETURN 1 AS value UNION RETURN 1 AS value UNION RETURN 2 AS value",
		"EXPLAIN MATCH (task:Task) RETURN task.rank AS rank",
		"CALL db.labels() YIELD label RETURN label ORDER BY label",
		"CALL { RETURN 1 AS inner } RETURN inner + 1 AS outer",
		"MATCH (task:Task) WHERE EXISTS { MATCH (other:Task) WHERE other.rank = task.rank RETURN other } RETURN task.rank AS rank ORDER BY rank",
		"CALL db.labels() YIELD label",
		"MATCH (:Missing) RETURN 1 AS value",
		"RETURN 1 AS first; RETURN 2 AS second",
	} {
		want, err := executor.Execute(context.Background(), app.ExecuteRequest{Query: query, ReadOnly: true})
		if err != nil {
			t.Fatalf("Execute(%q): %v", query, err)
		}
		collector := &resultCollector{}
		if err := executor.ExecuteStream(context.Background(), app.ExecuteRequest{Query: query, ReadOnly: true}, collector.emit); err != nil {
			t.Fatalf("ExecuteStream(%q): %v", query, err)
		}
		if collector.active != 0 {
			t.Fatalf("ExecuteStream(%q) left an active result", query)
		}
		if !reflect.DeepEqual(collector.batch, want) {
			t.Fatalf("ExecuteStream(%q)\n got: %#v\nwant: %#v", query, collector.batch, want)
		}
	}
}

func TestStreamingProjectionPreservesCumulativeRowBudget(t *testing.T) {
	document, err := cypher.Parse("RETURN value")
	if err != nil {
		t.Fatal(err)
	}
	query := document.Statements[0].(*cypher.QueryStatement)
	projection := query.Clauses[0].(*cypher.ProjectionClause)
	input := []row{{"value": int64(1)}, {"value": int64(2)}}
	known := variableScope{"value": variableUnknown}

	materialized := &queryExecution{ctx: context.Background(), source: document.Source, evaluator: newEvaluator(nil)}
	materialized.evaluator.ctx = materialized.ctx
	materialized.evaluator.rows = &rowBudget{limit: 1}
	_, _, materializedErr := materialized.project(input, projection, known, false)

	var events []app.ResultEventKind
	output := &queryResultEmitter{ctx: context.Background(), emit: func(event app.ResultEvent) error {
		events = append(events, event.Kind)
		return nil
	}}
	streamed := &queryExecution{ctx: context.Background(), source: document.Source, evaluator: newEvaluator(nil), output: output}
	streamed.evaluator.ctx = streamed.ctx
	streamed.evaluator.rows = &rowBudget{limit: 1}
	_, _, streamErr := streamed.project(input, projection, known, true)

	if materializedErr == nil || streamErr == nil || materializedErr.Error() != streamErr.Error() {
		t.Fatalf("row-budget errors = materialized %v, stream %v", materializedErr, streamErr)
	}
	if want := []app.ResultEventKind{app.ResultStart, app.ResultRow}; !reflect.DeepEqual(events, want) {
		t.Fatalf("row-budget stream events = %v, want %v", events, want)
	}
}

func TestExecuteStreamPreservesPaginationEvaluationOrder(t *testing.T) {
	executor, _ := testEngine(t)
	for _, query := range []string{
		"UNWIND ['bad', 1] AS value RETURN value + 1 AS result SKIP 1",
		"UNWIND [1, 'bad'] AS value RETURN value + 1 AS result LIMIT 1",
	} {
		_, materializedErr := executor.Execute(context.Background(), app.ExecuteRequest{Query: query, ReadOnly: true})
		var events []app.ResultEvent
		streamErr := executor.ExecuteStream(context.Background(), app.ExecuteRequest{Query: query, ReadOnly: true}, func(event app.ResultEvent) error {
			events = append(events, event)
			return nil
		})
		if materializedErr == nil || streamErr == nil {
			t.Fatalf("pagination query %q errors = materialized %v, stream %v", query, materializedErr, streamErr)
		}
		if materializedErr.Error() != streamErr.Error() {
			t.Fatalf("pagination query %q errors differ:\nmaterialized: %v\nstreamed: %v", query, materializedErr, streamErr)
		}
		if len(events) != 0 {
			t.Fatalf("pagination query %q emitted before its blocking projection completed: %#v", query, events)
		}
	}

	document, err := cypher.Parse("UNWIND [1, 2, 3] AS value RETURN datetime.realtime() AS now SKIP 1 LIMIT 1")
	if err != nil {
		t.Fatal(err)
	}
	graph := newMemoryGraph(0, nil, nil, nil)
	clock := func(counter *int) func() time.Time {
		return func() time.Time {
			*counter++
			return time.Unix(int64(*counter), 0).UTC()
		}
	}
	materializedCalls := 0
	want, err := executeDocumentWithClock(context.Background(), document, graph, nil, nil, clock(&materializedCalls))
	if err != nil {
		t.Fatal(err)
	}
	streamCalls := 0
	collector := &resultCollector{}
	if err := executeDocumentStreamWithClock(context.Background(), document, graph, nil, nil, collector.emit, clock(&streamCalls)); err != nil {
		t.Fatal(err)
	}
	if streamCalls != materializedCalls || !reflect.DeepEqual(collector.batch, want) {
		t.Fatalf("volatile pagination differed: calls %d/%d, streamed %#v, materialized %#v", streamCalls, materializedCalls, collector.batch, want)
	}
}

func TestExecuteStreamRejectsMutationsBeforeEffectsOrEvents(t *testing.T) {
	executor, database := testEngine(t)
	emitted := false
	err := executor.ExecuteStream(context.Background(), app.ExecuteRequest{
		Query: "CREATE (:Task) RETURN 1 AS value",
	}, func(app.ResultEvent) error {
		emitted = true
		return nil
	})
	if !errors.Is(err, app.ErrStreamingMutation) {
		t.Fatalf("streaming mutation error = %v", err)
	}
	if emitted {
		t.Fatal("streaming mutation emitted an event")
	}
	if revision, revisionErr := database.CurrentRevision(context.Background()); revisionErr != nil || revision != 0 {
		t.Fatalf("streaming mutation changed revision to %d: %v", revision, revisionErr)
	}
}

func TestExecuteStreamBackpressureErrorStopsWithoutEnd(t *testing.T) {
	executor, _ := testEngine(t)
	wantErr := errors.New("sink stopped")
	rows, ends := 0, 0
	err := executor.ExecuteStream(context.Background(), app.ExecuteRequest{
		Query: "UNWIND range(1, 100) AS value RETURN value", ReadOnly: true,
	}, func(event app.ResultEvent) error {
		switch event.Kind {
		case app.ResultRow:
			rows++
			if rows == 3 {
				return wantErr
			}
		case app.ResultEnd:
			ends++
		}
		return nil
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("stream error = %v, want sink error", err)
	}
	if rows != 3 || ends != 0 {
		t.Fatalf("rows/end events = %d/%d, want 3/0", rows, ends)
	}
}

func TestExecuteStreamCancellationAndRuntimeErrorsLeaveValidPrefix(t *testing.T) {
	executor, _ := testEngine(t)

	ctx, cancel := context.WithCancel(context.Background())
	rows, ends := 0, 0
	err := executor.ExecuteStream(ctx, app.ExecuteRequest{
		Query: "UNWIND range(1, 100) AS value RETURN value", ReadOnly: true,
	}, func(event app.ResultEvent) error {
		if event.Kind == app.ResultRow {
			rows++
			cancel()
		}
		if event.Kind == app.ResultEnd {
			ends++
		}
		return nil
	})
	if !errors.Is(err, context.Canceled) || rows != 1 || ends != 0 {
		t.Fatalf("canceled stream = error %v, rows/end %d/%d", err, rows, ends)
	}

	var kinds []app.ResultEventKind
	err = executor.ExecuteStream(context.Background(), app.ExecuteRequest{
		Query: "UNWIND [1, 'bad'] AS value RETURN value + 1 AS result", ReadOnly: true,
	}, func(event app.ResultEvent) error {
		kinds = append(kinds, event.Kind)
		return nil
	})
	if err == nil {
		t.Fatal("runtime-error stream succeeded")
	}
	if want := []app.ResultEventKind{app.ResultStart, app.ResultRow}; !reflect.DeepEqual(kinds, want) {
		t.Fatalf("runtime-error events = %v, want %v (error %v)", kinds, want, err)
	}
}

func TestExecuteStreamDetachesGraphValues(t *testing.T) {
	executor, _ := testEngine(t)
	execute(t, executor, "CREATE (:Task {title:'before'})", nil)
	var retained any
	if err := executor.ExecuteStream(context.Background(), app.ExecuteRequest{
		Query: "MATCH (task:Task) RETURN task", ReadOnly: true,
	}, func(event app.ResultEvent) error {
		if event.Kind == app.ResultRow {
			retained = event.Values[0]
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, ok := retained.(domain.Node); !ok {
		t.Fatalf("streamed graph value type = %T, want detached domain.Node", retained)
	}
	execute(t, executor, "MATCH (task:Task) SET task.title = 'after'", nil)
	if got := retained.(domain.Node).Properties["title"]; got != "before" {
		t.Fatalf("retained streamed node changed to %#v", got)
	}
}
