package engine

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/svlocks/sheets/internal/app"
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

func BenchmarkQueryPointMatch1K(b *testing.B) {
	executor := benchmarkEngine(b, 1_000)
	request := app.ExecuteRequest{
		Query:  "MATCH (n:Task {title:$title}) RETURN elementId(n), n.status",
		Params: map[string]any{"title": "task-000999"},
	}
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if _, err := executor.Execute(context.Background(), request); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkQueryPointMatch10K(b *testing.B) {
	executor := benchmarkEngine(b, 10_000)
	request := app.ExecuteRequest{
		Query:  "MATCH (n:Task {title:$title}) RETURN elementId(n), n.status",
		Params: map[string]any{"title": "task-009999"},
	}
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
		Query:  "MATCH (n:Task {title:$title}) RETURN elementId(n), n.status",
		Params: map[string]any{"title": "task-000999"},
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
		Query:  "MATCH (n:Task {title:$title}) RETURN elementId(n), n.status",
		Params: map[string]any{"title": "task-009999"},
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

func BenchmarkQueryHierarchyTraversal1K(b *testing.B) {
	executor := benchmarkEngine(b, 1_000)
	request := app.ExecuteRequest{
		Query:  "MATCH (root:Task {title:$title})-[:CHILD*1..4]->(descendant) RETURN count(descendant)",
		Params: map[string]any{"title": "task-000000"},
	}
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
