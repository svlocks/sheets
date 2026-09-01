package engine

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/svlocks/sheets/internal/app"
	"github.com/svlocks/sheets/internal/domain"
	"github.com/svlocks/sheets/internal/domain/temporal"
	"github.com/svlocks/sheets/internal/store"
)

func testEngine(t *testing.T) (*Engine, *store.Store) {
	t.Helper()
	database, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "sheets.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	engine, err := New(database)
	if err != nil {
		t.Fatal(err)
	}
	return engine, database
}

func execute(t *testing.T, engine *Engine, query string, params map[string]any) app.BatchResult {
	t.Helper()
	result, err := engine.Execute(context.Background(), app.ExecuteRequest{Query: query, Params: params})
	if err != nil {
		t.Fatalf("Execute(%q): %v", query, err)
	}
	return result
}

func TestEngineScalarQuery(t *testing.T) {
	engine, _ := testEngine(t)
	result := execute(t, engine, "RETURN 1 AS one, $value + 2 AS total", map[string]any{"value": int64(40)})
	if len(result.Results) != 1 || len(result.Results[0].Rows) != 1 {
		t.Fatalf("result = %#v", result)
	}
	row := result.Results[0].Rows[0]
	if row[0] != int64(1) || row[1] != int64(42) || result.Revision != nil {
		t.Fatalf("row/revision = %#v / %#v", row, result.Revision)
	}
}

func TestEngineCreateMatchUpdateAndHistory(t *testing.T) {
	engine, _ := testEngine(t)
	created := execute(t, engine, `
CREATE (root:Task {title: 'Payments', body: '# Payments'})
       -[:CHILD {position: 0}]->(child:Task {title: 'Integrate SDK'})
RETURN elementId(root) AS root_id, child.title AS child_title`, nil)
	if created.Revision == nil || *created.Revision != 1 {
		t.Fatalf("created revision = %#v", created.Revision)
	}
	if got := created.Results[0].Summary; got.NodesCreated != 2 || got.RelationshipsCreated != 1 {
		t.Fatalf("create summary = %#v", got)
	}

	matched := execute(t, engine, `
MATCH (parent:Task)-[edge:CHILD]->(child:Task)
WHERE parent.title = 'Payments'
RETURN parent.title AS parent, child.title AS child, edge.position AS position`, nil)
	if got := matched.Results[0].Rows; len(got) != 1 || got[0][0] != "Payments" || got[0][1] != "Integrate SDK" || got[0][2] != int64(0) {
		t.Fatalf("matched rows = %#v", got)
	}

	updated := execute(t, engine, `
MATCH (task:Task {title: 'Integrate SDK'})
SET task.status = 'doing', task.body = 'Use the current SDK'
RETURN task.status AS status, body(task) AS body`, nil)
	if updated.Revision == nil || *updated.Revision != 2 {
		t.Fatalf("updated revision = %#v", updated.Revision)
	}
	if got := updated.Results[0].Rows; len(got) != 1 || got[0][0] != "doing" || got[0][1] != "Use the current SDK" {
		t.Fatalf("updated rows = %#v", got)
	}

	revision := domain.Revision(1)
	historical, err := engine.Execute(context.Background(), app.ExecuteRequest{
		Query:    "MATCH (task:Task {title: 'Integrate SDK'}) RETURN task.status AS status, body(task) AS body",
		Snapshot: domain.Snapshot{Revision: &revision},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := historical.Results[0].Rows; len(got) != 1 || got[0][0] != nil || got[0][1] != "" {
		t.Fatalf("historical rows = %#v", got)
	}
}

func TestEngineAtomicMultiStatementRollback(t *testing.T) {
	engine, database := testEngine(t)
	_, err := engine.Execute(context.Background(), app.ExecuteRequest{Query: `
CREATE (:Task {title: 'will roll back'});
CREATE (:Task)-[:CHILD]->(:Task)-[:CHILD]->(:Task);
MATCH (first:Task {title: 'will roll back'}), (last:Task)
CREATE (last)-[:CHILD]->(first)`})
	if err == nil {
		t.Fatal("invalid batch succeeded")
	}
	revision, revisionErr := database.CurrentRevision(context.Background())
	if revisionErr != nil {
		t.Fatal(revisionErr)
	}
	if revision != 0 {
		t.Fatalf("revision after rollback = %d", revision)
	}
	snapshot, snapshotErr := engine.Snapshot(context.Background(), domain.Snapshot{})
	if snapshotErr != nil {
		t.Fatal(snapshotErr)
	}
	if len(snapshot.Nodes) != 0 || len(snapshot.Edges) != 0 {
		t.Fatalf("graph after rollback = %#v", snapshot)
	}
}

func TestEngineReadOnlyAndHistoricalWriteGuards(t *testing.T) {
	engine, _ := testEngine(t)
	_, err := engine.Execute(context.Background(), app.ExecuteRequest{Query: "CREATE (:Task)", ReadOnly: true})
	if !errors.Is(err, domain.ErrReadOnly) {
		t.Fatalf("read-only error = %v", err)
	}
	revision := domain.Revision(0)
	_, err = engine.Execute(context.Background(), app.ExecuteRequest{
		Query: "CREATE (:Task)", Snapshot: domain.Snapshot{Revision: &revision},
	})
	if !errors.Is(err, domain.ErrHistoricalWrite) {
		t.Fatalf("historical error = %v", err)
	}
}

func TestEngineOptionalUnwindAggregationAndMerge(t *testing.T) {
	engine, _ := testEngine(t)
	execute(t, engine, `UNWIND ['a', 'b', 'b'] AS title CREATE (:Task {title: title})`, nil)

	aggregated := execute(t, engine, `
MATCH (task:Task)
RETURN count(task) AS total, count(DISTINCT task.title) AS unique_titles,
       collect(task.title) AS titles`, nil)
	row := aggregated.Results[0].Rows[0]
	if row[0] != int64(3) || row[1] != int64(2) {
		t.Fatalf("aggregate row = %#v", row)
	}

	optional := execute(t, engine, `
MATCH (task:Task {title: 'a'})
OPTIONAL MATCH (task)-[:CHILD]->(child)
RETURN task.title AS title, child.title AS child`, nil)
	if got := optional.Results[0].Rows; len(got) != 1 || got[0][0] != "a" || got[0][1] != nil {
		t.Fatalf("optional rows = %#v", got)
	}

	merged := execute(t, engine, `
MERGE (task:Task {title: 'a'})
ON MATCH SET task.seen = true
ON CREATE SET task.created = true
RETURN task.seen AS seen`, nil)
	if got := merged.Results[0].Rows; len(got) != 1 || got[0][0] != true {
		t.Fatalf("merge rows = %#v", got)
	}
}

func TestEngineDeleteRequiresDetach(t *testing.T) {
	engine, _ := testEngine(t)
	execute(t, engine, "CREATE (:Task {title:'p'})-[:CHILD]->(:Task {title:'c'})", nil)
	_, err := engine.Execute(context.Background(), app.ExecuteRequest{Query: "MATCH (n:Task {title:'p'}) DELETE n"})
	if err == nil {
		t.Fatal("non-detach delete succeeded")
	}
	deleted := execute(t, engine, "MATCH (n:Task {title:'p'}) DETACH DELETE n", nil)
	if deleted.Results[0].Summary.NodesDeleted != 1 || deleted.Results[0].Summary.RelationshipsDeleted != 1 {
		t.Fatalf("delete summary = %#v", deleted.Results[0].Summary)
	}
}

func TestEngineReadOnlyProcedures(t *testing.T) {
	engine, _ := testEngine(t)
	execute(t, engine, "CREATE (:Task), (:Milestone)", nil)
	result, err := engine.Execute(context.Background(), app.ExecuteRequest{
		Query: "CALL db.labels() YIELD label RETURN label ORDER BY label", ReadOnly: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := result.Results[0].Rows; len(got) != 2 || got[0][0] != "Milestone" || got[1][0] != "Task" {
		t.Fatalf("procedure rows = %#v", got)
	}
}

func TestEnginePatternExpressionsAndExistsSubqueries(t *testing.T) {
	engine, _ := testEngine(t)
	execute(t, engine, `
CREATE (a:Task {title:'a'})-[:BLOCKS]->(b:Task {title:'b'})-[:BLOCKS]->(c:Task {title:'c'}),
       (a)-[:BLOCKS]->(c)`, nil)
	result := execute(t, engine, `
MATCH (a:Task {title:'a'}), (c:Task {title:'c'})
WHERE EXISTS { MATCH (a)-[:BLOCKS*]->(c) }
RETURN length(shortestPath((a)-[:BLOCKS*]->(c))) AS distance,
       size(relationships(shortestPath((a)-[:BLOCKS*]->(c)))) AS edges`, nil)
	if got := result.Results[0].Rows; len(got) != 1 || got[0][0] != int64(1) || got[0][1] != int64(1) {
		t.Fatalf("pattern result = %#v", got)
	}
}

func TestEngineListPredicatesAndReduce(t *testing.T) {
	engine, _ := testEngine(t)
	result := execute(t, engine, `
RETURN any(x IN [1,2,3] WHERE x = 2) AS any,
       all(x IN [1,2,3] WHERE x > 0) AS all,
       none(x IN [1,2,3] WHERE x < 0) AS none,
       single(x IN [1,2,3] WHERE x = 2) AS single,
       reduce(total = 0, x IN [1,2,3] | total + x) AS total`, nil)
	want := []any{true, true, true, true, int64(6)}
	if got := result.Results[0].Rows; len(got) != 1 {
		t.Fatalf("rows = %#v", got)
	} else {
		for index := range want {
			if got[0][index] != want[index] {
				t.Fatalf("row = %#v, want %#v", got[0], want)
			}
		}
	}
}

func TestEngineExplainDoesNotMutate(t *testing.T) {
	engine, database := testEngine(t)
	result := execute(t, engine, "EXPLAIN CREATE (:Task)", nil)
	if len(result.Results[0].Rows) != 1 || result.Revision != nil {
		t.Fatalf("explain result = %#v", result)
	}
	revision, err := database.CurrentRevision(context.Background())
	if err != nil || revision != 0 {
		t.Fatalf("revision = %d, %v", revision, err)
	}
}

func TestEngineTemporalValuesAndNullPropertyRemoval(t *testing.T) {
	engine, _ := testEngine(t)
	execute(t, engine, `
CREATE (n:Task {title:'temporal', created:datetime('2026-08-31T12:30:00Z'), estimate:duration('PT1H30M')})
SET n.obsolete = 'yes', n.obsolete = null`, nil)
	result := execute(t, engine, `
MATCH (n:Task {title:'temporal'})
RETURN n.created AS created, n.estimate AS estimate, n.obsolete AS obsolete`, nil)
	row := result.Results[0].Rows[0]
	created, ok := row[0].(temporal.DateTime)
	if !ok || created.String() != "2026-08-31T12:30Z" {
		t.Fatalf("created = %#v", row[0])
	}
	estimate, err := temporal.ParseDuration("PT1H30M")
	if err != nil {
		t.Fatal(err)
	}
	if row[1] != estimate || row[2] != nil {
		t.Fatalf("temporal row = %#v", row)
	}
}

func TestEngineDatetimeFixedOffsetRoundTripsAndMatches(t *testing.T) {
	executor, _ := testEngine(t)
	result := execute(t, executor, `
CREATE (n:Task {at: datetime('2026-08-31T12:34:56.123456789-05:00')})
RETURN n.at AS at`, nil)
	if len(result.Results) != 1 || len(result.Results[0].Rows) != 1 {
		t.Fatalf("create fixed-offset datetime result = %#v", result)
	}
	want, err := temporal.ParseDateTime("2026-08-31T12:34:56.123456789-05:00")
	if err != nil {
		t.Fatal(err)
	}
	got, ok := result.Results[0].Rows[0][0].(temporal.DateTime)
	if !ok || !got.Equal(want) {
		t.Fatalf("created fixed-offset datetime = %#v, want %s", result.Results[0].Rows[0][0], want)
	}
	matched := execute(t, executor, `
MATCH (n:Task {at: datetime('2026-08-31T12:34:56.123456789-05:00')})
RETURN count(n) AS count`, nil)
	if got := matched.Results[0].Rows; len(got) != 1 || got[0][0] != int64(1) {
		actual := execute(t, executor, "MATCH (n:Task) RETURN n.at AS at", nil)
		t.Fatalf("fixed-offset datetime predicate = %#v; stored = %#v", got, actual.Results[0].Rows)
	}
}

func TestEngineRejectsDurationOverflow(t *testing.T) {
	engine, _ := testEngine(t)
	for _, query := range []string{
		"RETURN duration('P9999999999999999999M')",
		"RETURN duration('PT9999999999999999999S')",
	} {
		_, err := engine.Execute(context.Background(), app.ExecuteRequest{Query: query})
		if err == nil || !strings.Contains(err.Error(), "overflow") {
			t.Fatalf("expected duration range error for %s, got %v", query, err)
		}
	}
	for _, query := range []string{"RETURN duration('P')", "RETURN duration('PT')"} {
		_, err := engine.Execute(context.Background(), app.ExecuteRequest{Query: query})
		if err == nil || !strings.Contains(err.Error(), "invalid duration") {
			t.Fatalf("expected invalid duration error for %s, got %v", query, err)
		}
	}
}

func TestEngineStandaloneProcedureReturnsColumns(t *testing.T) {
	engine, _ := testEngine(t)
	execute(t, engine, "CREATE (:Task), (:Milestone)", nil)
	result := execute(t, engine, "CALL db.labels()", nil)
	if len(result.Results[0].Columns) != 1 || result.Results[0].Columns[0] != "label" || len(result.Results[0].Rows) != 2 {
		t.Fatalf("procedure result = %#v", result.Results[0])
	}
}

func TestSheetsRevisionsProcedurePaginatesMetadata(t *testing.T) {
	engine, _ := testEngine(t)
	ctx := context.Background()
	for _, request := range []app.ExecuteRequest{
		{Query: "CREATE (:Task {title: 'one'})", Actor: "alice", Message: "first"},
		{Query: "CREATE (:Task {title: 'two'})", Actor: "bob", Message: "second"},
	} {
		if _, err := engine.Execute(ctx, request); err != nil {
			t.Fatal(err)
		}
	}
	first, err := engine.Execute(ctx, app.ExecuteRequest{
		Query: "CALL sheets.revisions(1) YIELD revision, time, actor, message, next RETURN revision, time, actor, message, next",
	})
	if err != nil {
		t.Fatal(err)
	}
	row := first.Results[0].Rows[0]
	if row[0] != int64(1) || row[2] != "alice" || row[3] != "first" || row[4] == "" {
		t.Fatalf("first revision row = %#v", row)
	}
	if _, ok := row[1].(time.Time); !ok {
		t.Fatalf("revision time type = %T", row[1])
	}
	second := execute(t, engine,
		"CALL sheets.revisions(1, $after) YIELD revision, actor, message, next RETURN revision, actor, message, next",
		map[string]any{"after": row[4]},
	)
	if got := second.Results[0].Rows[0]; got[0] != int64(2) || got[1] != "bob" || got[2] != "second" || got[3] != "" {
		t.Fatalf("second revision row = %#v", got)
	}
}

func TestGraphFreeAndFilteredNodeReadsAvoidCompleteSnapshotCache(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "sheets.db")
	database, err := store.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	writer, _ := New(database)
	execute(t, writer, "CREATE (:Task {status:'open'}), (:Task {status:'closed'})-[:LINK]->(:Task)", nil)

	reader, _ := New(database)
	if got := execute(t, reader, "RETURN 40 + 2 AS answer", nil).Results[0].Rows[0][0]; got != int64(42) {
		t.Fatalf("scalar result = %v", got)
	}
	if reader.cache != nil {
		t.Fatal("scalar query populated the complete graph cache")
	}
	result := execute(t, reader, "MATCH (task:Task {status:$status}) RETURN task.status", map[string]any{"status": "open"})
	if got := result.Results[0].Rows; len(got) != 1 || got[0][0] != "open" {
		t.Fatalf("filtered result = %#v", got)
	}
	if reader.cache != nil {
		t.Fatal("filtered single-node query populated the complete graph cache")
	}
}

func TestEngineCacheObservesOtherProcessesAndProtectsSnapshots(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "sheets.db")
	firstStore, err := store.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := firstStore.Close(); err != nil {
			t.Errorf("close first store: %v", err)
		}
	}()
	secondStore, err := store.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := secondStore.Close(); err != nil {
			t.Errorf("close second store: %v", err)
		}
	}()
	first, _ := New(firstStore)
	second, _ := New(secondStore)
	execute(t, first, "CREATE (:Task {title:'one'})", nil)
	if got := execute(t, first, "MATCH (n) RETURN count(n)", nil).Results[0].Rows[0][0]; got != int64(1) {
		t.Fatalf("initial count = %v", got)
	}
	execute(t, second, "CREATE (:Task {title:'two'})", nil)
	if got := execute(t, first, "MATCH (n) RETURN count(n)", nil).Results[0].Rows[0][0]; got != int64(2) {
		t.Fatalf("count after external write = %v", got)
	}

	snapshot, err := first.Snapshot(ctx, domain.Snapshot{})
	if err != nil {
		t.Fatal(err)
	}
	snapshot.Nodes[0].Properties["title"] = "tampered"
	result := execute(t, first, "MATCH (n {title:'one'}) RETURN n.title", nil)
	if len(result.Results[0].Rows) != 1 || result.Results[0].Rows[0][0] != "one" {
		t.Fatalf("snapshot mutation escaped into cache: %#v", result)
	}
}
