// Package tui implements sheets's full-screen terminal workspace.
package tui

import (
	"context"
	"errors"
	"fmt"

	"github.com/svlocks/sheets/internal/app"
	"github.com/svlocks/sheets/internal/domain"
)

// Backend is the application boundary used by the TUI. Graph reads and every
// mutation cross app.Executor as Cypher. CurrentRevision is deliberately the
// cheap invalidation token used by the poller; Revisions is the shared,
// paginated application read service exposed by the process adapter.
type Backend interface {
	app.Executor
	ProjectRoot() string
	CurrentRevision(context.Context) (domain.Revision, error)
	Revisions(context.Context) ([]domain.RevisionInfo, error)
}

const snapshotQuery = `CALL sheets.nodes() YIELD node RETURN node;
CALL sheets.edges() YIELD relationship RETURN relationship`

type snapshotLoad struct {
	graph    graphState
	revision domain.Revision
}

func loadSnapshot(ctx context.Context, backend Backend, snapshot domain.Snapshot) (snapshotLoad, error) {
	if backend == nil {
		return snapshotLoad{}, errors.New("TUI backend is nil")
	}

	revision := domain.Revision(0)
	selector := snapshot
	if snapshot.IsCurrent() {
		var err error
		revision, err = backend.CurrentRevision(ctx)
		if err != nil {
			return snapshotLoad{}, fmt.Errorf("read current revision: %w", err)
		}
		// Pin the snapshot to the token we just observed. This prevents nodes
		// and relationships from coming from different external-process writes.
		selector.Revision = &revision
	} else if snapshot.Revision != nil {
		revision = *snapshot.Revision
	}

	batch, err := backend.Execute(ctx, app.ExecuteRequest{
		Query:    snapshotQuery,
		Snapshot: selector,
		ReadOnly: true,
	})
	if err != nil {
		return snapshotLoad{}, fmt.Errorf("load graph snapshot: %w", err)
	}
	nodes, edges, err := decodeSnapshot(batch)
	if err != nil {
		return snapshotLoad{}, err
	}
	return snapshotLoad{graph: newGraphState(nodes, edges), revision: revision}, nil
}

func decodeSnapshot(batch app.BatchResult) ([]domain.Node, []domain.Edge, error) {
	if len(batch.Results) != 2 {
		return nil, nil, fmt.Errorf("snapshot query returned %d results; want 2", len(batch.Results))
	}

	nodes := make([]domain.Node, 0, len(batch.Results[0].Rows))
	for rowIndex, row := range batch.Results[0].Rows {
		if len(row) != 1 {
			return nil, nil, fmt.Errorf("snapshot node row %d has %d values; want 1", rowIndex, len(row))
		}
		switch value := row[0].(type) {
		case domain.Node:
			nodes = append(nodes, value)
		case *domain.Node:
			if value == nil {
				return nil, nil, fmt.Errorf("snapshot node row %d is null", rowIndex)
			}
			nodes = append(nodes, *value)
		default:
			return nil, nil, fmt.Errorf("snapshot node row %d contains %T; want domain.Node", rowIndex, row[0])
		}
	}

	edges := make([]domain.Edge, 0, len(batch.Results[1].Rows))
	for rowIndex, row := range batch.Results[1].Rows {
		if len(row) != 1 {
			return nil, nil, fmt.Errorf("snapshot relationship row %d has %d values; want 1", rowIndex, len(row))
		}
		switch value := row[0].(type) {
		case domain.Edge:
			edges = append(edges, value)
		case *domain.Edge:
			if value == nil {
				return nil, nil, fmt.Errorf("snapshot relationship row %d is null", rowIndex)
			}
			edges = append(edges, *value)
		default:
			return nil, nil, fmt.Errorf("snapshot relationship row %d contains %T; want domain.Edge", rowIndex, row[0])
		}
	}
	return nodes, edges, nil
}
