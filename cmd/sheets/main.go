package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/charmbracelet/fang"
	"github.com/svlocks/sheets/internal/app"
	"github.com/svlocks/sheets/internal/buildinfo"
	"github.com/svlocks/sheets/internal/cli"
	"github.com/svlocks/sheets/internal/domain"
	"github.com/svlocks/sheets/internal/engine"
	"github.com/svlocks/sheets/internal/project"
	"github.com/svlocks/sheets/internal/tui"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	version, _, _ := buildinfo.Info()
	command := cli.New(cli.Options{TUI: runTUI})
	if err := fang.Execute(ctx, command, fang.WithVersion(version)); err != nil {
		os.Exit(1)
	}
}

type tuiBackend struct {
	root   string
	engine *engine.Engine
}

func (b tuiBackend) ProjectRoot() string { return b.root }

func (b tuiBackend) Execute(ctx context.Context, request app.ExecuteRequest) (app.BatchResult, error) {
	return b.engine.Execute(ctx, request)
}

func (b tuiBackend) CurrentRevision(ctx context.Context) (domain.Revision, error) {
	return b.engine.CurrentRevision(ctx)
}

func (b tuiBackend) Graph(ctx context.Context, snapshot domain.Snapshot) ([]domain.Node, []domain.Edge, error) {
	graph, err := b.engine.Snapshot(ctx, snapshot)
	if err != nil {
		return nil, nil, err
	}
	return graph.Nodes, graph.Edges, nil
}

func (b tuiBackend) Revisions(ctx context.Context) ([]domain.RevisionInfo, error) {
	var revisions []domain.RevisionInfo
	page := domain.Page{Limit: 1000}
	for {
		values, info, err := b.engine.ListRevisions(ctx, page)
		if err != nil {
			return nil, err
		}
		revisions = append(revisions, values...)
		if info.Next == "" {
			return revisions, nil
		}
		page.After = info.Next
	}
}

func runTUI(ctx context.Context, found project.Project, executor *engine.Engine, options cli.TUIOptions) error {
	backend := tuiBackend{root: found.Root, engine: executor}
	if err := tui.Run(ctx, backend, tui.WithNoColor(options.NoColor)); err != nil {
		return fmt.Errorf("run terminal UI: %w", err)
	}
	return nil
}
