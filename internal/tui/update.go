package tui

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"
	"github.com/svlocks/sheets/internal/app"
	"github.com/svlocks/sheets/internal/domain"
)

func (m *Model) Update(message tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := message.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		return m, m.layoutComponents()
	case tea.BackgroundColorMsg:
		if !m.noColor && m.dark != msg.IsDark() {
			appearance := m.applyAppearance(msg.IsDark())
			if m.form != nil {
				appearance = tea.Batch(appearance, m.form.update(msg))
			}
			return m, appearance
		}
		if m.form != nil {
			return m, m.form.update(msg)
		}
		return m, nil
	case snapshotLoadedMsg:
		return m, m.applySnapshotLoaded(msg)
	case historyLoadedMsg:
		return m, m.applyHistoryLoaded(msg)
	case revisionCheckedMsg:
		return m, m.applyRevisionChecked(msg)
	case pollTickMsg:
		return m, m.checkRevisionCmd()
	case executionFinishedMsg:
		return m, m.applyExecutionFinished(msg)
	case inspectorRenderedMsg:
		m.inspector.apply(msg)
		return m, nil
	case listFilterMatchesMsg:
		return m, m.applyListFilterMatches(msg)
	case formSubmittedMsg:
		return m, m.submitForm(msg)
	case clearNoticeMsg:
		if msg.serial == m.notice.serial {
			m.notice.text = ""
		}
		return m, nil
	case spinner.TickMsg:
		if m.busy() {
			var command tea.Cmd
			m.spinner, command = m.spinner.Update(msg)
			return m, command
		}
		return m, nil
	case tea.MouseClickMsg:
		return m, m.handleMouseClick(msg)
	case tea.MouseWheelMsg:
		return m, m.handleMouseWheel(msg)
	}
	if paste, ok := message.(tea.PasteMsg); ok {
		message = terminalSafePaste(paste)
	}

	keyMessage, isKey := message.(tea.KeyPressMsg)
	if !isKey {
		return m, m.updateFocusedComponent(message)
	}
	keyMessage = terminalSafeKeyPress(keyMessage)

	if key.Matches(keyMessage, m.keys.Quit) {
		return m, tea.Quit
	}
	// Non-text global bindings are routed before editors, filters, and forms.
	// Printable input therefore has the same meaning in every text field.
	switch {
	case key.Matches(keyMessage, m.keys.Work):
		return m, m.navigateWorkspace(WorkWorkspace)
	case key.Matches(keyMessage, m.keys.Relationships):
		return m, m.navigateWorkspace(RelationshipsWorkspace)
	case key.Matches(keyMessage, m.keys.Query):
		return m, m.navigateWorkspace(QueryWorkspace)
	case key.Matches(keyMessage, m.keys.Timeline):
		return m, m.navigateWorkspace(TimelineWorkspace)
	case key.Matches(keyMessage, m.keys.PreviousTab):
		return m, m.navigateWorkspace((m.workspace + 3) % 4)
	case key.Matches(keyMessage, m.keys.NextTab):
		return m, m.navigateWorkspace((m.workspace + 1) % 4)
	case key.Matches(keyMessage, m.keys.Help):
		if m.overlay.kind == overlayHelp {
			return m, m.closeHelp()
		}
		return m, m.openHelp()
	case key.Matches(keyMessage, m.keys.Refresh):
		return m, tea.Batch(m.startSnapshotLoad(m.selectedSnapshot()), m.startHistoryLoad())
	case key.Matches(keyMessage, m.keys.ReturnLive) && m.historical():
		return m, m.returnLive()
	case key.Matches(keyMessage, m.keys.Palette) && m.overlay.kind == overlayNone:
		return m, m.openCommands()
	}
	if m.overlay.kind != overlayNone {
		return m, m.updateOverlay(keyMessage)
	}

	// Active list filters own ordinary keys, including escape and q.
	if m.workspace == RelationshipsWorkspace && m.relationships.filtering() {
		return m, m.relationships.update(keyMessage)
	}
	if m.workspace == TimelineWorkspace && m.timeline.filtering() {
		return m, m.timeline.update(keyMessage)
	}

	switch m.workspace {
	case WorkWorkspace:
		return m, m.updateWork(keyMessage)
	case RelationshipsWorkspace:
		return m, m.updateRelationships(keyMessage)
	case QueryWorkspace:
		return m, m.updateQuery(keyMessage)
	case TimelineWorkspace:
		return m, m.updateTimeline(keyMessage)
	default:
		return m, nil
	}
}

func (m *Model) updateOverlay(msg tea.KeyPressMsg) tea.Cmd {
	switch m.overlay.kind {
	case overlayFinder, overlayCommands:
		if key.Matches(msg, m.keys.Back) {
			m.overlay.kind = overlayNone
			m.overlay.picker.openWhenReady = false
			return nil
		}
		if key.Matches(msg, m.keys.Open) {
			if m.overlay.picker.filtering() && m.overlay.picker.filteredValue != m.overlay.picker.list.FilterValue() {
				m.overlay.picker.openWhenReady = true
				return nil
			}
			return m.activatePickerSelection()
		}
		return m.overlay.picker.update(msg)
	case overlayHelp:
		if key.Matches(msg, m.keys.Back) || key.Matches(msg, m.keys.Help) {
			return m.closeHelp()
		}
		updated, cmd := m.helpViewport.Update(msg)
		m.helpViewport = updated
		return cmd
	case overlayForm:
		if m.form == nil {
			m.overlay.kind = overlayNone
			return nil
		}
		if key.Matches(msg, m.keys.Back) && m.form.escapeCancels() {
			m.form = nil
			m.overlay.kind = overlayNone
			return m.setNotice(noticeInfo, "Form cancelled")
		}
		previousError := m.form.validationErr
		cmd := m.form.update(msg)
		if m.form.validationErr != "" && m.form.validationErr != previousError {
			return tea.Batch(cmd, m.setNotice(noticeError, m.form.validationErr))
		}
		if previousError != "" && m.form.validationErr == "" && m.notice.text == previousError {
			m.notice.serial++
			m.notice.text = ""
		}
		return cmd
	case overlayOperationError:
		switch msg.String() {
		case "esc":
			m.overlay.kind = overlayNone
			m.pending = nil
			m.operationErr = nil
			return nil
		case "r":
			if m.pending != nil {
				m.overlay.kind = overlayNone
				return m.startExecution(*m.pending)
			}
		}
	}
	return nil
}

func (m *Model) activatePickerSelection() tea.Cmd {
	switch m.overlay.kind {
	case overlayFinder:
		id, ok := m.overlay.picker.selectedNode()
		if !ok {
			return nil
		}
		m.overlay.kind = overlayNone
		m.workspace = WorkWorkspace
		m.focus = focusPrimary
		m.work.selectID(id)
		return tea.Batch(m.layoutComponents(), m.refreshInspector())
	case overlayCommands:
		command, ok := m.overlay.picker.selectedCommand()
		if ok {
			return m.runCommand(command)
		}
	}
	return nil
}

func (m *Model) applyListFilterMatches(msg listFilterMatchesMsg) tea.Cmd {
	switch msg.target {
	case listTargetRelationships:
		if msg.generation != m.relationships.filterSeq || msg.filter != m.relationships.list.FilterValue() {
			return nil
		}
		before := m.relationships.selectedID()
		cmd := m.relationships.update(msg.msg)
		if m.workspace == RelationshipsWorkspace && before != m.relationships.selectedID() {
			return tea.Batch(cmd, m.refreshInspector())
		}
		return cmd
	case listTargetTimeline:
		if msg.generation != m.timeline.filterSeq || msg.filter != m.timeline.list.FilterValue() {
			return nil
		}
		before, _ := m.timeline.selectedRevision()
		cmd := m.timeline.update(msg.msg)
		after, _ := m.timeline.selectedRevision()
		if m.workspace == TimelineWorkspace && before != after {
			return tea.Batch(cmd, m.refreshInspector())
		}
		return cmd
	case listTargetPicker:
		if msg.serial != m.overlay.picker.serial || msg.generation != m.overlay.picker.filterSeq ||
			msg.filter != m.overlay.picker.list.FilterValue() {
			return nil
		}
		cmd := m.overlay.picker.update(msg.msg)
		m.overlay.picker.filteredValue = msg.filter
		if m.overlay.picker.openWhenReady {
			m.overlay.picker.openWhenReady = false
			if m.overlay.kind == overlayFinder || m.overlay.kind == overlayCommands {
				return tea.Batch(cmd, m.activatePickerSelection())
			}
		}
		return cmd
	default:
		return nil
	}
}

// updateFocusedComponent routes component lifecycle messages such as cursor
// blinks and viewport updates. Bubbles widgets are models, so forwarding only
// key presses leaves them partially functional even when ordinary navigation
// appears to work.
func (m *Model) updateFocusedComponent(msg tea.Msg) tea.Cmd {
	switch m.overlay.kind {
	case overlayFinder, overlayCommands:
		return m.overlay.picker.update(msg)
	case overlayForm:
		if m.form != nil {
			return m.form.update(msg)
		}
		return nil
	case overlayHelp:
		updated, cmd := m.helpViewport.Update(msg)
		m.helpViewport = updated
		return cmd
	case overlayOperationError:
		return nil
	}

	if m.focus == focusInspector && m.workspace != QueryWorkspace {
		return m.inspector.update(msg)
	}
	switch m.workspace {
	case WorkWorkspace:
		return m.work.update(msg)
	case RelationshipsWorkspace:
		return m.relationships.update(msg)
	case QueryWorkspace:
		return m.query.update(msg)
	case TimelineWorkspace:
		return m.timeline.update(msg)
	default:
		return nil
	}
}

func (m *Model) updateWork(msg tea.KeyPressMsg) tea.Cmd {
	if key.Matches(msg, m.keys.TogglePane) {
		if m.focus == focusPrimary {
			m.focus = focusInspector
		} else {
			m.focus = focusPrimary
		}
		return nil
	}
	if m.focus == focusInspector {
		if key.Matches(msg, m.keys.Back) {
			m.focus = focusPrimary
			return nil
		}
		return m.inspector.update(msg)
	}
	switch {
	case key.Matches(msg, m.keys.Find):
		return m.openFinder()
	case key.Matches(msg, m.keys.NewNode):
		return m.openCreateForm()
	case key.Matches(msg, m.keys.Edit):
		return m.openEditNodeForm()
	case key.Matches(msg, m.keys.Move):
		return m.openMoveNodeForm()
	case key.Matches(msg, m.keys.Connect):
		return m.openConnectionForm()
	case key.Matches(msg, m.keys.Delete):
		return m.openDeleteNodeForm()
	}
	before := m.work.selected
	cmd := m.work.update(msg)
	if before != m.work.selected {
		return tea.Batch(cmd, m.refreshInspector())
	}
	return cmd
}

func (m *Model) updateRelationships(msg tea.KeyPressMsg) tea.Cmd {
	if key.Matches(msg, m.keys.TogglePane) {
		if m.focus == focusPrimary {
			m.focus = focusInspector
		} else {
			m.focus = focusPrimary
		}
		return nil
	}
	if m.focus == focusInspector {
		if key.Matches(msg, m.keys.Back) {
			m.focus = focusPrimary
			return nil
		}
		return m.inspector.update(msg)
	}
	switch {
	case key.Matches(msg, m.keys.Open):
		m.focus = focusInspector
		return nil
	case key.Matches(msg, m.keys.Edit):
		return m.openEditRelationshipForm()
	case key.Matches(msg, m.keys.Connect):
		return m.openConnectionForm()
	case key.Matches(msg, m.keys.Delete):
		return m.openDeleteRelationshipForm()
	}
	before := m.relationships.selectedID()
	cmd := m.relationships.update(msg)
	if before != m.relationships.selectedID() {
		return tea.Batch(cmd, m.refreshInspector())
	}
	return cmd
}

func (m *Model) updateQuery(msg tea.KeyPressMsg) tea.Cmd {
	switch {
	case key.Matches(msg, m.keys.TogglePane):
		return m.query.cycleFocus(msg.String() == "shift+tab")
	case key.Matches(msg, m.keys.RunQuery):
		return m.runQuery(true)
	case key.Matches(msg, m.keys.ExecQuery):
		return m.runQuery(false)
	case key.Matches(msg, m.keys.PreviousSet) && m.query.focus == queryFocusResults:
		m.query.moveResult(-1)
		return nil
	case key.Matches(msg, m.keys.NextSet) && m.query.focus == queryFocusResults:
		m.query.moveResult(1)
		return nil
	}
	return m.query.update(msg)
}

func (m *Model) updateTimeline(msg tea.KeyPressMsg) tea.Cmd {
	if key.Matches(msg, m.keys.TogglePane) {
		if m.focus == focusPrimary {
			m.focus = focusInspector
		} else {
			m.focus = focusPrimary
		}
		return nil
	}
	if m.focus == focusInspector {
		if key.Matches(msg, m.keys.Back) {
			m.focus = focusPrimary
			return nil
		}
		return m.inspector.update(msg)
	}
	if key.Matches(msg, m.keys.Open) {
		if revision, ok := m.timeline.selectedRevision(); ok {
			return m.openRevision(revision)
		}
		return nil
	}
	if key.Matches(msg, m.keys.LoadOlder) {
		return m.startOlderHistory()
	}
	before, _ := m.timeline.selectedRevision()
	cmd := m.timeline.update(msg)
	after, _ := m.timeline.selectedRevision()
	var older tea.Cmd
	if m.timeline.nearEnd() {
		older = m.startOlderHistory()
	}
	if before != after {
		return tea.Batch(cmd, m.refreshInspector(), older)
	}
	return tea.Batch(cmd, older)
}

func (m *Model) applySnapshotLoaded(msg snapshotLoadedMsg) tea.Cmd {
	if msg.serial != m.loadSeq {
		return nil
	}
	m.loadingGraph = false
	if msg.err != nil {
		if errors.Is(msg.err, context.Canceled) {
			return nil
		}
		m.graphErr = msg.err
		return m.setNotice(noticeError, msg.err.Error())
	}
	m.graphErr = nil
	m.snapshot = msg.snapshot
	m.graph = msg.loaded.graph
	m.loadedRevision = msg.loaded.revision
	if msg.snapshot.IsCurrent() {
		m.liveRevision = msg.loaded.revision
	}
	m.work.setGraph(m.graph)
	if m.selectAfterLoad != "" {
		m.work.selectID(m.selectAfterLoad)
		m.selectAfterLoad = ""
	}
	relationshipCmd := m.relationships.setGraph(m.graph)
	pickerCmd := m.refreshSnapshotPicker()
	var timelineCmd tea.Cmd
	if m.historyReady {
		timelineCmd = m.timeline.setRevisions(m.revisions, m.liveRevision, m.includeInitialRevision())
	}
	if !msg.snapshot.IsCurrent() {
		m.timeline.selectRevision(msg.loaded.revision)
	}
	mode := fmt.Sprintf("Loaded revision %d", msg.loaded.revision)
	if msg.snapshot.IsCurrent() {
		mode += " · Live"
	} else {
		mode += " · read-only"
	}
	return tea.Batch(relationshipCmd, timelineCmd, pickerCmd, m.layoutComponents(), m.refreshInspector(), m.setNotice(noticeSuccess, mode))
}

func (m *Model) refreshSnapshotPicker() tea.Cmd {
	effectiveOverlay := m.overlay.kind
	if effectiveOverlay == overlayHelp {
		effectiveOverlay = m.overlayReturn
	}
	switch effectiveOverlay {
	case overlayFinder:
		m.overlay.picker.replaceItems(nodePickerItems(m.graph))
	case overlayCommands:
		if m.overlay.picker.historical != m.historical() {
			m.overlay.picker.historical = m.historical()
			m.overlay.picker.replaceItems(commandPickerItems(m.historical()))
		}
	default:
		return nil
	}
	if m.overlay.kind != overlayHelp && m.overlay.picker.openWhenReady {
		m.overlay.picker.openWhenReady = false
		return m.activatePickerSelection()
	}
	return nil
}

func (m *Model) applyHistoryLoaded(msg historyLoadedMsg) tea.Cmd {
	if msg.serial != m.historySeq {
		return nil
	}
	m.loadingHistory = false
	if msg.err != nil {
		if errors.Is(msg.err, context.Canceled) {
			m.timeline.setPaging(false, msg.older, m.historyEnd, nil)
			return nil
		}
		m.historyErr = msg.err
		m.historyOlder = msg.older
		m.timeline.setPaging(false, msg.older, m.historyEnd, msg.err)
		return m.setNotice(noticeError, "load timeline: "+msg.err.Error())
	}
	if msg.older && msg.page.Next != "" && msg.page.Next == m.historyCursor {
		m.historyErr = errors.New("revision backend returned a non-advancing cursor")
		m.historyOlder = true
		m.timeline.setPaging(false, true, m.historyEnd, m.historyErr)
		return m.setNotice(noticeError, "load timeline: "+m.historyErr.Error())
	}
	if len(msg.revisions) > timelinePageSize || msg.page.Next != "" && len(msg.revisions) == 0 {
		m.historyErr = errors.New("revision backend returned an invalid page")
		m.historyOlder = msg.older
		m.timeline.setPaging(false, msg.older, m.historyEnd, m.historyErr)
		return m.setNotice(noticeError, "load timeline: "+m.historyErr.Error())
	}
	m.historyErr = nil
	m.historyReady = true
	m.historyOlder = msg.older
	m.historyCursor = msg.page.Next
	m.historyEnd = msg.page.Next == ""
	if m.historyCapacity == 0 {
		m.historyCapacity = timelinePageSize
	}
	if msg.older {
		m.historyCapacity += len(msg.revisions)
	}
	protected := make(map[domain.Revision]struct{}, 2)
	if selected, ok := m.timeline.selectedRevision(); ok {
		protected[selected] = struct{}{}
	}
	if selected := m.selectedSnapshot(); selected.Revision != nil {
		protected[*selected.Revision] = struct{}{}
	}
	m.revisions = mergeRevisionHistory(m.revisions, msg.revisions, m.historyCapacity, protected)
	m.timeline.setPaging(false, msg.older, m.historyEnd, nil)
	cmd := m.timeline.setRevisions(m.revisions, m.liveRevision, m.includeInitialRevision())
	if selected := m.selectedSnapshot(); selected.Revision != nil {
		m.timeline.selectRevision(*selected.Revision)
	}
	if m.workspace == TimelineWorkspace {
		return tea.Batch(cmd, m.refreshInspector())
	}
	return cmd
}

func (m *Model) includeInitialRevision() bool {
	if m.historyEnd {
		return true
	}
	if selected, ok := m.timeline.selectedRevision(); ok && selected == 0 {
		return true
	}
	selected := m.selectedSnapshot()
	return selected.Revision != nil && *selected.Revision == 0
}

func mergeRevisionHistory(current, incoming []domain.RevisionInfo, capacity int, protected map[domain.Revision]struct{}) []domain.RevisionInfo {
	byRevision := make(map[domain.Revision]domain.RevisionInfo, len(current)+len(incoming))
	for _, info := range current {
		if info.Revision != 0 {
			byRevision[info.Revision] = info
		}
	}
	for _, info := range incoming {
		if info.Revision != 0 {
			byRevision[info.Revision] = info
		}
	}
	merged := make([]domain.RevisionInfo, 0, len(byRevision))
	for _, info := range byRevision {
		merged = append(merged, info)
	}
	sort.Slice(merged, func(i, j int) bool { return merged[i].Revision > merged[j].Revision })
	if capacity <= 0 || len(merged) <= capacity {
		return merged
	}
	bounded := append([]domain.RevisionInfo(nil), merged[:capacity]...)
	for _, info := range merged[capacity:] {
		if _, keep := protected[info.Revision]; keep {
			bounded = append(bounded, info)
		}
	}
	return bounded
}

func (m *Model) applyRevisionChecked(msg revisionCheckedMsg) tea.Cmd {
	commands := []tea.Cmd{m.pollDelayCmd()}
	if msg.err != nil {
		if !errors.Is(msg.err, context.Canceled) {
			commands = append(commands, m.setNotice(noticeError, "revision poll: "+msg.err.Error()))
		}
		return tea.Batch(commands...)
	}
	if msg.revision == m.liveRevision {
		return tea.Batch(commands...)
	}
	m.liveRevision = msg.revision
	commands = append(commands, m.startHistoryLoad())
	if m.selectedSnapshot().IsCurrent() {
		commands = append(commands, m.startSnapshotLoad(domain.Snapshot{}))
	} else {
		commands = append(commands, m.setNotice(noticeInfo, fmt.Sprintf("Live advanced to revision %d; historical view unchanged", msg.revision)))
	}
	return tea.Batch(commands...)
}

func (m *Model) applyExecutionFinished(msg executionFinishedMsg) tea.Cmd {
	if msg.serial != m.execSeq {
		return nil
	}
	m.executing = false
	if msg.err != nil {
		if errors.Is(msg.err, context.Canceled) {
			return nil
		}
		m.operationErr = msg.err
		if msg.operation.kind == executionQuery {
			m.query.setResult(msg.result, msg.err)
			m.pending = nil
			m.overlay.kind = overlayNone
			return m.setNotice(noticeError, "query failed")
		}
		m.pending = &msg.operation
		m.overlay.kind = overlayOperationError
		return nil
	}

	m.pending = nil
	m.operationErr = nil
	m.overlay.kind = overlayNone
	changed := batchChanged(msg.result)
	if msg.operation.kind == executionMutation && !changed && mutationMustChange(msg.operation.purpose) {
		m.operationErr = errors.New("the request matched no current graph entity; another process may have changed it")
		m.pending = &msg.operation
		m.overlay.kind = overlayOperationError
		return nil
	}
	if msg.operation.kind == executionMutation && !changed && mutationMustReturnEntity(msg.operation.purpose) && !batchHasRows(msg.result) {
		m.operationErr = errors.New("the request matched no current graph entity; another process may have changed it")
		m.pending = &msg.operation
		m.overlay.kind = overlayOperationError
		return nil
	}
	commands := make([]tea.Cmd, 0, 4)
	if msg.operation.kind == executionQuery || msg.operation.kind == executionConsole {
		m.query.setResult(msg.result, nil)
		text := "Query completed"
		if msg.operation.kind == executionConsole {
			text = "Write-capable Cypher completed"
		}
		if !changed {
			text += " · no graph changes"
		}
		commands = append(commands, m.setNotice(noticeSuccess, text))
	} else {
		text := msg.operation.purpose.String() + " completed"
		if !changed {
			text += " · no graph changes"
		}
		commands = append(commands, m.setNotice(noticeSuccess, text))
		if msg.operation.purpose == formCreateNode || msg.operation.purpose == formEditNode || msg.operation.purpose == formMoveNode {
			m.selectAfterLoad = returnedNodeID(msg.result)
		}
	}
	if msg.result.Revision != nil {
		m.liveRevision = *msg.result.Revision
	}
	if changed {
		commands = append(commands, m.startSnapshotLoad(domain.Snapshot{}), m.startHistoryLoad())
	}
	return tea.Batch(commands...)
}

func mutationMustChange(purpose formPurpose) bool {
	switch purpose {
	case formCreateNode, formConnectNodes, formDeleteNode, formDeleteRelationship:
		return true
	default:
		return false
	}
}

func mutationMustReturnEntity(purpose formPurpose) bool {
	return purpose == formEditNode || purpose == formMoveNode || purpose == formEditRelationship
}

func batchHasRows(batch app.BatchResult) bool {
	for _, result := range batch.Results {
		if len(result.Rows) > 0 {
			return true
		}
	}
	return false
}

func returnedNodeID(batch app.BatchResult) domain.EntityID {
	for _, result := range batch.Results {
		for _, row := range result.Rows {
			for _, value := range row {
				switch node := value.(type) {
				case domain.Node:
					return node.ID
				case *domain.Node:
					if node != nil {
						return node.ID
					}
				}
			}
		}
	}
	return ""
}

func (m *Model) applyAppearance(dark bool) tea.Cmd {
	m.dark = dark
	m.styles = makeStyles(dark, m.noColor)
	m.work.setStyle(m.styles, dark)
	m.relationships.setStyle(m.styles, dark)
	m.query.setStyle(m.styles, dark)
	m.timeline.setStyle(m.styles, dark)
	m.help.Styles = m.styles.helpStyles(dark)
	return m.inspector.setAppearance(dark, m.noColor)
}

func (m *Model) handleMouseClick(msg tea.MouseClickMsg) tea.Cmd {
	if m.overlay.kind != overlayNone || msg.Button != tea.MouseLeft {
		return nil
	}
	if msg.Y == 0 {
		if workspace, ok := m.workspaceAt(msg.X); ok {
			return m.setWorkspace(workspace)
		}
	}
	if m.workspace == WorkWorkspace && m.focus == focusPrimary {
		if row, ok := m.workRowAt(msg.X, msg.Y); ok {
			before := m.work.selected
			m.work.clickRow(row)
			if before != m.work.selected {
				return m.refreshInspector()
			}
		}
	}
	return nil
}

func (m *Model) handleMouseWheel(msg tea.MouseWheelMsg) tea.Cmd {
	if m.overlay.kind == overlayHelp {
		updated, cmd := m.helpViewport.Update(msg)
		m.helpViewport = updated
		return cmd
	}
	if m.overlay.kind != overlayNone {
		return nil
	}
	delta := 3
	if msg.Button == tea.MouseWheelUp {
		delta = -3
	}
	if m.focus == focusInspector && m.workspace != QueryWorkspace {
		return m.inspector.update(msg)
	}
	switch m.workspace {
	case WorkWorkspace:
		before := m.work.selected
		m.work.move(delta)
		if before != m.work.selected {
			return m.refreshInspector()
		}
	case RelationshipsWorkspace:
		before := m.relationships.selectedID()
		m.relationships.wheel(delta)
		if before != m.relationships.selectedID() {
			return m.refreshInspector()
		}
	case TimelineWorkspace:
		before, _ := m.timeline.selectedRevision()
		m.timeline.wheel(delta)
		after, _ := m.timeline.selectedRevision()
		var older tea.Cmd
		if m.timeline.nearEnd() {
			older = m.startOlderHistory()
		}
		if before != after {
			return tea.Batch(m.refreshInspector(), older)
		}
		return older
	case QueryWorkspace:
		return m.query.update(msg)
	}
	return nil
}
