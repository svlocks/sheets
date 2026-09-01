package tui

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"charm.land/bubbles/v2/table"
	"charm.land/bubbles/v2/textarea"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/svlocks/sheets/internal/app"
	"github.com/svlocks/sheets/internal/domain/temporal"
)

type queryFocus uint8

const (
	queryFocusCypher queryFocus = iota
	queryFocusParams
	queryFocusResults
	queryFocusRow
)

type queryModel struct {
	cypher        textarea.Model
	params        textarea.Model
	table         table.Model
	tableViewport viewport.Model
	rowViewport   viewport.Model
	focus         queryFocus
	result        *app.BatchResult
	resultIndex   int
	err           error
	width         int
	height        int
	singleSection bool
	dark          bool
	styles        styleSet
}

func newQueryModel(styles styleSet, dark bool) queryModel {
	cypher := textarea.New()
	cypher.Placeholder = "MATCH (n) RETURN n LIMIT 50"
	cypher.ShowLineNumbers = true
	cypher.SetValue("MATCH (n)\nRETURN n\nLIMIT 50")
	cypher.SetWidth(72)
	cypher.SetHeight(7)
	cypher.SetStyles(styles.textareaStyles(dark))

	params := textarea.New()
	params.Placeholder = "{}"
	params.ShowLineNumbers = false
	params.SetValue("{}")
	params.SetWidth(72)
	params.SetHeight(4)
	params.SetStyles(styles.textareaStyles(dark))
	params.Blur()

	tbl := table.New(table.WithFocused(false), table.WithHeight(10), table.WithWidth(72), table.WithStyles(styles.tableStyles()))
	tableView := viewport.New(viewport.WithWidth(72), viewport.WithHeight(10))
	tableView.SoftWrap = false
	tableView.FillHeight = true
	tableView.SetHorizontalStep(8)
	rowView := viewport.New(viewport.WithWidth(40), viewport.WithHeight(10))
	rowView.SoftWrap = true
	rowView.FillHeight = true

	model := queryModel{
		cypher: cypher, params: params, table: tbl, tableViewport: tableView, rowViewport: rowView,
		focus: queryFocusCypher, width: 100, height: 24, dark: dark, styles: styles,
	}
	model.focusCurrent()
	model.setSize(100, 24)
	return model
}

func (m *queryModel) setStyle(styles styleSet, dark bool) {
	m.styles = styles
	m.dark = dark
	m.cypher.SetStyles(styles.textareaStyles(dark))
	m.params.SetStyles(styles.textareaStyles(dark))
	m.table.SetStyles(styles.tableStyles())
}

func (m *queryModel) setSize(width, height int) {
	m.width = clampSize(width, 1)
	m.height = clampSize(height, 1)

	editorHeight := 7
	paramsHeight := 4
	if m.height < 20 {
		editorHeight = 4
		paramsHeight = 3
	}

	editorsHorizontal := m.width >= 90
	resultsHorizontal := m.width >= 100
	editorBlockHeight := 2 + editorHeight + paramsHeight
	if editorsHorizontal {
		editorBlockHeight = 1 + max(editorHeight, paramsHeight)
	}
	minimumResultsHeight := 5 // two titles, two rows, and the vertical gap
	if resultsHorizontal {
		minimumResultsHeight = 2 // one shared row of titles and one row of content
	}
	m.singleSection = m.height < editorBlockHeight+1+minimumResultsHeight
	if m.singleSection || m.width < 90 {
		m.cypher.SetWidth(m.width)
		m.params.SetWidth(m.width)
	} else {
		editorWidth := max(20, (m.width-3)*2/3)
		m.cypher.SetWidth(editorWidth)
		m.params.SetWidth(max(20, m.width-editorWidth-3))
	}

	var resultHeight int
	if m.singleSection {
		sectionHeight := max(1, m.height-1)
		editorHeight, paramsHeight, resultHeight = sectionHeight, sectionHeight, sectionHeight
	} else {
		remaining := max(1, m.height-editorBlockHeight-1)
		if resultsHorizontal {
			resultHeight = max(1, remaining-1)
		} else {
			resultHeight = max(1, (remaining-3)/2)
		}
	}
	m.cypher.SetHeight(editorHeight)
	m.params.SetHeight(paramsHeight)

	tableWidth := m.width
	rowWidth := m.width
	if !m.singleSection && m.width >= 100 {
		tableWidth = max(35, m.width*3/5)
		rowWidth = max(25, m.width-tableWidth-3)
	}
	m.tableViewport.SetWidth(tableWidth)
	m.tableViewport.SetHeight(resultHeight)
	m.rowViewport.SetWidth(rowWidth)
	m.rowViewport.SetHeight(resultHeight)
	m.rebuildTable()
}

func (m *queryModel) cycleFocus(backward bool) tea.Cmd {
	if backward {
		m.focus = (m.focus + 3) % 4
	} else {
		m.focus = (m.focus + 1) % 4
	}
	return m.focusCurrent()
}

func (m *queryModel) focusCurrent() tea.Cmd {
	m.cypher.Blur()
	m.params.Blur()
	m.table.Blur()
	var cmd tea.Cmd
	switch m.focus {
	case queryFocusCypher:
		cmd = m.cypher.Focus()
	case queryFocusParams:
		cmd = m.params.Focus()
	case queryFocusResults:
		m.table.Focus()
	case queryFocusRow:
		// The row viewport has no explicit focus state; routing supplies it.
	}
	return cmd
}

func (m *queryModel) update(msg tea.Msg) tea.Cmd {
	var cmd tea.Cmd
	switch m.focus {
	case queryFocusCypher:
		m.cypher, cmd = m.cypher.Update(msg)
	case queryFocusParams:
		m.params, cmd = m.params.Update(msg)
	case queryFocusResults:
		m.table, cmd = m.table.Update(msg)
		updated, viewportCmd := m.tableViewport.Update(msg)
		m.tableViewport = updated
		cmd = tea.Batch(cmd, viewportCmd)
		m.refreshTableViewport()
		m.refreshSelectedRow()
	case queryFocusRow:
		m.rowViewport, cmd = m.rowViewport.Update(msg)
	}
	return cmd
}

func (m *queryModel) setResult(result app.BatchResult, err error) {
	m.err = err
	if err != nil {
		m.result = nil
		m.table.SetRows(nil)
		m.table.SetColumns(nil)
		m.tableViewport.SetContent("")
		m.rowViewport.SetContent("")
		return
	}
	m.result = &result
	if len(result.Results) == 0 {
		m.resultIndex = 0
	} else if m.resultIndex >= len(result.Results) {
		m.resultIndex = len(result.Results) - 1
	}
	m.rebuildTable()
}

func (m *queryModel) moveResult(delta int) {
	if m.result == nil || len(m.result.Results) == 0 {
		return
	}
	next := m.resultIndex + delta
	if next < 0 {
		next = len(m.result.Results) - 1
	}
	if next >= len(m.result.Results) {
		next = 0
	}
	m.resultIndex = next
	m.rebuildTable()
}

func (m *queryModel) rebuildTable() {
	if m.result == nil || len(m.result.Results) == 0 {
		m.table.SetColumns(nil)
		m.table.SetRows(nil)
		m.refreshTableViewport()
		m.rowViewport.SetContent("No result rows yet.")
		return
	}
	result := m.result.Results[m.resultIndex]
	rows := make([]table.Row, len(result.Rows))
	widths := make([]int, len(result.Columns))
	for index, column := range result.Columns {
		widths[index] = max(6, ansi.StringWidth(column))
	}
	for rowIndex, values := range result.Rows {
		row := make(table.Row, len(result.Columns))
		for columnIndex := range result.Columns {
			value := any(nil)
			if columnIndex < len(values) {
				value = values[columnIndex]
			}
			cell := queryCell(value)
			row[columnIndex] = cell
			widths[columnIndex] = min(80, max(widths[columnIndex], ansi.StringWidth(cell)))
		}
		rows[rowIndex] = row
	}
	columns := make([]table.Column, len(result.Columns))
	totalWidth := 0
	for index, title := range result.Columns {
		columns[index] = table.Column{Title: title, Width: widths[index]}
		totalWidth += widths[index] + 2
	}
	if totalWidth == 0 {
		totalWidth = m.tableViewport.Width()
	}
	m.table.SetColumns(columns)
	m.table.SetRows(rows)
	if len(rows) > 0 && m.table.Cursor() < 0 {
		m.table.SetCursor(0)
	}
	m.table.SetWidth(max(totalWidth, m.tableViewport.Width()))
	m.table.SetHeight(m.tableViewport.Height())
	m.refreshTableViewport()
	m.refreshSelectedRow()
}

func (m *queryModel) refreshTableViewport() {
	m.tableViewport.SetContent(m.table.View())
}

func (m *queryModel) refreshSelectedRow() {
	if m.result == nil || len(m.result.Results) == 0 {
		m.rowViewport.SetContent("No selected row.")
		return
	}
	result := m.result.Results[m.resultIndex]
	index := m.table.Cursor()
	if index < 0 || index >= len(result.Rows) {
		m.rowViewport.SetContent(summaryText(result.Summary))
		return
	}
	row := result.Rows[index]
	var detail strings.Builder
	for columnIndex, column := range result.Columns {
		value := any(nil)
		if columnIndex < len(row) {
			value = row[columnIndex]
		}
		fmt.Fprintf(&detail, "%d. %s\n%s\n\n", columnIndex+1, column, prettyJSON(value))
	}
	m.rowViewport.SetContent(strings.TrimSpace(detail.String()))
}

func (m queryModel) view() string {
	cypherTitle := m.sectionTitle("CYPHER", queryFocusCypher)
	paramsTitle := m.sectionTitle("PARAMETERS · JSON OBJECT", queryFocusParams)
	cypher := cypherTitle + "\n" + m.cypher.View()
	params := paramsTitle + "\n" + m.params.View()
	resultView, row := m.resultViews()
	if m.singleSection {
		switch m.focus {
		case queryFocusCypher:
			return cypher
		case queryFocusParams:
			return params
		case queryFocusResults:
			return resultView
		case queryFocusRow:
			return row
		}
	}
	editors := lipgloss.JoinVertical(lipgloss.Left, cypher, params)
	if m.width >= 90 {
		editors = lipgloss.JoinHorizontal(lipgloss.Top, cypher, "   ", params)
	}

	results := lipgloss.JoinVertical(lipgloss.Left, resultView, "", row)
	if m.width >= 100 {
		results = lipgloss.JoinHorizontal(lipgloss.Top, resultView, "   ", row)
	}
	return lipgloss.JoinVertical(lipgloss.Left, editors, "", results)
}

func (m queryModel) resultViews() (string, string) {
	resultTitle := m.sectionTitle("RESULTS", queryFocusResults)
	if m.result != nil && len(m.result.Results) > 0 {
		resultTitle += fmt.Sprintf(" · statement %d/%d", m.resultIndex+1, len(m.result.Results))
		resultTitle += " · " + summaryText(m.result.Results[m.resultIndex].Summary)
	}
	resultTitle = ansi.Truncate(resultTitle, m.tableViewport.Width(), "…")
	resultView := resultTitle + "\n"
	if m.err != nil {
		resultView += wrapText("Query failed: "+m.err.Error(), m.tableViewport.Width())
	} else if m.result == nil {
		guidance := "Run a read-only query with Ctrl+R. Write-capable execution uses Ctrl+X and asks for confirmation."
		resultView += wrapText(guidance, m.tableViewport.Width())
	} else {
		resultView += m.tableViewport.View()
	}
	rowTitle := ansi.Truncate(m.sectionTitle("SELECTED ROW", queryFocusRow), m.rowViewport.Width(), "…")
	row := rowTitle + "\n" + m.rowViewport.View()
	return resultView, row
}

func (m queryModel) sectionTitle(title string, focus queryFocus) string {
	prefix := "  "
	style := m.styles.paneTitle
	if m.focus == focus {
		prefix = "> "
		style = m.styles.focusedTitle
	}
	return style.Render(prefix + title)
}

func queryCell(value any) string {
	if value == nil {
		return "null"
	}
	var text string
	switch value := value.(type) {
	case string:
		text = value
	case json.Number:
		text = value.String()
	case temporal.Date:
		text = "date(" + value.String() + ")"
	case temporal.LocalTime:
		text = "localtime(" + value.String() + ")"
	case temporal.Time:
		text = "time(" + value.String() + ")"
	case temporal.LocalDateTime:
		text = "localdatetime(" + value.String() + ")"
	case temporal.DateTime:
		text = "datetime(" + value.String() + ")"
	case temporal.Duration:
		text = "duration(" + value.String() + ")"
	case time.Time:
		text = "legacy_time(" + value.Format(time.RFC3339Nano) + ")"
	case time.Duration:
		text = "legacy_duration(" + value.String() + ")"
	case []byte:
		text = "bytes(" + base64.StdEncoding.EncodeToString(value) + ")"
	default:
		text = stableJSON(value)
	}
	text = strings.ReplaceAll(text, "\r", "")
	text = strings.ReplaceAll(text, "\n", "↵")
	return text
}

func summaryText(summary app.Summary) string {
	parts := make([]string, 0, 4)
	if summary.NodesCreated > 0 {
		parts = append(parts, fmt.Sprintf("%d nodes created", summary.NodesCreated))
	}
	if summary.NodesUpdated > 0 {
		parts = append(parts, fmt.Sprintf("%d nodes updated", summary.NodesUpdated))
	}
	if summary.NodesDeleted > 0 {
		parts = append(parts, fmt.Sprintf("%d nodes deleted", summary.NodesDeleted))
	}
	if summary.RelationshipsCreated > 0 {
		parts = append(parts, fmt.Sprintf("%d relationships created", summary.RelationshipsCreated))
	}
	if summary.RelationshipsUpdated > 0 {
		parts = append(parts, fmt.Sprintf("%d relationships updated", summary.RelationshipsUpdated))
	}
	if summary.RelationshipsDeleted > 0 {
		parts = append(parts, fmt.Sprintf("%d relationships deleted", summary.RelationshipsDeleted))
	}
	if summary.PropertiesSet > 0 {
		parts = append(parts, fmt.Sprintf("%d properties set", summary.PropertiesSet))
	}
	if summary.LabelsAdded > 0 {
		parts = append(parts, fmt.Sprintf("%d labels added", summary.LabelsAdded))
	}
	if summary.LabelsRemoved > 0 {
		parts = append(parts, fmt.Sprintf("%d labels removed", summary.LabelsRemoved))
	}
	if len(parts) == 0 {
		return "no graph changes"
	}
	return strings.Join(parts, ", ")
}
