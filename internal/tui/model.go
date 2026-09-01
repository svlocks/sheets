package tui

import (
	"context"
	"os"
	"time"

	"charm.land/bubbles/v2/help"
	"charm.land/bubbles/v2/spinner"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"github.com/svlocks/sheets/internal/app"
	"github.com/svlocks/sheets/internal/domain"
)

const defaultPollInterval = 2 * time.Second

type Option func(*Model)

// WithPollInterval changes revision-token polling. A non-positive duration
// disables polling, which is useful for deterministic tests.
func WithPollInterval(interval time.Duration) Option {
	return func(model *Model) { model.pollInterval = interval }
}

// WithNoColor removes semantic color while preserving ASCII borders, cursor
// visibility, labels, and focus prefixes.
func WithNoColor(noColor bool) Option {
	return func(model *Model) { model.noColor = noColor }
}

// WithDarkBackground supplies an initial palette before Bubble Tea reports the
// terminal background.
func WithDarkBackground(dark bool) Option {
	return func(model *Model) { model.dark = dark }
}

type paneFocus uint8

const (
	focusPrimary paneFocus = iota
	focusInspector
)

type noticeLevel uint8

const (
	noticeInfo noticeLevel = iota
	noticeSuccess
	noticeError
)

type noticeState struct {
	text   string
	level  noticeLevel
	serial uint64
}

type executionKind uint8

const (
	executionQuery executionKind = iota + 1
	executionMutation
	executionConsole
)

type pendingOperation struct {
	request app.ExecuteRequest
	kind    executionKind
	purpose formPurpose
}

// Model is the root coordinator. Stateful widgets live in dedicated submodels;
// this type owns routing, async command generations, snapshot mode, and layout.
type Model struct {
	ctx     context.Context
	backend Backend

	width     int
	height    int
	workspace Workspace
	focus     paneFocus
	dark      bool
	noColor   bool
	styles    styleSet
	keys      keyMap

	work          workModel
	relationships relationshipsModel
	query         queryModel
	timeline      timelineModel
	inspector     inspectorModel
	help          help.Model
	helpViewport  viewport.Model
	spinner       spinner.Model

	graph          graphState
	revisions      []domain.RevisionInfo
	snapshot       domain.Snapshot
	loadTarget     domain.Snapshot
	loadedRevision domain.Revision
	liveRevision   domain.Revision

	overlay       pickerOverlay
	overlayReturn overlayKind
	form          *formController
	formSeq       uint64
	pickerSeq     uint64

	pollInterval  time.Duration
	loadSeq       uint64
	historySeq    uint64
	execSeq       uint64
	loadCancel    context.CancelFunc
	historyCancel context.CancelFunc
	execCancel    context.CancelFunc

	loadingGraph    bool
	loadingHistory  bool
	historyOlder    bool
	historyReady    bool
	historyEnd      bool
	historyCursor   string
	historyCapacity int
	executing       bool
	pending         *pendingOperation
	operationErr    error
	graphErr        error
	historyErr      error
	notice          noticeState
	selectAfterLoad domain.EntityID
}

type pickerOverlay struct {
	kind   overlayKind
	picker pickerModel
}

func NewModel(ctx context.Context, backend Backend, options ...Option) *Model {
	if ctx == nil {
		ctx = context.Background()
	}
	_, environmentNoColor := os.LookupEnv("NO_COLOR")
	model := &Model{
		ctx: ctx, backend: backend, width: 100, height: 30,
		workspace: WorkWorkspace, focus: focusPrimary, dark: true,
		noColor: environmentNoColor, keys: defaultKeyMap(), pollInterval: defaultPollInterval,
	}
	for _, option := range options {
		if option != nil {
			option(model)
		}
	}
	model.styles = makeStyles(model.dark, model.noColor)
	model.work = newWorkModel(model.styles, model.dark)
	model.relationships = newRelationshipsModel(model.styles, model.dark)
	model.query = newQueryModel(model.styles, model.dark)
	model.timeline = newTimelineModel(model.styles, model.dark)
	model.inspector = newInspectorModel(model.dark, model.noColor)
	model.help = help.New()
	model.help.Styles = model.styles.helpStyles(model.dark)
	model.helpViewport = viewport.New(viewport.WithWidth(80), viewport.WithHeight(20))
	model.helpViewport.SoftWrap = true
	model.helpViewport.FillHeight = true
	model.spinner = spinner.New(spinner.WithSpinner(spinner.Dot))
	_ = model.layoutComponents()
	return model
}

func Run(ctx context.Context, backend Backend, options ...Option) error {
	if ctx == nil {
		ctx = context.Background()
	}
	runContext, cancel := context.WithCancel(ctx)
	defer cancel()
	model := NewModel(runContext, backend, options...)
	_, err := tea.NewProgram(model, tea.WithContext(runContext)).Run()
	return err
}

func (m *Model) Init() tea.Cmd {
	commands := []tea.Cmd{
		m.startSnapshotLoad(domain.Snapshot{}),
		m.startHistoryLoad(),
		tea.RequestBackgroundColor,
	}
	if m.pollInterval > 0 {
		commands = append(commands, m.pollDelayCmd())
	}
	return tea.Batch(commands...)
}

func (m *Model) View() tea.View {
	view := tea.NewView(m.render())
	view.AltScreen = true
	view.MouseMode = tea.MouseModeCellMotion
	view.ReportFocus = true
	view.WindowTitle = "sheets"
	return view
}

func (m *Model) historical() bool {
	return !m.snapshot.IsCurrent() || m.loadingGraph && !m.loadTarget.IsCurrent()
}

// selectedSnapshot is the user's latest requested state, which can differ
// briefly from the last rendered snapshot while an asynchronous load runs.
// Polling, refresh, and read-only queries must honor that intent instead of
// canceling or bypassing a pending historical selection.
func (m *Model) selectedSnapshot() domain.Snapshot {
	if m.loadingGraph {
		return m.loadTarget
	}
	return m.snapshot
}

func (m *Model) busy() bool {
	return m.loadingGraph || m.loadingHistory || m.executing || m.inspector.loading
}
