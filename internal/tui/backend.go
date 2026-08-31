// Package tui implements sheets's full-screen terminal interface.
package tui

import (
	"context"

	"github.com/svlocks/sheets/internal/app"
	"github.com/svlocks/sheets/internal/domain"
)

// Backend is the deliberately small application boundary needed by the TUI.
// Implementations should return complete snapshots and revision history; the
// TUI never reaches through this interface to storage or SQLite.
type Backend interface {
	app.Executor
	ProjectRoot() string
	CurrentRevision(context.Context) (domain.Revision, error)
	Graph(context.Context, domain.Snapshot) ([]domain.Node, []domain.Edge, error)
	Revisions(context.Context) ([]domain.RevisionInfo, error)
}

// GraphSnapshot is a convenient adapter value for backends that already load
// nodes and edges together.
type GraphSnapshot struct {
	Nodes []domain.Node
	Edges []domain.Edge
}
