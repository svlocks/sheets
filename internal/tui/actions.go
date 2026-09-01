package tui

import (
	"errors"
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/huh/v2"
	"github.com/svlocks/sheets/internal/app"
	"github.com/svlocks/sheets/internal/domain"
)

func (m *Model) setWorkspace(workspace Workspace) tea.Cmd {
	if workspace > TimelineWorkspace {
		return nil
	}
	m.workspace = workspace
	m.focus = focusPrimary
	if m.overlay.kind == overlayForm {
		m.form = nil
	}
	m.overlay.kind = overlayNone
	m.overlayReturn = overlayNone
	if workspace == QueryWorkspace {
		return tea.Batch(m.query.focusCurrent(), m.layoutComponents(), m.refreshInspector())
	}
	return tea.Batch(m.layoutComponents(), m.refreshInspector())
}

// navigateWorkspace is the global, non-text navigation path. A form, error,
// or help overlay remains visible so changing the underlying destination never
// discards entered data or hides a retry decision.
func (m *Model) navigateWorkspace(workspace Workspace) tea.Cmd {
	if workspace > TimelineWorkspace {
		return nil
	}
	switch m.overlay.kind {
	case overlayForm, overlayOperationError, overlayHelp:
		m.workspace = workspace
		m.focus = focusPrimary
		commands := []tea.Cmd{m.layoutComponents(), m.refreshInspector()}
		if workspace == QueryWorkspace {
			commands = append(commands, m.query.focusCurrent())
		}
		return tea.Batch(commands...)
	default:
		return m.setWorkspace(workspace)
	}
}

func (m *Model) openFinder() tea.Cmd {
	m.pickerSeq++
	width, height := m.modalContentDimensions()
	m.overlay = pickerOverlay{
		kind:   overlayFinder,
		picker: newNodePicker(m.pickerSeq, m.graph, m.styles, m.dark, width, height),
	}
	return nil
}

func (m *Model) openCommands() tea.Cmd {
	m.pickerSeq++
	width, height := m.modalContentDimensions()
	m.overlay = pickerOverlay{
		kind:   overlayCommands,
		picker: newCommandPicker(m.pickerSeq, m.styles, m.dark, width, height, m.historical()),
	}
	return nil
}

func (m *Model) openHelp() tea.Cmd {
	if m.overlay.kind != overlayHelp {
		m.overlayReturn = m.overlay.kind
	}
	m.overlay.kind = overlayHelp
	m.refreshHelpContent()
	m.helpViewport.GotoTop()
	return nil
}

func (m *Model) closeHelp() tea.Cmd {
	m.overlay.kind = m.overlayReturn
	m.overlayReturn = overlayNone
	return m.layoutComponents()
}

func (m *Model) openCreateForm() tea.Cmd {
	if !m.requireLive() {
		return nil
	}
	m.formSeq++
	width, height := m.modalContentDimensions()
	parent := domain.EntityID("")
	if m.workspace == WorkWorkspace {
		parent = m.work.selected
	}
	m.form = newCreateNodeForm(m.formSeq, m.graph, parent, width, height, m.dark, m.noColor)
	m.overlay.kind = overlayForm
	return m.form.init()
}

func (m *Model) openEditNodeForm() tea.Cmd {
	if !m.requireLive() {
		return nil
	}
	node, ok := m.graph.nodeByID[m.work.selected]
	if !ok {
		return m.setNotice(noticeError, "Select a node in Work first")
	}
	m.formSeq++
	width, height := m.modalContentDimensions()
	m.form = newEditNodeForm(m.formSeq, node, width, height, m.dark, m.noColor)
	m.overlay.kind = overlayForm
	return m.form.init()
}

func (m *Model) openMoveNodeForm() tea.Cmd {
	if !m.requireLive() {
		return nil
	}
	node, ok := m.graph.nodeByID[m.work.selected]
	if !ok {
		return m.setNotice(noticeError, "Select a node in Work first")
	}
	m.formSeq++
	width, height := m.modalContentDimensions()
	m.form = newMoveNodeForm(m.formSeq, m.graph, node, width, height, m.dark, m.noColor)
	m.overlay.kind = overlayForm
	return m.form.init()
}

func (m *Model) openConnectionForm() tea.Cmd {
	if !m.requireLive() {
		return nil
	}
	if len(m.graph.nodes) == 0 {
		return m.setNotice(noticeError, "Create a node before adding a relationship")
	}
	m.formSeq++
	width, height := m.modalContentDimensions()
	source := domain.EntityID("")
	if m.workspace == WorkWorkspace {
		source = m.work.selected
	}
	m.form = newConnectionForm(m.formSeq, m.graph, source, width, height, m.dark, m.noColor)
	m.overlay.kind = overlayForm
	return m.form.init()
}

func (m *Model) openDeleteNodeForm() tea.Cmd {
	if !m.requireLive() {
		return nil
	}
	node, ok := m.graph.nodeByID[m.work.selected]
	if !ok {
		return m.setNotice(noticeError, "Select a node in Work first")
	}
	m.formSeq++
	width, height := m.modalContentDimensions()
	m.form = newDeleteNodeForm(m.formSeq, m.graph, node, width, height, m.dark, m.noColor)
	m.overlay.kind = overlayForm
	return m.form.init()
}

func (m *Model) openEditRelationshipForm() tea.Cmd {
	if !m.requireLive() {
		return nil
	}
	edge, ok := m.relationships.selectedEdge()
	if !ok {
		return m.setNotice(noticeError, "Select a relationship first")
	}
	m.formSeq++
	width, height := m.modalContentDimensions()
	m.form = newEditRelationshipForm(m.formSeq, edge, width, height, m.dark, m.noColor)
	m.overlay.kind = overlayForm
	return m.form.init()
}

func (m *Model) openDeleteRelationshipForm() tea.Cmd {
	if !m.requireLive() {
		return nil
	}
	edge, ok := m.relationships.selectedEdge()
	if !ok {
		return m.setNotice(noticeError, "Select a relationship first")
	}
	m.formSeq++
	width, height := m.modalContentDimensions()
	m.form = newDeleteRelationshipForm(m.formSeq, m.graph, edge, width, height, m.dark, m.noColor)
	m.overlay.kind = overlayForm
	return m.form.init()
}

func (m *Model) runQuery(readOnly bool) tea.Cmd {
	params, err := decodeParams(m.query.params.Value())
	if err != nil {
		m.query.setResult(app.BatchResult{}, fmt.Errorf("invalid parameters: %w", err))
		return nil
	}
	query := strings.TrimSpace(m.query.cypher.Value())
	if query == "" {
		m.query.setResult(app.BatchResult{}, errors.New("query is empty"))
		return nil
	}
	request := app.ExecuteRequest{
		Query: query, Params: params, Snapshot: m.selectedSnapshot(), ReadOnly: readOnly,
		Actor: "tui", Message: "execute query from TUI",
	}
	if readOnly {
		return m.startExecution(pendingOperation{request: request, kind: executionQuery})
	}
	if !m.requireLive() {
		return nil
	}
	m.formSeq++
	width, height := m.modalContentDimensions()
	m.form = newExecuteQueryForm(m.formSeq, request, width, height, m.dark, m.noColor)
	m.overlay.kind = overlayForm
	return m.form.init()
}

func (m *Model) submitForm(message formSubmittedMsg) tea.Cmd {
	if m.form == nil || message.serial != m.form.serial {
		return nil
	}
	request, err := m.form.request()
	if err != nil {
		if errors.Is(err, errConfirmationDeclined) {
			m.form = nil
			m.overlay.kind = overlayNone
			return m.setNotice(noticeInfo, "Operation cancelled; no graph changes")
		}
		// Cross-field checks run while constructing the parameterized request.
		// Keep the completed Huh form editable instead of showing an execution
		// retry modal with no executable pending request.
		m.form.form.State = huh.StateNormal
		return m.setNotice(noticeError, err.Error())
	}
	purpose := m.form.purpose
	kind := executionMutation
	if purpose == formExecuteQuery {
		kind = executionConsole
	}
	m.form = nil
	m.overlay.kind = overlayNone
	return m.startExecution(pendingOperation{request: request, kind: kind, purpose: purpose})
}

func (m *Model) runCommand(command commandID) tea.Cmd {
	m.overlay.kind = overlayNone
	switch command {
	case commandWork:
		return m.setWorkspace(WorkWorkspace)
	case commandRelationships:
		return m.setWorkspace(RelationshipsWorkspace)
	case commandQuery:
		return m.setWorkspace(QueryWorkspace)
	case commandTimeline:
		return m.setWorkspace(TimelineWorkspace)
	case commandFind:
		return m.openFinder()
	case commandCreate:
		return m.openCreateForm()
	case commandEditNode:
		return m.openEditNodeForm()
	case commandMoveNode:
		return m.openMoveNodeForm()
	case commandConnect:
		return m.openConnectionForm()
	case commandDeleteNode:
		return m.openDeleteNodeForm()
	case commandEditRelationship:
		return m.openEditRelationshipForm()
	case commandDeleteRelationship:
		return m.openDeleteRelationshipForm()
	case commandDetails:
		m.focus = focusInspector
		return nil
	case commandReturnLive:
		return m.returnLive()
	case commandRefresh:
		return tea.Batch(m.startSnapshotLoad(m.selectedSnapshot()), m.startHistoryLoad())
	case commandHelp:
		return m.openHelp()
	default:
		return nil
	}
}

func (m *Model) openRevision(revision domain.Revision) tea.Cmd {
	selector := revision
	return m.startSnapshotLoad(domain.Snapshot{Revision: &selector})
}

func (m *Model) returnLive() tea.Cmd {
	if !m.historical() {
		return m.setNotice(noticeInfo, "Already viewing the live graph")
	}
	return m.startSnapshotLoad(domain.Snapshot{})
}

func (m *Model) requireLive() bool {
	if !m.historical() {
		return true
	}
	m.setNotice(noticeError, "Historical revisions are read-only · press F6 to return to Live")
	return false
}

func (m *Model) refreshInspector() tea.Cmd {
	var command tea.Cmd
	switch m.workspace {
	case WorkWorkspace:
		if node, ok := m.graph.nodeByID[m.work.selected]; ok {
			command = m.inspector.inspectNode(node, m.graph)
			break
		}
		command = m.inspector.clear("Select a work item in the hierarchy.")
	case RelationshipsWorkspace:
		if edge, ok := m.relationships.selectedEdge(); ok {
			command = m.inspector.inspectEdge(edge, m.graph)
			break
		}
		command = m.inspector.clear("No relationship is selected.")
	case TimelineWorkspace:
		if info, ok := m.timeline.selectedInfo(); ok {
			command = m.inspector.inspectRevision(info, m.liveRevision)
			break
		}
		command = m.inspector.clear("No revision is selected.")
	default:
		return nil
	}
	if command == nil {
		return nil
	}
	return tea.Batch(m.spinner.Tick, command)
}
