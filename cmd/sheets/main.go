package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sync"
	"sync/atomic"
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
	os.Exit(run())
}

func run() int {
	ignoreBrokenPipeSignal()
	ctx, state, stop := processSignalContext(os.Stdin)
	defer stop()

	version, _, _ := buildinfo.Info()
	command := cli.New(cli.Options{TUI: runTUI})
	err := fang.Execute(ctx, command,
		fang.WithVersion(version),
		fang.WithErrorHandler(func(w io.Writer, styles fang.Styles, err error) {
			if !errors.Is(err, syscall.EPIPE) && state.exitCode.Load() == 0 {
				fang.DefaultErrorHandler(w, styles, err)
			}
		}),
	)
	return commandExitCode(err, state.exitCode.Load())
}

func commandExitCode(err error, signalCode int32) int {
	if signalCode != 0 {
		return int(signalCode)
	}
	if err == nil || errors.Is(err, syscall.EPIPE) {
		return 0
	}
	return 1
}

type processSignalState struct {
	exitCode atomic.Int32
}

// processSignalContext turns termination signals into command cancellation.
// Closing stdin releases the process descriptor on termination; the CLI's
// context-aware input path also ensures the command does not wait for a
// producer that never closes its pipe.
func processSignalContext(stdin io.Closer) (context.Context, *processSignalState, func()) {
	ctx, cancel := context.WithCancel(context.Background())
	state := &processSignalState{}
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	done := make(chan struct{})
	var stopOnce sync.Once
	stop := func() {
		stopOnce.Do(func() {
			signal.Stop(signals)
			close(done)
			cancel()
		})
	}
	go func() {
		select {
		case received := <-signals:
			state.exitCode.Store(int32(signalExitCode(received)))
			// Restore the platform's default handling so a second signal can
			// still force termination if graceful cancellation becomes stuck.
			signal.Stop(signals)
			cancel()
			if stdin != nil {
				_ = stdin.Close()
			}
		case <-done:
		}
	}()
	return ctx, state, stop
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

func (b tuiBackend) ListRevisionPage(ctx context.Context, page domain.RevisionPage) ([]domain.RevisionInfo, domain.PageInfo, error) {
	return b.engine.ListRevisionPage(ctx, page)
}

func runTUI(ctx context.Context, found project.Project, executor *engine.Engine, options cli.TUIOptions) error {
	backend := tuiBackend{root: found.Root, engine: executor}
	if err := tui.Run(ctx, backend, tui.WithNoColor(options.NoColor)); err != nil {
		return fmt.Errorf("run terminal UI: %w", err)
	}
	return nil
}
