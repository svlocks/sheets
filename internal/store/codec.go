package store

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"sort"
	"strconv"
	"time"

	"github.com/svlocks/sheets/internal/domain"
)

type encodedValue struct {
	Kind   string                  `json:"k"`
	Bool   bool                    `json:"b,omitempty"`
	Text   string                  `json:"s,omitempty"`
	Zone   string                  `json:"z,omitempty"`
	Offset int                     `json:"u,omitempty"`
	Items  []encodedValue          `json:"a,omitempty"`
	Map    map[string]encodedValue `json:"o,omitempty"`
}

func encodeProperties(p domain.Properties) ([]byte, error) {
	v, err := encodeValue(p)
	if err != nil {
		return nil, err
	}
	return json.Marshal(v)
}

func decodeProperties(data []byte) (domain.Properties, error) {
	var value encodedValue
	if err := json.Unmarshal(data, &value); err != nil {
		return nil, fmt.Errorf("decode properties: %w", err)
	}
	v, err := decodeValue(value)
	if err != nil {
		return nil, err
	}
	if v == nil {
		return nil, nil
	}
	p, ok := v.(domain.Properties)
	if !ok {
		return nil, fmt.Errorf("decode properties: root is %T, not a map", v)
	}
	return p, nil
}

func encodeValue(value any) (encodedValue, error) {
	switch v := value.(type) {
	case nil:
		return encodedValue{Kind: "null"}, nil
	case bool:
		return encodedValue{Kind: "bool", Bool: v}, nil
	case string:
		return encodedValue{Kind: "string", Text: v}, nil
	case []byte:
		return encodedValue{Kind: "bytes", Text: base64.StdEncoding.EncodeToString(v)}, nil
	case time.Time:
		data, err := v.MarshalBinary()
		if err != nil {
			return encodedValue{}, fmt.Errorf("encode time: %w", err)
		}
		zone, offset := v.Zone()
		location := v.Location().String()
		if location == "" {
			location = zone
		}
		return encodedValue{Kind: "time", Text: base64.StdEncoding.EncodeToString(data), Zone: location, Offset: offset}, nil
	case time.Duration:
		return encodedValue{Kind: "duration", Text: strconv.FormatInt(int64(v), 10)}, nil
	case int:
		return integerValue(int64(v)), nil
	case int8:
		return integerValue(int64(v)), nil
	case int16:
		return integerValue(int64(v)), nil
	case int32:
		return integerValue(int64(v)), nil
	case int64:
		return integerValue(v), nil
	case uint:
		if uint64(v) > math.MaxInt64 {
			return encodedValue{}, fmt.Errorf("property unsigned integer %d exceeds int64", v)
		}
		return integerValue(int64(v)), nil
	case uint8:
		return integerValue(int64(v)), nil
	case uint16:
		return integerValue(int64(v)), nil
	case uint32:
		return integerValue(int64(v)), nil
	case uint64:
		if v > math.MaxInt64 {
			return encodedValue{}, fmt.Errorf("property unsigned integer %d exceeds int64", v)
		}
		return integerValue(int64(v)), nil
	case float32:
		return floatValue(float64(v)), nil
	case float64:
		return floatValue(v), nil
	case domain.Properties:
		return encodeMap(map[string]any(v))
	case map[string]any:
		return encodeMap(v)
	case []any:
		return encodeList(v)
	}

	rv := reflect.ValueOf(value)
	if rv.IsValid() && (rv.Kind() == reflect.Slice || rv.Kind() == reflect.Array) {
		items := make([]any, rv.Len())
		for i := range items {
			items[i] = rv.Index(i).Interface()
		}
		return encodeList(items)
	}
	return encodedValue{}, fmt.Errorf("unsupported property value %T", value)
}

func integerValue(v int64) encodedValue {
	return encodedValue{Kind: "int", Text: strconv.FormatInt(v, 10)}
}

func floatValue(v float64) encodedValue {
	return encodedValue{Kind: "float", Text: strconv.FormatUint(math.Float64bits(v), 16)}
}

func encodeMap(values map[string]any) (encodedValue, error) {
	if values == nil {
		return encodedValue{Kind: "null"}, nil
	}
	out := make(map[string]encodedValue, len(values))
	for key, value := range values {
		encoded, err := encodeValue(value)
		if err != nil {
			return encodedValue{}, fmt.Errorf("property %q: %w", key, err)
		}
		out[key] = encoded
	}
	return encodedValue{Kind: "map", Map: out}, nil
}

func encodeList(values []any) (encodedValue, error) {
	if values == nil {
		return encodedValue{Kind: "null"}, nil
	}
	out := make([]encodedValue, len(values))
	for i, value := range values {
		encoded, err := encodeValue(value)
		if err != nil {
			return encodedValue{}, fmt.Errorf("list item %d: %w", i, err)
		}
		out[i] = encoded
	}
	return encodedValue{Kind: "list", Items: out}, nil
}

func decodeValue(v encodedValue) (any, error) {
	switch v.Kind {
	case "null":
		return nil, nil
	case "bool":
		return v.Bool, nil
	case "string":
		return v.Text, nil
	case "bytes":
		data, err := base64.StdEncoding.DecodeString(v.Text)
		if err != nil {
			return nil, fmt.Errorf("decode bytes: %w", err)
		}
		return data, nil
	case "time":
		data, err := base64.StdEncoding.DecodeString(v.Text)
		if err != nil {
			return nil, fmt.Errorf("decode time: %w", err)
		}
		var result time.Time
		if err := result.UnmarshalBinary(data); err != nil {
			return nil, fmt.Errorf("decode time: %w", err)
		}
		if v.Zone != "" {
			location, err := time.LoadLocation(v.Zone)
			if err != nil {
				location = time.FixedZone(v.Zone, v.Offset)
			}
			result = result.In(location)
		}
		return result, nil
	case "duration":
		n, err := strconv.ParseInt(v.Text, 10, 64)
		return time.Duration(n), err
	case "int":
		return strconv.ParseInt(v.Text, 10, 64)
	case "float":
		bits, err := strconv.ParseUint(v.Text, 16, 64)
		if err != nil {
			return nil, fmt.Errorf("decode float: %w", err)
		}
		return math.Float64frombits(bits), nil
	case "list":
		result := make([]any, len(v.Items))
		for i, item := range v.Items {
			decoded, err := decodeValue(item)
			if err != nil {
				return nil, err
			}
			result[i] = decoded
		}
		return result, nil
	case "map":
		result := make(domain.Properties, len(v.Map))
		for key, item := range v.Map {
			decoded, err := decodeValue(item)
			if err != nil {
				return nil, err
			}
			result[key] = decoded
		}
		return result, nil
	default:
		return nil, fmt.Errorf("decode properties: unknown value kind %q", v.Kind)
	}
}

func encodeLabels(labels []string) ([]byte, []string, error) {
	normalized := normalizeLabels(labels)
	data, err := json.Marshal(normalized)
	return data, normalized, err
}

func decodeLabels(data []byte) ([]string, error) {
	var labels []string
	if err := json.Unmarshal(data, &labels); err != nil {
		return nil, fmt.Errorf("decode labels: %w", err)
	}
	return labels, nil
}

func normalizeLabels(labels []string) []string {
	if labels == nil {
		return nil
	}
	copyOf := append([]string(nil), labels...)
	sort.Strings(copyOf)
	out := copyOf[:0]
	for _, label := range copyOf {
		if len(out) == 0 || out[len(out)-1] != label {
			out = append(out, label)
		}
	}
	return out
}
