package engine

import (
	"crypto/rand"
	"fmt"
	"strings"
	"time"

	"github.com/svlocks/sheets/internal/cypher"
	"github.com/svlocks/sheets/internal/domain"
	"github.com/svlocks/sheets/internal/domain/temporal"
)

func temporalValue(expression cypher.Expression, name string, arguments []any, now time.Time) (any, error) {
	if len(arguments) > 1 {
		return nil, evalError(expression, "%s expects zero or one argument", name)
	}
	if len(arguments) == 0 {
		return temporalFromClock(expression, name, now)
	}
	if arguments[0] == nil {
		return nil, nil
	}
	value, err := constructTemporal(name, arguments[0])
	if err != nil {
		return nil, evalError(expression, "invalid %s value: %v", name, err)
	}
	return value, nil
}

func durationValue(expression cypher.Expression, arguments []any) (any, error) {
	if len(arguments) != 1 {
		return nil, evalError(expression, "duration expects one argument")
	}
	if arguments[0] == nil {
		return nil, nil
	}
	var value temporal.Duration
	var err error
	switch argument := arguments[0].(type) {
	case temporal.Duration:
		return argument, nil
	case time.Duration:
		// Legacy stores represented every duration as elapsed nanoseconds. Keep
		// that conversion explicit rather than treating the two Go types as the
		// same Cypher value.
		value, err = temporal.NewDuration(0, 0, 0, int64(argument))
	case string:
		value, err = temporal.ParseDuration(argument)
	default:
		if components, ok := componentMap(argument); ok {
			value, err = temporal.DurationFromComponents(components)
		} else {
			return nil, evalError(expression, "duration expects a string, duration, or map, got %T", argument)
		}
	}
	if err != nil {
		return nil, evalError(expression, "invalid duration value: %v", err)
	}
	return value, nil
}

func temporalFromClock(expression cypher.Expression, name string, now time.Time) (any, error) {
	return temporalFromClockInZone(expression, name, now, "Z")
}

func (e evaluator) temporalClockFunction(expression cypher.Expression, name string, arguments []any) (any, error) {
	if len(arguments) > 1 {
		return nil, evalError(expression, "%s expects zero or one argument", name)
	}
	timezone := "Z"
	if len(arguments) == 1 {
		if arguments[0] == nil {
			return nil, nil
		}
		var ok bool
		timezone, ok = arguments[0].(string)
		if !ok {
			return nil, evalError(expression, "%s timezone must be a string", name)
		}
	}
	parts := strings.Split(name, ".")
	base, mode := parts[0], parts[1]
	instant := e.now()
	switch mode {
	case "transaction":
		instant = e.transactionNow
	case "statement":
		// e.now is captured once when the statement evaluator is created.
	case "realtime":
		instant = e.realtime()
	default:
		return nil, evalError(expression, "unknown clock mode %s", mode)
	}
	return temporalFromClockInZone(expression, base, instant, timezone)
}

func temporalFromClockInZone(expression cypher.Expression, name string, now time.Time, timezone string) (any, error) {
	dateTime, err := temporal.DateTimeFromEpoch(now.Unix(), int64(now.Nanosecond()), timezone)
	if err != nil {
		return nil, evalError(expression, "construct current %s in timezone %q: %v", name, timezone, err)
	}
	switch name {
	case "date":
		return dateTime.Date(), nil
	case "localtime":
		return dateTime.LocalTime(), nil
	case "time":
		return temporal.NewTime(dateTime.LocalTime(), dateTime.OffsetSeconds())
	case "localdatetime":
		return dateTime.LocalDateTime(), nil
	case "datetime":
		return dateTime, nil
	default:
		return nil, evalError(expression, "unknown temporal constructor %s", name)
	}
}

func constructTemporal(name string, argument any) (any, error) {
	if components, ok := componentMap(argument); ok {
		switch name {
		case "date":
			return temporal.DateFromComponents(components)
		case "localtime":
			return temporal.LocalTimeFromComponents(components)
		case "time":
			return temporal.TimeFromComponents(prepareTimeComponents(components), selectedTimeZone(components))
		case "localdatetime":
			return temporal.LocalDateTimeFromComponents(components)
		case "datetime":
			prepared, timezone, err := prepareDateTimeComponents(components)
			if err != nil {
				return nil, err
			}
			return temporal.DateTimeFromComponents(prepared, timezone)
		}
	}
	if text, ok := argument.(string); ok {
		switch name {
		case "date":
			return temporal.ParseDate(text)
		case "localtime":
			return temporal.ParseLocalTime(text)
		case "time":
			value, err := temporal.ParseTime(text)
			if err == nil {
				return value, nil
			}
			local, localErr := temporal.ParseLocalTime(text)
			if localErr != nil {
				return nil, err
			}
			return temporal.NewTime(local, 0)
		case "localdatetime":
			value, err := temporal.ParseLocalDateTime(text)
			if err == nil {
				return value, nil
			}
			date, dateErr := temporal.ParseDate(text)
			if dateErr != nil {
				return nil, err
			}
			return temporal.NewLocalDateTime(date, temporal.LocalTime{}), nil
		case "datetime":
			return temporal.ParseDateTime(text)
		}
	}
	switch name {
	case "date":
		switch value := argument.(type) {
		case temporal.Date:
			return value, nil
		case temporal.LocalDateTime:
			return value.Date(), nil
		case temporal.DateTime:
			return value.Date(), nil
		case time.Time:
			return temporal.NewDate(int64(value.Year()), int(value.Month()), value.Day())
		}
	case "localtime":
		switch value := argument.(type) {
		case temporal.LocalTime:
			return value, nil
		case temporal.Time:
			return value.LocalTime(), nil
		case temporal.LocalDateTime:
			return value.LocalTime(), nil
		case temporal.DateTime:
			return value.LocalTime(), nil
		case time.Time:
			return temporal.NewLocalTime(value.Hour(), value.Minute(), value.Second(), value.Nanosecond())
		}
	case "time":
		switch value := argument.(type) {
		case temporal.Time:
			return value, nil
		case temporal.LocalTime:
			return temporal.NewTime(value, 0)
		case temporal.LocalDateTime:
			return temporal.NewTime(value.LocalTime(), 0)
		case temporal.DateTime:
			return temporal.NewTime(value.LocalTime(), value.OffsetSeconds())
		case time.Time:
			_, offset := value.Zone()
			local, err := temporal.NewLocalTime(value.Hour(), value.Minute(), value.Second(), value.Nanosecond())
			if err != nil {
				return nil, err
			}
			return temporal.NewTime(local, offset)
		}
	case "localdatetime":
		switch value := argument.(type) {
		case temporal.LocalDateTime:
			return value, nil
		case temporal.DateTime:
			return value.LocalDateTime(), nil
		case temporal.Date:
			return temporal.NewLocalDateTime(value, temporal.LocalTime{}), nil
		case time.Time:
			date, err := temporal.NewDate(int64(value.Year()), int(value.Month()), value.Day())
			if err != nil {
				return nil, err
			}
			local, err := temporal.NewLocalTime(value.Hour(), value.Minute(), value.Second(), value.Nanosecond())
			if err != nil {
				return nil, err
			}
			return temporal.NewLocalDateTime(date, local), nil
		}
	case "datetime":
		switch value := argument.(type) {
		case temporal.DateTime:
			return value, nil
		case temporal.LocalDateTime:
			return temporal.NewDateTime(value, "Z")
		case temporal.Date:
			return temporal.NewDateTime(temporal.NewLocalDateTime(value, temporal.LocalTime{}), "Z")
		case time.Time:
			converted, ok := dateTimeFromLegacy(value)
			if !ok {
				return nil, fmt.Errorf("legacy time has an offset outside the Cypher DateTime range")
			}
			return converted, nil
		}
	}
	return nil, fmt.Errorf("expected a string, compatible temporal value, or map, got %T", argument)
}

func selectedTimeZone(components temporal.ComponentMap) string {
	if timezone, ok := components["timezone"].(string); ok {
		return timezone
	}
	switch value := components["time"].(type) {
	case temporal.Time:
		return value.Offset()
	case temporal.DateTime:
		return value.Offset()
	default:
		return "Z"
	}
}

func prepareTimeComponents(components temporal.ComponentMap) temporal.ComponentMap {
	// The domain constructor already converts an offset selector when the
	// requested timezone differs. Copy the map so engine evaluation never
	// mutates a query parameter supplied by its caller.
	return cloneComponentMap(components)
}

func prepareDateTimeComponents(components temporal.ComponentMap) (temporal.ComponentMap, string, error) {
	prepared := cloneComponentMap(components)
	timezone := "Z"
	if explicit, ok := prepared["timezone"]; ok {
		var valid bool
		timezone, valid = explicit.(string)
		if !valid {
			return nil, "", fmt.Errorf("timezone must be a string")
		}
	} else {
		switch selected := prepared["datetime"].(type) {
		case temporal.DateTime:
			timezone = selected.Timezone()
		default:
			switch selected := prepared["time"].(type) {
			case temporal.Time:
				timezone = selected.Offset()
			case temporal.DateTime:
				timezone = selected.Timezone()
			}
		}
	}
	if selected, ok := prepared["time"]; ok {
		switch selected := selected.(type) {
		case temporal.Time:
			converted, err := projectTimeSelector(prepared, selected.Offset(), timezone)
			if err != nil {
				return nil, "", err
			}
			prepared["time"] = converted
		case temporal.DateTime:
			converted, err := projectTimeSelector(prepared, selected.Timezone(), timezone)
			if err != nil {
				return nil, "", err
			}
			prepared["time"] = converted
		}
	}
	return prepared, timezone, nil
}

func projectTimeSelector(components temporal.ComponentMap, sourceTimezone, targetTimezone string) (temporal.LocalTime, error) {
	localComponents := cloneComponentMap(components)
	delete(localComponents, "timezone")
	projected, err := temporal.LocalDateTimeFromComponents(localComponents)
	if err != nil {
		return temporal.LocalTime{}, err
	}
	source, err := temporal.NewDateTime(projected, sourceTimezone)
	if err != nil {
		return temporal.LocalTime{}, err
	}
	converted, err := source.WithTimezone(targetTimezone)
	if err != nil {
		return temporal.LocalTime{}, err
	}
	return converted.LocalTime(), nil
}

func cloneComponentMap(source temporal.ComponentMap) temporal.ComponentMap {
	result := make(temporal.ComponentMap, len(source)+1)
	for key, value := range source {
		result[key] = value
	}
	return result
}

func truncateTemporalValue(expression cypher.Expression, name string, arguments []any) (any, error) {
	if len(arguments) != 2 && len(arguments) != 3 {
		return nil, evalError(expression, "%s expects a unit, temporal value, and optional map", name)
	}
	if arguments[0] == nil || arguments[1] == nil || len(arguments) == 3 && arguments[2] == nil {
		return nil, nil
	}
	unit, ok := arguments[0].(string)
	if !ok {
		return nil, evalError(expression, "%s unit must be a string", name)
	}
	replacements := temporal.ComponentMap{}
	if len(arguments) == 3 {
		var mapOK bool
		replacements, mapOK = componentMap(arguments[2])
		if !mapOK {
			return nil, evalError(expression, "%s replacement argument must be a map", name)
		}
		replacements = cloneComponentMap(replacements)
	}
	constructor := name[:len(name)-len(".truncate")]
	converted, err := constructTemporal(constructor, arguments[1])
	if err != nil {
		return nil, evalError(expression, "%s cannot convert its temporal argument: %v", name, err)
	}
	var result any
	switch value := converted.(type) {
	case temporal.Date:
		truncated, truncateErr := value.Truncate(unit)
		if truncateErr == nil {
			replacements["date"] = truncated
			result, truncateErr = temporal.DateFromComponents(replacements)
		}
		err = truncateErr
	case temporal.LocalTime:
		truncated, truncateErr := value.Truncate(unit)
		if truncateErr == nil {
			if replacementErr := mergeTruncatedNanosecond(replacements, truncated); replacementErr != nil {
				return nil, evalError(expression, "%s failed: %v", name, replacementErr)
			}
			replacements["time"] = truncated
			result, truncateErr = temporal.LocalTimeFromComponents(replacements)
		}
		err = truncateErr
	case temporal.Time:
		truncated, truncateErr := value.Truncate(unit)
		if truncateErr == nil {
			if replacementErr := mergeTruncatedNanosecond(replacements, truncated.LocalTime()); replacementErr != nil {
				return nil, evalError(expression, "%s failed: %v", name, replacementErr)
			}
			timezone := truncated.Offset()
			if replacement, exists := replacements["timezone"]; exists {
				var timezoneOK bool
				timezone, timezoneOK = replacement.(string)
				if !timezoneOK {
					return nil, evalError(expression, "%s timezone must be a string", name)
				}
			}
			// Truncation's replacement map changes local components; unlike an
			// ordinary selector constructor, changing timezone here does not
			// preserve the pre-replacement instant.
			replacements["time"] = truncated.LocalTime()
			result, truncateErr = temporal.TimeFromComponents(replacements, timezone)
		}
		err = truncateErr
	case temporal.LocalDateTime:
		truncated, truncateErr := value.Truncate(unit)
		if truncateErr == nil {
			if replacementErr := mergeTruncatedNanosecond(replacements, truncated.LocalTime()); replacementErr != nil {
				return nil, evalError(expression, "%s failed: %v", name, replacementErr)
			}
			replacements["datetime"] = truncated
			result, truncateErr = temporal.LocalDateTimeFromComponents(replacements)
		}
		err = truncateErr
	case temporal.DateTime:
		truncated, truncateErr := value.Truncate(unit)
		if truncateErr == nil {
			if replacementErr := mergeTruncatedNanosecond(replacements, truncated.LocalTime()); replacementErr != nil {
				return nil, evalError(expression, "%s failed: %v", name, replacementErr)
			}
			timezone := truncated.Timezone()
			if replacement, exists := replacements["timezone"]; exists {
				var timezoneOK bool
				timezone, timezoneOK = replacement.(string)
				if !timezoneOK {
					return nil, evalError(expression, "%s timezone must be a string", name)
				}
			}
			replacements["datetime"] = truncated.LocalDateTime()
			result, truncateErr = temporal.DateTimeFromComponents(replacements, timezone)
		}
		err = truncateErr
	default:
		return nil, evalError(expression, "%s received unsupported temporal type %T", name, converted)
	}
	if err != nil {
		return nil, evalError(expression, "%s failed: %v", name, err)
	}
	return result, nil
}

func mergeTruncatedNanosecond(replacements temporal.ComponentMap, base temporal.LocalTime) error {
	replacement, exists := replacements["nanosecond"]
	if !exists {
		return nil
	}
	nanosecond, ok := integer(replacement)
	if !ok || nanosecond < 0 || nanosecond > 999_999_999 {
		return fmt.Errorf("nanosecond replacement must be an integer in [0,999999999]")
	}
	combined := int64(base.Nanosecond()) + nanosecond
	if combined >= 1_000_000_000 {
		return fmt.Errorf("nanosecond replacement exceeds one second")
	}
	replacements["nanosecond"] = combined
	return nil
}

type temporalPoint struct {
	hasDate      bool
	date         temporal.Date
	time         temporal.LocalTime
	hasZone      bool
	timezone     string
	offsetSecond int
	hasInstant   bool
	epochSecond  int64
	nanosecond   int
}

func durationBetweenValue(expression cypher.Expression, name string, arguments []any) (any, error) {
	if len(arguments) != 2 {
		return nil, evalError(expression, "%s expects 2 arguments", name)
	}
	if arguments[0] == nil || arguments[1] == nil {
		return nil, nil
	}
	left, err := describeTemporal(arguments[0])
	if err != nil {
		return nil, evalError(expression, "%s left argument: %v", name, err)
	}
	right, err := describeTemporal(arguments[1])
	if err != nil {
		return nil, evalError(expression, "%s right argument: %v", name, err)
	}
	var result temporal.Duration
	switch name {
	case "duration.inmonths":
		months, monthErr := monthsBetween(left, right)
		if monthErr != nil {
			err = monthErr
		} else {
			result, err = temporal.NewDuration(months, 0, 0, 0)
		}
	case "duration.indays":
		days, dayErr := daysBetween(left, right)
		if dayErr != nil {
			err = dayErr
		} else {
			result, err = temporal.NewDuration(0, days, 0, 0)
		}
	case "duration.inseconds":
		seconds, nanoseconds, differenceErr := secondsBetween(left, right)
		if differenceErr != nil {
			err = differenceErr
		} else {
			result, err = temporal.NewDuration(0, 0, seconds, nanoseconds)
		}
	case "duration.between":
		result, err = durationBetween(left, right)
	default:
		return nil, evalError(expression, "unknown duration-between function %s", name)
	}
	if err != nil {
		return nil, evalError(expression, "%s failed: %v", name, err)
	}
	return result, nil
}

func dateArithmeticDuration(value temporal.Duration) (temporal.Duration, error) {
	elapsedDays := value.CanonicalSeconds() / 86_400
	days := value.Days()
	if elapsedDays > 0 && days > int64(^uint64(0)>>1)-elapsedDays || elapsedDays < 0 && days < -int64(^uint64(0)>>1)-1-elapsedDays {
		return temporal.Duration{}, fmt.Errorf("date duration day overflow")
	}
	return temporal.NewDuration(value.Months(), days+elapsedDays, 0, 0)
}

func describeTemporal(value any) (temporalPoint, error) {
	switch value := value.(type) {
	case temporal.Date:
		return temporalPoint{hasDate: true, date: value}, nil
	case temporal.LocalTime:
		return temporalPoint{time: value}, nil
	case temporal.Time:
		return temporalPoint{time: value.LocalTime(), hasZone: true, timezone: value.Offset(), offsetSecond: value.OffsetSeconds()}, nil
	case temporal.LocalDateTime:
		return temporalPoint{hasDate: true, date: value.Date(), time: value.LocalTime()}, nil
	case temporal.DateTime:
		return temporalPoint{
			hasDate: true, date: value.Date(), time: value.LocalTime(), hasZone: true,
			timezone: value.Timezone(), offsetSecond: value.OffsetSeconds(), hasInstant: true,
			epochSecond: value.EpochSecond(), nanosecond: value.Nanosecond(),
		}, nil
	case time.Time:
		date, err := temporal.NewDate(int64(value.Year()), int(value.Month()), value.Day())
		if err != nil {
			return temporalPoint{}, err
		}
		local, err := temporal.NewLocalTime(value.Hour(), value.Minute(), value.Second(), value.Nanosecond())
		if err != nil {
			return temporalPoint{}, err
		}
		_, offset := value.Zone()
		return temporalPoint{
			hasDate: true, date: date, time: local, hasZone: true, timezone: temporal.FormatOffset(offset), offsetSecond: offset,
			hasInstant: true, epochSecond: value.Unix(), nanosecond: value.Nanosecond(),
		}, nil
	default:
		return temporalPoint{}, fmt.Errorf("expected a temporal value, got %T", value)
	}
}

func monthsBetween(left, right temporalPoint) (int64, error) {
	if !left.hasDate || !right.hasDate {
		return 0, nil
	}
	months := (right.date.Year()-left.date.Year())*12 + int64(right.date.Month()-left.date.Month())
	cursorDate, err := left.date.AddMonths(months)
	if err != nil {
		return 0, err
	}
	cursor := left
	cursor.date = cursorDate
	cursor.hasInstant = false
	cursorSeconds, cursorNanos, err := comparableScalar(cursor, right)
	if err != nil {
		return 0, err
	}
	targetSeconds, targetNanos, err := comparableScalar(right, cursor)
	if err != nil {
		return 0, err
	}
	comparison := compareScalar(cursorSeconds, cursorNanos, targetSeconds, targetNanos)
	if months > 0 && comparison > 0 {
		months--
	} else if months < 0 && comparison < 0 {
		months++
	}
	return months, nil
}

func daysBetween(left, right temporalPoint) (int64, error) {
	if !left.hasDate || !right.hasDate {
		return 0, nil
	}
	days := right.date.EpochDay() - left.date.EpochDay()
	cursor := left
	cursor.date = right.date
	cursor.hasInstant = false
	cursorSeconds, cursorNanos, err := comparableScalar(cursor, right)
	if err != nil {
		return 0, err
	}
	targetSeconds, targetNanos, err := comparableScalar(right, cursor)
	if err != nil {
		return 0, err
	}
	comparison := compareScalar(cursorSeconds, cursorNanos, targetSeconds, targetNanos)
	if days > 0 && comparison > 0 {
		days--
	} else if days < 0 && comparison < 0 {
		days++
	}
	return days, nil
}

func durationBetween(left, right temporalPoint) (temporal.Duration, error) {
	months, err := monthsBetween(left, right)
	if err != nil {
		return temporal.Duration{}, err
	}
	cursorDate := left.date
	if left.hasDate && right.hasDate {
		cursorDate, err = left.date.AddMonths(months)
		if err != nil {
			return temporal.Duration{}, err
		}
	}
	days := int64(0)
	if left.hasDate && right.hasDate {
		days = right.date.EpochDay() - cursorDate.EpochDay()
		cursorDate, err = cursorDate.AddDays(days)
		if err != nil {
			return temporal.Duration{}, err
		}
		cursor := left
		cursor.date = cursorDate
		cursor.hasInstant = false
		cursorSeconds, cursorNanos, scalarErr := comparableScalar(cursor, right)
		if scalarErr != nil {
			return temporal.Duration{}, scalarErr
		}
		rightSeconds, rightNanos, scalarErr := comparableScalar(right, cursor)
		if scalarErr != nil {
			return temporal.Duration{}, scalarErr
		}
		comparison := compareScalar(cursorSeconds, cursorNanos, rightSeconds, rightNanos)
		if days > 0 && comparison > 0 {
			days--
			cursorDate, err = cursorDate.AddDays(-1)
		} else if days < 0 && comparison < 0 {
			days++
			cursorDate, err = cursorDate.AddDays(1)
		}
		if err != nil {
			return temporal.Duration{}, err
		}
	}
	cursor := left
	if left.hasDate && right.hasDate {
		cursor.date = cursorDate
		cursor.hasInstant = false
	}
	seconds, nanoseconds, err := secondsBetween(cursor, right)
	if err != nil {
		return temporal.Duration{}, err
	}
	return temporal.NewDuration(months, days, seconds, nanoseconds)
}

func secondsBetween(left, right temporalPoint) (int64, int64, error) {
	leftSeconds, leftNanos, err := comparableScalar(left, right)
	if err != nil {
		return 0, 0, err
	}
	rightSeconds, rightNanos, err := comparableScalar(right, left)
	if err != nil {
		return 0, 0, err
	}
	return rightSeconds - leftSeconds, int64(rightNanos - leftNanos), nil
}

func comparableScalar(point, other temporalPoint) (int64, int, error) {
	useDate := point.hasDate || other.hasDate
	if useDate {
		date := point.date
		if !point.hasDate {
			date = other.date
		}
		if point.hasInstant && point.hasDate {
			return point.epochSecond, point.nanosecond, nil
		}
		zone := point.timezone
		if !point.hasZone && other.hasZone {
			zone = other.timezone
		}
		if zone != "" {
			value, err := temporal.NewDateTime(temporal.NewLocalDateTime(date, point.time), zone)
			if err != nil {
				return 0, 0, err
			}
			return value.EpochSecond(), value.Nanosecond(), nil
		}
		seconds := date.EpochDay()*86_400 + localSecondOfDay(point.time)
		return seconds, point.time.Nanosecond(), nil
	}
	offset := 0
	if point.hasZone {
		offset = point.offsetSecond
	} else if other.hasZone {
		offset = other.offsetSecond
	}
	return localSecondOfDay(point.time) - int64(offset), point.time.Nanosecond(), nil
}

func localSecondOfDay(value temporal.LocalTime) int64 {
	return int64(value.Hour()*3600 + value.Minute()*60 + value.Second())
}

func compareScalar(leftSeconds int64, leftNanos int, rightSeconds int64, rightNanos int) int {
	if leftSeconds < rightSeconds {
		return -1
	}
	if leftSeconds > rightSeconds {
		return 1
	}
	if leftNanos < rightNanos {
		return -1
	}
	if leftNanos > rightNanos {
		return 1
	}
	return 0
}

func componentMap(value any) (temporal.ComponentMap, bool) {
	switch value := value.(type) {
	case map[string]any:
		return temporal.ComponentMap(value), true
	case domain.Properties:
		return temporal.ComponentMap(value), true
	default:
		return nil, false
	}
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
