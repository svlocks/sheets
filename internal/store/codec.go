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
	maxPropertyDepth  = domain.MaxPropertyDepth
	maxPropertyValues = domain.MaxPropertyValues
	maxPropertyBytes  = domain.MaxCanonicalPropertyBytes

	// Below this envelope, even a maximally fragmented tagged tree has a small,
	// fixed allocation ceiling. The materializing decoder's exact shape/count
	// checks are cheaper than a second tokenizing pass for ordinary CLI values.
	streamingPropertyPreflightThreshold = 64 << 10
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

// canonicalEncodedValueSize computes the exact size encoding/json will emit
// for encodedValue. Checking before Marshal prevents an escape-heavy string
// from forcing an allocation well beyond the durable 64 MiB ceiling.
func canonicalEncodedValueSize(value encodedValue) int64 {
	size := int64(2) // braces
	fields := 0
	addField := func(name string, valueSize int64) {
		if fields > 0 {
			size++
		}
		size += jsonStringSize(name) + 1 + valueSize
		fields++
	}
	addField("k", jsonStringSize(value.Kind))
	if value.Bool {
		addField("b", 4)
	}
	if value.Text != "" {
		addField("s", jsonStringSize(value.Text))
	}
	if value.Zone != "" {
		addField("z", jsonStringSize(value.Zone))
	}
	if value.Offset != 0 {
		addField("u", int64(len(strconv.Itoa(value.Offset))))
	}
	if len(value.Items) > 0 {
		itemsSize := int64(2)
		for index, item := range value.Items {
			if index > 0 {
				itemsSize++
			}
			itemsSize += canonicalEncodedValueSize(item)
		}
		addField("a", itemsSize)
	}
	if len(value.Map) > 0 {
		mapSize := int64(2)
		index := 0
		for key, item := range value.Map {
			if index > 0 {
				mapSize++
			}
			mapSize += jsonStringSize(key) + 1 + canonicalEncodedValueSize(item)
			index++
		}
		addField("o", mapSize)
	}
	return size
}

// jsonStringSize matches encoding/json's default HTML-safe string spelling.
// All callers validate UTF-8 before invoking it.
func jsonStringSize(value string) int64 {
	size := int64(2) // quotes
	for index := 0; index < len(value); {
		character := value[index]
		if character < utf8.RuneSelf {
			index++
			switch character {
			case '\\', '"', '\b', '\f', '\n', '\r', '\t':
				size += 2
			case '<', '>', '&':
				size += 6
			default:
				if character < 0x20 {
					size += 6
				} else {
					size++
				}
			}
			continue
		}
		r, width := utf8.DecodeRuneInString(value[index:])
		index += width
		if r == '\u2028' || r == '\u2029' {
			size += 6
		} else {
			size += int64(width)
		}
	}
	return size
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
	if err := preflightPropertyInput(p); err != nil {
		return nil, err
	}
	state := encodeState{visiting: make(map[encodeReference]struct{})}
	v, err := state.encodeValue(p, 0)
	if err != nil {
		return nil, err
	}
	if err := validateEncodedIndexBudget(v); err != nil {
		return nil, err
	}
	encodedSize := canonicalEncodedValueSize(v)
	if encodedSize > int64(maxPropertyBytes) {
		return nil, &domain.ResourceLimitError{
			Field: "canonical property encoding", Unit: "bytes",
			Limit: maxPropertyBytes, Actual: int(encodedSize),
		}
	}
	data, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("encode properties: %w", err)
	}
	if len(data) > maxPropertyBytes {
		return nil, &domain.ResourceLimitError{
			Field: "canonical property encoding", Unit: "bytes",
			Limit: maxPropertyBytes, Actual: len(data),
		}
	}
	return data, nil
}

func decodeProperties(data []byte) (domain.Properties, error) {
	if err := domain.ValidateBytes("canonical property encoding", data, maxPropertyBytes); err != nil {
		return nil, fmt.Errorf("decode properties: %w", err)
	}
	if len(data) > streamingPropertyPreflightThreshold {
		if err := preflightEncodedProperties(data); err != nil {
			return nil, fmt.Errorf("decode properties: %w", err)
		}
	} else if err := preflightSmallPropertyDepth(data); err != nil {
		return nil, fmt.Errorf("decode properties: %w", err)
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
	if err := validateEncodedIndexBudget(value); err != nil {
		return nil, fmt.Errorf("decode properties: %w", err)
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

// preflightSmallPropertyDepth is allocation-free. The byte threshold already
// bounds cardinality/materialization; this keeps a tiny but pathologically
// deep JSON value from reaching encoding/json's recursive object decoder.
func preflightSmallPropertyDepth(data []byte) error {
	const maximumStructuralDepth = 2*maxPropertyDepth + 2
	depth := 0
	inString := false
	escaped := false
	for _, character := range data {
		if inString {
			switch {
			case escaped:
				escaped = false
			case character == '\\':
				escaped = true
			case character == '"':
				inString = false
			}
			continue
		}
		switch character {
		case '"':
			inString = true
		case '{', '[':
			depth++
			if depth > maximumStructuralDepth {
				return &domain.ResourceLimitError{
					Field: "encoded property structural nesting", Unit: "levels",
					Limit: maximumStructuralDepth, Actual: depth,
				}
			}
		case '}', ']':
			depth--
		}
	}
	return nil
}

// These preflight passes account the complete input before the materializing
// codec constructs an encoded or decoded collection tree. They intentionally
// retain only recursion-stack references and the current JSON token.
func preflightPropertyInput(value any) error {
	state := propertyInputPreflight{}
	return state.value(value, 0)
}

type propertyInputPreflight struct {
	visiting      map[encodeReference]struct{}
	values        int
	base64Payload int
	indexedCount  int
	indexedBytes  int64
}

func (s *propertyInputPreflight) value(value any, depth int) error {
	if depth > maxPropertyDepth {
		return &domain.ResourceLimitError{
			Field: "property nesting", Unit: "levels",
			Limit: maxPropertyDepth, Actual: depth,
		}
	}
	s.values++
	if s.values > maxPropertyValues {
		return &domain.ResourceLimitError{
			Field: "property collection", Unit: "values",
			Limit: maxPropertyValues, Actual: s.values,
		}
	}

	switch typed := value.(type) {
	case string:
		return domain.ValidateText("property string", typed, domain.MaxPropertyScalarBytes)
	case []byte:
		return s.bytes(len(typed))
	case domain.Properties:
		return s.mapValue(map[string]any(typed), depth)
	case map[string]any:
		return s.mapValue(typed, depth)
	case []any:
		return s.listValue(typed, depth)
	case time.Time:
		name := typed.Location().String()
		if name == "" || isCanonicalFixedZone(name) {
			return nil
		}
		return domain.ValidateText("time zone name", name, domain.MaxTimeZoneNameBytes)
	}

	reflected := reflect.ValueOf(value)
	if !reflected.IsValid() {
		return nil
	}
	switch reflected.Kind() {
	case reflect.String:
		return domain.ValidateText("property string", reflected.String(), domain.MaxPropertyScalarBytes)
	case reflect.Map:
		if reflected.Type().Key().Kind() != reflect.String || reflected.IsNil() {
			return nil
		}
		return s.withReference(reflected, func() error {
			if err := s.children("property map", reflected.Len()); err != nil {
				return err
			}
			iterator := reflected.MapRange()
			for iterator.Next() {
				key := iterator.Key().String()
				if err := domain.ValidateText("property map key", key, domain.MaxPropertyKeyBytes); err != nil {
					return err
				}
				if err := s.value(iterator.Value().Interface(), depth+1); err != nil {
					return fmt.Errorf("property %q: %w", key, err)
				}
				if depth == 0 {
					scalar, encodedSize, err := scalarPropertyEncodedSize(iterator.Value().Interface())
					if err != nil {
						return fmt.Errorf("property %q: %w", key, err)
					}
					if scalar {
						if err := s.indexed(len(key), encodedSize); err != nil {
							return fmt.Errorf("property %q: %w", key, err)
						}
					}
				}
			}
			return nil
		})
	case reflect.Slice:
		if reflected.IsNil() {
			return nil
		}
		if reflected.Type().Elem().Kind() == reflect.Uint8 {
			return s.bytes(reflected.Len())
		}
		return s.withReference(reflected, func() error {
			return s.list(reflected, depth)
		})
	case reflect.Array:
		return s.list(reflected, depth)
	default:
		return nil
	}
}

func (s *propertyInputPreflight) bytes(length int) error {
	if length > domain.MaxPropertyScalarBytes {
		return &domain.ResourceLimitError{
			Field: "property byte string", Unit: "bytes",
			Limit: domain.MaxPropertyScalarBytes, Actual: length,
		}
	}
	encodedBytes := base64.StdEncoding.EncodedLen(length)
	if s.base64Payload > domain.MaxCanonicalPropertyBytes-encodedBytes {
		return &domain.ResourceLimitError{
			Field: "aggregate encoded property byte strings", Unit: "bytes",
			Limit: domain.MaxCanonicalPropertyBytes, Actual: s.base64Payload + encodedBytes,
		}
	}
	s.base64Payload += encodedBytes
	return nil
}

func (s *propertyInputPreflight) children(field string, count int) error {
	if count > maxPropertyValues-s.values {
		return &domain.ResourceLimitError{
			Field: field, Unit: "values",
			Limit: maxPropertyValues, Actual: s.values + count,
		}
	}
	return nil
}

func (s *propertyInputPreflight) list(values reflect.Value, depth int) error {
	if err := s.children("property list", values.Len()); err != nil {
		return err
	}
	for index := range values.Len() {
		if err := s.value(values.Index(index).Interface(), depth+1); err != nil {
			return fmt.Errorf("list item %d: %w", index, err)
		}
	}
	return nil
}

func (s *propertyInputPreflight) mapValue(values map[string]any, depth int) error {
	if values == nil {
		return nil
	}
	return s.withReference(reflect.ValueOf(values), func() error {
		if err := s.children("property map", len(values)); err != nil {
			return err
		}
		for key, value := range values {
			if err := domain.ValidateText("property map key", key, domain.MaxPropertyKeyBytes); err != nil {
				return err
			}
			if err := s.value(value, depth+1); err != nil {
				return fmt.Errorf("property %q: %w", key, err)
			}
			if depth == 0 {
				scalar, encodedSize, err := scalarPropertyEncodedSize(value)
				if err != nil {
					return fmt.Errorf("property %q: %w", key, err)
				}
				if scalar {
					if err := s.indexed(len(key), encodedSize); err != nil {
						return fmt.Errorf("property %q: %w", key, err)
					}
				}
			}
		}
		return nil
	})
}

func (s *propertyInputPreflight) listValue(values []any, depth int) error {
	if values == nil {
		return nil
	}
	return s.withReference(reflect.ValueOf(values), func() error {
		if err := s.children("property list", len(values)); err != nil {
			return err
		}
		for index, value := range values {
			if err := s.value(value, depth+1); err != nil {
				return fmt.Errorf("list item %d: %w", index, err)
			}
		}
		return nil
	})
}

func (s *propertyInputPreflight) withReference(value reflect.Value, fn func() error) error {
	reference := encodeReference{kind: value.Kind(), ptr: value.Pointer()}
	if _, exists := s.visiting[reference]; exists {
		return fmt.Errorf("cyclic property value")
	}
	if s.visiting == nil {
		s.visiting = make(map[encodeReference]struct{})
	}
	s.visiting[reference] = struct{}{}
	defer delete(s.visiting, reference)
	return fn()
}

func (s *propertyInputPreflight) indexed(keyBytes int, encodedSize int64) error {
	s.indexedCount++
	if s.indexedCount > domain.MaxIndexedPropertiesPerVersion {
		return &domain.ResourceLimitError{
			Field: "indexed properties", Unit: "values",
			Limit: domain.MaxIndexedPropertiesPerVersion, Actual: s.indexedCount,
		}
	}
	s.indexedBytes += int64(keyBytes) + encodedSize
	if s.indexedBytes > int64(domain.MaxDerivedPropertyBytesPerVersion) {
		return &domain.ResourceLimitError{
			Field: "derived property index", Unit: "bytes",
			Limit: domain.MaxDerivedPropertyBytesPerVersion, Actual: int(s.indexedBytes),
		}
	}
	return nil
}

func scalarPropertyEncodedSize(value any) (bool, int64, error) {
	switch typed := value.(type) {
	case nil:
		return true, canonicalEncodedValueSize(encodedValue{Kind: "null"}), nil
	case bool:
		return true, canonicalEncodedValueSize(encodedValue{Kind: "bool", Bool: typed}), nil
	case string:
		return true, canonicalEncodedValueSize(encodedValue{Kind: "string", Text: typed}), nil
	case []byte:
		return true, canonicalSafeTextValueSize("bytes", base64.StdEncoding.EncodedLen(len(typed))), nil
	case temporal.Date:
		return temporalScalarEncodedSize("date", typed.MarshalBinary)
	case temporal.LocalTime:
		return temporalScalarEncodedSize("local_time", typed.MarshalBinary)
	case temporal.Time:
		return temporalScalarEncodedSize("offset_time", typed.MarshalBinary)
	case temporal.LocalDateTime:
		return temporalScalarEncodedSize("local_datetime", typed.MarshalBinary)
	case temporal.DateTime:
		return temporalScalarEncodedSize("zoned_datetime", typed.MarshalBinary)
	case temporal.Duration:
		return temporalScalarEncodedSize("cypher_duration", typed.MarshalBinary)
	case time.Duration:
		return true, canonicalSafeTextValueSize("duration", decimalInt64Length(int64(typed))), nil
	case int:
		return true, canonicalSafeTextValueSize("int", decimalInt64Length(int64(typed))), nil
	case int8:
		return true, canonicalSafeTextValueSize("int", decimalInt64Length(int64(typed))), nil
	case int16:
		return true, canonicalSafeTextValueSize("int", decimalInt64Length(int64(typed))), nil
	case int32:
		return true, canonicalSafeTextValueSize("int", decimalInt64Length(int64(typed))), nil
	case int64:
		return true, canonicalSafeTextValueSize("int", decimalInt64Length(typed)), nil
	case uint:
		return unsignedScalarEncodedSize(uint64(typed))
	case uint8:
		return unsignedScalarEncodedSize(uint64(typed))
	case uint16:
		return unsignedScalarEncodedSize(uint64(typed))
	case uint32:
		return unsignedScalarEncodedSize(uint64(typed))
	case uint64:
		return unsignedScalarEncodedSize(typed)
	case float32:
		return true, canonicalSafeTextValueSize("float", hexadecimalUint64Length(math.Float64bits(float64(typed)))), nil
	case float64:
		return true, canonicalSafeTextValueSize("float", hexadecimalUint64Length(math.Float64bits(typed))), nil
	}
	reflected := reflect.ValueOf(value)
	if reflected.IsValid() {
		switch reflected.Kind() {
		case reflect.Map:
			if !reflected.IsNil() {
				return false, 0, nil
			}
		case reflect.Slice:
			if !reflected.IsNil() && reflected.Type().Elem().Kind() != reflect.Uint8 {
				return false, 0, nil
			}
			if !reflected.IsNil() && reflected.Type().Elem().Kind() == reflect.Uint8 {
				encodedLength := base64.StdEncoding.EncodedLen(reflected.Len())
				return true, canonicalSafeTextValueSize("bytes", encodedLength), nil
			}
		case reflect.Array:
			return false, 0, nil
		case reflect.Bool:
			return true, canonicalEncodedValueSize(encodedValue{Kind: "bool", Bool: reflected.Bool()}), nil
		case reflect.String:
			return true, canonicalEncodedValueSize(encodedValue{Kind: "string", Text: reflected.String()}), nil
		case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
			return true, canonicalSafeTextValueSize("int", decimalInt64Length(reflected.Int())), nil
		case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
			return unsignedScalarEncodedSize(reflected.Uint())
		case reflect.Float32, reflect.Float64:
			converted := reflected.Convert(reflect.TypeFor[float64]()).Float()
			return true, canonicalSafeTextValueSize("float", hexadecimalUint64Length(math.Float64bits(converted))), nil
		}
	}
	state := encodeState{}
	encoded, err := state.encodeValue(value, 0)
	if err != nil {
		return false, 0, err
	}
	if encoded.Kind == "map" || encoded.Kind == "list" {
		return false, 0, nil
	}
	return true, canonicalEncodedValueSize(encoded), nil
}

func temporalScalarEncodedSize(kind string, marshal func() ([]byte, error)) (bool, int64, error) {
	payload, err := marshal()
	if err != nil {
		return false, 0, err
	}
	return true, canonicalSafeTextValueSize(kind, base64.StdEncoding.EncodedLen(len(payload))), nil
}

func unsignedScalarEncodedSize(value uint64) (bool, int64, error) {
	if value > math.MaxInt64 {
		return false, 0, fmt.Errorf("property unsigned integer %d exceeds int64", value)
	}
	return true, canonicalSafeTextValueSize("int", decimalInt64Length(int64(value))), nil
}

func canonicalSafeTextValueSize(kind string, textLength int) int64 {
	if textLength == 0 {
		return canonicalEncodedValueSize(encodedValue{Kind: kind})
	}
	// Decimal, hexadecimal, and base64 payloads contain no characters escaped
	// by encoding/json.
	return int64(2 +
		jsonStringSize("k") + 1 + jsonStringSize(kind) + 1 +
		jsonStringSize("s") + 1 + 2 + int64(textLength))
}

func decimalInt64Length(value int64) int {
	if value >= 0 {
		return decimalUint64Length(uint64(value))
	}
	return 1 + decimalUint64Length(uint64(-(value+1))+1)
}

func decimalUint64Length(value uint64) int {
	length := 1
	for value >= 10 {
		value /= 10
		length++
	}
	return length
}

func hexadecimalUint64Length(value uint64) int {
	length := 1
	for value >= 16 {
		value >>= 4
		length++
	}
	return length
}

func preflightEncodedProperties(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	state := encodedPropertyPreflight{}
	_, encodedSize, err := state.value(decoder, 0)
	if err != nil {
		return err
	}
	if encodedSize > int64(maxPropertyBytes) {
		return &domain.ResourceLimitError{
			Field: "canonical property encoding", Unit: "bytes",
			Limit: maxPropertyBytes, Actual: int(encodedSize),
		}
	}
	if err := ensureJSONEnd(decoder); err != nil {
		return err
	}
	return nil
}

type encodedPropertyPreflight struct {
	values int
}

func (s *encodedPropertyPreflight) value(decoder *json.Decoder, depth int) (string, int64, error) {
	if depth > maxPropertyDepth {
		return "", 0, &domain.ResourceLimitError{
			Field: "property nesting", Unit: "levels",
			Limit: maxPropertyDepth, Actual: depth,
		}
	}
	s.values++
	if s.values > maxPropertyValues {
		return "", 0, &domain.ResourceLimitError{
			Field: "property collection", Unit: "values",
			Limit: maxPropertyValues, Actual: s.values,
		}
	}
	opening, err := decoder.Token()
	if err != nil {
		return "", 0, err
	}
	if opening != json.Delim('{') {
		return "", 0, fmt.Errorf("encoded value is not an object")
	}

	var seen uint8
	kind := ""
	textLength := -1
	textTail := ""
	size := int64(2)
	fields := 0
	addField := func(name string, valueSize int64) {
		if fields > 0 {
			size++
		}
		size += jsonStringSize(name) + 1 + valueSize
		fields++
	}
	for decoder.More() {
		rawName, err := decoder.Token()
		if err != nil {
			return "", 0, err
		}
		name, ok := rawName.(string)
		if !ok {
			return "", 0, fmt.Errorf("encoded value field name is not text")
		}
		bit, known := encodedFieldBit(name)
		if !known {
			return "", 0, fmt.Errorf("unknown encoded value field %q", name)
		}
		if seen&bit != 0 {
			return "", 0, fmt.Errorf("duplicate encoded value field %q", name)
		}
		seen |= bit
		switch name {
		case "k", "s", "z":
			raw, err := decoder.Token()
			if err != nil {
				return "", 0, err
			}
			text, ok := raw.(string)
			if !ok {
				return "", 0, fmt.Errorf("encoded value field %q is not text", name)
			}
			if name == "k" {
				kind = text
			}
			if name == "s" {
				textLength = len(text)
				if len(text) > 2 {
					textTail = text[len(text)-2:]
				} else {
					textTail = text
				}
				if textLength > base64.StdEncoding.EncodedLen(domain.MaxPropertyScalarBytes) {
					return "", 0, &domain.ResourceLimitError{
						Field: "encoded property scalar", Unit: "bytes",
						Limit: base64.StdEncoding.EncodedLen(domain.MaxPropertyScalarBytes), Actual: textLength,
					}
				}
			}
			if name == "z" {
				if err := domain.ValidateText("time zone name", text, domain.MaxTimeZoneNameBytes); err != nil {
					return "", 0, err
				}
			}
			addField(name, jsonStringSize(text))
		case "b":
			raw, err := decoder.Token()
			if err != nil {
				return "", 0, err
			}
			boolean, ok := raw.(bool)
			if !ok {
				return "", 0, fmt.Errorf("encoded value field %q is not boolean", name)
			}
			if boolean {
				addField(name, 4)
			} else {
				addField(name, 5)
			}
		case "u":
			raw, err := decoder.Token()
			if err != nil {
				return "", 0, err
			}
			number, ok := raw.(json.Number)
			if !ok {
				return "", 0, fmt.Errorf("encoded value field %q is not numeric", name)
			}
			addField(name, int64(len(number.String())))
		case "a":
			arraySize, err := s.array(decoder, depth)
			if err != nil {
				return "", 0, err
			}
			addField(name, arraySize)
		case "o":
			mapSize, err := s.propertyMap(decoder, depth)
			if err != nil {
				return "", 0, err
			}
			addField(name, mapSize)
		}
	}
	closing, err := decoder.Token()
	if err != nil {
		return "", 0, err
	}
	if closing != json.Delim('}') {
		return "", 0, fmt.Errorf("invalid encoded value terminator")
	}
	if err := validatePreflightScalarText(kind, textLength, textTail); err != nil {
		return "", 0, err
	}
	return kind, size, nil
}

func encodedFieldBit(name string) (uint8, bool) {
	switch name {
	case "k":
		return 1, true
	case "b":
		return 2, true
	case "s":
		return 4, true
	case "z":
		return 8, true
	case "u":
		return 16, true
	case "a":
		return 32, true
	case "o":
		return 64, true
	default:
		return 0, false
	}
}

func validatePreflightScalarText(kind string, length int, tail string) error {
	if length < 0 {
		return nil
	}
	limit := base64.StdEncoding.EncodedLen(domain.MaxPropertyScalarBytes)
	switch kind {
	case "string":
		limit = domain.MaxPropertyScalarBytes
	case "bytes":
		if length == limit && tail != "==" {
			return &domain.ResourceLimitError{
				Field: "encoded property byte string", Unit: "bytes",
				Limit: limit, Actual: length,
			}
		}
	case "date":
		limit = base64.StdEncoding.EncodedLen(7)
	case "local_time":
		limit = base64.StdEncoding.EncodedLen(9)
	case "offset_time":
		limit = base64.StdEncoding.EncodedLen(13)
	case "local_datetime":
		limit = base64.StdEncoding.EncodedLen(15)
	case "zoned_datetime":
		limit = base64.StdEncoding.EncodedLen(20 + math.MaxUint16)
	case "cypher_duration":
		limit = base64.StdEncoding.EncodedLen(29)
	case "time":
		limit = base64.StdEncoding.EncodedLen(32)
	case "duration", "int", "float":
		limit = 64
	}
	if length > limit {
		return &domain.ResourceLimitError{
			Field: "encoded " + kind + " property", Unit: "bytes",
			Limit: limit, Actual: length,
		}
	}
	return nil
}

func (s *encodedPropertyPreflight) array(decoder *json.Decoder, depth int) (int64, error) {
	opening, err := decoder.Token()
	if err != nil {
		return 0, err
	}
	if opening != json.Delim('[') {
		return 0, fmt.Errorf("encoded list payload is not an array")
	}
	size := int64(2)
	items := 0
	for decoder.More() {
		_, childSize, err := s.value(decoder, depth+1)
		if err != nil {
			return 0, err
		}
		if items > 0 {
			size++
		}
		size += childSize
		items++
	}
	closing, err := decoder.Token()
	if err != nil {
		return 0, err
	}
	if closing != json.Delim(']') {
		return 0, fmt.Errorf("invalid encoded list terminator")
	}
	return size, nil
}

func (s *encodedPropertyPreflight) propertyMap(decoder *json.Decoder, depth int) (int64, error) {
	opening, err := decoder.Token()
	if err != nil {
		return 0, err
	}
	if opening != json.Delim('{') {
		return 0, fmt.Errorf("encoded map payload is not an object")
	}
	size := int64(2)
	items := 0
	indexedCount := 0
	indexedBytes := int64(0)
	for decoder.More() {
		rawKey, err := decoder.Token()
		if err != nil {
			return 0, err
		}
		key, ok := rawKey.(string)
		if !ok {
			return 0, fmt.Errorf("property map key is not text")
		}
		if err := domain.ValidateText("property map key", key, domain.MaxPropertyKeyBytes); err != nil {
			return 0, err
		}
		kind, childSize, err := s.value(decoder, depth+1)
		if err != nil {
			return 0, err
		}
		if items > 0 {
			size++
		}
		size += jsonStringSize(key) + 1 + childSize
		items++
		if depth == 0 && kind != "map" && kind != "list" {
			indexedCount++
			indexedBytes += int64(len(key)) + childSize
			if indexedCount > domain.MaxIndexedPropertiesPerVersion {
				return 0, &domain.ResourceLimitError{
					Field: "indexed properties", Unit: "values",
					Limit: domain.MaxIndexedPropertiesPerVersion, Actual: indexedCount,
				}
			}
			if indexedBytes > int64(domain.MaxDerivedPropertyBytesPerVersion) {
				return 0, &domain.ResourceLimitError{
					Field: "derived property index", Unit: "bytes",
					Limit: domain.MaxDerivedPropertyBytesPerVersion, Actual: int(indexedBytes),
				}
			}
		}
	}
	closing, err := decoder.Token()
	if err != nil {
		return 0, err
	}
	if closing != json.Delim('}') {
		return 0, fmt.Errorf("invalid encoded map terminator")
	}
	return size, nil
}

func validateEncodedIndexBudget(value encodedValue) error {
	if err := validateEncodedShape(value); err != nil {
		return err
	}
	if value.Kind != "map" {
		return nil
	}
	count := 0
	payloadBytes := int64(0)
	for key, property := range value.Map {
		if err := validateEncodedShape(property); err != nil {
			return err
		}
		if property.Kind == "map" || property.Kind == "list" {
			continue
		}
		count++
		payloadBytes += int64(len(key)) + canonicalEncodedValueSize(property)
	}
	if count > domain.MaxIndexedPropertiesPerVersion {
		return &domain.ResourceLimitError{
			Field: "indexed properties", Unit: "values",
			Limit: domain.MaxIndexedPropertiesPerVersion, Actual: count,
		}
	}
	if payloadBytes > int64(domain.MaxDerivedPropertyBytesPerVersion) {
		return &domain.ResourceLimitError{
			Field: "derived property index", Unit: "bytes",
			Limit: domain.MaxDerivedPropertyBytesPerVersion, Actual: int(payloadBytes),
		}
	}
	return nil
}

type encodeReference struct {
	kind reflect.Kind
	ptr  uintptr
}

type encodeState struct {
	visiting      map[encodeReference]struct{}
	values        int
	base64Payload int
}

func (s *encodeState) encodeBytes(value []byte) (encodedValue, error) {
	if err := domain.ValidateBytes("property byte string", value, domain.MaxPropertyScalarBytes); err != nil {
		return encodedValue{}, err
	}
	if err := s.reserveEncodedBytes(len(value)); err != nil {
		return encodedValue{}, err
	}
	return encodedValue{Kind: "bytes", Text: base64.StdEncoding.EncodeToString(value)}, nil
}

func (s *encodeState) reserveEncodedBytes(length int) error {
	encodedBytes := base64.StdEncoding.EncodedLen(length)
	if s.base64Payload > domain.MaxCanonicalPropertyBytes-encodedBytes {
		return &domain.ResourceLimitError{
			Field: "aggregate encoded property byte strings", Unit: "bytes",
			Limit: domain.MaxCanonicalPropertyBytes, Actual: s.base64Payload + encodedBytes,
		}
	}
	s.base64Payload += encodedBytes
	return nil
}

func (s *encodeState) reserveChildren(field string, count int) error {
	if count > domain.MaxPropertyValues-s.values {
		return &domain.ResourceLimitError{
			Field: field, Unit: "values",
			Limit: domain.MaxPropertyValues, Actual: s.values + count,
		}
	}
	return nil
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
		if err := domain.ValidateText("property string", v, domain.MaxPropertyScalarBytes); err != nil {
			return encodedValue{}, err
		}
		return encodedValue{Kind: "string", Text: v}, nil
	case []byte:
		return s.encodeBytes(v)
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
		location := v.Location()
		locationName := location.String()
		wireZone := locationName
		if location == time.Local || locationName == "Local" {
			// time.Local is process configuration, not a durable zone identity.
			// Its String value may become "UTC" or an IANA name after lazy
			// initialization, so recognize the pointer and retain the historical
			// Local wire tag with only the observed offset.
			wireZone = "Local"
			v = v.In(time.FixedZone(canonicalFixedZone(offset), offset))
		} else if locationName == "" || isCanonicalFixedZone(locationName) {
			// Normalize an unnamed fixed offset before MarshalBinary so the
			// decoder never has to guess whether it was time.Local. Preserve the
			// empty Zone field for compatibility with previously stored values.
			v = v.In(time.FixedZone(canonicalFixedZone(offset), offset))
			wireZone = ""
		}
		data, err := v.MarshalBinary()
		if err != nil {
			return encodedValue{}, fmt.Errorf("encode time: %w", err)
		}
		if err := domain.ValidateText("time zone name", wireZone, domain.MaxTimeZoneNameBytes); err != nil {
			return encodedValue{}, err
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
		if err := domain.ValidateText("property string", text, domain.MaxPropertyScalarBytes); err != nil {
			return encodedValue{}, err
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
			if rv.Len() > domain.MaxPropertyScalarBytes {
				return encodedValue{}, &domain.ResourceLimitError{
					Field: "property byte string", Unit: "bytes",
					Limit: domain.MaxPropertyScalarBytes, Actual: rv.Len(),
				}
			}
			if err := s.reserveEncodedBytes(rv.Len()); err != nil {
				return encodedValue{}, err
			}
			bytesValue := make([]byte, rv.Len())
			for i := range bytesValue {
				bytesValue[i] = byte(rv.Index(i).Uint())
			}
			return encodedValue{Kind: "bytes", Text: base64.StdEncoding.EncodeToString(bytesValue)}, nil
		}
		fallthrough
	case reflect.Array:
		if rv.Kind() == reflect.Slice {
			return s.withReference(rv, func() (encodedValue, error) { return s.encodeReflectedList(rv, depth) })
		}
		return s.encodeReflectedList(rv, depth)
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
	if err := s.reserveChildren("property map", len(values)); err != nil {
		return encodedValue{}, err
	}
	out := make(map[string]encodedValue, len(values))
	for key, value := range values {
		if err := domain.ValidateText("property map key", key, domain.MaxPropertyKeyBytes); err != nil {
			return encodedValue{}, err
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
		if err := s.reserveChildren("property map", values.Len()); err != nil {
			return encodedValue{}, err
		}
		out := make(map[string]encodedValue, values.Len())
		iterator := values.MapRange()
		for iterator.Next() {
			key := iterator.Key().String()
			if err := domain.ValidateText("property map key", key, domain.MaxPropertyKeyBytes); err != nil {
				return encodedValue{}, err
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
	if err := s.reserveChildren("property list", len(values)); err != nil {
		return encodedValue{}, err
	}
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

func (s *encodeState) encodeReflectedList(values reflect.Value, depth int) (encodedValue, error) {
	if err := s.reserveChildren("property list", values.Len()); err != nil {
		return encodedValue{}, err
	}
	out := make([]encodedValue, values.Len())
	for index := range values.Len() {
		encoded, err := s.encodeValue(values.Index(index).Interface(), depth+1)
		if err != nil {
			return encodedValue{}, fmt.Errorf("list item %d: %w", index, err)
		}
		out[index] = encoded
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

func (s *decodeState) reserveChildren(field string, count int) error {
	if count > maxPropertyValues-s.values {
		return &domain.ResourceLimitError{
			Field: field, Unit: "values",
			Limit: maxPropertyValues, Actual: s.values + count,
		}
	}
	return nil
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
		if err := domain.ValidateText("property string", v.Text, domain.MaxPropertyScalarBytes); err != nil {
			return nil, fmt.Errorf("decode properties: %w", err)
		}
		return v.Text, nil
	case "bytes":
		if len(v.Text) > base64.StdEncoding.EncodedLen(domain.MaxPropertyScalarBytes) {
			return nil, &domain.ResourceLimitError{
				Field: "encoded property byte string", Unit: "bytes",
				Limit: base64.StdEncoding.EncodedLen(domain.MaxPropertyScalarBytes), Actual: len(v.Text),
			}
		}
		data, err := base64.StdEncoding.DecodeString(v.Text)
		if err != nil {
			return nil, fmt.Errorf("decode bytes: %w", err)
		}
		if err := domain.ValidateBytes("property byte string", data, domain.MaxPropertyScalarBytes); err != nil {
			return nil, fmt.Errorf("decode properties: %w", err)
		}
		return data, nil
	case "date":
		return decodeTemporalBinary(v.Text, "date", 7, 7, func(data []byte) (any, error) { return temporal.DateFromBinary(data) })
	case "local_time":
		return decodeTemporalBinary(v.Text, "local_time", 9, 9, func(data []byte) (any, error) { return temporal.LocalTimeFromBinary(data) })
	case "offset_time":
		return decodeTemporalBinary(v.Text, "offset_time", 13, 13, func(data []byte) (any, error) { return temporal.TimeFromBinary(data) })
	case "local_datetime":
		return decodeTemporalBinary(v.Text, "local_datetime", 15, 15, func(data []byte) (any, error) { return temporal.LocalDateTimeFromBinary(data) })
	case "zoned_datetime":
		return decodeTemporalBinary(v.Text, "zoned_datetime", 20, 20+math.MaxUint16, func(data []byte) (any, error) { return temporal.DateTimeFromBinary(data) })
	case "cypher_duration":
		return decodeTemporalBinary(v.Text, "cypher_duration", 29, 29, func(data []byte) (any, error) { return temporal.DurationFromBinary(data) })
	case "time":
		if err := domain.ValidateText("time zone name", v.Zone, domain.MaxTimeZoneNameBytes); err != nil {
			return nil, fmt.Errorf("decode properties: %w", err)
		}
		data, err := base64.StdEncoding.DecodeString(v.Text)
		if err != nil {
			return nil, fmt.Errorf("decode time: %w", err)
		}
		var result time.Time
		if err := result.UnmarshalBinary(data); err != nil {
			return nil, fmt.Errorf("decode time: %w", err)
		}
		if v.Zone == "Local" {
			// Legacy writers persisted the process-local sentinel plus the
			// observed offset. Local carries no portable rule identity, and its
			// String value depends on host initialization. Reconstruct a fixed
			// zone named Local so legacy bytes recanonicalize identically on every
			// host instead of consulting the current process configuration.
			result = result.In(time.FixedZone("Local", v.Offset))
		} else if v.Zone != "" {
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
		if err := s.reserveChildren("decoded property list", len(v.Items)); err != nil {
			return nil, err
		}
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
		if err := s.reserveChildren("decoded property map", len(v.Map)); err != nil {
			return nil, err
		}
		result := make(domain.Properties, len(v.Map))
		for key, item := range v.Map {
			if err := domain.ValidateText("property map key", key, domain.MaxPropertyKeyBytes); err != nil {
				return nil, fmt.Errorf("decode properties: %w", err)
			}
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

func decodeTemporalBinary(text, kind string, minimumBytes, maximumBytes int, decode func([]byte) (any, error)) (any, error) {
	if len(text) < base64.StdEncoding.EncodedLen(minimumBytes) || len(text) > base64.StdEncoding.EncodedLen(maximumBytes) {
		return nil, fmt.Errorf("decode %s: binary payload length is outside [%d,%d] bytes", kind, minimumBytes, maximumBytes)
	}
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
	if len(labels) > domain.MaxLabelsPerNode {
		return nil, nil, &domain.ResourceLimitError{
			Field: "node labels", Unit: "values",
			Limit: domain.MaxLabelsPerNode, Actual: len(labels),
		}
	}
	for _, label := range labels {
		if err := domain.ValidateTextWithoutNUL("node label", label, domain.MaxLabelBytes); err != nil {
			return nil, nil, err
		}
	}
	normalized := normalizeLabels(labels)
	encodedSize := int64(4) // nil labels encode as null.
	if normalized != nil {
		encodedSize = 2
		derivedBytes := 0
		for index, label := range normalized {
			derivedBytes += len(label)
			if derivedBytes > domain.MaxDerivedLabelBytesPerVersion {
				return nil, nil, &domain.ResourceLimitError{
					Field: "derived label index", Unit: "bytes",
					Limit: domain.MaxDerivedLabelBytesPerVersion, Actual: derivedBytes,
				}
			}
			if index > 0 {
				encodedSize++
			}
			encodedSize += jsonStringSize(label)
		}
	}
	if encodedSize > int64(domain.MaxEncodedLabelsBytes) {
		return nil, nil, &domain.ResourceLimitError{
			Field: "canonical label encoding", Unit: "bytes",
			Limit: domain.MaxEncodedLabelsBytes, Actual: int(encodedSize),
		}
	}
	data, err := json.Marshal(normalized)
	if len(data) > domain.MaxEncodedLabelsBytes {
		return nil, nil, &domain.ResourceLimitError{
			Field: "canonical label encoding", Unit: "bytes",
			Limit: domain.MaxEncodedLabelsBytes, Actual: len(data),
		}
	}
	return data, normalized, err
}

func decodeLabels(data []byte) ([]string, error) {
	if err := domain.ValidateBytes("canonical label encoding", data, domain.MaxEncodedLabelsBytes); err != nil {
		return nil, fmt.Errorf("decode labels: %w", err)
	}
	count, isNull, err := preflightLabels(data)
	if err != nil {
		return nil, fmt.Errorf("decode labels: %w", err)
	}
	if isNull {
		return nil, nil
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	token, err := decoder.Token()
	if err != nil {
		return nil, fmt.Errorf("decode labels: %w", err)
	}
	opening, ok := token.(json.Delim)
	if !ok || opening != '[' {
		return nil, fmt.Errorf("decode labels: root is not an array or null")
	}
	labels := make([]string, 0, count)
	for decoder.More() {
		var label string
		if err := decoder.Decode(&label); err != nil {
			return nil, fmt.Errorf("decode labels: %w", err)
		}
		labels = append(labels, label)
	}
	closing, err := decoder.Token()
	if err != nil {
		return nil, fmt.Errorf("decode labels: invalid array terminator: %w", err)
	}
	if closing != json.Delim(']') {
		return nil, fmt.Errorf("decode labels: invalid array terminator")
	}
	if err := ensureJSONEnd(decoder); err != nil {
		return nil, fmt.Errorf("decode labels: %w", err)
	}
	return labels, nil
}

// preflightLabels validates count, aggregate bytes, ordering, uniqueness, and
// exact JSON spelling while retaining only the previous label. decodeLabels
// can therefore allocate its result once, after the complete input is known
// to be bounded and canonical.
func preflightLabels(data []byte) (count int, isNull bool, err error) {
	if bytes.Equal(data, []byte("null")) {
		return 0, true, nil
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	opening, err := decoder.Token()
	if err != nil {
		return 0, false, err
	}
	if opening != json.Delim('[') {
		return 0, false, fmt.Errorf("root is not an array or null")
	}
	position := 1
	derivedBytes := 0
	previous := ""
	for decoder.More() {
		count++
		if count > domain.MaxLabelsPerNode {
			return 0, false, &domain.ResourceLimitError{
				Field: "node labels", Unit: "values",
				Limit: domain.MaxLabelsPerNode, Actual: count,
			}
		}
		if count > 1 {
			if position >= len(data) || data[position] != ',' {
				return 0, false, fmt.Errorf("non-canonical label encoding")
			}
			position++
		}
		var label string
		if err := decoder.Decode(&label); err != nil {
			return 0, false, err
		}
		if err := domain.ValidateTextWithoutNUL("node label", label, domain.MaxLabelBytes); err != nil {
			return 0, false, err
		}
		if count > 1 && label <= previous {
			return 0, false, fmt.Errorf("non-canonical label order or duplicate")
		}
		previous = label
		derivedBytes += len(label)
		if derivedBytes > domain.MaxDerivedLabelBytesPerVersion {
			return 0, false, &domain.ResourceLimitError{
				Field: "derived label index", Unit: "bytes",
				Limit: domain.MaxDerivedLabelBytesPerVersion, Actual: derivedBytes,
			}
		}
		canonical, err := json.Marshal(label)
		if err != nil {
			return 0, false, err
		}
		end := position + len(canonical)
		if end > len(data) || !bytes.Equal(data[position:end], canonical) {
			return 0, false, fmt.Errorf("non-canonical label encoding")
		}
		position = end
	}
	closing, err := decoder.Token()
	if err != nil {
		return 0, false, err
	}
	if closing != json.Delim(']') {
		return 0, false, fmt.Errorf("invalid array terminator")
	}
	if err := ensureJSONEnd(decoder); err != nil {
		return 0, false, err
	}
	if position != len(data)-1 || data[position] != ']' {
		return 0, false, fmt.Errorf("non-canonical label encoding")
	}
	return count, false, nil
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
