package temporal

import (
	"fmt"
	"strconv"
)

// DateFromComponents implements the calendar, ISO week, ordinal, and quarter
// map forms. A date selector supplies defaults in the selected representation.
func DateFromComponents(components ComponentMap) (Date, error) {
	allowed := map[string]struct{}{
		"date": {}, "year": {}, "month": {}, "day": {}, "week": {},
		"dayOfWeek": {}, "ordinalDay": {}, "quarter": {}, "dayOfQuarter": {},
	}
	if err := rejectUnknownComponents(components, allowed, "date"); err != nil {
		return Date{}, err
	}
	base, hasBase, err := selectedDate(components["date"])
	if err != nil {
		return Date{}, err
	}
	weekMode := hasAny(components, "week", "dayOfWeek")
	ordinalMode := hasAny(components, "ordinalDay")
	quarterMode := hasAny(components, "quarter", "dayOfQuarter")
	calendarMode := hasAny(components, "month", "day")
	modes := 0
	for _, enabled := range []bool{weekMode, ordinalMode, quarterMode, calendarMode} {
		if enabled {
			modes++
		}
	}
	if modes > 1 {
		return Date{}, fmt.Errorf("%w: mixed date component representations", ErrInvalid)
	}
	if weekMode {
		defaultYear, defaultWeek, defaultDay := int64(0), int64(1), int64(1)
		if hasBase {
			var defaultWeekInt int
			defaultYear, defaultWeekInt = base.ISOWeek()
			defaultWeek = int64(defaultWeekInt)
			defaultDay = int64(base.WeekDay())
		}
		year, ok, err := componentInt64(components, "year", defaultYear)
		if err != nil {
			return Date{}, err
		}
		if !ok && !hasBase {
			return Date{}, fmt.Errorf("%w: date component year is required", ErrInvalid)
		}
		week, _, err := componentInt64(components, "week", defaultWeek)
		if err != nil {
			return Date{}, err
		}
		day, _, err := componentInt64(components, "dayOfWeek", defaultDay)
		if err != nil {
			return Date{}, err
		}
		return dateFromWeekInt64(year, week, day)
	}
	if ordinalMode {
		defaultYear, defaultOrdinal := int64(0), int64(1)
		if hasBase {
			defaultYear, defaultOrdinal = base.Year(), int64(base.OrdinalDay())
		}
		year, ok, err := componentInt64(components, "year", defaultYear)
		if err != nil {
			return Date{}, err
		}
		if !ok && !hasBase {
			return Date{}, fmt.Errorf("%w: date component year is required", ErrInvalid)
		}
		ordinal, _, err := componentInt64(components, "ordinalDay", defaultOrdinal)
		if err != nil {
			return Date{}, err
		}
		ordinalInt, err := intFromInt64(ordinal, "ordinalDay")
		if err != nil {
			return Date{}, err
		}
		return DateFromOrdinal(year, ordinalInt)
	}
	if quarterMode {
		defaultYear, defaultQuarter, defaultDay := int64(0), int64(1), int64(1)
		if hasBase {
			defaultYear, defaultQuarter, defaultDay = base.Year(), int64(base.Quarter()), int64(base.DayOfQuarter())
		}
		year, ok, err := componentInt64(components, "year", defaultYear)
		if err != nil {
			return Date{}, err
		}
		if !ok && !hasBase {
			return Date{}, fmt.Errorf("%w: date component year is required", ErrInvalid)
		}
		quarter, _, err := componentInt64(components, "quarter", defaultQuarter)
		if err != nil {
			return Date{}, err
		}
		day, _, err := componentInt64(components, "dayOfQuarter", defaultDay)
		if err != nil {
			return Date{}, err
		}
		quarterInt, err := intFromInt64(quarter, "quarter")
		if err != nil {
			return Date{}, err
		}
		dayInt, err := intFromInt64(day, "dayOfQuarter")
		if err != nil {
			return Date{}, err
		}
		return DateFromQuarter(year, quarterInt, dayInt)
	}
	defaultYear, defaultMonth, defaultDay := int64(0), int64(1), int64(1)
	if hasBase {
		defaultYear, defaultMonth, defaultDay = base.Year(), int64(base.Month()), int64(base.Day())
	}
	year, ok, err := componentInt64(components, "year", defaultYear)
	if err != nil {
		return Date{}, err
	}
	if !ok && !hasBase {
		return Date{}, fmt.Errorf("%w: date component year is required", ErrInvalid)
	}
	month, _, err := componentInt64(components, "month", defaultMonth)
	if err != nil {
		return Date{}, err
	}
	day, _, err := componentInt64(components, "day", defaultDay)
	if err != nil {
		return Date{}, err
	}
	monthInt, err := intFromInt64(month, "month")
	if err != nil {
		return Date{}, err
	}
	dayInt, err := intFromInt64(day, "day")
	if err != nil {
		return Date{}, err
	}
	return NewDate(year, monthInt, dayInt)
}

// LocalTimeFromComponents constructs a local time. When multiple subsecond
// fields are present, millisecond, microsecond, and nanosecond each contribute
// one three-digit group, as prescribed by M23.
func LocalTimeFromComponents(components ComponentMap) (LocalTime, error) {
	allowed := map[string]struct{}{
		"time": {}, "hour": {}, "minute": {}, "second": {},
		"millisecond": {}, "microsecond": {}, "nanosecond": {},
	}
	if err := rejectUnknownComponents(components, allowed, "local time"); err != nil {
		return LocalTime{}, err
	}
	base, hasBase, err := selectedLocalTime(components["time"])
	if err != nil {
		return LocalTime{}, err
	}
	defaults := [4]int64{}
	if hasBase {
		defaults = [4]int64{int64(base.Hour()), int64(base.Minute()), int64(base.Second()), int64(base.Nanosecond())}
	}
	hour, _, err := componentInt64(components, "hour", defaults[0])
	if err != nil {
		return LocalTime{}, err
	}
	minute, _, err := componentInt64(components, "minute", defaults[1])
	if err != nil {
		return LocalTime{}, err
	}
	second, _, err := componentInt64(components, "second", defaults[2])
	if err != nil {
		return LocalTime{}, err
	}
	nanosecond, err := subsecondFromComponents(components, defaults[3])
	if err != nil {
		return LocalTime{}, err
	}
	hourInt, err := intFromInt64(hour, "hour")
	if err != nil {
		return LocalTime{}, err
	}
	minuteInt, err := intFromInt64(minute, "minute")
	if err != nil {
		return LocalTime{}, err
	}
	secondInt, err := intFromInt64(second, "second")
	if err != nil {
		return LocalTime{}, err
	}
	nanosecondInt, err := intFromInt64(nanosecond, "nanosecond")
	if err != nil {
		return LocalTime{}, err
	}
	return NewLocalTime(hourInt, minuteInt, secondInt, nanosecondInt)
}

func subsecondFromComponents(components ComponentMap, fallback int64) (int64, error) {
	names := []string{"millisecond", "microsecond", "nanosecond"}
	present := 0
	for _, name := range names {
		if _, ok := components[name]; ok {
			present++
		}
	}
	if present == 0 {
		return fallback, nil
	}
	result := int64(0)
	for index, name := range names {
		value, ok, err := componentInt64(components, name, 0)
		if err != nil {
			return 0, err
		}
		if !ok {
			continue
		}
		maximum := int64(999_999_999)
		factor := int64(1)
		switch index {
		case 0:
			maximum, factor = 999, 1_000_000
			if present == 1 {
				maximum = 999
			}
		case 1:
			maximum, factor = 999_999, 1_000
			if present > 1 {
				maximum = 999
			}
		case 2:
			if present > 1 {
				maximum = 999
			}
		}
		if value < 0 || value > maximum {
			return 0, fmt.Errorf("%w: %s %d outside [0,%d]", ErrInvalid, name, value, maximum)
		}
		contribution, err := checkedMul(value, factor)
		if err != nil {
			return 0, err
		}
		result, err = checkedAdd(result, contribution)
		if err != nil {
			return 0, err
		}
	}
	if result >= nanosecondsPerSecond {
		return 0, fmt.Errorf("%w: combined subsecond fields exceed one second", ErrInvalid)
	}
	return result, nil
}

// TimeFromComponents constructs an offset time. timezone defaults to
// defaultTimezone, which must be a numeric offset. A zoned selector is first
// converted to the requested offset before explicit wall-clock overrides.
func TimeFromComponents(components ComponentMap, defaultTimezone string) (Time, error) {
	allowed := map[string]struct{}{
		"time": {}, "hour": {}, "minute": {}, "second": {}, "millisecond": {},
		"microsecond": {}, "nanosecond": {}, "timezone": {},
	}
	if err := rejectUnknownComponents(components, allowed, "time"); err != nil {
		return Time{}, err
	}
	timezone := defaultTimezone
	if timezone == "" {
		timezone = "Z"
	}
	if value, ok := components["timezone"]; ok {
		text, ok := value.(string)
		if !ok {
			return Time{}, fmt.Errorf("%w: timezone must be a string", ErrInvalid)
		}
		timezone = text
	}
	offset, err := ParseOffset(timezone)
	if err != nil {
		return Time{}, fmt.Errorf("%w: Time requires a numeric UTC offset", ErrInvalid)
	}
	baseLocal, hasBase, err := selectedLocalTime(components["time"])
	if err != nil {
		return Time{}, err
	}
	if hasBase {
		if baseOffset, zoned := selectedOffset(components["time"]); zoned {
			delta := int64(offset-baseOffset) * nanosecondsPerSecond
			baseLocal, _ = localTimeFromNanoOfDay(floorMod(baseLocal.nanoOfDay+delta, nanosecondsPerDay))
		}
	}
	localComponents := filterComponents(components, "hour", "minute", "second", "millisecond", "microsecond", "nanosecond")
	if hasBase {
		localComponents["time"] = baseLocal
	}
	local, err := LocalTimeFromComponents(localComponents)
	if err != nil {
		return Time{}, err
	}
	return NewTime(local, offset)
}

// LocalDateTimeFromComponents composes date and time selectors with explicit
// component overrides.
func LocalDateTimeFromComponents(components ComponentMap) (LocalDateTime, error) {
	allowed := map[string]struct{}{
		"datetime": {}, "date": {}, "time": {}, "year": {}, "month": {}, "day": {},
		"week": {}, "dayOfWeek": {}, "ordinalDay": {}, "quarter": {}, "dayOfQuarter": {},
		"hour": {}, "minute": {}, "second": {}, "millisecond": {}, "microsecond": {}, "nanosecond": {},
	}
	if err := rejectUnknownComponents(components, allowed, "local date-time"); err != nil {
		return LocalDateTime{}, err
	}
	var base *LocalDateTime
	if value, ok := components["datetime"]; ok {
		switch typed := value.(type) {
		case LocalDateTime:
			copy := typed
			base = &copy
		case DateTime:
			copy := typed.LocalDateTime()
			base = &copy
		default:
			return LocalDateTime{}, fmt.Errorf("%w: datetime selector has type %T", ErrInvalid, value)
		}
	}
	dateComponents := filterComponents(components, "year", "month", "day", "week", "dayOfWeek", "ordinalDay", "quarter", "dayOfQuarter")
	if value, ok := components["date"]; ok {
		dateComponents["date"] = value
	} else if base != nil {
		dateComponents["date"] = base.date
	}
	date, err := DateFromComponents(dateComponents)
	if err != nil {
		return LocalDateTime{}, err
	}
	timeComponents := filterComponents(components, "hour", "minute", "second", "millisecond", "microsecond", "nanosecond")
	if value, ok := components["time"]; ok {
		timeComponents["time"] = value
	} else if base != nil {
		timeComponents["time"] = base.time
	}
	localTime, err := LocalTimeFromComponents(timeComponents)
	if err != nil {
		return LocalDateTime{}, err
	}
	return NewLocalDateTime(date, localTime), nil
}

// DateTimeFromComponents constructs a zoned DateTime from local components.
// Engine-level projection can use WithTimezone before supplying overrides when
// it needs instant-preserving conversion of a datetime selector.
func DateTimeFromComponents(components ComponentMap, defaultTimezone string) (DateTime, error) {
	allowed := map[string]struct{}{
		"datetime": {}, "date": {}, "time": {}, "year": {}, "month": {}, "day": {},
		"week": {}, "dayOfWeek": {}, "ordinalDay": {}, "quarter": {}, "dayOfQuarter": {},
		"hour": {}, "minute": {}, "second": {}, "millisecond": {}, "microsecond": {}, "nanosecond": {},
		"timezone": {},
	}
	if err := rejectUnknownComponents(components, allowed, "date-time"); err != nil {
		return DateTime{}, err
	}
	timezone := defaultTimezone
	if timezone == "" {
		timezone = "Z"
	}
	if value, ok := components["timezone"]; ok {
		text, ok := value.(string)
		if !ok {
			return DateTime{}, fmt.Errorf("%w: timezone must be a string", ErrInvalid)
		}
		timezone = text
	}
	localComponents := filterComponents(components,
		"datetime", "date", "time", "year", "month", "day", "week", "dayOfWeek",
		"ordinalDay", "quarter", "dayOfQuarter", "hour", "minute", "second",
		"millisecond", "microsecond", "nanosecond")
	if base, ok := components["datetime"].(DateTime); ok {
		converted, err := base.WithTimezone(timezone)
		if err != nil {
			return DateTime{}, err
		}
		localComponents["datetime"] = converted.LocalDateTime()
	}
	local, err := LocalDateTimeFromComponents(localComponents)
	if err != nil {
		return DateTime{}, err
	}
	return NewDateTime(local, timezone)
}

// WithTimezone preserves the instant while changing its timezone
// representation.
func (d DateTime) WithTimezone(timezone string) (DateTime, error) {
	if offset, err := ParseOffset(timezone); err == nil {
		zone, _ := FixedZone(offset)
		return restoreDateTime(d.epochSecond, int64(d.nanosecond), zone)
	}
	return DateTimeFromEpoch(d.epochSecond, int64(d.nanosecond), timezone)
}

func selectedDate(value any) (Date, bool, error) {
	if value == nil {
		return Date{}, false, nil
	}
	switch typed := value.(type) {
	case Date:
		return typed, true, nil
	case LocalDateTime:
		return typed.Date(), true, nil
	case DateTime:
		return typed.Date(), true, nil
	default:
		return Date{}, false, fmt.Errorf("%w: date selector has type %T", ErrInvalid, value)
	}
}

func dateFromWeekInt64(year, week, day int64) (Date, error) {
	weekInt, err := intFromInt64(week, "week")
	if err != nil {
		return Date{}, err
	}
	dayInt, err := intFromInt64(day, "dayOfWeek")
	if err != nil {
		return Date{}, err
	}
	return DateFromWeek(year, weekInt, dayInt)
}

func selectedLocalTime(value any) (LocalTime, bool, error) {
	if value == nil {
		return LocalTime{}, false, nil
	}
	switch typed := value.(type) {
	case LocalTime:
		return typed, true, nil
	case Time:
		return typed.LocalTime(), true, nil
	case LocalDateTime:
		return typed.LocalTime(), true, nil
	case DateTime:
		return typed.LocalTime(), true, nil
	default:
		return LocalTime{}, false, fmt.Errorf("%w: time selector has type %T", ErrInvalid, value)
	}
}

func selectedOffset(value any) (int, bool) {
	switch typed := value.(type) {
	case Time:
		return typed.OffsetSeconds(), true
	case DateTime:
		return typed.OffsetSeconds(), true
	default:
		return 0, false
	}
}

func componentInt64(components ComponentMap, name string, fallback int64) (int64, bool, error) {
	value, ok := components[name]
	if !ok {
		return fallback, false, nil
	}
	rational, err := numberRat(value)
	if err != nil {
		return 0, true, fmt.Errorf("%w: component %s: %v", ErrInvalid, name, err)
	}
	if !rational.IsInt() || !rational.Num().IsInt64() {
		return 0, true, fmt.Errorf("%w: component %s must be an int64", ErrInvalid, name)
	}
	return rational.Num().Int64(), true, nil
}

func rejectUnknownComponents(components ComponentMap, allowed map[string]struct{}, kind string) error {
	for key := range components {
		if _, ok := allowed[key]; !ok {
			return fmt.Errorf("%w: unknown %s component %q", ErrInvalid, kind, key)
		}
	}
	return nil
}

func hasAny(components ComponentMap, names ...string) bool {
	for _, name := range names {
		if _, ok := components[name]; ok {
			return true
		}
	}
	return false
}

func filterComponents(components ComponentMap, names ...string) ComponentMap {
	result := make(ComponentMap)
	for _, name := range names {
		if value, ok := components[name]; ok {
			result[name] = value
		}
	}
	return result
}

func intFromInt64(value int64, name string) (int, error) {
	if strconv.IntSize == 32 && (value < -1<<31 || value > 1<<31-1) {
		return 0, fmt.Errorf("%w: component %s overflows int", ErrOverflow, name)
	}
	return int(value), nil
}
