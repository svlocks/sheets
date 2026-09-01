package store

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/svlocks/sheets/internal/domain"
)

var (
	benchmarkEncoded []byte
	benchmarkError   error
)

func BenchmarkDurableValueValidation(b *testing.B) {
	b.Run("revision_metadata", func(b *testing.B) {
		meta := RevisionMeta{Actor: "benchmark", Message: "validate bounded metadata"}
		b.ReportAllocs()
		for range b.N {
			benchmarkError = validateRevisionMeta(meta)
		}
	})
	b.Run("markdown_body_4KiB", func(b *testing.B) {
		body := "# Heading\n\n" + strings.Repeat("x", 4<<10)
		b.ReportAllocs()
		for range b.N {
			benchmarkError = validateNodeBody(body)
		}
	})
	b.Run("nested_properties", func(b *testing.B) {
		properties := domain.Properties{
			"title": "benchmark", "tags": []any{"one", "two", "three"},
			"metadata": domain.Properties{"rank": int64(7), "enabled": true},
		}
		b.ReportAllocs()
		for range b.N {
			benchmarkEncoded, benchmarkError = encodeProperties(properties)
		}
	})
}

func BenchmarkWriteBatch(b *testing.B) {
	ctx := context.Background()
	store := openTestStore(b, filepath.Join(b.TempDir(), "benchmark.db"))
	b.Cleanup(func() { _ = store.Close() })
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, err := store.Write(ctx, RevisionMeta{}, func(tx *WriteTx) error {
			for j := 0; j < 25; j++ {
				if _, err := tx.CreateNode(NodeInput{Properties: domain.Properties{"batch": int64(i), "item": int64(j)}}); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkHistoricalList(b *testing.B) {
	ctx := context.Background()
	store := openTestStore(b, filepath.Join(b.TempDir(), "benchmark.db"))
	b.Cleanup(func() { _ = store.Close() })
	for i := 0; i < 1000; i++ {
		_, err := store.Write(ctx, RevisionMeta{}, func(tx *WriteTx) error {
			_, err := tx.CreateNode(NodeInput{Labels: []string{"Item"}, Body: fmt.Sprintf("item-%d", i)})
			return err
		})
		if err != nil {
			b.Fatal(err)
		}
	}
	revision := domain.Revision(500)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		nodes, _, err := store.ListNodes(ctx, domain.Snapshot{Revision: &revision}, NodeFilter{Labels: []string{"Item"}}, domain.Page{Limit: 1000})
		if err != nil || len(nodes) != 500 {
			b.Fatalf("nodes = %d: %v", len(nodes), err)
		}
	}
}

func BenchmarkIndexedColdScalarLookup(b *testing.B) {
	ctx := context.Background()
	store := openTestStore(b, filepath.Join(b.TempDir(), "benchmark.db"))
	b.Cleanup(func() { _ = store.Close() })
	_, err := store.Write(ctx, RevisionMeta{}, func(tx *WriteTx) error {
		for i := 0; i < 10_000; i++ {
			if _, err := tx.CreateNode(NodeInput{Labels: []string{"Item"}, Properties: domain.Properties{
				"rank": int64(i), "bucket": int64(i % 100),
			}}); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		b.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, "ANALYZE"); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		view, err := store.View(ctx, domain.Snapshot{})
		if err != nil {
			b.Fatal(err)
		}
		nodes, _, err := view.ScanNodes(ctx, NodePredicate{
			AllLabels: []string{"Item"}, Properties: domain.Properties{"rank": int64(7777)},
		}, domain.Page{Limit: 1})
		if err != nil || len(nodes) != 1 {
			b.Fatalf("nodes = %d: %v", len(nodes), err)
		}
	}
}
