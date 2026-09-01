package tui

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"reflect"
	"time"

	"github.com/svlocks/sheets/internal/app"
	"github.com/svlocks/sheets/internal/domain/temporal"
)

const (
	maxSearchTextBytes       = 4 << 10
	maxDetailJSONBytes       = 256 << 10
	maxNodeTitleRunes        = 256
	maxDisplayLabelRunes     = 128
	maxDisplayLabels         = 16
	maxTimelineTextRunes     = 512
	maxInspectorBodyRunes    = 256 << 10
	maxInspectorListItems    = 1_000
	maxSelectedRowDetailSize = 1 << 20
)

func prettyJSONPreview(value any) string {
	return jsonPreview(value, true, maxDetailJSONBytes)
}

func jsonPreview(value any, indent bool, budget int) string {
	preview, _ := jsonPreviewStatus(value, indent, budget)
	return preview
}

func jsonPreviewStatus(value any, indent bool, budget int) (string, bool) {
	if !presentationValueWithinBudget(reflect.ValueOf(value), budget, 0) {
		return fmt.Sprintf(`{"$preview":"value omitted because it exceeds the %d-byte TUI detail budget"}`, budget), true
	}
	normalized := app.JSONValue(value)
	var encoded []byte
	var err error
	if indent {
		encoded, err = json.MarshalIndent(normalized, "", "  ")
	} else {
		encoded, err = json.Marshal(normalized)
	}
	if err != nil {
		return fmt.Sprintf(`{"$preview":"cannot encode value of type %T"}`, value), false
	}
	if len(encoded) > budget {
		return fmt.Sprintf(`{"$preview":"encoded value omitted because it exceeds the %d-byte TUI detail budget"}`, budget), true
	}
	return terminalSafeJSON(string(encoded)), false
}

// presentationValueWithinBudget is deliberately conservative. It prevents a
// large durable value from being cloned by app.JSONValue only to be clipped by
// a viewport afterward. Store-decoded values are acyclic; the depth guard also
// makes presentation of malformed test/backend values terminate safely.
func presentationValueWithinBudget(value reflect.Value, remaining, depth int) bool {
	return consumePresentationBudget(value, &remaining, depth)
}

func consumePresentationBudget(value reflect.Value, remaining *int, depth int) bool {
	if *remaining < 0 || depth > 128 {
		return false
	}
	if !value.IsValid() {
		return takePresentationBudget(remaining, 4)
	}
	for value.Kind() == reflect.Interface || value.Kind() == reflect.Pointer {
		if value.IsNil() {
			return takePresentationBudget(remaining, 4)
		}
		value = value.Elem()
		depth++
		if depth > 128 {
			return false
		}
	}
	if value.CanInterface() {
		switch item := value.Interface().(type) {
		case time.Time:
			return takeScaledPresentationBudget(remaining, len(item.Location().String()), 6, 128)
		case temporal.Date, temporal.LocalTime, temporal.Time, temporal.LocalDateTime, temporal.DateTime, temporal.Duration:
			return takeScaledPresentationBudget(remaining, len(fmt.Sprint(item)), 3, 0)
		}
	}
	switch value.Kind() {
	case reflect.Bool, reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr,
		reflect.Float32, reflect.Float64:
		return takePresentationBudget(remaining, 64)
	case reflect.String:
		// JSON escaping can expand one input byte to six ASCII bytes.
		return takeScaledPresentationBudget(remaining, value.Len(), 6, 2)
	case reflect.Slice:
		if value.IsNil() {
			return takePresentationBudget(remaining, 4)
		}
		if value.Type().Elem().Kind() == reflect.Uint8 {
			return takeScaledPresentationBudget(remaining, value.Len(), 2, 32)
		}
		fallthrough
	case reflect.Array:
		if !takePresentationBudget(remaining, 2) {
			return false
		}
		for index := 0; index < value.Len(); index++ {
			if !takePresentationBudget(remaining, 1) || !consumePresentationBudget(value.Index(index), remaining, depth+1) {
				return false
			}
		}
		return true
	case reflect.Map:
		if value.IsNil() {
			return takePresentationBudget(remaining, 4)
		}
		if !takePresentationBudget(remaining, 2) {
			return false
		}
		iterator := value.MapRange()
		for iterator.Next() {
			key := iterator.Key()
			if key.Kind() != reflect.String {
				return false
			}
			if !takeScaledPresentationBudget(remaining, key.Len(), 6, 4) {
				return false
			}
			if !consumePresentationBudget(iterator.Value(), remaining, depth+1) {
				return false
			}
		}
		return true
	case reflect.Struct:
		if !takePresentationBudget(remaining, 2) {
			return false
		}
		info := value.Type()
		for index := 0; index < value.NumField(); index++ {
			if info.Field(index).PkgPath != "" {
				continue
			}
			if !takePresentationBudget(remaining, 64) || !consumePresentationBudget(value.Field(index), remaining, depth+1) {
				return false
			}
		}
		return true
	default:
		return takePresentationBudget(remaining, 128)
	}
}

func takePresentationBudget(remaining *int, amount int) bool {
	if amount < 0 || amount > *remaining {
		return false
	}
	*remaining -= amount
	return true
}

func takeScaledPresentationBudget(remaining *int, length, multiplier, extra int) bool {
	if length < 0 || multiplier < 0 || extra < 0 || extra > *remaining {
		return false
	}
	available := *remaining - extra
	if multiplier != 0 && length > available/multiplier {
		return false
	}
	*remaining = available - length*multiplier
	return true
}

type searchTextBuilder struct {
	text      []byte
	limit     int
	truncated bool
}

func (b *searchTextBuilder) write(value string) {
	if b.truncated || b.limit <= len(b.text) {
		b.truncated = true
		return
	}
	for _, character := range value {
		encoded := string(character)
		if len(b.text)+len(encoded) > b.limit {
			b.truncated = true
			return
		}
		b.text = append(b.text, encoded...)
	}
}

func boundedSearchText(values ...any) string {
	builder := searchTextBuilder{limit: maxSearchTextBytes}
	for index, value := range values {
		if index > 0 {
			builder.write(" ")
		}
		appendSearchValue(&builder, value, 0)
		if builder.truncated {
			break
		}
	}
	return string(builder.text)
}

func appendSearchValue(builder *searchTextBuilder, value any, depth int) {
	if builder.truncated || depth > 16 {
		builder.truncated = true
		return
	}
	switch value := value.(type) {
	case nil:
		builder.write("null")
	case string:
		builder.write(value)
	case bool, int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64, uintptr,
		float32, float64, json.Number, time.Time, time.Duration,
		temporal.Date, temporal.LocalTime, temporal.Time, temporal.LocalDateTime, temporal.DateTime, temporal.Duration:
		builder.write(fmt.Sprint(value))
	case []byte:
		maximum := min(len(value), 96)
		builder.write(base64.StdEncoding.EncodeToString(value[:maximum]))
		if maximum < len(value) {
			builder.write(fmt.Sprintf(" %d bytes", len(value)))
		}
	case []any:
		for index, item := range value {
			if index > 0 {
				builder.write(" ")
			}
			appendSearchValue(builder, item, depth+1)
			if builder.truncated {
				return
			}
		}
	case []string:
		for index, item := range value {
			if index > 0 {
				builder.write(" ")
			}
			builder.write(item)
			if builder.truncated {
				return
			}
		}
	case map[string]any:
		for key, item := range value {
			builder.write(key)
			builder.write(" ")
			appendSearchValue(builder, item, depth+1)
			builder.write(" ")
			if builder.truncated {
				return
			}
		}
	default:
		reflected := reflect.ValueOf(value)
		if reflected.IsValid() && reflected.Kind() == reflect.Map && reflected.Type().Key().Kind() == reflect.String {
			iterator := reflected.MapRange()
			for iterator.Next() {
				builder.write(iterator.Key().String())
				builder.write(" ")
				appendSearchValue(builder, iterator.Value().Interface(), depth+1)
				builder.write(" ")
				if builder.truncated {
					return
				}
			}
			return
		}
		builder.write(fmt.Sprintf("%T", value))
	}
}
