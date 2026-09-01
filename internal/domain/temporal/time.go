package temporal

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// LocalTime is a nanosecond-precision wall-clock time without an offset. Its
// zero value is the valid value 00:00.
type LocalTime struct {
	nanoOfDay int64
}

// NewLocalTime constructs a wall-clock time.
func NewLocalTime(hour, minute, second, nanosecond int) (LocalTime, error) {
	if hour < 0 || hour > 23 {
		return LocalTime{}, fmt.Errorf("%w: hour %d outside [0,23]", ErrInvalid, hour)
	}
	if minute < 0 || minute > 59 {
		return LocalTime{}, fmt.Errorf("%w: minute %d outside [0,59]", ErrInvalid, minute)
	}
	if second < 0 || second > 59 {
		return LocalTime{}, fmt.Errorf("%w: second %d outside [0,59]", ErrInvalid, second)
	}
	if nanosecond < 0 || nanosecond >= int(nanosecondsPerSecond) {
		return LocalTime{}, fmt.Errorf("%w: nanosecond %d outside [0,999999999]", ErrInvalid, nanosecond)
	}
	nanoOfDay := (int64(hour)*3600+int64(minute)*60+int64(second))*nanosecondsPerSecond + int64(nanosecond)
	return LocalTime{nanoOfDay: nanoOfDay}, nil
}

func localTimeFromNanoOfDay(nanoseconds int64) (LocalTime, error) {
	if nanoseconds < 0 || nanoseconds >= nanosecondsPerDay {
		return LocalTime{}, fmt.Errorf("%w: nanoseconds of day outside valid range", ErrInvalid)
	}
	return LocalTime{nanoOfDay: nanoseconds}, nil
}

// ParseLocalTime accepts the basic and extended M23 local-time forms. Missing
// minute or second components default to zero.
func ParseLocalTime(input string) (LocalTime, error) {
	if input == "" {
		return LocalTime{}, fmt.Errorf("%w: empty local time", ErrInvalid)
	}
	if strings.ContainsAny(input, "+-Zz") {
		return LocalTime{}, fmt.Errorf("%w: local time contains an offset", ErrInvalid)
	}
	whole := input
	fraction := ""
	if index := strings.IndexAny(whole, ".,"); index >= 0 {
		fraction = whole[index+1:]
		whole = whole[:index]
		if fraction == "" || strings.ContainsAny(fraction, ".,") {
			return LocalTime{}, fmt.Errorf("%w: malformed fractional second", ErrInvalid)
		}
	}
	var pieces []string
	if strings.Contains(whole, ":") {
		pieces = strings.Split(whole, ":")
		if len(pieces) < 1 || len(pieces) > 3 {
			return LocalTime{}, fmt.Errorf("%w: malformed local time %q", ErrInvalid, input)
		}
		for _, piece := range pieces {
			if len(piece) != 2 {
				return LocalTime{}, fmt.Errorf("%w: time components must have two digits", ErrInvalid)
			}
		}
	} else {
		if len(whole) != 2 && len(whole) != 4 && len(whole) != 6 {
			return LocalTime{}, fmt.Errorf("%w: malformed basic local time %q", ErrInvalid, input)
		}
		for index := 0; index < len(whole); index += 2 {
			pieces = append(pieces, whole[index:index+2])
		}
	}
	if fraction != "" && len(pieces) != 3 {
		return LocalTime{}, fmt.Errorf("%w: fractional value requires seconds", ErrInvalid)
	}
	values := [3]int{}
	for index, piece := range pieces {
		value, err := parseUnsigned(piece, "time component")
		if err != nil {
			return LocalTime{}, err
		}
		values[index] = value
	}
	nanosecond := 0
	if fraction != "" {
		if len(fraction) > 9 {
			return LocalTime{}, fmt.Errorf("%w: subsecond precision exceeds nanoseconds", ErrInvalid)
		}
		for _, character := range fraction {
			if character < '0' || character > '9' {
				return LocalTime{}, fmt.Errorf("%w: malformed fractional second", ErrInvalid)
			}
		}
		padded := fraction + strings.Repeat("0", 9-len(fraction))
		parsed, err := strconv.Atoi(padded)
		if err != nil {
			return LocalTime{}, fmt.Errorf("%w: parse fractional second: %v", ErrInvalid, err)
		}
		nanosecond = parsed
	}
	return NewLocalTime(values[0], values[1], values[2], nanosecond)
}

// Hour returns the hour of day.
func (t LocalTime) Hour() int { return int(t.nanoOfDay / (3600 * nanosecondsPerSecond)) }

// Minute returns the minute of hour.
func (t LocalTime) Minute() int { return int(t.nanoOfDay/(60*nanosecondsPerSecond)) % 60 }

// Second returns the second of minute.
func (t LocalTime) Second() int { return int(t.nanoOfDay/nanosecondsPerSecond) % 60 }

// Nanosecond returns the nanosecond of second.
func (t LocalTime) Nanosecond() int { return int(t.nanoOfDay % nanosecondsPerSecond) }

// Millisecond returns the millisecond of second.
func (t LocalTime) Millisecond() int { return t.Nanosecond() / 1_000_000 }

// Microsecond returns the microsecond of second.
func (t LocalTime) Microsecond() int { return t.Nanosecond() / 1_000 }

// NanosecondOfDay returns the canonical scalar representation.
func (t LocalTime) NanosecondOfDay() int64 { return t.nanoOfDay }

// Add adds only the seconds component group of a duration and wraps at 24
// hours, as required for Cypher time-of-day arithmetic.
func (t LocalTime) Add(duration Duration) (LocalTime, error) {
	seconds := floorMod(duration.seconds, secondsPerDay)
	delta := seconds*nanosecondsPerSecond + int64(duration.nanoseconds)
	return localTimeFromNanoOfDay(floorMod(t.nanoOfDay+delta, nanosecondsPerDay))
}

// Subtract subtracts only the seconds component group of a duration.
func (t LocalTime) Subtract(duration Duration) (LocalTime, error) {
	negated, err := duration.Negate()
	if err != nil {
		return LocalTime{}, err
	}
	return t.Add(negated)
}

// Compare returns -1, 0, or 1 in wall-clock order.
func (t LocalTime) Compare(other LocalTime) int { return compareInt64(t.nanoOfDay, other.nanoOfDay) }

// Equal reports exact LocalTime equality.
func (t LocalTime) Equal(other LocalTime) bool { return t == other }

func (t LocalTime) String() string { return formatLocalTime(t) }

// MarshalText emits the canonical parseable M23 representation.
func (t LocalTime) MarshalText() ([]byte, error) { return []byte(t.String()), nil }

// MarshalJSON emits the canonical temporal string.
func (t LocalTime) MarshalJSON() ([]byte, error) { return json.Marshal(t.String()) }

func formatLocalTime(value LocalTime) string {
	hour, minute, second, nanosecond := value.Hour(), value.Minute(), value.Second(), value.Nanosecond()
	result := fmt.Sprintf("%02d:%02d", hour, minute)
	if second != 0 || nanosecond != 0 {
		result += fmt.Sprintf(":%02d", second)
	}
	if nanosecond != 0 {
		fraction := strings.TrimRight(fmt.Sprintf("%09d", nanosecond), "0")
		result += "." + fraction
	}
	return result
}

// Time is a nanosecond-precision wall-clock time with a fixed UTC offset. It
// never contains a named timezone because a date is required to resolve one.
type Time struct {
	local         LocalTime
	offsetSeconds int32
}

// NewTime constructs an offset time.
func NewTime(local LocalTime, offsetSeconds int) (Time, error) {
	if err := validateOffset(offsetSeconds); err != nil {
		return Time{}, err
	}
	return Time{local: local, offsetSeconds: int32(offsetSeconds)}, nil
}

// ParseTime parses a local time followed by Z or a numeric UTC offset.
func ParseTime(input string) (Time, error) {
	localText, offsetText, err := splitOffsetSuffix(input)
	if err != nil {
		return Time{}, err
	}
	local, err := ParseLocalTime(localText)
	if err != nil {
		return Time{}, err
	}
	offset, err := ParseOffset(offsetText)
	if err != nil {
		return Time{}, err
	}
	return NewTime(local, offset)
}

// ParseOffset parses Z, ±HH, ±HHMM, ±HH:MM, and corresponding forms with an
// optional seconds component.
func ParseOffset(input string) (int, error) {
	if input == "Z" || input == "z" {
		return 0, nil
	}
	if len(input) < 3 || input[0] != '+' && input[0] != '-' {
		return 0, fmt.Errorf("%w: malformed UTC offset %q", ErrInvalid, input)
	}
	sign := 1
	if input[0] == '-' {
		sign = -1
	}
	body := input[1:]
	validShape := false
	switch len(body) {
	case 2, 4, 6:
		validShape = !strings.Contains(body, ":")
	case 5:
		validShape = body[2] == ':' && strings.Count(body, ":") == 1
	case 8:
		validShape = body[2] == ':' && body[5] == ':' && strings.Count(body, ":") == 2
	}
	if !validShape {
		return 0, fmt.Errorf("%w: malformed UTC offset %q", ErrInvalid, input)
	}
	digits := strings.ReplaceAll(body, ":", "")
	if len(digits) != 2 && len(digits) != 4 && len(digits) != 6 {
		return 0, fmt.Errorf("%w: malformed UTC offset %q", ErrInvalid, input)
	}
	hour, err := parseUnsigned(digits[:2], "offset hour")
	if err != nil {
		return 0, err
	}
	minute, second := 0, 0
	if len(digits) >= 4 {
		minute, err = parseUnsigned(digits[2:4], "offset minute")
		if err != nil {
			return 0, err
		}
	}
	if len(digits) == 6 {
		second, err = parseUnsigned(digits[4:6], "offset second")
		if err != nil {
			return 0, err
		}
	}
	if minute > 59 || second > 59 {
		return 0, fmt.Errorf("%w: malformed UTC offset %q", ErrInvalid, input)
	}
	offset := sign * (hour*3600 + minute*60 + second)
	if err := validateOffset(offset); err != nil {
		return 0, err
	}
	return offset, nil
}

func validateOffset(offset int) error {
	const maximum = 18 * 60 * 60
	if offset < -maximum || offset > maximum || (offset == maximum || offset == -maximum) && offset%3600 != 0 {
		return fmt.Errorf("%w: UTC offset %d outside ±18:00", ErrInvalid, offset)
	}
	return nil
}

func splitOffsetSuffix(input string) (string, string, error) {
	if len(input) < 3 {
		return "", "", fmt.Errorf("%w: offset time is missing a UTC offset", ErrInvalid)
	}
	if input[len(input)-1] == 'Z' || input[len(input)-1] == 'z' {
		return input[:len(input)-1], input[len(input)-1:], nil
	}
	for index := len(input) - 1; index >= 1; index-- {
		if input[index] == '+' || input[index] == '-' {
			return input[:index], input[index:], nil
		}
	}
	return "", "", fmt.Errorf("%w: offset time is missing a UTC offset", ErrInvalid)
}

// LocalTime returns the wall-clock component.
func (t Time) LocalTime() LocalTime { return t.local }

// Hour returns the hour of day.
func (t Time) Hour() int { return t.local.Hour() }

// Minute returns the minute of hour.
func (t Time) Minute() int { return t.local.Minute() }

// Second returns the second of minute.
func (t Time) Second() int { return t.local.Second() }

// Nanosecond returns the nanosecond of second.
func (t Time) Nanosecond() int { return t.local.Nanosecond() }

// Millisecond returns the millisecond of second.
func (t Time) Millisecond() int { return t.local.Millisecond() }

// Microsecond returns the microsecond of second.
func (t Time) Microsecond() int { return t.local.Microsecond() }

// OffsetSeconds returns the signed UTC offset in seconds.
func (t Time) OffsetSeconds() int { return int(t.offsetSeconds) }

// OffsetMinutes returns the signed UTC offset truncated toward zero to minutes.
func (t Time) OffsetMinutes() int { return int(t.offsetSeconds) / 60 }

// Offset returns the canonical offset string.
func (t Time) Offset() string { return FormatOffset(int(t.offsetSeconds)) }

// Timezone returns the canonical offset string.
func (t Time) Timezone() string { return t.Offset() }

// Add adds only the duration's seconds group and preserves the offset.
func (t Time) Add(duration Duration) (Time, error) {
	local, err := t.local.Add(duration)
	if err != nil {
		return Time{}, err
	}
	return Time{local: local, offsetSeconds: t.offsetSeconds}, nil
}

// Subtract subtracts only the duration's seconds group.
func (t Time) Subtract(duration Duration) (Time, error) {
	local, err := t.local.Subtract(duration)
	if err != nil {
		return Time{}, err
	}
	return Time{local: local, offsetSeconds: t.offsetSeconds}, nil
}

// Compare implements M23's Time ordering: UTC-normalized time first and the
// effective offset west-to-east as the deterministic tie-breaker.
func (t Time) Compare(other Time) int {
	instant := t.local.nanoOfDay - int64(t.offsetSeconds)*nanosecondsPerSecond
	otherInstant := other.local.nanoOfDay - int64(other.offsetSeconds)*nanosecondsPerSecond
	if comparison := compareInt64(instant, otherInstant); comparison != 0 {
		return comparison
	}
	return compareInt64(int64(t.offsetSeconds), int64(other.offsetSeconds))
}

// Equal reports exact offset-time equality.
func (t Time) Equal(other Time) bool { return t == other }

func (t Time) String() string { return t.local.String() + FormatOffset(int(t.offsetSeconds)) }

// MarshalText emits the canonical parseable M23 representation.
func (t Time) MarshalText() ([]byte, error) { return []byte(t.String()), nil }

// MarshalJSON emits the canonical temporal string.
func (t Time) MarshalJSON() ([]byte, error) { return json.Marshal(t.String()) }

// FormatOffset renders a valid offset using Z, ±HH:MM, or ±HH:MM:SS.
func FormatOffset(offset int) string {
	if offset == 0 {
		return "Z"
	}
	sign := '+'
	if offset < 0 {
		sign = '-'
		offset = -offset
	}
	hour := offset / 3600
	minute := offset / 60 % 60
	second := offset % 60
	if second == 0 {
		return fmt.Sprintf("%c%02d:%02d", sign, hour, minute)
	}
	return fmt.Sprintf("%c%02d:%02d:%02d", sign, hour, minute, second)
}
