package store

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/svlocks/sheets/internal/domain"
	"github.com/svlocks/sheets/internal/domain/temporal"
)

const (
	maxPropertyDepth  = 128
	maxPropertyValues = 1_000_000
	maxPropertyBytes  = 64 << 20
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

// An unnamed fixed offset is a real, lossless time zone representation (for
// example an RFC3339 value ending in -05:00). time.Time.UnmarshalBinary may
// otherwise substitute time.Local when that offset happens to match locally,
// which makes a second encoding differ from the bytes that the writer just
// produced. Keep an internal, non-loadable location name for decoded values;
// the wire format deliberately retains the historical empty Zone field.
const canonicalFixedZonePrefix = "sheets-fixed-offset:"

func canonicalFixedZone(offset int) string {
	return canonicalFixedZonePrefix + strconv.Itoa(offset)
}

func isCanonicalFixedZone(name string) bool {
	return strings.HasPrefix(name, canonicalFixedZonePrefix)
}

func encodeProperties(p domain.Properties) ([]byte, error) {
	state := encodeState{visiting: make(map[encodeReference]struct{})}
	v, err := state.encodeValue(p, 0)
	if err != nil {
		return nil, err
	}
	data, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("encode properties: %w", err)
	}
	if len(data) > maxPropertyBytes {
		return nil, fmt.Errorf("encode properties: encoded value exceeds %d bytes", maxPropertyBytes)
	}
	return data, nil
}

func decodeProperties(data []byte) (domain.Properties, error) {
	if len(data) > maxPropertyBytes {
		return nil, fmt.Errorf("decode properties: encoded value exceeds %d bytes", maxPropertyBytes)
	}
	var value encodedValue
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&value); err != nil {
		return nil, fmt.Errorf("decode properties: %w", err)
	}
	if err := ensureJSONEnd(decoder); err != nil {
		return nil, fmt.Errorf("decode properties: %w", err)
	}
	canonical, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("decode properties: canonicalize: %w", err)
	}
	if !bytes.Equal(data, canonical) {
		return nil, fmt.Errorf("decode properties: non-canonical or ambiguous encoding")
	}
	state := decodeState{}
	v, err := state.decodeValue(value, 0)
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
	// json.Marshal above proves that the JSON syntax and field layout are
	// canonical, but it cannot prove that scalar payloads are canonical.  For
	// example, strconv accepts both "+1" and "1" as an integer payload.  Encode
	// the decoded value once more so every accepted representation is exactly
	// one the writer itself could have produced.  This also checks canonical
	// base64, floating-point bit text, durations, and temporal payloads.
	reencoded, err := encodeProperties(p)
	if err != nil {
		return nil, fmt.Errorf("decode properties: recanonicalize: %w", err)
	}
	if !bytes.Equal(data, reencoded) {
		return nil, fmt.Errorf("decode properties: non-canonical scalar encoding")
	}
	return p, nil
}

type encodeReference struct {
	kind reflect.Kind
	ptr  uintptr
}

type encodeState struct {
	visiting map[encodeReference]struct{}
	values   int
}

func (s *encodeState) encodeValue(value any, depth int) (encodedValue, error) {
	if depth > maxPropertyDepth {
		return encodedValue{}, fmt.Errorf("property nesting exceeds %d levels", maxPropertyDepth)
	}
	s.values++
	if s.values > maxPropertyValues {
		return encodedValue{}, fmt.Errorf("property collection exceeds %d values", maxPropertyValues)
	}
	switch v := value.(type) {
	case nil:
		return encodedValue{Kind: "null"}, nil
	case bool:
		return encodedValue{Kind: "bool", Bool: v}, nil
	case string:
		if !utf8.ValidString(v) {
			return encodedValue{}, fmt.Errorf("property string is not valid UTF-8")
		}
		return encodedValue{Kind: "string", Text: v}, nil
	case []byte:
		return encodedValue{Kind: "bytes", Text: base64.StdEncoding.EncodeToString(v)}, nil
	case temporal.Date:
		return encodeTemporalBinary("date", v.MarshalBinary)
	case temporal.LocalTime:
		return encodeTemporalBinary("local_time", v.MarshalBinary)
	case temporal.Time:
		return encodeTemporalBinary("offset_time", v.MarshalBinary)
	case temporal.LocalDateTime:
		return encodeTemporalBinary("local_datetime", v.MarshalBinary)
	case temporal.DateTime:
		return encodeTemporalBinary("zoned_datetime", v.MarshalBinary)
	case temporal.Duration:
		return encodeTemporalBinary("cypher_duration", v.MarshalBinary)
	case time.Time:
		_, offset := v.Zone()
		location := v.Location().String()
		wireZone := location
		if location == "" || location == "Local" || isCanonicalFixedZone(location) {
			// Normalize an unnamed fixed offset before MarshalBinary so the
			// decoder never has to guess whether it was time.Local. Preserve the
			// empty/Local Zone field for compatibility with previously stored
			// values.
			v = v.In(time.FixedZone(canonicalFixedZone(offset), offset))
			if location == "" || isCanonicalFixedZone(location) {
				wireZone = ""
			}
		}
		data, err := v.MarshalBinary()
		if err != nil {
			return encodedValue{}, fmt.Errorf("encode time: %w", err)
		}
		if !utf8.ValidString(wireZone) {
			return encodedValue{}, fmt.Errorf("time zone name is not valid UTF-8")
		}
		return encodedValue{Kind: "time", Text: base64.StdEncoding.EncodeToString(data), Zone: wireZone, Offset: offset}, nil
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
		return s.encodeMap(map[string]any(v), depth)
	case map[string]any:
		return s.encodeMap(v, depth)
	case []any:
		return s.encodeList(v, depth)
	}

	rv := reflect.ValueOf(value)
	if !rv.IsValid() {
		return encodedValue{Kind: "null"}, nil
	}
	switch rv.Kind() {
	case reflect.Bool:
		return encodedValue{Kind: "bool", Bool: rv.Bool()}, nil
	case reflect.String:
		text := rv.String()
		if !utf8.ValidString(text) {
			return encodedValue{}, fmt.Errorf("property string is not valid UTF-8")
		}
		return encodedValue{Kind: "string", Text: text}, nil
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return integerValue(rv.Int()), nil
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		unsigned := rv.Uint()
		if unsigned > math.MaxInt64 {
			return encodedValue{}, fmt.Errorf("property unsigned integer %d exceeds int64", unsigned)
		}
		return integerValue(int64(unsigned)), nil
	case reflect.Float32, reflect.Float64:
		return floatValue(rv.Convert(reflect.TypeFor[float64]()).Float()), nil
	case reflect.Map:
		if rv.Type().Key().Kind() != reflect.String {
			return encodedValue{}, fmt.Errorf("unsupported property value %T", value)
		}
		if rv.IsNil() {
			return encodedValue{Kind: "null"}, nil
		}
		return s.encodeReflectedMap(rv, depth)
	case reflect.Slice:
		if rv.IsNil() {
			return encodedValue{Kind: "null"}, nil
		}
		if rv.Type().Elem().Kind() == reflect.Uint8 {
			bytesValue := make([]byte, rv.Len())
			for i := range bytesValue {
				bytesValue[i] = byte(rv.Index(i).Uint())
			}
			return encodedValue{Kind: "bytes", Text: base64.StdEncoding.EncodeToString(bytesValue)}, nil
		}
		fallthrough
	case reflect.Array:
		items := make([]any, rv.Len())
		for i := range items {
			items[i] = rv.Index(i).Interface()
		}
		if rv.Kind() == reflect.Slice {
			return s.withReference(rv, func() (encodedValue, error) { return s.encodeListItems(items, depth) })
		}
		return s.encodeListItems(items, depth)
	}
	return encodedValue{}, fmt.Errorf("unsupported property value %T", value)
}

func encodeTemporalBinary(kind string, marshal func() ([]byte, error)) (encodedValue, error) {
	data, err := marshal()
	if err != nil {
		return encodedValue{}, fmt.Errorf("encode %s: %w", kind, err)
	}
	return encodedValue{Kind: kind, Text: base64.StdEncoding.EncodeToString(data)}, nil
}

func integerValue(v int64) encodedValue {
	return encodedValue{Kind: "int", Text: strconv.FormatInt(v, 10)}
}

func floatValue(v float64) encodedValue {
	return encodedValue{Kind: "float", Text: strconv.FormatUint(math.Float64bits(v), 16)}
}

func (s *encodeState) encodeMap(values map[string]any, depth int) (encodedValue, error) {
	if values == nil {
		return encodedValue{Kind: "null"}, nil
	}
	rv := reflect.ValueOf(values)
	return s.withReference(rv, func() (encodedValue, error) {
		return s.encodeMapItems(values, depth)
	})
}

func (s *encodeState) encodeMapItems(values map[string]any, depth int) (encodedValue, error) {
	out := make(map[string]encodedValue, len(values))
	for key, value := range values {
		if !utf8.ValidString(key) {
			return encodedValue{}, fmt.Errorf("property map key is not valid UTF-8")
		}
		encoded, err := s.encodeValue(value, depth+1)
		if err != nil {
			return encodedValue{}, fmt.Errorf("property %q: %w", key, err)
		}
		out[key] = encoded
	}
	return encodedValue{Kind: "map", Map: out}, nil
}

func (s *encodeState) encodeReflectedMap(values reflect.Value, depth int) (encodedValue, error) {
	return s.withReference(values, func() (encodedValue, error) {
		out := make(map[string]encodedValue, values.Len())
		iterator := values.MapRange()
		for iterator.Next() {
			key := iterator.Key().String()
			if !utf8.ValidString(key) {
				return encodedValue{}, fmt.Errorf("property map key is not valid UTF-8")
			}
			encoded, err := s.encodeValue(iterator.Value().Interface(), depth+1)
			if err != nil {
				return encodedValue{}, fmt.Errorf("property %q: %w", key, err)
			}
			out[key] = encoded
		}
		return encodedValue{Kind: "map", Map: out}, nil
	})
}

func (s *encodeState) encodeList(values []any, depth int) (encodedValue, error) {
	if values == nil {
		return encodedValue{Kind: "null"}, nil
	}
	return s.withReference(reflect.ValueOf(values), func() (encodedValue, error) {
		return s.encodeListItems(values, depth)
	})
}

func (s *encodeState) encodeListItems(values []any, depth int) (encodedValue, error) {
	out := make([]encodedValue, len(values))
	for i, value := range values {
		encoded, err := s.encodeValue(value, depth+1)
		if err != nil {
			return encodedValue{}, fmt.Errorf("list item %d: %w", i, err)
		}
		out[i] = encoded
	}
	return encodedValue{Kind: "list", Items: out}, nil
}

func (s *encodeState) withReference(value reflect.Value, fn func() (encodedValue, error)) (encodedValue, error) {
	reference := encodeReference{kind: value.Kind(), ptr: value.Pointer()}
	if _, exists := s.visiting[reference]; exists {
		return encodedValue{}, fmt.Errorf("cyclic property value")
	}
	s.visiting[reference] = struct{}{}
	defer delete(s.visiting, reference)
	return fn()
}

type decodeState struct {
	values int
}

func (s *decodeState) decodeValue(v encodedValue, depth int) (any, error) {
	if depth > maxPropertyDepth {
		return nil, fmt.Errorf("decode properties: nesting exceeds %d levels", maxPropertyDepth)
	}
	s.values++
	if s.values > maxPropertyValues {
		return nil, fmt.Errorf("decode properties: collection exceeds %d values", maxPropertyValues)
	}
	if err := validateEncodedShape(v); err != nil {
		return nil, err
	}
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
	case "date":
		return decodeTemporalBinary(v.Text, "date", func(data []byte) (any, error) { return temporal.DateFromBinary(data) })
	case "local_time":
		return decodeTemporalBinary(v.Text, "local_time", func(data []byte) (any, error) { return temporal.LocalTimeFromBinary(data) })
	case "offset_time":
		return decodeTemporalBinary(v.Text, "offset_time", func(data []byte) (any, error) { return temporal.TimeFromBinary(data) })
	case "local_datetime":
		return decodeTemporalBinary(v.Text, "local_datetime", func(data []byte) (any, error) { return temporal.LocalDateTimeFromBinary(data) })
	case "zoned_datetime":
		return decodeTemporalBinary(v.Text, "zoned_datetime", func(data []byte) (any, error) { return temporal.DateTimeFromBinary(data) })
	case "cypher_duration":
		return decodeTemporalBinary(v.Text, "cypher_duration", func(data []byte) (any, error) { return temporal.DurationFromBinary(data) })
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
			candidate := result.In(location)
			_, actualOffset := candidate.Zone()
			if actualOffset != v.Offset {
				candidate = result.In(time.FixedZone(v.Zone, v.Offset))
			}
			result = candidate
		} else {
			// See canonicalFixedZone: never let UnmarshalBinary's opportunistic
			// time.Local reconstruction alter the representation of an unnamed
			// RFC3339 offset.
			result = result.In(time.FixedZone(canonicalFixedZone(v.Offset), v.Offset))
		}
		return result, nil
	case "duration":
		n, err := strconv.ParseInt(v.Text, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("decode duration: %w", err)
		}
		return time.Duration(n), nil
	case "int":
		n, err := strconv.ParseInt(v.Text, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("decode integer: %w", err)
		}
		return n, nil
	case "float":
		bits, err := strconv.ParseUint(v.Text, 16, 64)
		if err != nil {
			return nil, fmt.Errorf("decode float: %w", err)
		}
		return math.Float64frombits(bits), nil
	case "list":
		result := make([]any, len(v.Items))
		for i, item := range v.Items {
			decoded, err := s.decodeValue(item, depth+1)
			if err != nil {
				return nil, err
			}
			result[i] = decoded
		}
		return result, nil
	case "map":
		result := make(domain.Properties, len(v.Map))
		for key, item := range v.Map {
			decoded, err := s.decodeValue(item, depth+1)
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

func decodeTemporalBinary(text, kind string, decode func([]byte) (any, error)) (any, error) {
	data, err := base64.StdEncoding.DecodeString(text)
	if err != nil {
		return nil, fmt.Errorf("decode %s: %w", kind, err)
	}
	value, err := decode(data)
	if err != nil {
		return nil, fmt.Errorf("decode %s: %w", kind, err)
	}
	return value, nil
}

func validateEncodedShape(v encodedValue) error {
	noBool := !v.Bool
	noText := v.Text == ""
	noZone := v.Zone == "" && v.Offset == 0
	noItems := len(v.Items) == 0
	noMap := len(v.Map) == 0
	valid := false
	switch v.Kind {
	case "null":
		valid = noBool && noText && noZone && noItems && noMap
	case "bool":
		valid = noText && noZone && noItems && noMap
	case "string", "bytes", "duration", "int", "float",
		"date", "local_time", "offset_time", "local_datetime", "zoned_datetime", "cypher_duration":
		valid = noBool && noZone && noItems && noMap
	case "time":
		valid = noBool && noItems && noMap
	case "list":
		valid = noBool && noText && noZone && noMap
	case "map":
		valid = noBool && noText && noZone && noItems
	}
	if !valid {
		return fmt.Errorf("decode properties: invalid fields for value kind %q", v.Kind)
	}
	return nil
}

func ensureJSONEnd(decoder *json.Decoder) error {
	var extra any
	err := decoder.Decode(&extra)
	if err == io.EOF {
		return nil
	}
	if err == nil {
		return fmt.Errorf("multiple JSON values")
	}
	return err
}

func encodeLabels(labels []string) ([]byte, []string, error) {
	normalized := normalizeLabels(labels)
	for _, label := range normalized {
		if !utf8.ValidString(label) || strings.IndexByte(label, 0) >= 0 {
			return nil, nil, fmt.Errorf("label must be valid UTF-8 without NUL bytes")
		}
	}
	data, err := json.Marshal(normalized)
	if len(data) > maxPropertyBytes {
		return nil, nil, fmt.Errorf("encoded labels exceed %d bytes", maxPropertyBytes)
	}
	return data, normalized, err
}

func decodeLabels(data []byte) ([]string, error) {
	if len(data) > maxPropertyBytes {
		return nil, fmt.Errorf("decode labels: encoded value exceeds %d bytes", maxPropertyBytes)
	}
	var labels []string
	if err := json.Unmarshal(data, &labels); err != nil {
		return nil, fmt.Errorf("decode labels: %w", err)
	}
	normalized := normalizeLabels(labels)
	canonical, err := json.Marshal(normalized)
	if err != nil || !bytes.Equal(data, canonical) {
		return nil, fmt.Errorf("decode labels: non-canonical label encoding")
	}
	for _, label := range normalized {
		if !utf8.ValidString(label) || strings.IndexByte(label, 0) >= 0 {
			return nil, fmt.Errorf("decode labels: invalid UTF-8 or NUL byte")
		}
	}
	return normalized, nil
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
