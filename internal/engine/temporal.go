package engine

import (
	"crypto/rand"
	"fmt"
	"math/big"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/svlocks/sheets/internal/cypher"
)

func temporalValue(expression cypher.Expression, name string, arguments []any, now time.Time) (any, error) {
	if len(arguments) > 1 {
		return nil, evalError(expression, "%s expects zero or one argument", name)
	}
	location := time.UTC
	if name == "localdatetime" || name == "localtime" {
		location = now.Location()
	}
	if len(arguments) == 0 {
		value := now.In(location)
		switch name {
		case "date":
			return time.Date(value.Year(), value.Month(), value.Day(), 0, 0, 0, 0, location), nil
		case "time", "localtime":
			return time.Date(0, time.January, 1, value.Hour(), value.Minute(), value.Second(), value.Nanosecond(), location), nil
		default:
			return value, nil
		}
	}
	if arguments[0] == nil {
		return nil, nil
	}
	if value, ok := arguments[0].(time.Time); ok {
		return value, nil
	}
	if fields, ok := arguments[0].(map[string]any); ok {
		if epoch, exists := fields["epochMillis"]; exists {
			milliseconds, ok := integer(epoch)
			if !ok {
				return nil, evalError(expression, "%s epochMillis must be an integer", name)
			}
			return time.UnixMilli(milliseconds).In(location), nil
		}
		return nil, evalError(expression, "%s map requires epochMillis", name)
	}
	text, ok := arguments[0].(string)
	if !ok {
		return nil, evalError(expression, "%s expects a string, temporal value, or map", name)
	}
	var layouts []string
	switch name {
	case "date":
		layouts = []string{"2006-01-02"}
	case "time", "localtime":
		layouts = []string{"15:04:05.999999999Z07:00", "15:04:05.999999999", "15:04:05"}
	default:
		layouts = []string{time.RFC3339Nano, "2006-01-02T15:04:05.999999999"}
	}
	for _, layout := range layouts {
		value, err := time.ParseInLocation(layout, text, location)
		if err == nil {
			return value, nil
		}
	}
	return nil, evalError(expression, "invalid %s value %q", name, text)
}

var isoDuration = regexp.MustCompile(`^(-)?P(?:(\d+)D)?(?:T(?:(\d+)H)?(?:(\d+)M)?(?:(\d+(?:\.\d+)?)S)?)?$`)

func durationValue(expression cypher.Expression, arguments []any) (any, error) {
	if len(arguments) != 1 {
		return nil, evalError(expression, "duration expects one argument")
	}
	if arguments[0] == nil {
		return nil, nil
	}
	if value, ok := arguments[0].(time.Duration); ok {
		return value, nil
	}
	text, ok := arguments[0].(string)
	if !ok {
		return nil, evalError(expression, "duration expects a string")
	}
	if value, err := time.ParseDuration(text); err == nil {
		return value, nil
	}
	matches := isoDuration.FindStringSubmatch(strings.ToUpper(text))
	if matches == nil || matches[2]+matches[3]+matches[4]+matches[5] == "" {
		return nil, evalError(expression, "invalid duration %q", text)
	}
	parseInteger := func(raw string) (int64, error) {
		if raw == "" {
			return 0, nil
		}
		return strconv.ParseInt(raw, 10, 64)
	}
	days, err := parseInteger(matches[2])
	if err != nil {
		return nil, evalError(expression, "invalid duration %q", text)
	}
	hours, err := parseInteger(matches[3])
	if err != nil {
		return nil, evalError(expression, "invalid duration %q", text)
	}
	minutes, err := parseInteger(matches[4])
	if err != nil {
		return nil, evalError(expression, "invalid duration %q", text)
	}
	total := int64(0)
	addInteger := func(value int64, unit time.Duration) bool {
		if value > (int64(^uint64(0)>>1)-total)/int64(unit) {
			return false
		}
		total += value * int64(unit)
		return true
	}
	if !addInteger(days, 24*time.Hour) || !addInteger(hours, time.Hour) || !addInteger(minutes, time.Minute) {
		return nil, evalError(expression, "duration %q exceeds the supported range", text)
	}
	if matches[5] != "" {
		seconds, ok := new(big.Rat).SetString(matches[5])
		if !ok {
			return nil, evalError(expression, "invalid duration %q", text)
		}
		nanoseconds := new(big.Rat).Mul(seconds, big.NewRat(int64(time.Second), 1))
		wholeNanoseconds := new(big.Int).Quo(nanoseconds.Num(), nanoseconds.Denom())
		if !wholeNanoseconds.IsInt64() || wholeNanoseconds.Int64() > int64(^uint64(0)>>1)-total {
			return nil, evalError(expression, "duration %q exceeds the supported range", text)
		}
		total += wholeNanoseconds.Int64()
	}
	value := time.Duration(total)
	if matches[1] != "" {
		value = -value
	}
	return value, nil
}

func randomUUID() (string, error) {
	var data [16]byte
	if _, err := rand.Read(data[:]); err != nil {
		return "", fmt.Errorf("generate random UUID: %w", err)
	}
	data[6] = data[6]&0x0f | 0x40
	data[8] = data[8]&0x3f | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		data[0:4], data[4:6], data[6:8], data[8:10], data[10:16]), nil
}
