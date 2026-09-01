package temporal

import "fmt"

// Truncate returns the first instant of the requested date-based unit.
func (d Date) Truncate(unit string) (Date, error) {
	switch unit {
	case "millennium", "century", "decade":
		var size int64
		switch unit {
		case "millennium":
			size = 1_000
		case "century":
			size = 100
		default:
			size = 10
		}
		return NewDate(floorDiv(d.Year(), size)*size, 1, 1)
	case "year":
		return NewDate(d.Year(), 1, 1)
	case "weekYear":
		return DateFromWeek(d.WeekYear(), 1, 1)
	case "quarter":
		return NewDate(d.Year(), (d.Quarter()-1)*3+1, 1)
	case "month":
		return NewDate(d.Year(), d.Month(), 1)
	case "week":
		return d.AddDays(int64(1 - d.WeekDay()))
	case "day":
		return d, nil
	default:
		return Date{}, fmt.Errorf("%w: cannot truncate Date to %q", ErrInvalid, unit)
	}
}

// Truncate returns the first local time of the requested time-based unit.
func (t LocalTime) Truncate(unit string) (LocalTime, error) {
	divisor := int64(0)
	switch unit {
	case "day":
		return LocalTime{}, nil
	case "hour":
		divisor = 3600 * nanosecondsPerSecond
	case "minute":
		divisor = 60 * nanosecondsPerSecond
	case "second":
		divisor = nanosecondsPerSecond
	case "millisecond":
		divisor = 1_000_000
	case "microsecond":
		divisor = 1_000
	default:
		return LocalTime{}, fmt.Errorf("%w: cannot truncate LocalTime to %q", ErrInvalid, unit)
	}
	return localTimeFromNanoOfDay(t.nanoOfDay / divisor * divisor)
}

// Truncate truncates the local fields and preserves the fixed offset.
func (t Time) Truncate(unit string) (Time, error) {
	local, err := t.local.Truncate(unit)
	if err != nil {
		return Time{}, err
	}
	return Time{local: local, offsetSeconds: t.offsetSeconds}, nil
}

// Truncate returns the first local date-time of the requested unit.
func (d LocalDateTime) Truncate(unit string) (LocalDateTime, error) {
	switch unit {
	case "millennium", "century", "decade", "year", "weekYear", "quarter", "month", "week", "day":
		date, err := d.date.Truncate(unit)
		if err != nil {
			return LocalDateTime{}, err
		}
		return NewLocalDateTime(date, LocalTime{}), nil
	case "hour", "minute", "second", "millisecond", "microsecond":
		localTime, err := d.time.Truncate(unit)
		if err != nil {
			return LocalDateTime{}, err
		}
		return NewLocalDateTime(d.date, localTime), nil
	default:
		return LocalDateTime{}, fmt.Errorf("%w: cannot truncate LocalDateTime to %q", ErrInvalid, unit)
	}
}

// Truncate truncates local fields and resolves the resulting instant in the
// same timezone representation.
func (d DateTime) Truncate(unit string) (DateTime, error) {
	return d.TruncateWithDatabase(unit, GoZoneDatabase{})
}

// TruncateWithDatabase is the provider-injected variant of Truncate.
func (d DateTime) TruncateWithDatabase(unit string, database ZoneDatabase) (DateTime, error) {
	local, err := d.LocalDateTime().Truncate(unit)
	if err != nil {
		return DateTime{}, err
	}
	if d.zone.kind == NamedZone {
		return newNamedDateTime(local, d.zone.name, nil, database)
	}
	return newFixedDateTime(local, int(d.zone.offsetSeconds))
}
