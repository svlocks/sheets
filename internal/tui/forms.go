package tui

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"charm.land/bubbles/v2/help"
	tea "charm.land/bubbletea/v2"
	"charm.land/huh/v2"
	"charm.land/lipgloss/v2"
	"github.com/svlocks/sheets/internal/app"
	"github.com/svlocks/sheets/internal/domain"
)

type formPurpose uint8

const (
	formCreateNode formPurpose = iota + 1
	formEditNode
	formMoveNode
	formConnectNodes
	formEditRelationship
	formDeleteNode
	formDeleteRelationship
	formExecuteQuery
)

func (p formPurpose) String() string {
	switch p {
	case formCreateNode:
		return "Create node"
	case formEditNode:
		return "Edit node"
	case formMoveNode:
		return "Move / order node"
	case formConnectNodes:
		return "Connect nodes"
	case formEditRelationship:
		return "Edit relationship"
	case formDeleteNode:
		return "Delete node"
	case formDeleteRelationship:
		return "Delete relationship"
	case formExecuteQuery:
		return "Execute write-capable Cypher"
	default:
		return "Form"
	}
}

type formSubmittedMsg struct{ serial uint64 }

var errConfirmationDeclined = errors.New("confirmation declined")

type nodeFormData struct {
	title      string
	labels     string
	properties string
	body       string
	parent     string
	position   string
	original   domain.Node
}

type moveFormData struct {
	node     domain.Node
	parent   string
	position string
	old      *domain.Edge
}

type connectionFormData struct {
	from       string
	to         string
	typeName   string
	properties string
}

type relationshipFormData struct {
	edge       domain.Edge
	properties string
}

type confirmFormData struct {
	confirmed bool
	node      *domain.Node
	edge      *domain.Edge
	request   *app.ExecuteRequest
}

type formController struct {
	serial  uint64
	purpose formPurpose
	form    *huh.Form
	data    any
	title   string
	width   int
	height  int
	dark    bool
	noColor bool

	validationErr string
}

func newCreateNodeForm(serial uint64, graph graphState, selected domain.EntityID, width, height int, dark, noColor bool) *formController {
	data := &nodeFormData{
		labels: "Task", properties: "{}", parent: string(selected),
	}
	parent := parentField("Parent", "Choose root or an existing node. Type / to filter.", &data.parent, graph, "")
	fields := []huh.Field{
		huh.NewInput().Title("Title").Description("A human-readable title; stored as the title property.").Value(&data.title).Validate(huh.ValidateNotEmpty()),
		huh.NewInput().Title("Labels").Description("Comma-separated, for example Task, Feature.").Value(&data.labels).Validate(validateLabelInput),
		parent,
		huh.NewInput().Title("Sibling position").Description("Integer for ordered siblings; leave blank for unordered.").Value(&data.position).Validate(validateOptionalInteger),
		huh.NewText().Title("Other properties · JSON object").Description("Schema-free properties other than title and body.").Lines(5).ShowLineNumbers(true).Value(&data.properties).Validate(validateNodePropertiesJSON),
		huh.NewText().Title("Markdown body").Description("Stored directly in the graph.").Lines(7).ShowLineNumbers(true).Value(&data.body),
	}
	return newFormController(serial, formCreateNode, data, fields, width, height, dark, noColor)
}

func newEditNodeForm(serial uint64, node domain.Node, width, height int, dark, noColor bool) *formController {
	properties := copyProperties(node.Properties)
	title := ""
	if value, ok := properties["title"].(string); ok {
		title = value
		delete(properties, "title")
	}
	data := &nodeFormData{
		title: title, labels: strings.Join(node.Labels, ", "), properties: prettyJSON(properties), body: node.Body, original: node,
	}
	fields := []huh.Field{
		huh.NewInput().Title("Title").Description("Optional for schema-free nodes; blank removes title.").Value(&data.title),
		huh.NewInput().Title("Labels").Description("Comma-separated; blank removes all labels.").Value(&data.labels).Validate(validateLabelInput),
		huh.NewText().Title("Other properties · JSON object").Description("Replacing this object removes properties not listed here.").Lines(6).ShowLineNumbers(true).Value(&data.properties).Validate(validateNodePropertiesJSON),
		huh.NewText().Title("Markdown body").Lines(8).ShowLineNumbers(true).Value(&data.body),
	}
	return newFormController(serial, formEditNode, data, fields, width, height, dark, noColor)
}

func newMoveNodeForm(serial uint64, graph graphState, node domain.Node, width, height int, dark, noColor bool) *formController {
	data := &moveFormData{node: node}
	if edge, ok := graph.parent[node.ID]; ok {
		copy := edge
		data.old = &copy
		data.parent = string(edge.From)
		if edge.Position != nil {
			data.position = strconv.FormatInt(*edge.Position, 10)
		}
	}
	parent := parentField("New parent", "Descendants are excluded to prevent cycles. Choose root to detach.", &data.parent, graph, node.ID)
	fields := []huh.Field{
		huh.NewNote().Title("Move " + nodeTitle(node)).Description("This atomically removes the old CHILD edge and creates the new one."),
		parent,
		huh.NewInput().Title("Sibling position").Description("Integer for ordered siblings; leave blank for unordered.").Value(&data.position).Validate(validateOptionalInteger),
	}
	return newFormController(serial, formMoveNode, data, fields, width, height, dark, noColor)
}

func newConnectionForm(serial uint64, graph graphState, selected domain.EntityID, width, height int, dark, noColor bool) *formController {
	data := &connectionFormData{from: string(selected), properties: "{}"}
	if data.from == "" && len(graph.nodes) > 0 {
		data.from = string(graph.nodes[0].ID)
	}
	if len(graph.nodes) > 1 {
		for _, node := range graph.nodes {
			if string(node.ID) != data.from {
				data.to = string(node.ID)
				break
			}
		}
	} else {
		data.to = data.from
	}
	fields := []huh.Field{
		nodeField("From", "Relationship source. Type / to filter.", &data.from, graph, ""),
		huh.NewInput().Title("Relationship type").Description("Any non-CHILD type, for example BLOCKED_BY or RELATES_TO.").Value(&data.typeName).Validate(validateRelationshipType),
		nodeField("To", "Relationship target. Self-relationships are allowed.", &data.to, graph, ""),
		huh.NewText().Title("Properties · JSON object").Description("position is reserved for CHILD sibling order.").Lines(7).ShowLineNumbers(true).Value(&data.properties).Validate(validateRelationshipPropertiesJSON),
	}
	return newFormController(serial, formConnectNodes, data, fields, width, height, dark, noColor)
}

func newEditRelationshipForm(serial uint64, edge domain.Edge, width, height int, dark, noColor bool) *formController {
	data := &relationshipFormData{edge: edge, properties: prettyJSON(edge.Properties)}
	description := fmt.Sprintf("%s → %s. Type and endpoints are stable; recreate the relationship to change them.", edge.From, edge.To)
	fields := []huh.Field{
		huh.NewNote().Title("Edit " + edge.Type).Description(description),
		huh.NewText().Title("Properties · JSON object").Description("Replacing this object removes properties not listed here; position is reserved.").Lines(9).ShowLineNumbers(true).Value(&data.properties).Validate(validateRelationshipPropertiesJSON),
	}
	return newFormController(serial, formEditRelationship, data, fields, width, height, dark, noColor)
}

func newDeleteNodeForm(serial uint64, graph graphState, node domain.Node, width, height int, dark, noColor bool) *formController {
	data := &confirmFormData{node: &node}
	children := len(graph.children[node.ID])
	incident := len(graph.incoming[node.ID]) + len(graph.outgoing[node.ID])
	description := fmt.Sprintf("This permanently closes the node and its %d incident relationships at a new revision. Its %d direct children become roots. Historical revisions remain viewable.", incident, children)
	fields := []huh.Field{
		huh.NewNote().Title("Delete " + nodeTitle(node) + "?").Description(description),
		huh.NewConfirm().Title("Confirm DETACH DELETE").Affirmative("Delete node").Negative("Keep node").Value(&data.confirmed),
	}
	return newFormController(serial, formDeleteNode, data, fields, width, height, dark, noColor)
}

func newDeleteRelationshipForm(serial uint64, graph graphState, edge domain.Edge, width, height int, dark, noColor bool) *formController {
	data := &confirmFormData{edge: &edge}
	description := fmt.Sprintf("Remove %s —%s→ %s. The relationship remains visible in earlier revisions.", nodeTitle(graph.nodeByID[edge.From]), edge.Type, nodeTitle(graph.nodeByID[edge.To]))
	fields := []huh.Field{
		huh.NewNote().Title("Delete relationship?").Description(description),
		huh.NewConfirm().Title("Confirm deletion").Affirmative("Delete relationship").Negative("Keep relationship").Value(&data.confirmed),
	}
	return newFormController(serial, formDeleteRelationship, data, fields, width, height, dark, noColor)
}

func newExecuteQueryForm(serial uint64, request app.ExecuteRequest, width, height int, dark, noColor bool) *formController {
	data := &confirmFormData{request: &request}
	preview := strings.TrimSpace(request.Query)
	if len(preview) > 600 {
		preview = preview[:600] + "…"
	}
	fields := []huh.Field{
		huh.NewNote().Title("Execute write-capable Cypher?").Description(preview),
		huh.NewConfirm().Title("This request may create a new revision").Affirmative("Execute").Negative("Cancel").Value(&data.confirmed),
	}
	return newFormController(serial, formExecuteQuery, data, fields, width, height, dark, noColor)
}

func newFormController(serial uint64, purpose formPurpose, data any, fields []huh.Field, width, height int, dark, noColor bool) *formController {
	// The surrounding modal already carries the operation title. Repeating it
	// as a Huh group title wastes the one line that makes the focused option
	// visible at the supported 44×12 minimum.
	form := huh.NewForm(huh.NewGroup(fields...)).
		WithShowHelp(true).
		WithShowErrors(true).
		WithWidth(clampSize(width, 30)).
		WithHeight(clampSize(height, 1)).
		WithTheme(formTheme(noColor))
	controller := &formController{
		serial: serial, purpose: purpose, form: form, data: data, title: purpose.String(),
		width: width, height: height, dark: dark, noColor: noColor,
	}
	form.SubmitCmd = func() tea.Msg { return formSubmittedMsg{serial: serial} }
	return controller
}

func (m *formController) init() tea.Cmd { return m.form.Init() }

func (m *formController) update(msg tea.Msg) tea.Cmd {
	_, cmd := m.form.Update(msg)
	m.validationErr = ""
	if field := m.form.GetFocusedField(); field != nil && field.Error() != nil {
		m.validationErr = field.Error().Error()
	}
	return cmd
}

func (m *formController) view() string { return m.form.View() }

func (m *formController) setSize(width, height int) {
	m.width, m.height = width, height
	m.form.WithWidth(clampSize(width, 30)).WithHeight(clampSize(height, 1))
}

func (m *formController) escapeCancels() bool {
	field := m.form.GetFocusedField()
	switch value := field.(type) {
	case *huh.Select[string]:
		return !value.GetFiltering()
	case *huh.MultiSelect[string]:
		return !value.GetFiltering()
	default:
		return true
	}
}

func (m *formController) request() (app.ExecuteRequest, error) {
	request := app.ExecuteRequest{Actor: "tui", ReadOnly: false}
	switch data := m.data.(type) {
	case *nodeFormData:
		labels, err := parseLabels(data.labels)
		if err != nil {
			return request, err
		}
		properties, err := decodeNodeProperties(data.properties)
		if err != nil {
			return request, err
		}
		if m.purpose == formEditNode {
			properties, err = restorePropertyMapTypes(properties, data.original.Properties)
			if err != nil {
				return request, err
			}
		}
		if strings.TrimSpace(data.title) != "" {
			properties["title"] = strings.TrimSpace(data.title)
		}
		request.Params = map[string]any{"properties": properties, "body": data.body}
		nodeSet := "SET n = $properties, n.body = $body"
		if position, exists := properties["position"]; exists {
			delete(properties, "position")
			request.Params["node_position"] = position
			nodeSet += ", n.position = $node_position"
		}
		switch m.purpose {
		case formCreateNode:
			labelsClause := cypherLabels(labels)
			parent := strings.TrimSpace(data.parent)
			if parent == "" {
				if strings.TrimSpace(data.position) != "" {
					return request, errors.New("sibling position requires a parent")
				}
				request.Query = "CREATE (n" + labelsClause + ") " + nodeSet + " RETURN n"
			} else {
				request.Params["parent"] = parent
				relationship, err := childPattern(strings.TrimSpace(data.position), request.Params)
				if err != nil {
					return request, err
				}
				request.Query = "MATCH (p) WHERE elementId(p) = $parent CREATE (p)-" + relationship + "->(n" + labelsClause + ") " + nodeSet + " RETURN n"
			}
			request.Message = "create node: " + strings.TrimSpace(data.title)
		case formEditNode:
			request.Params["id"] = string(data.original.ID)
			query := "MATCH (n) WHERE elementId(n) = $id " + nodeSet
			if len(data.original.Labels) > 0 {
				query += " REMOVE n" + cypherLabels(data.original.Labels)
			}
			if len(labels) > 0 {
				query += " SET n" + cypherLabels(labels)
			}
			request.Query = query + " RETURN n"
			request.Message = "edit node: " + nodeTitle(data.original)
		default:
			return request, errors.New("unsupported node form")
		}
	case *moveFormData:
		request.Params = map[string]any{"id": string(data.node.ID)}
		query := "MATCH (n) WHERE elementId(n) = $id"
		if data.old != nil {
			request.Params["old"] = string(data.old.ID)
			query += " MATCH ()-[old:CHILD]->(n) WHERE elementId(old) = $old"
		}
		parent := strings.TrimSpace(data.parent)
		position, err := parseOptionalPosition(strings.TrimSpace(data.position))
		if err != nil {
			return request, err
		}
		if parent == "" && position != nil {
			return request, errors.New("sibling position requires a parent")
		}
		if data.old != nil && parent == string(data.old.From) {
			if equalOptionalPosition(position, data.old.Position) {
				request.Query = query + " RETURN n"
			} else {
				request.Params["position"] = optionalPositionParameter(position)
				request.Query = query + " SET old.position = $position RETURN n"
			}
			request.Message = "move node: " + nodeTitle(data.node)
			break
		}
		pattern := ""
		if parent != "" {
			request.Params["parent"] = parent
			pattern, err = childPattern(strings.TrimSpace(data.position), request.Params)
			if err != nil {
				return request, err
			}
			// Resolve the new parent before deleting the old edge. If another
			// process removed the selected destination, the complete move now
			// matches no rows and cannot accidentally commit a detach-to-root.
			query += " MATCH (p) WHERE elementId(p) = $parent"
		}
		if data.old != nil {
			query += " DELETE old"
		}
		if parent != "" {
			query += " CREATE (p)-" + pattern + "->(n)"
		}
		request.Query = query + " RETURN n"
		request.Message = "move node: " + nodeTitle(data.node)
	case *connectionFormData:
		properties, err := decodeRelationshipProperties(data.properties)
		if err != nil {
			return request, err
		}
		typeName := strings.TrimSpace(data.typeName)
		if err := validateRelationshipType(typeName); err != nil {
			return request, err
		}
		request.Query = "MATCH (a), (b) WHERE elementId(a) = $from AND elementId(b) = $to CREATE (a)-[r:" + cypherIdentifierName(typeName) + "]->(b) SET r = $properties RETURN r"
		request.Params = map[string]any{"from": data.from, "to": data.to, "properties": properties}
		if body, exists := properties["body"]; exists {
			delete(properties, "body")
			request.Params["relationship_body"] = body
			request.Query = strings.Replace(request.Query, "SET r = $properties", "SET r = $properties, r.body = $relationship_body", 1)
		}
		request.Message = "connect nodes: " + typeName
	case *relationshipFormData:
		properties, err := decodeRelationshipProperties(data.properties)
		if err != nil {
			return request, err
		}
		properties, err = restorePropertyMapTypes(properties, data.edge.Properties)
		if err != nil {
			return request, err
		}
		request.Query = "MATCH ()-[r]->() WHERE elementId(r) = $id SET r = $properties RETURN r"
		request.Params = map[string]any{"id": string(data.edge.ID), "properties": properties}
		if body, exists := properties["body"]; exists {
			delete(properties, "body")
			request.Params["relationship_body"] = body
			request.Query = strings.Replace(request.Query, "SET r = $properties", "SET r = $properties, r.body = $relationship_body", 1)
		}
		request.Message = "edit relationship: " + data.edge.Type
	case *confirmFormData:
		if !data.confirmed {
			return request, errConfirmationDeclined
		}
		switch m.purpose {
		case formDeleteNode:
			request.Query = "MATCH (n) WHERE elementId(n) = $id DETACH DELETE n"
			request.Params = map[string]any{"id": string(data.node.ID)}
			request.Message = "delete node: " + nodeTitle(*data.node)
		case formDeleteRelationship:
			request.Query = "MATCH ()-[r]->() WHERE elementId(r) = $id DELETE r"
			request.Params = map[string]any{"id": string(data.edge.ID)}
			request.Message = "delete relationship: " + data.edge.Type
		case formExecuteQuery:
			if data.request == nil {
				return request, errors.New("query request is missing")
			}
			request = *data.request
		default:
			return request, errors.New("unsupported confirmation form")
		}
	default:
		return request, fmt.Errorf("unsupported form data %T", m.data)
	}
	return request, nil
}

func formTheme(noColor bool) huh.Theme {
	if !noColor {
		return huh.ThemeFunc(huh.ThemeCharm)
	}
	return huh.ThemeFunc(func(bool) *huh.Styles {
		styles := huh.ThemeBase(false)
		button := lipgloss.NewStyle().Padding(0, 2).MarginRight(1)
		styles.Focused.FocusedButton = button.Border(lipgloss.NormalBorder())
		styles.Focused.BlurredButton = button
		styles.Focused.TextInput.Placeholder = lipgloss.NewStyle()
		styles.Blurred.FocusedButton = styles.Focused.FocusedButton
		styles.Blurred.BlurredButton = styles.Focused.BlurredButton
		styles.Blurred.TextInput.Placeholder = lipgloss.NewStyle()
		styles.Help = help.Styles{}
		return styles
	})
}

func parentField(title, description string, value *string, graph graphState, moving domain.EntityID) *huh.Select[string] {
	excluded := map[domain.EntityID]bool{}
	if moving != "" {
		excluded[moving] = true
		for id := range graph.descendants(moving) {
			excluded[id] = true
		}
	}
	options := []huh.Option[string]{huh.NewOption("(root · no parent)", "")}
	options = append(options, nodeOptions(graph, excluded)...)
	return huh.NewSelect[string]().Title(title).Description(description).Options(options...).Height(8).Value(value)
}

func nodeField(title, description string, value *string, graph graphState, exclude domain.EntityID) *huh.Select[string] {
	excluded := map[domain.EntityID]bool{}
	if exclude != "" {
		excluded[exclude] = true
	}
	return huh.NewSelect[string]().Title(title).Description(description).Options(nodeOptions(graph, excluded)...).Height(8).Value(value)
}

func nodeOptions(graph graphState, excluded map[domain.EntityID]bool) []huh.Option[string] {
	nodes := append([]domain.Node(nil), graph.nodes...)
	sort.SliceStable(nodes, func(i, j int) bool {
		left, right := strings.ToLower(nodeTitle(nodes[i])), strings.ToLower(nodeTitle(nodes[j]))
		if left != right {
			return left < right
		}
		return nodes[i].ID < nodes[j].ID
	})
	options := make([]huh.Option[string], 0, len(nodes))
	for _, node := range nodes {
		if excluded[node.ID] {
			continue
		}
		label := fmt.Sprintf("%s · %s", nodeTitle(node), shortID(node.ID))
		options = append(options, huh.NewOption(label, string(node.ID)))
	}
	return options
}

func validateLabelInput(value string) error {
	_, err := parseLabels(value)
	return err
}

func parseLabels(value string) ([]string, error) {
	if strings.TrimSpace(value) == "" {
		return nil, nil
	}
	parts := strings.Split(value, ",")
	labels := make([]string, 0, len(parts))
	seen := make(map[string]bool, len(parts))
	for _, part := range parts {
		label := strings.TrimSpace(part)
		if label == "" {
			return nil, errors.New("labels cannot contain an empty entry")
		}
		if seen[label] {
			return nil, fmt.Errorf("label %q appears more than once", label)
		}
		seen[label] = true
		labels = append(labels, label)
	}
	return labels, nil
}

func validateNodePropertiesJSON(value string) error {
	_, err := decodeNodeProperties(value)
	return err
}

func validateRelationshipPropertiesJSON(value string) error {
	_, err := decodeRelationshipProperties(value)
	return err
}

func decodeNodeProperties(value string) (domain.Properties, error) {
	properties, err := decodeProperties(value)
	if err != nil {
		return nil, err
	}
	for _, reserved := range []string{"body"} {
		if _, exists := properties[reserved]; exists {
			return nil, fmt.Errorf("property %q is reserved; use its dedicated form field", reserved)
		}
	}
	return properties, nil
}

func decodeRelationshipProperties(value string) (domain.Properties, error) {
	properties, err := decodeProperties(value)
	if err != nil {
		return nil, err
	}
	for _, reserved := range []string{"position"} {
		if _, exists := properties[reserved]; exists {
			return nil, fmt.Errorf("property %q is reserved; use Move / order for CHILD position", reserved)
		}
	}
	return properties, nil
}

func decodeProperties(value string) (domain.Properties, error) {
	return decodeJSONObject(value, true)
}

func decodeJSONObject(value string, decodeFloatTags bool) (domain.Properties, error) {
	decoder := json.NewDecoder(strings.NewReader(value))
	decoder.UseNumber()
	var properties map[string]any
	if err := decoder.Decode(&properties); err != nil {
		return nil, fmt.Errorf("properties must be a JSON object: %w", err)
	}
	if properties == nil {
		return nil, errors.New("properties must be a JSON object, not null")
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return nil, errors.New("properties contain an extra JSON value")
		}
		return nil, fmt.Errorf("properties contain trailing data: %w", err)
	}
	normalized, err := normalizeJSONMap(properties, decodeFloatTags)
	if err != nil {
		return nil, err
	}
	return domain.Properties(normalized), nil
}

func decodeParams(value string) (map[string]any, error) {
	// Query parameters deliberately follow the CLI's plain-JSON input
	// contract. $float objects are an editable-property extension, not an
	// alternate meaning for the same CLI/TUI console parameter document.
	properties, err := decodeJSONObject(value, false)
	return map[string]any(properties), err
}

func normalizeJSONMap(values map[string]any, decodeFloatTags bool) (map[string]any, error) {
	for key, value := range values {
		normalized, err := normalizeJSONValue(value, decodeFloatTags)
		if err != nil {
			return nil, fmt.Errorf("property %q: %w", key, err)
		}
		values[key] = normalized
	}
	return values, nil
}

func normalizeJSONValue(value any, decodeFloatTags bool) (any, error) {
	switch value := value.(type) {
	case json.Number:
		text := value.String()
		if jsonIntegerLiteral.MatchString(text) {
			integer, err := strconv.ParseInt(text, 10, 64)
			if err != nil {
				return nil, fmt.Errorf("integer %q is outside the signed 64-bit range", text)
			}
			return integer, nil
		}
		decimal, err := strconv.ParseFloat(text, 64)
		if err != nil {
			return nil, fmt.Errorf("invalid number %q", text)
		}
		return decimal, nil
	case []any:
		for index, item := range value {
			normalized, err := normalizeJSONValue(item, decodeFloatTags)
			if err != nil {
				return nil, fmt.Errorf("item %d: %w", index, err)
			}
			value[index] = normalized
		}
		return value, nil
	case map[string]any:
		if decodeFloatTags && len(value) == 1 {
			if marker, tagged := value["$float"]; tagged {
				name, ok := marker.(string)
				if !ok {
					return nil, errors.New("$float marker must be a string")
				}
				switch name {
				case "NaN":
					return math.NaN(), nil
				case "+Infinity":
					return math.Inf(1), nil
				case "-Infinity":
					return math.Inf(-1), nil
				default:
					return nil, fmt.Errorf("unknown $float marker %q", name)
				}
			}
		}
		return normalizeJSONMap(value, decodeFloatTags)
	default:
		return value, nil
	}
}

var jsonIntegerLiteral = regexp.MustCompile(`^-?(?:0|[1-9][0-9]*)$`)

// restorePropertyMapTypes preserves durable types that JSON represents only
// ambiguously. Edit forms start from prettyJSON(original), so corresponding
// temporal, duration, byte, and floating values remain the same type even
// when another property in the containing object is changed.
func restorePropertyMapTypes(values domain.Properties, originals domain.Properties) (domain.Properties, error) {
	for key, value := range values {
		original, exists := originals[key]
		if !exists {
			continue
		}
		restored, err := restorePropertyType(value, original)
		if err != nil {
			return nil, fmt.Errorf("property %q: %w", key, err)
		}
		values[key] = restored
	}
	return values, nil
}

func restorePropertyType(value, original any) (any, error) {
	switch original := original.(type) {
	case time.Time:
		text, ok := value.(string)
		if !ok {
			return nil, errors.New("temporal value must remain an RFC3339 string")
		}
		parsed, err := time.Parse(time.RFC3339Nano, text)
		if err != nil {
			return nil, fmt.Errorf("invalid temporal value: %w", err)
		}
		if original.Location() != nil {
			parsed = parsed.In(original.Location())
		}
		return parsed, nil
	case time.Duration:
		integer, ok := value.(int64)
		if !ok {
			return nil, errors.New("duration must remain an integer number of nanoseconds")
		}
		return time.Duration(integer), nil
	case []byte:
		text, ok := value.(string)
		if !ok {
			return nil, errors.New("byte value must remain a base64 string")
		}
		decoded, err := base64.StdEncoding.DecodeString(text)
		if err != nil {
			return nil, fmt.Errorf("invalid base64 byte value: %w", err)
		}
		return decoded, nil
	case float32, float64:
		switch number := value.(type) {
		case int64:
			return float64(number), nil
		case float64:
			return number, nil
		default:
			return nil, fmt.Errorf("floating value has incompatible JSON type %T", value)
		}
	case domain.Properties:
		values, ok := value.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("nested object has incompatible JSON type %T", value)
		}
		return restorePropertyMapTypes(domain.Properties(values), original)
	case map[string]any:
		values, ok := value.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("nested object has incompatible JSON type %T", value)
		}
		return restorePropertyMapTypes(domain.Properties(values), domain.Properties(original))
	case []any:
		values, ok := value.([]any)
		if !ok {
			return nil, fmt.Errorf("nested list has incompatible JSON type %T", value)
		}
		for index := range values {
			if index >= len(original) {
				break
			}
			restored, err := restorePropertyType(values[index], original[index])
			if err != nil {
				return nil, fmt.Errorf("item %d: %w", index, err)
			}
			values[index] = restored
		}
		return values, nil
	default:
		return value, nil
	}
}

func validateOptionalInteger(value string) error {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	_, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
	if err != nil {
		return errors.New("position must be a whole number or blank")
	}
	return nil
}

func childPattern(position string, params map[string]any) (string, error) {
	parsed, err := parseOptionalPosition(position)
	if err != nil {
		return "", err
	}
	if parsed == nil {
		return "[:CHILD]", nil
	}
	params["position"] = *parsed
	return "[:CHILD {position: $position}]", nil
}

func parseOptionalPosition(value string) (*int64, error) {
	if strings.TrimSpace(value) == "" {
		return nil, nil
	}
	parsed, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
	if err != nil {
		return nil, errors.New("position must be a whole number or blank")
	}
	return &parsed, nil
}

func equalOptionalPosition(left, right *int64) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}

func optionalPositionParameter(position *int64) any {
	if position == nil {
		return nil
	}
	return *position
}

func validateRelationshipType(value string) error {
	value = strings.TrimSpace(value)
	if value == "" {
		return errors.New("relationship type is required")
	}
	if strings.EqualFold(value, "CHILD") {
		return errors.New("use Move / order to create CHILD hierarchy relationships")
	}
	return nil
}

var bareCypherIdentifier = regexp.MustCompile(`^[\pL_][\pL\pN_]*$`)

func cypherLabels(labels []string) string {
	var result strings.Builder
	for _, label := range labels {
		result.WriteByte(':')
		result.WriteString(cypherIdentifierName(label))
	}
	return result.String()
}

func cypherIdentifierName(value string) string {
	if bareCypherIdentifier.MatchString(value) {
		return value
	}
	return "`" + strings.ReplaceAll(value, "`", "``") + "`"
}

func copyProperties(properties domain.Properties) domain.Properties {
	copy := make(domain.Properties, len(properties))
	for key, value := range properties {
		copy[key] = value
	}
	return copy
}
