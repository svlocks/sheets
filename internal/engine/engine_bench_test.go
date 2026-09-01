package engine

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/svlocks/sheets/internal/app"
	"github.com/svlocks/sheets/internal/cypher"
	"github.com/svlocks/sheets/internal/domain"
	"github.com/svlocks/sheets/internal/store"
)

func benchmarkEngine(b *testing.B, nodes int) *Engine {
	b.Helper()
	database, err := store.Open(context.Background(), filepath.Join(b.TempDir(), "sheets.db"))
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = database.Close() })
	_, err = database.Write(context.Background(), store.RevisionMeta{}, func(transaction *store.WriteTx) error {
		created := make([]domain.Node, nodes)
		for index := range created {
			created[index], err = transaction.CreateNode(store.NodeInput{
				Labels: []string{"Task"},
				Properties: domain.Properties{
					"title":  fmt.Sprintf("task-%06d", index),
					"status": []string{"todo", "doing", "done"}[index%3],
					"rank":   int64(index),
				},
			})
			if err != nil {
				return err
			}
			if index > 0 {
				position := int64(index)
				if _, err = transaction.CreateEdge(store.EdgeInput{
					From: created[(index-1)/2].ID, Type: "CHILD", To: created[index].ID, Position: &position,
				}); err != nil {
					return err
				}
			}
		}
		return nil
	})
	if err != nil {
		b.Fatal(err)
	}
	executor, err := New(database)
	if err != nil {
		b.Fatal(err)
	}
	return executor
}

func primeQuery(b *testing.B, executor *Engine, request app.ExecuteRequest) {
	b.Helper()
	if _, err := executor.Execute(context.Background(), request); err != nil {
		b.Fatal(err)
	}
}

func BenchmarkQueryIndexedPointMatch1K(b *testing.B) {
	executor := benchmarkEngine(b, 1_000)
	request := app.ExecuteRequest{
		Query:  "MATCH (n:Task {title:$title}) RETURN elementId(n), n.status",
		Params: map[string]any{"title": "task-000999"},
	}
	primeQuery(b, executor, request)
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if _, err := executor.Execute(context.Background(), request); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkQueryIndexedPointMatch10K(b *testing.B) {
	executor := benchmarkEngine(b, 10_000)
	request := app.ExecuteRequest{
		Query:  "MATCH (n:Task {title:$title}) RETURN elementId(n), n.status",
		Params: map[string]any{"title": "task-009999"},
	}
	primeQuery(b, executor, request)
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if _, err := executor.Execute(context.Background(), request); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkQueryColdSnapshot1K(b *testing.B) {
	executor := benchmarkEngine(b, 1_000)
	request := app.ExecuteRequest{
		Query:  "MATCH (n:Task {title:$title})-[:CHILD]->(child) RETURN elementId(child)",
		Params: map[string]any{"title": "task-000000"},
	}
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		executor.cacheMu.Lock()
		executor.cache = nil
		executor.cacheMu.Unlock()
		if _, err := executor.Execute(context.Background(), request); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkQueryColdSnapshot10K(b *testing.B) {
	executor := benchmarkEngine(b, 10_000)
	request := app.ExecuteRequest{
		Query:  "MATCH (n:Task {title:$title})-[:CHILD]->(child) RETURN elementId(child)",
		Params: map[string]any{"title": "task-000000"},
	}
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		executor.cacheMu.Lock()
		executor.cache = nil
		executor.cacheMu.Unlock()
		if _, err := executor.Execute(context.Background(), request); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkQueryColdSnapshot100K(b *testing.B) {
	if os.Getenv("SHEETS_BENCH_100K") != "1" {
		b.Skip("set SHEETS_BENCH_100K=1 to opt into the expensive 100K fixture build")
	}
	executor := benchmarkEngine(b, 100_000)
	request := app.ExecuteRequest{
		Query:  "MATCH (n:Task {title:$title})-[:CHILD]->(child) RETURN elementId(child)",
		Params: map[string]any{"title": "task-000000"},
	}
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		executor.cacheMu.Lock()
		executor.cache = nil
		executor.cacheMu.Unlock()
		if _, err := executor.Execute(context.Background(), request); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkQueryColdFourHop10K(b *testing.B) {
	executor := benchmarkEngine(b, 10_000)
	request := app.ExecuteRequest{
		Query:  "MATCH (root:Task {title:$title})-[:CHILD*1..4]->(descendant) RETURN count(descendant)",
		Params: map[string]any{"title": "task-000000"},
	}
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		executor.cacheMu.Lock()
		executor.cache = nil
		executor.cacheMu.Unlock()
		if _, err := executor.Execute(context.Background(), request); err != nil {
			b.Fatal(err)
		}
	}
}

// These paired benchmarks use independently built but equivalent on-disk
// fixtures and the same request. Each arm is preconditioned once outside the
// timer so one-time SQLite page/statement setup does not dominate a short run;
// the engine graph cache is still cleared before every measured operation.
func BenchmarkQueryColdPoint10K(b *testing.B) {
	benchmarkColdComparison(b,
		"MATCH (n:Task {title:$title}) RETURN elementId(n), n.status",
		map[string]any{"title": "task-009999"},
	)
}

// This deliberately supplies a float for an integer-backed property. Cypher
// numeric equality considers 9999 and 9999.0 equal, so the iterator cannot use
// the store's representation-exact property index without risking false
// negatives. The paired full-snapshot arm makes that correctness cost visible.
func BenchmarkQueryColdNumericCrossTypePoint10K(b *testing.B) {
	benchmarkColdComparison(b,
		"MATCH (n:Task {rank:$rank}) RETURN elementId(n), n.status",
		map[string]any{"rank": float64(9_999)},
	)
}

func BenchmarkQueryColdSingleHop10K(b *testing.B) {
	benchmarkColdComparison(b,
		"MATCH (n:Task {title:$title})-[:CHILD]->(child) RETURN elementId(child)",
		map[string]any{"title": "task-000000"},
	)
}

func BenchmarkQueryColdFourHopComparison10K(b *testing.B) {
	benchmarkColdComparison(b,
		"MATCH (root:Task {title:$title})-[:CHILD*1..4]->(descendant) RETURN count(descendant)",
		map[string]any{"title": "task-000000"},
	)
}

func benchmarkColdComparison(b *testing.B, query string, params map[string]any) {
	b.Helper()
	request := app.ExecuteRequest{Query: query, Params: params}
	for _, implementation := range []struct {
		name string
		run  func(context.Context, *Engine, app.ExecuteRequest) error
	}{
		{name: "iterator", run: func(ctx context.Context, executor *Engine, request app.ExecuteRequest) error {
			_, err := executor.Execute(ctx, request)
			return err
		}},
		{name: "full_snapshot", run: executeForcedFullSnapshot},
	} {
		b.Run(implementation.name, func(b *testing.B) {
			b.StopTimer()
			executor := benchmarkEngine(b, 10_000)
			if err := implementation.run(context.Background(), executor, request); err != nil {
				b.Fatal(err)
			}
			executor.cacheMu.Lock()
			executor.cache = nil
			executor.cacheMu.Unlock()
			b.ReportAllocs()
			b.ResetTimer()
			b.ReportMetric(10_000, "fixture_nodes")
			b.StartTimer()
			for range b.N {
				executor.cacheMu.Lock()
				executor.cache = nil
				executor.cacheMu.Unlock()
				if err := implementation.run(context.Background(), executor, request); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func BenchmarkQueryFrontendPoint(b *testing.B) {
	const query = "MATCH (n:Task {title:$title}) RETURN elementId(n), n.status"
	b.ReportAllocs()
	for range b.N {
		if _, err := cypher.Parse(query); err != nil {
			b.Fatal(err)
		}
	}
}

func executeForcedFullSnapshot(ctx context.Context, executor *Engine, request app.ExecuteRequest) error {
	if err := request.Validate(); err != nil {
		return err
	}
	document, err := cypher.Parse(request.Query)
	if err != nil {
		return err
	}
	if len(document.Statements) == 0 {
		return fmt.Errorf("query has no statements")
	}
	if err := validateDocumentSemantics(document); err != nil {
		return err
	}
	graph, err := executor.loadGraph(ctx, request.Snapshot)
	if err != nil {
		return err
	}
	_, err = executeDocument(ctx, document, graph, request.Params, executor.store.ListRevisions)
	return err
}

func BenchmarkQueryGraphFreeScalar10K(b *testing.B) {
	executor := benchmarkEngine(b, 10_000)
	request := app.ExecuteRequest{Query: "RETURN $value + 1", Params: map[string]any{"value": int64(41)}}
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if _, err := executor.Execute(context.Background(), request); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkQueryWarmHierarchyTraversal1K(b *testing.B) {
	executor := benchmarkEngine(b, 1_000)
	request := app.ExecuteRequest{
		Query:  "MATCH (root:Task {title:$title})-[:CHILD*1..4]->(descendant) RETURN count(descendant)",
		Params: map[string]any{"title": "task-000000"},
	}
	primeQuery(b, executor, request)
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if _, err := executor.Execute(context.Background(), request); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkAtomicCreate100(b *testing.B) {
	database, err := store.Open(context.Background(), filepath.Join(b.TempDir(), "sheets.db"))
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = database.Close() })
	executor, _ := New(database)
	titles := make([]any, 100)
	for index := range titles {
		titles[index] = fmt.Sprintf("task-%d", index)
	}
	request := app.ExecuteRequest{
		Query:  "UNWIND $titles AS title CREATE (:Task {title:title})",
		Params: map[string]any{"titles": titles},
	}
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if _, err := executor.Execute(context.Background(), request); err != nil {
			b.Fatal(err)
		}
	}
}
