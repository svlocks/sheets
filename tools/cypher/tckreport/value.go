package main

import (
	"fmt"
	"math"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/svlocks/sheets/internal/domain"
	"github.com/svlocks/sheets/internal/engine"
)

type normalizedValue struct {
	kind       string
	scalar     string
	items      []normalizedValue
	properties map[string]normalizedValue
	labels     []string
	directions []string
}

func (v normalizedValue) key(ignoreListOrder bool) string {
	switch v.kind {
	case "null", "bool", "int", "float", "string", "temporal", "duration":
		return v.kind + ":" + v.scalar
	case "list":
		items := make([]string, len(v.items))
		for index, item := range v.items {
			items[index] = item.key(ignoreListOrder)
		}
		if ignoreListOrder {
			sort.Strings(items)
		}
		return "list:[" + strings.Join(items, ",") + "]"
	case "map":
		return "map:" + propertiesKey(v.properties, ignoreListOrder)
	case "node":
		labels := append([]string(nil), v.labels...)
		sort.Strings(labels)
		return "node:[" + strings.Join(labels, ",") + "]" + propertiesKey(v.properties, ignoreListOrder)
	case "relationship":
		return "relationship:" + v.scalar + propertiesKey(v.properties, ignoreListOrder)
	case "path":
		parts := make([]string, 0, len(v.items)+len(v.directions))
		if len(v.items) > 0 {
			parts = append(parts, v.items[0].key(ignoreListOrder))
		}
		for index, direction := range v.directions {
			parts = append(parts, direction)
			edgeIndex := index*2 + 1
			nodeIndex := edgeIndex + 1
			if edgeIndex < len(v.items) {
				parts = append(parts, v.items[edgeIndex].key(ignoreListOrder))
			}
			if nodeIndex < len(v.items) {
				parts = append(parts, v.items[nodeIndex].key(ignoreListOrder))
			}
		}
		return "path:<" + strings.Join(parts, "|") + ">"
	default:
		return "unknown:" + v.scalar
	}
}

func propertiesKey(properties map[string]normalizedValue, ignoreListOrder bool) string {
	keys := make([]string, 0, len(properties))
	for key := range properties {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		parts = append(parts, strconv.Quote(key)+":"+properties[key].key(ignoreListOrder))
	}
	return "{" + strings.Join(parts, ",") + "}"
}

func normalizeActual(value any) (normalizedValue, error) {
	switch value := value.(type) {
	case nil:
		return normalizedValue{kind: "null"}, nil
	case bool:
		return normalizedValue{kind: "bool", scalar: strconv.FormatBool(value)}, nil
	case string:
		return normalizedValue{kind: "string", scalar: strconv.Quote(value)}, nil
	case int:
		return normalizedValue{kind: "int", scalar: strconv.FormatInt(int64(value), 10)}, nil
	case int8:
		return normalizedValue{kind: "int", scalar: strconv.FormatInt(int64(value), 10)}, nil
	case int16:
		return normalizedValue{kind: "int", scalar: strconv.FormatInt(int64(value), 10)}, nil
	case int32:
		return normalizedValue{kind: "int", scalar: strconv.FormatInt(int64(value), 10)}, nil
	case int64:
		return normalizedValue{kind: "int", scalar: strconv.FormatInt(value, 10)}, nil
	case uint:
		return normalizedValue{kind: "int", scalar: strconv.FormatUint(uint64(value), 10)}, nil
	case uint8:
		return normalizedValue{kind: "int", scalar: strconv.FormatUint(uint64(value), 10)}, nil
	case uint16:
		return normalizedValue{kind: "int", scalar: strconv.FormatUint(uint64(value), 10)}, nil
	case uint32:
		return normalizedValue{kind: "int", scalar: strconv.FormatUint(uint64(value), 10)}, nil
	case uint64:
		return normalizedValue{kind: "int", scalar: strconv.FormatUint(value, 10)}, nil
	case float32:
		return normalizedFloat(float64(value)), nil
	case float64:
		return normalizedFloat(value), nil
	case time.Time:
		// M23's TCK API has no temporal CypherValue variant. Its adapters
		// serialize temporal results into the canonical CypherString notation
		// used by the feature tables.
		return normalizedValue{kind: "string", scalar: strconv.Quote(formatTCKTemporal(value))}, nil
	case time.Duration:
		return normalizedValue{kind: "string", scalar: strconv.Quote(formatTCKDuration(value))}, nil
	case domain.Node:
		return normalizeNode(value)
	case *domain.Node:
		if value == nil {
			return normalizedValue{kind: "null"}, nil
		}
		return normalizeNode(*value)
	case domain.Edge:
		return normalizeRelationship(value)
	case *domain.Edge:
		if value == nil {
			return normalizedValue{kind: "null"}, nil
		}
		return normalizeRelationship(*value)
	case engine.PathValue:
		return normalizePath(value)
	}

	reflected := reflect.ValueOf(value)
	if reflected.IsValid() && (reflected.Kind() == reflect.Slice || reflected.Kind() == reflect.Array) {
		items := make([]normalizedValue, reflected.Len())
		for index := 0; index < reflected.Len(); index++ {
			item, err := normalizeActual(reflected.Index(index).Interface())
			if err != nil {
				return normalizedValue{}, err
			}
			items[index] = item
		}
		return normalizedValue{kind: "list", items: items}, nil
	}
	if reflected.IsValid() && reflected.Kind() == reflect.Map && reflected.Type().Key().Kind() == reflect.String {
		properties := make(map[string]normalizedValue, reflected.Len())
		iterator := reflected.MapRange()
		for iterator.Next() {
			item, err := normalizeActual(iterator.Value().Interface())
			if err != nil {
				return normalizedValue{}, err
			}
			properties[iterator.Key().String()] = item
		}
		return normalizedValue{kind: "map", properties: properties}, nil
	}
	return normalizedValue{}, fmt.Errorf("unsupported result value %T", value)
}

func formatTCKTemporal(value time.Time) string {
	clock := value.Format("15:04:05.999999999")
	if value.Year() == 0 {
		if value.Location() == time.Local {
			return clock
		}
		_, offset := value.Zone()
		if offset == 0 {
			return clock + "Z"
		}
		return clock + value.Format("-07:00")
	}
	if value.Location() == time.Local {
		return value.Format("2006-01-02T15:04:05.999999999")
	}
	if value.Location() == time.UTC && value.Hour() == 0 && value.Minute() == 0 &&
		value.Second() == 0 && value.Nanosecond() == 0 {
		return value.Format("2006-01-02")
	}
	return value.Format(time.RFC3339Nano)
}

func formatTCKDuration(value time.Duration) string {
	var result strings.Builder
	if value < 0 {
		result.WriteByte('-')
		value = -value
	}
	result.WriteByte('P')
	days := value / (24 * time.Hour)
	value %= 24 * time.Hour
	if days != 0 {
		fmt.Fprintf(&result, "%dD", days)
	}
	if value == 0 {
		if days == 0 {
			result.WriteString("T0S")
		}
		return result.String()
	}
	result.WriteByte('T')
	hours := value / time.Hour
	value %= time.Hour
	minutes := value / time.Minute
	value %= time.Minute
	if hours != 0 {
		fmt.Fprintf(&result, "%dH", hours)
	}
	if minutes != 0 {
		fmt.Fprintf(&result, "%dM", minutes)
	}
	if value != 0 {
		seconds := float64(value) / float64(time.Second)
		result.WriteString(strconv.FormatFloat(seconds, 'f', -1, 64))
		result.WriteByte('S')
	}
	return result.String()
}

func normalizedFloat(value float64) normalizedValue {
	scalar := ""
	switch {
	case math.IsNaN(value):
		scalar = "NaN"
	case math.IsInf(value, 1):
		scalar = "+Infinity"
	case math.IsInf(value, -1):
		scalar = "-Infinity"
	case value == 0:
		// Cypher numeric equality does not distinguish signed zero, and the TCK
		// result notation canonicalizes both representations as 0.0.
		scalar = "0"
	default:
		scalar = strconv.FormatFloat(value, 'g', -1, 64)
	}
	return normalizedValue{kind: "float", scalar: scalar}
}

func normalizeNode(node domain.Node) (normalizedValue, error) {
	properties, err := normalizeProperties(node.Properties)
	if err != nil {
		return normalizedValue{}, err
	}
	if node.Body != "" {
		properties["body"] = normalizedValue{kind: "string", scalar: strconv.Quote(node.Body)}
	}
	return normalizedValue{kind: "node", labels: append([]string(nil), node.Labels...), properties: properties}, nil
}

func normalizeRelationship(edge domain.Edge) (normalizedValue, error) {
	properties, err := normalizeProperties(edge.Properties)
	if err != nil {
		return normalizedValue{}, err
	}
	if edge.Position != nil {
		properties["position"] = normalizedValue{kind: "int", scalar: strconv.FormatInt(*edge.Position, 10)}
	}
	return normalizedValue{kind: "relationship", scalar: edge.Type, properties: properties}, nil
}

func normalizeProperties(source domain.Properties) (map[string]normalizedValue, error) {
	properties := make(map[string]normalizedValue, len(source))
	for key, value := range source {
		normalized, err := normalizeActual(value)
		if err != nil {
			return nil, err
		}
		properties[key] = normalized
	}
	return properties, nil
}

func normalizePath(path engine.PathValue) (normalizedValue, error) {
	items := make([]normalizedValue, 0, len(path.Nodes)+len(path.Relationships))
	directions := make([]string, 0, len(path.Relationships))
	for index, node := range path.Nodes {
		normalized, err := normalizeNode(node)
		if err != nil {
			return normalizedValue{}, err
		}
		items = append(items, normalized)
		if index >= len(path.Relationships) {
			continue
		}
		edge := path.Relationships[index]
		normalizedEdge, err := normalizeRelationship(edge)
		if err != nil {
			return normalizedValue{}, err
		}
		items = append(items, normalizedEdge)
		if index+1 < len(path.Nodes) && edge.From == node.ID && edge.To == path.Nodes[index+1].ID {
			directions = append(directions, "out")
		} else {
			directions = append(directions, "in")
		}
	}
	return normalizedValue{kind: "path", items: items, directions: directions}, nil
}

type valueParser struct {
	source string
	offset int
}

func parseExpectedValue(source string) (normalizedValue, error) {
	parser := &valueParser{source: strings.TrimSpace(source)}
	value, err := parser.value()
	if err != nil {
		return normalizedValue{}, err
	}
	parser.space()
	if parser.offset != len(parser.source) {
		return normalizedValue{}, fmt.Errorf("unsupported trailing value syntax %q", parser.source[parser.offset:])
	}
	return value, nil
}

func (p *valueParser) value() (normalizedValue, error) {
	p.space()
	if p.offset >= len(p.source) {
		return normalizedValue{}, fmt.Errorf("empty value")
	}
	switch p.source[p.offset] {
	case '\'', '"':
		value, err := p.string()
		return normalizedValue{kind: "string", scalar: strconv.Quote(value)}, err
	case '[':
		if p.nextNonSpace(p.offset+1) < len(p.source) && p.source[p.nextNonSpace(p.offset+1)] == ':' {
			return p.relationship()
		}
		return p.list()
	case '{':
		properties, err := p.mapValue()
		return normalizedValue{kind: "map", properties: properties}, err
	case '(':
		return p.node()
	case '<':
		return p.path()
	}
	return p.atom()
}

func (p *valueParser) atom() (normalizedValue, error) {
	start := p.offset
	for p.offset < len(p.source) && !strings.ContainsRune(" \t\r\n,]}>)", rune(p.source[p.offset])) {
		p.offset++
	}
	raw := p.source[start:p.offset]
	switch strings.ToLower(raw) {
	case "null":
		return normalizedValue{kind: "null"}, nil
	case "true", "false":
		return normalizedValue{kind: "bool", scalar: strings.ToLower(raw)}, nil
	case "nan":
		return normalizedValue{kind: "float", scalar: "NaN"}, nil
	case "infinity", "+infinity", "inf", "+inf":
		return normalizedValue{kind: "float", scalar: "+Infinity"}, nil
	case "-infinity", "-inf":
		return normalizedValue{kind: "float", scalar: "-Infinity"}, nil
	}
	if integer, err := strconv.ParseInt(raw, 0, 64); err == nil {
		return normalizedValue{kind: "int", scalar: strconv.FormatInt(integer, 10)}, nil
	}
	if number, err := strconv.ParseFloat(raw, 64); err == nil {
		return normalizedFloat(number), nil
	}
	return normalizedValue{}, fmt.Errorf("unsupported value atom %q", raw)
}

func (p *valueParser) list() (normalizedValue, error) {
	p.offset++
	items := make([]normalizedValue, 0)
	for {
		p.space()
		if p.consume("]") {
			return normalizedValue{kind: "list", items: items}, nil
		}
		item, err := p.value()
		if err != nil {
			return normalizedValue{}, err
		}
		items = append(items, item)
		p.space()
		if !p.consume(",") && !p.hasPrefix("]") {
			return normalizedValue{}, fmt.Errorf("expected ',' or ']' at %q", p.source[p.offset:])
		}
	}
}

func (p *valueParser) mapValue() (map[string]normalizedValue, error) {
	p.offset++
	properties := make(map[string]normalizedValue)
	for {
		p.space()
		if p.consume("}") {
			return properties, nil
		}
		key, err := p.name()
		if err != nil {
			return nil, err
		}
		p.space()
		if !p.consume(":") {
			return nil, fmt.Errorf("expected ':' after map key %q", key)
		}
		value, err := p.value()
		if err != nil {
			return nil, err
		}
		properties[key] = value
		p.space()
		if !p.consume(",") && !p.hasPrefix("}") {
			return nil, fmt.Errorf("expected ',' or '}' at %q", p.source[p.offset:])
		}
	}
}

func (p *valueParser) node() (normalizedValue, error) {
	p.offset++
	labels := make([]string, 0)
	properties := make(map[string]normalizedValue)
	p.space()
	if !p.hasPrefix(":") && !p.hasPrefix("{") && !p.hasPrefix(")") {
		if _, err := p.name(); err != nil {
			return normalizedValue{}, err
		}
	}
	for {
		p.space()
		if !p.consume(":") {
			break
		}
		label, err := p.name()
		if err != nil {
			return normalizedValue{}, err
		}
		labels = append(labels, label)
	}
	p.space()
	if p.hasPrefix("{") {
		var err error
		properties, err = p.mapValue()
		if err != nil {
			return normalizedValue{}, err
		}
	}
	p.space()
	if !p.consume(")") {
		return normalizedValue{}, fmt.Errorf("expected ')' at %q", p.source[p.offset:])
	}
	return normalizedValue{kind: "node", labels: labels, properties: properties}, nil
}

func (p *valueParser) relationship() (normalizedValue, error) {
	p.offset++
	p.space()
	if !p.hasPrefix(":") && !p.hasPrefix("{") && !p.hasPrefix("]") {
		if _, err := p.name(); err != nil {
			return normalizedValue{}, err
		}
	}
	relationshipType := ""
	p.space()
	if p.consume(":") {
		var err error
		relationshipType, err = p.name()
		if err != nil {
			return normalizedValue{}, err
		}
	}
	p.space()
	properties := make(map[string]normalizedValue)
	if p.hasPrefix("{") {
		var err error
		properties, err = p.mapValue()
		if err != nil {
			return normalizedValue{}, err
		}
	}
	p.space()
	if !p.consume("]") {
		return normalizedValue{}, fmt.Errorf("expected ']' at %q", p.source[p.offset:])
	}
	return normalizedValue{kind: "relationship", scalar: relationshipType, properties: properties}, nil
}

func (p *valueParser) path() (normalizedValue, error) {
	p.offset++
	first, err := p.node()
	if err != nil {
		return normalizedValue{}, err
	}
	items := []normalizedValue{first}
	directions := make([]string, 0)
	for {
		p.space()
		if p.consume(">") {
			return normalizedValue{kind: "path", items: items, directions: directions}, nil
		}
		direction := "out"
		if p.consume("<-") {
			direction = "in"
		} else if !p.consume("-") {
			return normalizedValue{}, fmt.Errorf("expected path connector at %q", p.source[p.offset:])
		}
		relationship, err := p.relationship()
		if err != nil {
			return normalizedValue{}, err
		}
		if direction == "in" {
			if !p.consume("-") {
				return normalizedValue{}, fmt.Errorf("expected '-' after incoming relationship")
			}
		} else if p.consume("->") {
			direction = "out"
		} else if p.consume("-") {
			direction = "undirected"
		} else {
			return normalizedValue{}, fmt.Errorf("expected relationship arrow at %q", p.source[p.offset:])
		}
		node, err := p.node()
		if err != nil {
			return normalizedValue{}, err
		}
		items = append(items, relationship, node)
		directions = append(directions, direction)
	}
}

func (p *valueParser) name() (string, error) {
	p.space()
	if p.offset >= len(p.source) {
		return "", fmt.Errorf("expected name at end of value")
	}
	if p.source[p.offset] == '`' {
		p.offset++
		var result strings.Builder
		for p.offset < len(p.source) {
			if p.source[p.offset] != '`' {
				result.WriteByte(p.source[p.offset])
				p.offset++
				continue
			}
			p.offset++
			if p.offset < len(p.source) && p.source[p.offset] == '`' {
				result.WriteByte('`')
				p.offset++
				continue
			}
			return result.String(), nil
		}
		return "", fmt.Errorf("unterminated escaped name")
	}
	if p.source[p.offset] == '\'' || p.source[p.offset] == '"' {
		return p.string()
	}
	start := p.offset
	for p.offset < len(p.source) && !strings.ContainsRune(" \t\r\n,:{}[]()<>=-", rune(p.source[p.offset])) {
		p.offset++
	}
	if start == p.offset {
		return "", fmt.Errorf("expected name at %q", p.source[p.offset:])
	}
	return p.source[start:p.offset], nil
}

func (p *valueParser) string() (string, error) {
	quote := p.source[p.offset]
	p.offset++
	var result strings.Builder
	for p.offset < len(p.source) {
		character := p.source[p.offset]
		p.offset++
		if character == quote {
			return result.String(), nil
		}
		if character != '\\' {
			result.WriteByte(character)
			continue
		}
		if p.offset >= len(p.source) {
			return "", fmt.Errorf("unterminated string escape")
		}
		escape := p.source[p.offset]
		p.offset++
		switch escape {
		case '\\', '\'':
			// This parses the TCK value notation, not a Cypher expression. The
			// M23 reference value parser recognizes only escaped apostrophes and
			// backslashes; Gherkin has already decoded table escapes such as \n.
			result.WriteByte(escape)
		default:
			return "", fmt.Errorf("unsupported string escape \\%c", escape)
		}
	}
	return "", fmt.Errorf("unterminated string")
}

func (p *valueParser) space() {
	for p.offset < len(p.source) && strings.ContainsRune(" \t\r\n", rune(p.source[p.offset])) {
		p.offset++
	}
}

func (p *valueParser) hasPrefix(value string) bool {
	return strings.HasPrefix(p.source[p.offset:], value)
}

func (p *valueParser) consume(value string) bool {
	if !p.hasPrefix(value) {
		return false
	}
	p.offset += len(value)
	return true
}

func (p *valueParser) nextNonSpace(offset int) int {
	for offset < len(p.source) && strings.ContainsRune(" \t\r\n", rune(p.source[offset])) {
		offset++
	}
	return offset
}
