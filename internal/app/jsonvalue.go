package app

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/svlocks/sheets/internal/domain"
	"github.com/svlocks/sheets/internal/domain/temporal"
)

// exactTemporalJSON is the machine-readable envelope used for Cypher temporal
// values. Text keeps output useful to people while Binary makes the envelope
// independent of timezone database changes and exactly round-trippable.
type exactTemporalJSON struct {
	Text   string `json:"text"`
	Binary string `json:"binary"`
}

type legacyTimeJSON struct {
	Text          string `json:"text"`
	Binary        string `json:"binary"`
	Zone          string `json:"zone"`
	OffsetSeconds int    `json:"offset_seconds"`
}

type legacyDurationJSON struct {
	Text        string `json:"text"`
	Nanoseconds string `json:"nanoseconds"`
}

// JSONValue returns a detached, JSON-safe representation of a query value.
// Values that plain JSON cannot type or represent exactly use explicit tagged
// envelopes. The input and all nested graph properties remain untouched.
func JSONValue(value any) any {
	switch value := value.(type) {
	case nil, bool, string, json.Number:
		return value
	case temporal.Date:
		return temporalJSON("$date", value.String(), value.MarshalBinary)
	case temporal.LocalTime:
		return temporalJSON("$local_time", value.String(), value.MarshalBinary)
	case temporal.Time:
		return temporalJSON("$offset_time", value.String(), value.MarshalBinary)
	case temporal.LocalDateTime:
		return temporalJSON("$local_datetime", value.String(), value.MarshalBinary)
	case temporal.DateTime:
		return temporalJSON("$zoned_datetime", value.String(), value.MarshalBinary)
	case temporal.Duration:
		return temporalJSON("$cypher_duration", value.String(), value.MarshalBinary)
	case time.Time:
		_, offset := value.Zone()
		binary, err := value.MarshalBinary()
		if err != nil {
			return map[string]any{"$legacy_time": map[string]any{"text": value.String(), "error": err.Error()}}
		}
		return map[string]any{"$legacy_time": legacyTimeJSON{
			Text: value.Format(time.RFC3339Nano), Binary: base64.StdEncoding.EncodeToString(binary),
			Zone: value.Location().String(), OffsetSeconds: offset,
		}}
	case time.Duration:
		return map[string]any{"$legacy_duration": legacyDurationJSON{
			Text: value.String(), Nanoseconds: fmt.Sprintf("%d", int64(value)),
		}}
	case []byte:
		return map[string]any{"$bytes": base64.StdEncoding.EncodeToString(value)}
	case domain.Node:
		result := value
		result.Properties = JSONValue(value.Properties).(map[string]any)
		return result
	case domain.Edge:
		result := value
		result.Properties = JSONValue(value.Properties).(map[string]any)
		return result
	case float64:
		return jsonFloat(value)
	case float32:
		return jsonFloat(float64(value))
	}
	return jsonReflectValue(reflect.ValueOf(value))
}

func temporalJSON(tag, text string, marshal func() ([]byte, error)) any {
	binary, err := marshal()
	if err != nil {
		// Public constructors and store decoders only produce valid values. Keep
		// an invalid value visible if one is nevertheless supplied by a caller.
		return map[string]any{tag: map[string]any{"text": text, "error": err.Error()}}
	}
	return map[string]any{tag: exactTemporalJSON{
		Text: text, Binary: base64.StdEncoding.EncodeToString(binary),
	}}
}

func jsonFloat(value float64) any {
	switch {
	case math.IsNaN(value):
		return map[string]any{"$float": "NaN"}
	case math.IsInf(value, 1):
		return map[string]any{"$float": "+Infinity"}
	case math.IsInf(value, -1):
		return map[string]any{"$float": "-Infinity"}
	default:
		return value
	}
}

func jsonReflectValue(value reflect.Value) any {
	if !value.IsValid() {
		return nil
	}
	for value.Kind() == reflect.Interface || value.Kind() == reflect.Pointer {
		if value.IsNil() {
			return nil
		}
		value = value.Elem()
	}
	switch value.Kind() {
	case reflect.Map:
		if value.Type().Key().Kind() != reflect.String {
			return fmt.Sprint(value.Interface())
		}
		result := make(map[string]any, value.Len())
		iterator := value.MapRange()
		for iterator.Next() {
			result[iterator.Key().String()] = JSONValue(iterator.Value().Interface())
		}
		return result
	case reflect.Slice, reflect.Array:
		result := make([]any, value.Len())
		for index := range result {
			result[index] = JSONValue(value.Index(index).Interface())
		}
		return result
	case reflect.Struct:
		result := make(map[string]any)
		typeInfo := value.Type()
		for index := 0; index < value.NumField(); index++ {
			fieldInfo := typeInfo.Field(index)
			if fieldInfo.PkgPath != "" {
				continue
			}
			tag := fieldInfo.Tag.Get("json")
			name, options, _ := strings.Cut(tag, ",")
			if name == "-" {
				continue
			}
			if name == "" {
				name = fieldInfo.Name
			}
			field := value.Field(index)
			if strings.Contains(options, "omitempty") && field.IsZero() {
				continue
			}
			result[name] = JSONValue(field.Interface())
		}
		if len(result) == 0 {
			return fmt.Sprint(value.Interface())
		}
		return result
	case reflect.Invalid, reflect.Chan, reflect.Func, reflect.UnsafePointer:
		return fmt.Sprint(value.Interface())
	default:
		return value.Interface()
	}
}

// DecodeTaggedJSONValue restores exact envelopes emitted by JSONValue.
// Ordinary maps and lists are copied recursively so decoding never mutates the
// caller's JSON tree. decodeFloatTags is false for the intentionally plain-JSON
// query parameter contract and true for editable values emitted by JSONValue.
func DecodeTaggedJSONValue(value any, decodeFloatTags bool) (any, error) {
	switch value := value.(type) {
	case []any:
		result := make([]any, len(value))
		for index, item := range value {
			decoded, err := DecodeTaggedJSONValue(item, decodeFloatTags)
			if err != nil {
				return nil, fmt.Errorf("item %d: %w", index, err)
			}
			result[index] = decoded
		}
		return result, nil
	case map[string]any:
		if len(value) == 1 {
			for tag, payload := range value {
				if decoded, recognized, err := decodeTaggedEnvelope(tag, payload, decodeFloatTags); recognized || err != nil {
					return decoded, err
				}
			}
		}
		result := make(map[string]any, len(value))
		for key, item := range value {
			decoded, err := DecodeTaggedJSONValue(item, decodeFloatTags)
			if err != nil {
				return nil, fmt.Errorf("property %q: %w", key, err)
			}
			result[key] = decoded
		}
		return result, nil
	default:
		return value, nil
	}
}

func decodeTaggedEnvelope(tag string, payload any, decodeFloatTags bool) (any, bool, error) {
	switch tag {
	case "$date":
		return decodeTemporalEnvelope(tag, payload, func(data []byte) (any, error) { return temporal.DateFromBinary(data) })
	case "$local_time":
		return decodeTemporalEnvelope(tag, payload, func(data []byte) (any, error) { return temporal.LocalTimeFromBinary(data) })
	case "$offset_time":
		return decodeTemporalEnvelope(tag, payload, func(data []byte) (any, error) { return temporal.TimeFromBinary(data) })
	case "$local_datetime":
		return decodeTemporalEnvelope(tag, payload, func(data []byte) (any, error) { return temporal.LocalDateTimeFromBinary(data) })
	case "$zoned_datetime":
		return decodeTemporalEnvelope(tag, payload, func(data []byte) (any, error) { return temporal.DateTimeFromBinary(data) })
	case "$cypher_duration":
		return decodeTemporalEnvelope(tag, payload, func(data []byte) (any, error) { return temporal.DurationFromBinary(data) })
	case "$float":
		if !decodeFloatTags {
			return nil, false, nil
		}
		name, ok := payload.(string)
		if !ok {
			return nil, true, errors.New("$float marker must be a string")
		}
		switch name {
		case "NaN":
			return math.NaN(), true, nil
		case "+Infinity":
			return math.Inf(1), true, nil
		case "-Infinity":
			return math.Inf(-1), true, nil
		default:
			return nil, true, fmt.Errorf("unknown $float marker %q", name)
		}
	case "$bytes":
		text, ok := payload.(string)
		if !ok {
			return nil, true, errors.New("$bytes marker must be a base64 string")
		}
		decoded, err := base64.StdEncoding.DecodeString(text)
		if err != nil {
			return nil, true, fmt.Errorf("invalid $bytes marker: %w", err)
		}
		return decoded, true, nil
	case "$legacy_duration":
		return decodeLegacyDuration(payload)
	case "$legacy_time":
		return decodeLegacyTime(payload)
	default:
		return nil, false, nil
	}
}

func decodeTemporalEnvelope(tag string, payload any, decode func([]byte) (any, error)) (any, bool, error) {
	fields, ok := payload.(map[string]any)
	if !ok || len(fields) != 2 {
		return nil, true, fmt.Errorf("%s marker must contain exactly text and binary strings", tag)
	}
	text, textOK := fields["text"].(string)
	binaryText, binaryOK := fields["binary"].(string)
	if !textOK || !binaryOK {
		return nil, true, fmt.Errorf("%s marker must contain exactly text and binary strings", tag)
	}
	minimumBytes, maximumBytes := temporalBinaryBounds(tag)
	if len(binaryText) < base64.StdEncoding.EncodedLen(minimumBytes) || len(binaryText) > base64.StdEncoding.EncodedLen(maximumBytes) {
		return nil, true, fmt.Errorf("%s binary payload length is outside [%d,%d] bytes", tag, minimumBytes, maximumBytes)
	}
	binary, err := base64.StdEncoding.DecodeString(binaryText)
	if err != nil {
		return nil, true, fmt.Errorf("invalid %s binary: %w", tag, err)
	}
	value, err := decode(binary)
	if err != nil {
		return nil, true, fmt.Errorf("invalid %s binary: %w", tag, err)
	}
	if fmt.Sprint(value) != text {
		return nil, true, fmt.Errorf("%s text does not match its binary value", tag)
	}
	return value, true, nil
}

func temporalBinaryBounds(tag string) (int, int) {
	switch tag {
	case "$date":
		return 7, 7
	case "$local_time":
		return 9, 9
	case "$offset_time":
		return 13, 13
	case "$local_datetime":
		return 15, 15
	case "$zoned_datetime":
		return 20, 20 + math.MaxUint16
	case "$cypher_duration":
		return 29, 29
	default:
		return 0, 0
	}
}

func decodeLegacyDuration(payload any) (any, bool, error) {
	fields, ok := payload.(map[string]any)
	if !ok || len(fields) != 2 {
		return nil, true, errors.New("$legacy_duration marker must contain exactly text and nanoseconds strings")
	}
	text, textOK := fields["text"].(string)
	nanoseconds, nanoOK := fields["nanoseconds"].(string)
	if !textOK || !nanoOK {
		return nil, true, errors.New("$legacy_duration marker must contain exactly text and nanoseconds strings")
	}
	raw, err := strconv.ParseInt(nanoseconds, 10, 64)
	if err != nil || strconv.FormatInt(raw, 10) != nanoseconds {
		return nil, true, errors.New("$legacy_duration nanoseconds must be a canonical signed 64-bit integer")
	}
	value := time.Duration(raw)
	if value.String() != text {
		return nil, true, errors.New("$legacy_duration text does not match its nanoseconds")
	}
	return value, true, nil
}

func decodeLegacyTime(payload any) (any, bool, error) {
	fields, ok := payload.(map[string]any)
	if !ok || len(fields) != 4 {
		return nil, true, errors.New("$legacy_time marker must contain exactly text, binary, zone, and offset_seconds")
	}
	text, textOK := fields["text"].(string)
	binaryText, binaryOK := fields["binary"].(string)
	zone, zoneOK := fields["zone"].(string)
	if !textOK || !binaryOK || !zoneOK {
		return nil, true, errors.New("$legacy_time marker has invalid field types")
	}
	if len(binaryText) > base64.StdEncoding.EncodedLen(64) {
		return nil, true, errors.New("$legacy_time binary payload is too large")
	}
	var offset64 int64
	switch offset := fields["offset_seconds"].(type) {
	case json.Number:
		var err error
		offset64, err = offset.Int64()
		if err != nil {
			return nil, true, errors.New("$legacy_time offset_seconds is invalid")
		}
	case int64:
		offset64 = offset
	default:
		return nil, true, errors.New("$legacy_time marker has invalid field types")
	}
	if offset64 < -86_400 || offset64 > 86_400 {
		return nil, true, errors.New("$legacy_time offset_seconds is invalid")
	}
	binary, err := base64.StdEncoding.DecodeString(binaryText)
	if err != nil {
		return nil, true, fmt.Errorf("invalid $legacy_time binary: %w", err)
	}
	var decoded time.Time
	if err := decoded.UnmarshalBinary(binary); err != nil {
		return nil, true, fmt.Errorf("invalid $legacy_time binary: %w", err)
	}
	location := time.FixedZone(zone, int(offset64))
	if zone == "UTC" && offset64 == 0 {
		location = time.UTC
	} else if loaded, loadErr := time.LoadLocation(zone); loadErr == nil {
		candidate := decoded.In(loaded)
		_, candidateOffset := candidate.Zone()
		if candidateOffset == int(offset64) {
			location = loaded
		}
	}
	result := decoded.In(location)
	_, resultOffset := result.Zone()
	if resultOffset != int(offset64) {
		return nil, true, errors.New("$legacy_time text does not match offset_seconds")
	}
	if result.Format(time.RFC3339Nano) != text {
		return nil, true, errors.New("$legacy_time text does not match its binary value")
	}
	return result, true, nil
}

// RejectDuplicateJSONKeys rejects duplicate keys at every nesting level,
// including keys whose spellings differ only through JSON escapes.
func RejectDuplicateJSONKeys(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	return scanJSONValue(decoder)
}

func scanJSONValue(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delimiter, composite := token.(json.Delim)
	if !composite {
		return nil
	}
	switch delimiter {
	case '{':
		keys := make(map[string]struct{})
		for decoder.More() {
			rawKey, err := decoder.Token()
			if err != nil {
				return err
			}
			key, ok := rawKey.(string)
			if !ok {
				return errors.New("invalid JSON object key")
			}
			if _, exists := keys[key]; exists {
				return fmt.Errorf("duplicate object key %q", key)
			}
			keys[key] = struct{}{}
			if err := scanJSONValue(decoder); err != nil {
				return err
			}
		}
	case '[':
		for decoder.More() {
			if err := scanJSONValue(decoder); err != nil {
				return err
			}
		}
	default:
		return fmt.Errorf("invalid JSON delimiter %q", delimiter)
	}
	closing, err := decoder.Token()
	if err != nil {
		return err
	}
	want := json.Delim('}')
	if delimiter == '[' {
		want = ']'
	}
	if closing != want {
		return fmt.Errorf("invalid JSON delimiter %q", closing)
	}
	return nil
}
