package engine

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/svlocks/sheets/internal/app"
	"github.com/svlocks/sheets/internal/domain"
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
