package tui

import (
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strconv"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/svlocks/sheets/internal/app"
	"github.com/svlocks/sheets/internal/domain"
)

type paletteCommand struct {
	Name     string
	Keywords string
	Action   string
}

var allCommands = []paletteCommand{
	{Name: "Go to Outline", Keywords: "tree hierarchy 1", Action: "outline"},
	{Name: "Go to Graph", Keywords: "topology network 2", Action: "graph"},
	{Name: "Go to Query console", Keywords: "cypher editor 3", Action: "query"},
	{Name: "Go to History", Keywords: "timeline revision 4", Action: "history"},
	{Name: "Create node", Keywords: "add new", Action: "create"},
	{Name: "Edit selected node", Keywords: "update properties body labels", Action: "edit"},
	{Name: "Reparent selected node", Keywords: "move child", Action: "reparent"},
	{Name: "Delete selected node", Keywords: "remove detach", Action: "delete"},
	{Name: "Search nodes", Keywords: "filter find", Action: "search"},
	{Name: "Inspect selected node", Keywords: "details properties edges", Action: "inspect"},
	{Name: "Return to Live", Keywords: "current writable", Action: "live"},
	{Name: "Refresh now", Keywords: "reload", Action: "refresh"},
	{Name: "Keyboard help", Keywords: "shortcuts keys", Action: "help"},
}

type createForm struct {
	Labels     []string          `json:"labels"`
	Properties domain.Properties `json:"properties"`
	Body       string            `json:"body"`
	ParentID   string            `json:"parent_id"`
	Position   *int64            `json:"position"`
}

type editForm struct {
	Labels     []string          `json:"labels"`
	Properties domain.Properties `json:"properties"`
	Body       string            `json:"body"`
}

type reparentForm struct {
	ParentID string `json:"parent_id"`
	Position *int64 `json:"position"`
}

func (m *Model) openSearch() {
	m.overlay = overlaySearch
	_ = m.search.Focus()
	m.search.CursorEnd()
}

func (m *Model) openPalette() {
	m.overlay = overlayPalette
	m.palette.SetValue("")
	m.paletteIndex = 0
	_ = m.palette.Focus()
}

func (m *Model) filteredCommands() []paletteCommand {
	query := strings.ToLower(strings.TrimSpace(m.palette.Value()))
	if query == "" {
		return allCommands
	}
	result := make([]paletteCommand, 0)
	for _, command := range allCommands {
		if strings.Contains(strings.ToLower(command.Name+" "+command.Keywords), query) {
			result = append(result, command)
		}
	}
	return result
}

func (m *Model) runPaletteCommand(command paletteCommand) tea.Cmd {
	m.palette.Blur()
	m.overlay = overlayNone
	switch command.Action {
	case "outline":
		m.setTab(OutlineTab)
	case "graph":
		m.setTab(GraphTab)
	case "query":
		m.setTab(QueryTab)
	case "history":
		m.setTab(HistoryTab)
	case "create":
		m.openMutation(mutationCreate)
	case "edit":
		m.openMutation(mutationEdit)
	case "reparent":
		m.openMutation(mutationReparent)
	case "delete":
		m.openMutation(mutationDelete)
	case "search":
		m.openSearch()
	case "inspect":
		m.overlay = overlayInspector
	case "live":
		return m.returnLive()
	case "refresh":
		return tea.Batch(m.loadGraphCmd(m.snapshot), m.loadHistoryCmd())
	case "help":
		m.overlay = overlayHelp
	}
	return nil
}

func (m *Model) openMutation(kind mutationKind) {
	if !m.snapshot.IsCurrent() {
		m.status = "Historical mode is read-only — press l in History to return to Live"
		return
	}
	if kind != mutationCreate && m.selected == "" {
		m.status = "Select a node first"
		return
	}
	m.mutation = kind
	m.mutationErr = nil
	m.overlay = overlayMutation
	var value any
	switch kind {
	case mutationCreate:
		value = createForm{Labels: []string{"Task"}, Properties: domain.Properties{"title": "New task"}, ParentID: string(m.selected)}
	case mutationEdit:
		node := m.nodeByID[m.selected]
		value = editForm{Labels: append([]string(nil), node.Labels...), Properties: cloneProperties(node.Properties), Body: node.Body}
	case mutationReparent:
		form := reparentForm{}
		for _, edge := range m.edges {
			if strings.EqualFold(edge.Type, "CHILD") && edge.To == m.selected {
				form.ParentID = string(edge.From)
				if edge.Position != nil {
					position := *edge.Position
					form.Position = &position
				}
				break
			}
		}
		value = form
	case mutationDelete:
		m.mutationInput.SetValue("")
		return
	}
	encoded, _ := json.MarshalIndent(value, "", "  ")
	m.mutationInput.SetValue(string(encoded))
	_ = m.mutationInput.Focus()
}

func (m *Model) submitMutation() tea.Cmd {
	if !m.snapshot.IsCurrent() {
		m.mutationErr = fmt.Errorf("historical mode is read-only")
		return nil
	}
	var request app.ExecuteRequest
	request.ReadOnly = false
	request.Actor = "tui"
	request.Message = strings.ToLower(mutationName(m.mutation))

	switch m.mutation {
	case mutationCreate:
		var form createForm
		if err := decodeObject(m.mutationInput.Value(), &form); err != nil {
			m.mutationErr = err
			return nil
		}
		if err := validateLabels(form.Labels); err != nil {
			m.mutationErr = err
			return nil
		}
		if form.Properties == nil {
			form.Properties = domain.Properties{}
		}
		labels := cypherLabels(form.Labels)
		request.Params = map[string]any{"properties": form.Properties, "body": form.Body}
		if strings.TrimSpace(form.ParentID) == "" {
			request.Query = "CREATE (n" + labels + ") SET n = $properties, n.body = $body RETURN n"
		} else {
			request.Params["parent"] = strings.TrimSpace(form.ParentID)
			relationship := "[:CHILD]"
			if form.Position != nil {
				request.Params["position"] = *form.Position
				relationship = "[:CHILD {position: $position}]"
			}
			request.Query = "MATCH (p) WHERE elementId(p) = $parent CREATE (p)-" + relationship + "->(n" + labels + ") SET n = $properties, n.body = $body RETURN n"
		}
	case mutationEdit:
		var form editForm
		if err := decodeObject(m.mutationInput.Value(), &form); err != nil {
			m.mutationErr = err
			return nil
		}
		if err := validateLabels(form.Labels); err != nil {
			m.mutationErr = err
			return nil
		}
		if form.Properties == nil {
			form.Properties = domain.Properties{}
		}
		node := m.nodeByID[m.selected]
		query := "MATCH (n) WHERE elementId(n) = $id SET n = $properties, n.body = $body"
		if len(node.Labels) > 0 {
			query += " REMOVE n" + cypherLabels(node.Labels)
		}
		if len(form.Labels) > 0 {
			query += " SET n" + cypherLabels(form.Labels)
		}
		request.Query = query + " RETURN n"
		request.Params = map[string]any{"id": string(m.selected), "properties": form.Properties, "body": form.Body}
	case mutationReparent:
		var form reparentForm
		if err := decodeObject(m.mutationInput.Value(), &form); err != nil {
			m.mutationErr = err
			return nil
		}
		parent := strings.TrimSpace(form.ParentID)
		if parent == string(m.selected) {
			m.mutationErr = fmt.Errorf("a node cannot be its own parent")
			return nil
		}
		var old *domain.Edge
		for index := range m.edges {
			if strings.EqualFold(m.edges[index].Type, "CHILD") && m.edges[index].To == m.selected {
				old = &m.edges[index]
				break
			}
		}
		if old == nil && parent == "" {
			m.overlay = overlayNone
			m.status = "Node is already a root"
			return nil
		}
		request.Params = map[string]any{"id": string(m.selected)}
		query := "MATCH (n) WHERE elementId(n) = $id"
		if old != nil {
			request.Params["edge"] = string(old.ID)
			query += " MATCH ()-[old:CHILD]->(n) WHERE elementId(old) = $edge DELETE old"
		}
		if parent != "" {
			request.Params["parent"] = parent
			query += " WITH n MATCH (p) WHERE elementId(p) = $parent"
			relationship := "[:CHILD]"
			if form.Position != nil {
				request.Params["position"] = *form.Position
				relationship = "[:CHILD {position: $position}]"
			}
			query += " CREATE (p)-" + relationship + "->(n)"
		}
		request.Query = query + " RETURN n"
	case mutationDelete:
		request.Query = "MATCH (n) WHERE elementId(n) = $id DETACH DELETE n"
		request.Params = map[string]any{"id": string(m.selected)}
	default:
		return nil
	}
	m.mutationErr = nil
	return m.executeCmd(request, m.mutation)
}

var cypherIdentifier = regexp.MustCompile(`^[\pL_][\pL\pN_]*$`)

func validateLabels(labels []string) error {
	seen := make(map[string]bool, len(labels))
	for _, label := range labels {
		if strings.TrimSpace(label) == "" {
			return fmt.Errorf("labels cannot be empty")
		}
		if seen[label] {
			return fmt.Errorf("label %q appears more than once", label)
		}
		seen[label] = true
	}
	return nil
}

func cypherLabels(labels []string) string {
	result := strings.Builder{}
	for _, label := range labels {
		result.WriteByte(':')
		if cypherIdentifier.MatchString(label) {
			result.WriteString(label)
		} else {
			result.WriteByte('`')
			result.WriteString(strings.ReplaceAll(label, "`", "``"))
			result.WriteByte('`')
		}
	}
	return result.String()
}

func decodeObject(source string, destination any) error {
	decoder := json.NewDecoder(strings.NewReader(source))
	decoder.UseNumber()
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return fmt.Errorf("invalid form JSON: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return fmt.Errorf("invalid form JSON: unexpected trailing value")
		}
		return fmt.Errorf("invalid form JSON: %w", err)
	}
	return normalizeJSONNumbers(destination)
}

func normalizeJSONNumbers(destination any) error {
	// Concrete form integer pointers are already decoded as int64. The generic
	// property maps still contain json.Number and are normalized recursively.
	switch form := destination.(type) {
	case *createForm:
		properties, err := normalizeProperties(form.Properties)
		form.Properties = properties
		return err
	case *editForm:
		properties, err := normalizeProperties(form.Properties)
		form.Properties = properties
		return err
	case *map[string]any:
		properties, err := normalizeProperties(*form)
		*form = properties
		return err
	default:
		return nil
	}
}

func normalizeProperties(properties map[string]any) (map[string]any, error) {
	for key, value := range properties {
		normalized, err := normalizeJSONValue(value)
		if err != nil {
			return nil, fmt.Errorf("property %q: %w", key, err)
		}
		properties[key] = normalized
	}
	return properties, nil
}

func normalizeJSONValue(value any) (any, error) {
	switch value := value.(type) {
	case json.Number:
		if integer, err := strconv.ParseInt(value.String(), 10, 64); err == nil {
			return integer, nil
		}
		decimal, err := strconv.ParseFloat(value.String(), 64)
		if err != nil {
			return nil, fmt.Errorf("invalid number %q", value)
		}
		return decimal, nil
	case []any:
		for index, item := range value {
			normalized, err := normalizeJSONValue(item)
			if err != nil {
				return nil, fmt.Errorf("item %d: %w", index, err)
			}
			value[index] = normalized
		}
		return value, nil
	case map[string]any:
		return normalizeProperties(value)
	default:
		return value, nil
	}
}

func cloneProperties(properties domain.Properties) domain.Properties {
	clone := make(domain.Properties, len(properties))
	keys := make([]string, 0, len(properties))
	for key := range properties {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		clone[key] = properties[key]
	}
	return clone
}

func mutationName(kind mutationKind) string {
	switch kind {
	case mutationCreate:
		return "Create"
	case mutationEdit:
		return "Edit"
	case mutationReparent:
		return "Reparent"
	case mutationDelete:
		return "Delete"
	default:
		return "Mutation"
	}
}
