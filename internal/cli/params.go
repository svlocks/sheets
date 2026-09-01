package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
)

// parameterInput describes the two CLI parameter forms. Object accepts a JSON
// object, @path, or - for stdin. Values contains name=JSON assignments.
type parameterInput struct {
	Object string
	Values []string
}

func (p parameterInput) load(stdin io.Reader) (map[string]any, error) {
	params := make(map[string]any)
	if p.Object != "" {
		data, err := readParameterSource(p.Object, stdin)
		if err != nil {
			return nil, err
		}
		if err := decodeJSON(data, &params); err != nil {
			return nil, fmt.Errorf("decode --params: %w", err)
		}
	}

	for _, assignment := range p.Values {
		name, value, ok := strings.Cut(assignment, "=")
		name = strings.TrimSpace(name)
		if !ok || name == "" {
			return nil, fmt.Errorf("invalid --param %q: expected name=JSON", assignment)
		}
		if _, exists := params[name]; exists {
			return nil, fmt.Errorf("parameter %q was supplied more than once", name)
		}
		var decoded any
		if err := decodeJSON([]byte(value), &decoded); err != nil {
			return nil, fmt.Errorf("decode --param %s: %w", name, err)
		}
		params[name] = decoded
	}
	return params, nil
}

func readParameterSource(source string, stdin io.Reader) ([]byte, error) {
	switch {
	case source == "-":
		data, err := io.ReadAll(stdin)
		if err != nil {
			return nil, fmt.Errorf("read parameters from stdin: %w", err)
		}
		return data, nil
	case strings.HasPrefix(source, "@"):
		if len(source) == 1 {
			return nil, fmt.Errorf("parameter file path is empty")
		}
		data, err := os.ReadFile(source[1:])
		if err != nil {
			return nil, fmt.Errorf("read parameter file: %w", err)
		}
		return data, nil
	default:
		return []byte(source), nil
	}
}

func decodeJSON(data []byte, dst any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := decoder.Decode(dst); err != nil {
		return err
	}
	if decoder.More() {
		return fmt.Errorf("unexpected content after JSON value")
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return fmt.Errorf("unexpected content after JSON value")
		}
		return err
	}
	return normalizeJSONNumbers(dst)
}

// normalizeJSONNumbers changes json.Number values in decoded data into int64
// or float64. This preserves integer semantics expected by Cypher.
func normalizeJSONNumbers(dst any) error {
	switch value := dst.(type) {
	case *map[string]any:
		normalized, err := normalizeMap(*value)
		if err != nil {
			return err
		}
		*value = normalized
		return nil
	case *any:
		normalized, err := normalizeJSONValue(*value)
		if err != nil {
			return err
		}
		*value = normalized
		return nil
	default:
		return fmt.Errorf("unsupported JSON destination %T", dst)
	}
}

func normalizeMap(input map[string]any) (map[string]any, error) {
	for key, value := range input {
		normalized, err := normalizeJSONValue(value)
		if err != nil {
			return nil, fmt.Errorf("parameter %q: %w", key, err)
		}
		input[key] = normalized
	}
	return input, nil
}

func normalizeJSONValue(value any) (any, error) {
	switch value := value.(type) {
	case json.Number:
		raw := value.String()
		if !strings.ContainsAny(raw, ".eE") {
			integer, err := strconv.ParseInt(raw, 10, 64)
			if err != nil {
				return nil, fmt.Errorf("integer %q is outside Cypher's signed 64-bit range", raw)
			}
			return integer, nil
		}
		decimal, err := strconv.ParseFloat(raw, 64)
		if err != nil {
			return nil, fmt.Errorf("invalid number %q", raw)
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
		return normalizeMap(value)
	default:
		return value, nil
	}
}
