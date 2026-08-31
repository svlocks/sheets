package tui

import (
	"context"
	"time"

	"charm.land/bubbles/v2/textarea"
	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"github.com/svlocks/sheets/internal/app"
	"github.com/svlocks/sheets/internal/domain"
)

// Tab identifies one of the four primary workspaces.
type Tab uint8

const (
	OutlineTab Tab = iota
	GraphTab
	QueryTab
	HistoryTab
)

var tabNames = [...]string{"Outline", "Graph", "Query", "History"}

func (t Tab) String() string {
	if int(t) < len(tabNames) {
		return tabNames[t]
	}
	return "Unknown"
}

type overlayKind uint8

const (
	overlayNone overlayKind = iota
	overlaySearch
	overlayPalette
	overlayHelp
	overlayInspector
	overlayMutation
)

type mutationKind uint8

const (
	mutationCreate mutationKind = iota + 1
	mutationEdit
	mutationReparent
	mutationDelete
)

type focusArea uint8

const (
	focusQuery focusArea = iota
	focusParams
	focusResults
)

type outlineRow struct {
	ID       domain.EntityID
	Depth    int
	Orphan   bool
	Cycle    bool
	HasKids  bool
	Section  string
	Position *int64
}

type graphRow struct {
	ID    domain.EntityID
	Lines []string
}

type hitKind uint8

const (
	hitTab hitKind = iota + 1
	hitNode
	hitHistory
	hitOverlay
)

type hitArea struct {
	Kind       hitKind
	X0, X1     int
	Y0, Y1     int
	Tab        Tab
	Node       domain.EntityID
	Revision   domain.Revision
	OverlayRow int
}

// Option configures a model. The defaults are appropriate for an interactive
// program; tests can turn polling off for deterministic command behavior.
type Option func(*Model)

// WithPollInterval controls revision-token polling. A non-positive duration
// disables polling.
func WithPollInterval(interval time.Duration) Option {
	return func(m *Model) { m.pollInterval = interval }
}

// WithNoColor disables all semantic colors while preserving spacing, borders,
// selection markers, and emphasis.
func WithNoColor(noColor bool) Option {
	return func(m *Model) {
		m.noColor = noColor
		m.styles = makeStyles(m.dark, noColor)
	}
}

// WithDarkBackground chooses the initial adaptive palette. Interactive runs
// refine this after Bubble Tea reports the terminal background.
func WithDarkBackground(dark bool) Option {
	return func(m *Model) {
		m.dark = dark
		m.styles = makeStyles(dark, m.noColor)
	}
}

// Model is a Bubble Tea model and is intentionally usable directly in tests.
// Update and View do not require a real terminal.
type Model struct {
	backend Backend
	ctx     context.Context

	width, height int
	tab           Tab
	styles        styles
	dark          bool
	noColor       bool

	nodes      []domain.Node
	edges      []domain.Edge
	nodeByID   map[domain.EntityID]domain.Node
	history    []domain.RevisionInfo
	liveRev    domain.Revision
	snapshot   domain.Snapshot
	loading    bool
	loadSerial uint64
	err        error
	status     string

	selected        domain.EntityID
	outlineRows     []outlineRow
	outlineIndex    int
	outlineOffset   int
	collapsed       map[domain.EntityID]bool
	graphRows       []graphRow
	graphIndex      int
	graphPanX       int
	graphPanY       int
	graphZoom       int
	inspectorScroll int

	query      textarea.Model
	params     textarea.Model
	queryFocus focusArea
	result     *app.BatchResult
	queryErr   error
	resultY    int
	resultX    int
	executing  bool
	execSerial uint64

	historyIndex  int
	historyOffset int

	overlay       overlayKind
	search        textinput.Model
	palette       textinput.Model
	paletteIndex  int
	mutation      mutationKind
	mutationInput textarea.Model
	mutationErr   error

	showInspector bool
	pollInterval  time.Duration
	hits          []hitArea
	hitOffsetX    int
}

// NewModel constructs a testable TUI model without starting terminal I/O.
func NewModel(ctx context.Context, backend Backend, options ...Option) *Model {
	if ctx == nil {
		ctx = context.Background()
	}
	query := textarea.New()
	query.Placeholder = "MATCH (n) RETURN n"
	query.ShowLineNumbers = true
	query.SetHeight(7)
	query.SetWidth(72)
	query.SetValue("MATCH (n) RETURN n")
	_ = query.Focus()

	params := textarea.New()
	params.Placeholder = "{}"
	params.ShowLineNumbers = false
	params.SetHeight(4)
	params.SetWidth(72)
	params.SetValue("{}")

	search := textinput.New()
	search.Prompt = "/ "
	search.Placeholder = "title, label, property, ID…"
	search.SetWidth(48)

	palette := textinput.New()
	palette.Prompt = "> "
	palette.Placeholder = "Type a command…"
	palette.SetWidth(52)

	mutation := textarea.New()
	mutation.ShowLineNumbers = false
	mutation.SetHeight(14)
	mutation.SetWidth(70)

	m := &Model{
		backend:       backend,
		ctx:           ctx,
		width:         100,
		height:        30,
		dark:          true,
		styles:        makeStyles(true, false),
		nodeByID:      make(map[domain.EntityID]domain.Node),
		collapsed:     make(map[domain.EntityID]bool),
		graphZoom:     2,
		query:         query,
		params:        params,
		queryFocus:    focusQuery,
		search:        search,
		palette:       palette,
		mutationInput: mutation,
		showInspector: true,
		pollInterval:  2 * time.Second,
	}
	for _, option := range options {
		if option != nil {
			option(m)
		}
	}
	return m
}

// Run starts the full-screen terminal program and honors caller cancellation.
func Run(ctx context.Context, backend Backend, options ...Option) error {
	m := NewModel(ctx, backend, options...)
	_, err := tea.NewProgram(m, tea.WithContext(m.ctx)).Run()
	return err
}

// Init starts the initial snapshot/history loads and requests terminal color
// information. Polling uses only CurrentRevision as an invalidation token.
func (m *Model) Init() tea.Cmd {
	cmds := []tea.Cmd{m.loadGraphCmd(domain.Snapshot{}), m.loadHistoryCmd(), tea.RequestBackgroundColor}
	if m.pollInterval > 0 {
		cmds = append(cmds, m.pollCmd())
	}
	return tea.Batch(cmds...)
}
