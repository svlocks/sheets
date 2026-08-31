package store

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/svlocks/sheets/internal/domain"
)

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
