package tui

import (
	"context"
	"errors"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/svlocks/sheets/internal/app"
	"github.com/svlocks/sheets/internal/domain"
)

type snapshotLoadedMsg struct {
	serial   uint64
	snapshot domain.Snapshot
	loaded   snapshotLoad
	err      error
}

type historyLoadedMsg struct {
	serial    uint64
	revisions []domain.RevisionInfo
	err       error
}

type pollTickMsg struct{}

type revisionCheckedMsg struct {
	revision domain.Revision
	err      error
}

type executionFinishedMsg struct {
	serial    uint64
	operation pendingOperation
	result    app.BatchResult
	err       error
}

type clearNoticeMsg struct{ serial uint64 }

func (m *Model) startSnapshotLoad(snapshot domain.Snapshot) tea.Cmd {
	if m.loadCancel != nil {
		m.loadCancel()
	}
	ctx, cancel := context.WithCancel(m.ctx)
	m.loadCancel = cancel
	m.loadSeq++
	serial := m.loadSeq
	m.loadTarget = snapshot
	m.loadingGraph = true
	m.graphErr = nil
	backend := m.backend
	load := func() tea.Msg {
		loaded, err := loadSnapshot(ctx, backend, snapshot)
		return snapshotLoadedMsg{serial: serial, snapshot: snapshot, loaded: loaded, err: err}
	}
	return tea.Batch(m.spinner.Tick, load)
}

func (m *Model) startHistoryLoad() tea.Cmd {
	if m.historyCancel != nil {
		m.historyCancel()
	}
	ctx, cancel := context.WithCancel(m.ctx)
	m.historyCancel = cancel
	m.historySeq++
	serial := m.historySeq
	m.loadingHistory = true
	m.historyErr = nil
	backend := m.backend
	load := func() tea.Msg {
		if backend == nil {
			return historyLoadedMsg{serial: serial, err: errors.New("TUI backend is nil")}
		}
		revisions, err := backend.Revisions(ctx)
		return historyLoadedMsg{serial: serial, revisions: revisions, err: err}
	}
	return tea.Batch(m.spinner.Tick, load)
}

func (m *Model) startExecution(operation pendingOperation) tea.Cmd {
	if m.execCancel != nil {
		m.execCancel()
	}
	ctx, cancel := context.WithCancel(m.ctx)
	m.execCancel = cancel
	m.execSeq++
	serial := m.execSeq
	m.executing = true
	m.pending = &operation
	m.operationErr = nil
	backend := m.backend
	return tea.Batch(m.spinner.Tick, func() tea.Msg {
		if backend == nil {
			return executionFinishedMsg{serial: serial, operation: operation, err: errors.New("TUI backend is nil")}
		}
		result, err := backend.Execute(ctx, operation.request)
		return executionFinishedMsg{serial: serial, operation: operation, result: result, err: err}
	})
}

func (m *Model) pollDelayCmd() tea.Cmd {
	if m.pollInterval <= 0 {
		return nil
	}
	return tea.Tick(m.pollInterval, func(time.Time) tea.Msg { return pollTickMsg{} })
}

func (m *Model) checkRevisionCmd() tea.Cmd {
	backend := m.backend
	ctx := m.ctx
	return func() tea.Msg {
		if backend == nil {
			return revisionCheckedMsg{err: errors.New("TUI backend is nil")}
		}
		revision, err := backend.CurrentRevision(ctx)
		return revisionCheckedMsg{revision: revision, err: err}
	}
}

func (m *Model) setNotice(level noticeLevel, text string) tea.Cmd {
	m.notice.serial++
	m.notice.level = level
	m.notice.text = terminalLine(text)
	serial := m.notice.serial
	return tea.Tick(5*time.Second, func(time.Time) tea.Msg { return clearNoticeMsg{serial: serial} })
}

func batchChanged(batch app.BatchResult) bool {
	for _, result := range batch.Results {
		if result.Summary.Changed() {
			return true
		}
	}
	return batch.Revision != nil
}
