package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/svlocks/sheets/internal/domain"
)

func TestCRUDHistoryAndReopen(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "graph.db")
	store := openTestStore(t, path)

	t1 := time.Date(2025, 1, 2, 3, 4, 5, 6, time.FixedZone("test", -6*60*60))
	when := time.Date(2024, 5, 6, 7, 8, 9, 10, time.FixedZone("property", 90*60))
	properties := domain.Properties{
		"nil": nil, "bool": true, "string": "text", "integer": int64(-42),
		"float": 3.25, "bytes": []byte{0, 1, 255}, "time": when,
		"duration": 5*time.Minute + 3, "list": []any{int64(1), "two", false},
		"map": map[string]any{"nested": int64(9)},
	}
	var first, second domain.Node
	var edge domain.Edge
	result, err := store.Write(ctx, RevisionMeta{Time: t1, Actor: "test", Message: "create graph"}, func(tx *WriteTx) error {
		var err error
		first, err = tx.CreateNode(NodeInput{Labels: []string{"Task", "Task", "Pinned"}, Properties: properties, Body: "# body"})
		if err != nil {
			return err
		}
		second, err = tx.CreateNode(NodeInput{Labels: []string{"Task"}})
		if err != nil {
			return err
		}
		position := int64(3)
		edge, err = tx.CreateEdge(EdgeInput{From: first.ID, Type: "CHILD", To: second.ID, Position: &position, Properties: domain.Properties{"kind": "ordered"}})
		if err != nil {
			return err
		}
		nodes, err := tx.ListNodes()
		if err != nil || len(nodes) != 2 {
			return fmt.Errorf("transaction nodes = %d: %w", len(nodes), err)
		}
		edges, err := tx.ListEdges()
		if err != nil || len(edges) != 1 {
			return fmt.Errorf("transaction edges = %d: %w", len(edges), err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Changed || result.Revision != 1 || result.Info == nil || result.Info.Actor != "test" {
		t.Fatalf("unexpected write result: %#v", result)
	}
	if first.ValidFrom != 1 || second.ValidFrom != 1 || edge.ValidFrom != 1 {
		t.Fatalf("one batch did not share a revision: %#v %#v %#v", first, second, edge)
	}
	if first.ID == "" || len(first.ID) != 36 || first.ID[14] != '7' {
		t.Fatalf("generated ID is not UUIDv7-shaped: %q", first.ID)
	}

	got, err := store.GetNode(ctx, first.ID, domain.Snapshot{})
	if err != nil {
		t.Fatal(err)
	}
	wantEncoded, encodeErr := encodeProperties(properties)
	gotEncoded, gotEncodeErr := encodeProperties(got.Properties)
	if got.Body != "# body" || !reflect.DeepEqual(got.Labels, []string{"Pinned", "Task"}) || encodeErr != nil || gotEncodeErr != nil || !reflect.DeepEqual(gotEncoded, wantEncoded) {
		t.Fatalf("node did not round-trip: %#v", got)
	}
	gotTime := got.Properties["time"].(time.Time)
	if !gotTime.Equal(when) || gotTime.Location().String() != when.Location().String() {
		t.Fatalf("time did not round-trip: got %v (%s), want %v (%s)", gotTime, gotTime.Location(), when, when.Location())
	}

	t2 := t1.Add(time.Second)
	newBody := "updated"
	result, err = store.Write(ctx, RevisionMeta{Time: t2, Message: "twice in one batch"}, func(tx *WriteTx) error {
		if _, err := tx.UpdateNode(first.ID, NodeUpdate{Body: &newBody}); err != nil {
			return err
		}
		finalBody := "updated again"
		updated, err := tx.UpdateNode(first.ID, NodeUpdate{Body: &finalBody})
		if err != nil {
			return err
		}
		if updated.ValidFrom != 2 || updated.Body != finalBody {
			return fmt.Errorf("unexpected in-batch update: %#v", updated)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Revision != 2 {
		t.Fatalf("update revision = %d", result.Revision)
	}
	rev1 := domain.Revision(1)
	historical, err := store.GetNode(ctx, first.ID, domain.Snapshot{Revision: &rev1})
	if err != nil || historical.Body != "# body" || historical.ValidTo == nil || *historical.ValidTo != 2 {
		t.Fatalf("unexpected historical node %#v, %v", historical, err)
	}
	resolved, err := store.ResolveSnapshot(ctx, domain.Snapshot{Time: ptrTime(t1.Add(500 * time.Millisecond))})
	if err != nil || resolved != 1 {
		t.Fatalf("time resolved to %d: %v", resolved, err)
	}

	result, err = store.Write(ctx, RevisionMeta{Time: t2.Add(time.Second), Message: "delete"}, func(tx *WriteTx) error {
		return tx.DeleteNode(first.ID)
	})
	if err != nil || result.Revision != 3 {
		t.Fatalf("delete = %#v, %v", result, err)
	}
	if _, err := store.GetNode(ctx, first.ID, domain.Snapshot{}); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("current deleted node error = %v", err)
	}
	if _, err := store.GetEdge(ctx, edge.ID, domain.Snapshot{}); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("incident edge remained current: %v", err)
	}
	if _, err := store.GetEdge(ctx, edge.ID, domain.Snapshot{Revision: &rev1}); err != nil {
		t.Fatalf("historical incident edge unavailable: %v", err)
	}
	history, _, err := store.ListRevisions(ctx, domain.Page{})
	if err != nil || len(history) != 3 || history[1].Message != "twice in one batch" {
		t.Fatalf("history = %#v, %v", history, err)
	}

	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store = openTestStore(t, path)
	t.Cleanup(func() { _ = store.Close() })
	current, err := store.CurrentRevision(ctx)
	if err != nil || current != 3 {
		t.Fatalf("reopened revision = %d: %v", current, err)
	}
	if got, err := store.GetNode(ctx, second.ID, domain.Snapshot{}); err != nil || got.ID != second.ID {
		t.Fatalf("reopened node = %#v: %v", got, err)
	}
}

func TestWriteRollbackNoopAndUseAfterCallback(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t, filepath.Join(t.TempDir(), "graph.db"))
	t.Cleanup(func() { _ = store.Close() })
	sentinel := errors.New("stop")
	var rolledBack domain.EntityID
	_, err := store.Write(ctx, RevisionMeta{}, func(tx *WriteTx) error {
		node, err := tx.CreateNode(NodeInput{Body: "not committed"})
		rolledBack = node.ID
		if err != nil {
			return err
		}
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("rollback error = %v", err)
	}
	if current, _ := store.CurrentRevision(ctx); current != 0 {
		t.Fatalf("rollback consumed revision %d", current)
	}
	if _, err := store.GetNode(ctx, rolledBack, domain.Snapshot{}); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("rolled-back node exists: %v", err)
	}

	var held *WriteTx
	result, err := store.Write(ctx, RevisionMeta{}, func(tx *WriteTx) error {
		held = tx
		return nil
	})
	if err != nil || result.Changed || result.Revision != 0 {
		t.Fatalf("read-only write = %#v, %v", result, err)
	}
	if _, err := held.ListNodes(); err == nil {
		t.Fatal("WriteTx remained usable after callback")
	}

	var id domain.EntityID
	_, err = store.Write(ctx, RevisionMeta{}, func(tx *WriteTx) error {
		node, err := tx.CreateNode(NodeInput{Body: "same"})
		id = node.ID
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	same := "same"
	result, err = store.Write(ctx, RevisionMeta{}, func(tx *WriteTx) error {
		_, err := tx.UpdateNode(id, NodeUpdate{Body: &same})
		return err
	})
	if err != nil || result.Changed || result.Revision != 1 {
		t.Fatalf("idempotent update created a revision: %#v, %v", result, err)
	}
}

func TestUpdateThenDeletePreservesHistory(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t, filepath.Join(t.TempDir(), "graph.db"))
	t.Cleanup(func() { _ = store.Close() })
	var parent, child domain.Node
	var edge domain.Edge
	_, err := store.Write(ctx, RevisionMeta{}, func(tx *WriteTx) error {
		var err error
		parent, err = tx.CreateNode(NodeInput{Body: "original"})
		if err != nil {
			return err
		}
		child, err = tx.CreateNode(NodeInput{})
		if err != nil {
			return err
		}
		edge, err = tx.CreateEdge(EdgeInput{From: parent.ID, Type: "CHILD", To: child.ID})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.Write(ctx, RevisionMeta{}, func(tx *WriteTx) error {
		body := "temporary"
		if _, err := tx.UpdateNode(parent.ID, NodeUpdate{Body: &body}); err != nil {
			return err
		}
		properties := domain.Properties{"temporary": true}
		if _, err := tx.UpdateEdge(edge.ID, EdgeUpdate{Properties: &properties}); err != nil {
			return err
		}
		return tx.DeleteNode(parent.ID)
	})
	if err != nil {
		t.Fatal(err)
	}
	rev1 := domain.Revision(1)
	if got, err := store.GetNode(ctx, parent.ID, domain.Snapshot{Revision: &rev1}); err != nil || got.Body != "original" {
		t.Fatalf("node history lost after update+delete: %#v, %v", got, err)
	}
	if got, err := store.GetEdge(ctx, edge.ID, domain.Snapshot{Revision: &rev1}); err != nil || got.ID != edge.ID {
		t.Fatalf("edge history lost after update+delete: %#v, %v", got, err)
	}
}

func TestChildConstraints(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t, filepath.Join(t.TempDir(), "graph.db"))
	t.Cleanup(func() { _ = store.Close() })
	ids := make([]domain.EntityID, 4)
	_, err := store.Write(ctx, RevisionMeta{}, func(tx *WriteTx) error {
		for i := range ids {
			node, err := tx.CreateNode(NodeInput{Body: fmt.Sprintf("n%d", i)})
			if err != nil {
				return err
			}
			ids[i] = node.ID
		}
		if _, err := tx.CreateEdge(EdgeInput{From: ids[0], Type: "CHILD", To: ids[1]}); err != nil {
			return err
		}
		_, err := tx.CreateEdge(EdgeInput{From: ids[1], Type: "CHILD", To: ids[2]})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name       string
		input      EdgeInput
		constraint string
	}{
		{"second parent", EdgeInput{From: ids[3], Type: "CHILD", To: ids[2]}, "child_parent"},
		{"cycle", EdgeInput{From: ids[2], Type: "CHILD", To: ids[0]}, "child_cycle"},
		{"missing endpoint", EdgeInput{From: ids[0], Type: "LINK", To: "missing"}, "edge_endpoint"},
		{"generic position", EdgeInput{From: ids[0], Type: "LINK", To: ids[3], Position: ptrInt64(1)}, "edge_position"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			before, _ := store.CurrentRevision(ctx)
			_, err := store.Write(ctx, RevisionMeta{}, func(tx *WriteTx) error {
				_, err := tx.CreateEdge(tt.input)
				return err
			})
			var constraint *domain.ConstraintError
			if !errors.As(err, &constraint) || constraint.Constraint != tt.constraint {
				t.Fatalf("error = %v", err)
			}
			after, _ := store.CurrentRevision(ctx)
			if after != before {
				t.Fatalf("failed constraint consumed revision: %d -> %d", before, after)
			}
		})
	}
}

func TestConcurrentStoresReadersAndWriters(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "graph.db")
	first := openTestStore(t, path)
	second := openTestStore(t, path)
	t.Cleanup(func() { _ = first.Close(); _ = second.Close() })

	const writers = 24
	start := make(chan struct{})
	errorsCh := make(chan error, writers+1)
	var wg sync.WaitGroup
	var stop atomic.Bool
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-start
		for !stop.Load() {
			_, _, err := first.ListNodes(ctx, domain.Snapshot{}, NodeFilter{}, domain.Page{Limit: 100})
			if err != nil {
				errorsCh <- err
				return
			}
		}
	}()
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			target := first
			if i%2 == 1 {
				target = second
			}
			_, err := target.Write(ctx, RevisionMeta{Actor: fmt.Sprintf("writer-%d", i)}, func(tx *WriteTx) error {
				_, err := tx.CreateNode(NodeInput{Body: fmt.Sprintf("node-%d", i)})
				return err
			})
			if err != nil {
				errorsCh <- err
			}
		}(i)
	}
	close(start)
	for {
		current, err := first.CurrentRevision(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if current == writers {
			break
		}
		time.Sleep(time.Millisecond)
	}
	stop.Store(true)
	wg.Wait()
	close(errorsCh)
	for err := range errorsCh {
		t.Errorf("concurrent operation: %v", err)
	}
	nodes, _, err := second.ListNodes(ctx, domain.Snapshot{}, NodeFilter{}, domain.Page{Limit: writers})
	if err != nil || len(nodes) != writers {
		t.Fatalf("nodes = %d: %v", len(nodes), err)
	}
}

func TestOpenInvalidDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bad.db")
	if err := os.WriteFile(path, []byte("this is not sqlite"), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := Open(context.Background(), path)
	if err == nil {
		_ = store.Close()
		t.Fatal("Open accepted an invalid SQLite file")
	}
}

func openTestStore(t testing.TB, path string) *Store {
	t.Helper()
	store, err := Open(context.Background(), path, WithBusyTimeout(10*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func ptrTime(value time.Time) *time.Time { return &value }

func ptrInt64(value int64) *int64 { return &value }
