package temporal

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

// LocalDateTime combines a proleptic Gregorian date with a local wall-clock
// time and carries no timezone information.
type LocalDateTime struct {
	date Date
	time LocalTime
}

// NewLocalDateTime constructs a local date-time from validated components.
func NewLocalDateTime(date Date, localTime LocalTime) LocalDateTime {
	return LocalDateTime{date: date, time: localTime}
}

// ParseLocalDateTime parses an M23 date, T, and local-time representation.
func ParseLocalDateTime(input string) (LocalDateTime, error) {
	separator := strings.LastIndexByte(input, 'T')
	if separator <= 0 || separator == len(input)-1 {
		return LocalDateTime{}, fmt.Errorf("%w: malformed local date-time %q", ErrInvalid, input)
	}
	date, err := ParseDate(input[:separator])
	if err != nil {
		return LocalDateTime{}, err
	}
	localTime, err := ParseLocalTime(input[separator+1:])
	if err != nil {
		return LocalDateTime{}, err
	}
	return NewLocalDateTime(date, localTime), nil
}

// Date returns the date component.
func (d LocalDateTime) Date() Date { return d.date }

// LocalTime returns the wall-clock component.
func (d LocalDateTime) LocalTime() LocalTime { return d.time }

// Year returns the astronomical local year.
func (d LocalDateTime) Year() int64 { return d.date.Year() }

// Quarter returns the local calendar quarter.
func (d LocalDateTime) Quarter() int { return d.date.Quarter() }

// Month returns the local month of year.
func (d LocalDateTime) Month() int { return d.date.Month() }

// Week returns the local ISO week number.
func (d LocalDateTime) Week() int { return d.date.Week() }

// WeekYear returns the local ISO week-year.
func (d LocalDateTime) WeekYear() int64 { return d.date.WeekYear() }

// Day returns the local day of month.
func (d LocalDateTime) Day() int { return d.date.Day() }

// OrdinalDay returns the local day of year.
func (d LocalDateTime) OrdinalDay() int { return d.date.OrdinalDay() }

// WeekDay returns the ISO weekday.
func (d LocalDateTime) WeekDay() int { return d.date.WeekDay() }

// DayOfQuarter returns the local day within the calendar quarter.
func (d LocalDateTime) DayOfQuarter() int { return d.date.DayOfQuarter() }

// Hour returns the local hour.
func (d LocalDateTime) Hour() int { return d.time.Hour() }

// Minute returns the local minute.
func (d LocalDateTime) Minute() int { return d.time.Minute() }

// Second returns the local second.
func (d LocalDateTime) Second() int { return d.time.Second() }

// Nanosecond returns the local nanosecond of second.
func (d LocalDateTime) Nanosecond() int { return d.time.Nanosecond() }

// Millisecond returns the local millisecond of second.
func (d LocalDateTime) Millisecond() int { return d.time.Millisecond() }

// Microsecond returns the local microsecond of second.
func (d LocalDateTime) Microsecond() int { return d.time.Microsecond() }

// Add performs M23 local date-time arithmetic: months, then days, then elapsed
// seconds. Calendar-month rollover clamps invalid target days.
func (d LocalDateTime) Add(duration Duration) (LocalDateTime, error) {
	date, err := d.date.AddMonths(duration.months)
	if err != nil {
		return LocalDateTime{}, err
	}
	date, err = date.AddDays(duration.days)
	if err != nil {
		return LocalDateTime{}, err
	}
	secondDays := floorDiv(duration.seconds, secondsPerDay)
	secondOfDay := floorMod(duration.seconds, secondsPerDay)
	nanoseconds := d.time.nanoOfDay + secondOfDay*nanosecondsPerSecond + int64(duration.nanoseconds)
	secondDays, err = checkedAdd(secondDays, floorDiv(nanoseconds, nanosecondsPerDay))
	if err != nil {
		return LocalDateTime{}, err
	}
	date, err = date.AddDays(secondDays)
	if err != nil {
		return LocalDateTime{}, err
	}
	localTime, err := localTimeFromNanoOfDay(floorMod(nanoseconds, nanosecondsPerDay))
	if err != nil {
		return LocalDateTime{}, err
	}
	return NewLocalDateTime(date, localTime), nil
}

// Subtract subtracts an M23 duration.
func (d LocalDateTime) Subtract(duration Duration) (LocalDateTime, error) {
	negated, err := duration.Negate()
	if err != nil {
		return LocalDateTime{}, err
	}
	return d.Add(negated)
}

// Compare returns -1, 0, or 1 in civil chronological order.
func (d LocalDateTime) Compare(other LocalDateTime) int {
	if comparison := d.date.Compare(other.date); comparison != 0 {
		return comparison
	}
	return d.time.Compare(other.time)
}

// Equal reports exact LocalDateTime equality.
func (d LocalDateTime) Equal(other LocalDateTime) bool { return d == other }

func (d LocalDateTime) String() string { return d.date.String() + "T" + d.time.String() }

// MarshalText emits the canonical parseable M23 representation.
func (d LocalDateTime) MarshalText() ([]byte, error) {
	if _, err := NewDate(d.date.Year(), d.date.Month(), d.date.Day()); err != nil {
		return nil, err
	}
	return []byte(d.String()), nil
}

// MarshalJSON emits the canonical temporal string.
func (d LocalDateTime) MarshalJSON() ([]byte, error) {
	text, err := d.MarshalText()
	if err != nil {
		return nil, err
	}
	return json.Marshal(string(text))
}

// Add adds the calendar groups of a duration to a Date; its seconds group is
// intentionally ignored by the M23 Date arithmetic rules.
func (d Date) Add(duration Duration) (Date, error) {
	result, err := d.AddMonths(duration.months)
	if err != nil {
		return Date{}, err
	}
	return result.AddDays(duration.days)
}

// Subtract subtracts only a duration's calendar groups from a Date.
func (d Date) Subtract(duration Duration) (Date, error) {
	months, err := checkedSub(0, duration.months)
	if err != nil {
		return Date{}, err
	}
	days, err := checkedSub(0, duration.days)
	if err != nil {
		return Date{}, err
	}
	return d.Add(Duration{months: months, days: days})
}

// ZoneKind distinguishes numeric UTC offsets from named IANA zones.
type ZoneKind uint8

const (
	// OffsetZone is an unnamed fixed UTC offset.
	OffsetZone ZoneKind = iota
	// NamedZone is an IANA timezone identifier plus its resolved offset.
	NamedZone
)

// Zone is the durable timezone identity of a DateTime. Named zones retain the
// resolved offset so old values remain stable even when timezone rules change.
type Zone struct {
	kind          ZoneKind
	offsetSeconds int32
	name          string
}

// FixedZone constructs an unnamed fixed-offset zone.
func FixedZone(offsetSeconds int) (Zone, error) {
	if err := validateOffset(offsetSeconds); err != nil {
		return Zone{}, err
	}
	return Zone{kind: OffsetZone, offsetSeconds: int32(offsetSeconds)}, nil
}

// ResolvedNamedZone constructs a durable named-zone snapshot. It validates
// shape and offset bounds but deliberately does not require the current tzdb to
// agree; persisted values must remain decodable after rule changes.
func ResolvedNamedZone(name string, offsetSeconds int) (Zone, error) {
	if name == "" || !utf8.ValidString(name) || strings.IndexByte(name, 0) >= 0 || strings.ContainsAny(name, "[]") || strings.HasPrefix(name, "+") || strings.HasPrefix(name, "-") {
		return Zone{}, fmt.Errorf("%w: invalid timezone name %q", ErrInvalid, name)
	}
	if err := validateOffset(offsetSeconds); err != nil {
		return Zone{}, err
	}
	return Zone{kind: NamedZone, offsetSeconds: int32(offsetSeconds), name: name}, nil
}

// Kind returns whether the zone is fixed or named.
func (z Zone) Kind() ZoneKind { return z.kind }

// Name returns the IANA identifier of a named zone and the empty string for a
// fixed offset.
func (z Zone) Name() string { return z.name }

// OffsetSeconds returns the resolved UTC offset.
func (z Zone) OffsetSeconds() int { return int(z.offsetSeconds) }

// String returns the M23 timezone accessor representation.
func (z Zone) String() string {
	if z.kind == NamedZone {
		return z.name
	}
	return FormatOffset(int(z.offsetSeconds))
}

// ZoneDatabase resolves named IANA zones without placing a mutable location
// pointer inside a durable value.
type ZoneDatabase interface {
	LoadLocation(name string) (*time.Location, error)
}

// GoZoneDatabase is retained for source compatibility. It resolves from the
// same deterministic archive as PinnedZoneDatabase and does not consult the
// host timezone database.
type GoZoneDatabase struct{}

// LoadLocation implements ZoneDatabase.
func (GoZoneDatabase) LoadLocation(name string) (*time.Location, error) {
	return (PinnedZoneDatabase{}).LoadLocation(name)
}

// DateTime is an absolute nanosecond-precision instant plus its exact timezone
// representation. Its zero value is a valid instant at the Unix epoch in UTC.
type DateTime struct {
	epochSecond int64
	nanosecond  int32
	zone        Zone
}

// NewDateTime constructs a DateTime from local fields and either Z/a numeric
// offset or an IANA zone name.
func NewDateTime(local LocalDateTime, timezone string) (DateTime, error) {
	return NewDateTimeWithDatabase(local, timezone, PinnedZoneDatabase{})
}

// NewDateTimeWithDatabase is NewDateTime with an explicit timezone provider.
func NewDateTimeWithDatabase(local LocalDateTime, timezone string, database ZoneDatabase) (DateTime, error) {
	if offset, err := ParseOffset(timezone); err == nil {
		return newFixedDateTime(local, offset)
	}
	return newNamedDateTime(local, timezone, nil, database)
}

// NewDateTimeWithNamedOffset constructs a named DateTime while validating an
// explicitly supplied offset. This disambiguates overlaps and rejects an
// offset that does not match the zone rules at the requested local instant.
func NewDateTimeWithNamedOffset(local LocalDateTime, name string, offsetSeconds int) (DateTime, error) {
	return NewDateTimeWithNamedOffsetAndDatabase(local, name, offsetSeconds, PinnedZoneDatabase{})
}

// NewDateTimeWithNamedOffsetAndDatabase is the provider-injected variant.
func NewDateTimeWithNamedOffsetAndDatabase(local LocalDateTime, name string, offsetSeconds int, database ZoneDatabase) (DateTime, error) {
	if err := validateOffset(offsetSeconds); err != nil {
		return DateTime{}, err
	}
	return newNamedDateTime(local, name, &offsetSeconds, database)
}

func newFixedDateTime(local LocalDateTime, offset int) (DateTime, error) {
	zone, err := FixedZone(offset)
	if err != nil {
		return DateTime{}, err
	}
	epochSecond, err := epochSecondFromLocal(local, offset)
	if err != nil {
		return DateTime{}, err
	}
	return restoreDateTime(epochSecond, int64(local.time.Nanosecond()), zone)
}

type namedCandidate struct {
	epochSecond int64
	offset      int
}

func newNamedDateTime(local LocalDateTime, name string, explicitOffset *int, database ZoneDatabase) (DateTime, error) {
	if database == nil {
		return DateTime{}, fmt.Errorf("%w: nil timezone database", ErrInvalid)
	}
	if _, err := ResolvedNamedZone(name, 0); err != nil {
		return DateTime{}, err
	}
	location, err := database.LoadLocation(name)
	if err != nil {
		return DateTime{}, err
	}
	if location == nil {
		return DateTime{}, fmt.Errorf("%w: timezone database returned nil for %q", ErrInvalid, name)
	}
	naive, err := epochSecondFromLocal(local, 0)
	if err != nil {
		return DateTime{}, err
	}
	offsets := make(map[int]struct{})
	for delta := -72 * time.Hour; delta <= 72*time.Hour; delta += 6 * time.Hour {
		_, offset := time.Unix(naive+int64(delta/time.Second), 0).In(location).Zone()
		offsets[offset] = struct{}{}
	}
	candidates := make([]namedCandidate, 0, 2)
	for offset := range offsets {
		epochSecond, subtractErr := checkedSub(naive, int64(offset))
		if subtractErr != nil {
			continue
		}
		if locationMatchesLocal(location, epochSecond, local) {
			candidates = append(candidates, namedCandidate{epochSecond: epochSecond, offset: offset})
		}
	}
	sort.Slice(candidates, func(left, right int) bool { return candidates[left].epochSecond < candidates[right].epochSecond })
	if explicitOffset != nil {
		for _, candidate := range candidates {
			if candidate.offset == *explicitOffset {
				zone, _ := ResolvedNamedZone(name, candidate.offset)
				return restoreDateTime(candidate.epochSecond, int64(local.time.Nanosecond()), zone)
			}
		}
		return DateTime{}, fmt.Errorf("%w: offset %s does not match timezone %q at %s", ErrInvalid, FormatOffset(*explicitOffset), name, local)
	}
	if len(candidates) != 0 {
		// During an overlap, M23 implementations follow java.time's earlier
		// offset choice; sorted epoch seconds make that choice explicit.
		candidate := candidates[0]
		zone, _ := ResolvedNamedZone(name, candidate.offset)
		return restoreDateTime(candidate.epochSecond, int64(local.time.Nanosecond()), zone)
	}
	// A nonexistent local time is in a forward gap. Move it forward by the
	// transition width, matching java.time ZonedDateTime construction.
	_, before := time.Unix(naive-int64(72*time.Hour/time.Second), 0).In(location).Zone()
	_, after := time.Unix(naive+int64(72*time.Hour/time.Second), 0).In(location).Zone()
	if after <= before {
		return DateTime{}, fmt.Errorf("%w: timezone %q cannot resolve local date-time %s", ErrInvalid, name, local)
	}
	shifted, err := local.Add(Duration{seconds: int64(after - before)})
	if err != nil {
		return DateTime{}, err
	}
	return newNamedDateTime(shifted, name, &after, database)
}

func locationMatchesLocal(location *time.Location, epochSecond int64, local LocalDateTime) bool {
	resolved := time.Unix(epochSecond, int64(local.time.Nanosecond())).In(location)
	year, month, day := resolved.Date()
	hour, minute, second := resolved.Clock()
	return int64(year) == local.date.Year() && int(month) == local.date.Month() && day == local.date.Day() &&
		hour == local.time.Hour() && minute == local.time.Minute() && second == local.time.Second() &&
		resolved.Nanosecond() == local.time.Nanosecond()
}

func epochSecondFromLocal(local LocalDateTime, offset int) (int64, error) {
	days, err := checkedMul(local.date.EpochDay(), secondsPerDay)
	if err != nil {
		return 0, fmt.Errorf("%w: date-time epoch", ErrOverflow)
	}
	secondsOfDay := local.time.nanoOfDay / nanosecondsPerSecond
	seconds, err := checkedAdd(days, secondsOfDay)
	if err != nil {
		return 0, fmt.Errorf("%w: date-time epoch", ErrOverflow)
	}
	seconds, err = checkedSub(seconds, int64(offset))
	if err != nil {
		return 0, fmt.Errorf("%w: date-time offset", ErrOverflow)
	}
	return seconds, nil
}

func localDateTimeAt(epochSecond, nanosecond int64, offset int) (LocalDateTime, error) {
	localSecond, err := checkedAdd(epochSecond, int64(offset))
	if err != nil {
		return LocalDateTime{}, fmt.Errorf("%w: resolve local date-time", ErrOverflow)
	}
	epochDay := floorDiv(localSecond, secondsPerDay)
	year, month, day := civilFromDays(epochDay)
	date, err := NewDate(year, month, day)
	if err != nil {
		return LocalDateTime{}, err
	}
	nanoOfDay := floorMod(localSecond, secondsPerDay)*nanosecondsPerSecond + nanosecond
	localTime, err := localTimeFromNanoOfDay(nanoOfDay)
	if err != nil {
		return LocalDateTime{}, err
	}
	return NewLocalDateTime(date, localTime), nil
}

func restoreDateTime(epochSecond, nanosecond int64, zone Zone) (DateTime, error) {
	carry := floorDiv(nanosecond, nanosecondsPerSecond)
	normalizedSecond, err := checkedAdd(epochSecond, carry)
	if err != nil {
		return DateTime{}, fmt.Errorf("%w: normalize date-time", ErrOverflow)
	}
	normalizedNano := floorMod(nanosecond, nanosecondsPerSecond)
	if zone.kind != OffsetZone && zone.kind != NamedZone {
		return DateTime{}, fmt.Errorf("%w: unknown timezone kind", ErrInvalid)
	}
	if zone.kind == OffsetZone && zone.name != "" || zone.kind == NamedZone && zone.name == "" {
		return DateTime{}, fmt.Errorf("%w: inconsistent timezone identity", ErrInvalid)
	}
	if err := validateOffset(int(zone.offsetSeconds)); err != nil {
		return DateTime{}, err
	}
	if _, err := localDateTimeAt(normalizedSecond, normalizedNano, int(zone.offsetSeconds)); err != nil {
		return DateTime{}, err
	}
	return DateTime{epochSecond: normalizedSecond, nanosecond: int32(normalizedNano), zone: zone}, nil
}

// DateTimeFromEpoch constructs an epoch-relative DateTime. Numeric epochs
// default to UTC when timezone is empty.
func DateTimeFromEpoch(epochSecond, nanosecond int64, timezone string) (DateTime, error) {
	return DateTimeFromEpochWithDatabase(epochSecond, nanosecond, timezone, PinnedZoneDatabase{})
}

// DateTimeFromEpochWithDatabase is the provider-injected variant.
func DateTimeFromEpochWithDatabase(epochSecond, nanosecond int64, timezone string, database ZoneDatabase) (DateTime, error) {
	if timezone == "" {
		timezone = "Z"
	}
	if offset, err := ParseOffset(timezone); err == nil {
		zone, _ := FixedZone(offset)
		return restoreDateTime(epochSecond, nanosecond, zone)
	}
	if database == nil {
		return DateTime{}, fmt.Errorf("%w: nil timezone database", ErrInvalid)
	}
	location, err := database.LoadLocation(timezone)
	if err != nil {
		return DateTime{}, err
	}
	if location == nil {
		return DateTime{}, fmt.Errorf("%w: timezone database returned nil for %q", ErrInvalid, timezone)
	}
	carry := floorDiv(nanosecond, nanosecondsPerSecond)
	normalizedSecond, err := checkedAdd(epochSecond, carry)
	if err != nil {
		return DateTime{}, err
	}
	_, offset := time.Unix(normalizedSecond, floorMod(nanosecond, nanosecondsPerSecond)).In(location).Zone()
	zone, err := ResolvedNamedZone(timezone, offset)
	if err != nil {
		return DateTime{}, err
	}
	return restoreDateTime(epochSecond, nanosecond, zone)
}

// DateTimeFromEpochMillis constructs a UTC DateTime from Unix milliseconds.
func DateTimeFromEpochMillis(milliseconds int64) (DateTime, error) {
	seconds := floorDiv(milliseconds, 1_000)
	nanoseconds := floorMod(milliseconds, 1_000) * 1_000_000
	return DateTimeFromEpoch(seconds, nanoseconds, "Z")
}

// ParseDateTime parses an M23 date-time with a required offset or named zone.
func ParseDateTime(input string) (DateTime, error) {
	name := ""
	base := input
	if strings.HasSuffix(input, "]") {
		open := strings.LastIndexByte(input, '[')
		if open < 0 || open == len(input)-2 {
			return DateTime{}, fmt.Errorf("%w: malformed named timezone", ErrInvalid)
		}
		name = input[open+1 : len(input)-1]
		base = input[:open]
	}
	separator := strings.LastIndexByte(base, 'T')
	if separator <= 0 || separator == len(base)-1 {
		return DateTime{}, fmt.Errorf("%w: malformed date-time %q", ErrInvalid, input)
	}
	date, err := ParseDate(base[:separator])
	if err != nil {
		return DateTime{}, err
	}
	timeAndOffset := base[separator+1:]
	localText, offsetText, offsetErr := splitOffsetSuffix(timeAndOffset)
	if offsetErr != nil {
		if name == "" {
			return DateTime{}, offsetErr
		}
		localText = timeAndOffset
	}
	localTime, err := ParseLocalTime(localText)
	if err != nil {
		return DateTime{}, err
	}
	local := NewLocalDateTime(date, localTime)
	if name == "" {
		offset, err := ParseOffset(offsetText)
		if err != nil {
			return DateTime{}, err
		}
		return newFixedDateTime(local, offset)
	}
	if offsetErr == nil {
		offset, err := ParseOffset(offsetText)
		if err != nil {
			return DateTime{}, err
		}
		return NewDateTimeWithNamedOffset(local, name, offset)
	}
	return NewDateTime(local, name)
}

// EpochSecond returns whole Unix seconds.
func (d DateTime) EpochSecond() int64 { return d.epochSecond }

// Nanosecond returns the nanosecond within the epoch second.
func (d DateTime) Nanosecond() int { return int(d.nanosecond) }

// EpochMillis returns Unix milliseconds using floor semantics for negative
// instants.
func (d DateTime) EpochMillis() (int64, error) {
	milliseconds, err := checkedMul(d.epochSecond, 1_000)
	if err != nil {
		return 0, fmt.Errorf("%w: epoch milliseconds", ErrOverflow)
	}
	return checkedAdd(milliseconds, int64(d.nanosecond)/1_000_000)
}

// Zone returns the durable timezone identity.
func (d DateTime) Zone() Zone { return d.zone }

// Offset returns the resolved canonical offset.
func (d DateTime) Offset() string { return FormatOffset(int(d.zone.offsetSeconds)) }

// OffsetSeconds returns the resolved offset in seconds.
func (d DateTime) OffsetSeconds() int { return int(d.zone.offsetSeconds) }

// OffsetMinutes returns the offset truncated toward zero to minutes.
func (d DateTime) OffsetMinutes() int { return int(d.zone.offsetSeconds) / 60 }

// Timezone returns the IANA ID for named zones or the offset for fixed zones.
func (d DateTime) Timezone() string { return d.zone.String() }

// LocalDateTime returns local fields using the durable resolved offset.
func (d DateTime) LocalDateTime() LocalDateTime {
	local, _ := localDateTimeAt(d.epochSecond, int64(d.nanosecond), int(d.zone.offsetSeconds))
	return local
}

// Date returns the local date component.
func (d DateTime) Date() Date { return d.LocalDateTime().Date() }

// LocalTime returns the local wall-clock component.
func (d DateTime) LocalTime() LocalTime { return d.LocalDateTime().LocalTime() }

// Year returns the astronomical local year.
func (d DateTime) Year() int64 { return d.Date().Year() }

// Quarter returns the local calendar quarter.
func (d DateTime) Quarter() int { return d.Date().Quarter() }

// Month returns the local month of year.
func (d DateTime) Month() int { return d.Date().Month() }

// Week returns the local ISO week number.
func (d DateTime) Week() int { return d.Date().Week() }

// WeekYear returns the local ISO week-year.
func (d DateTime) WeekYear() int64 { return d.Date().WeekYear() }

// Day returns the local day of month.
func (d DateTime) Day() int { return d.Date().Day() }

// OrdinalDay returns the local day of year.
func (d DateTime) OrdinalDay() int { return d.Date().OrdinalDay() }

// WeekDay returns the local ISO weekday.
func (d DateTime) WeekDay() int { return d.Date().WeekDay() }

// DayOfQuarter returns the local day within the calendar quarter.
func (d DateTime) DayOfQuarter() int { return d.Date().DayOfQuarter() }

// Hour returns the local hour.
func (d DateTime) Hour() int { return d.LocalTime().Hour() }

// Minute returns the local minute.
func (d DateTime) Minute() int { return d.LocalTime().Minute() }

// Second returns the local second.
func (d DateTime) Second() int { return d.LocalTime().Second() }

// Millisecond returns the local millisecond of second.
func (d DateTime) Millisecond() int { return d.LocalTime().Millisecond() }

// Microsecond returns the local microsecond of second.
func (d DateTime) Microsecond() int { return d.LocalTime().Microsecond() }

// Add performs calendar-month and calendar-day arithmetic in local time, then
// adds the elapsed-seconds group on the instant timeline. Named-zone results
// are resolved with the current ZoneDatabase rules.
func (d DateTime) Add(duration Duration) (DateTime, error) {
	return d.AddWithDatabase(duration, PinnedZoneDatabase{})
}

// AddWithDatabase is the provider-injected variant of Add.
func (d DateTime) AddWithDatabase(duration Duration, database ZoneDatabase) (DateTime, error) {
	calendarOnly := Duration{months: duration.months, days: duration.days}
	local, err := d.LocalDateTime().Add(calendarOnly)
	if err != nil {
		return DateTime{}, err
	}
	var calendarResult DateTime
	if d.zone.kind == NamedZone {
		calendarResult, err = newNamedDateTime(local, d.zone.name, nil, database)
	} else {
		calendarResult, err = newFixedDateTime(local, int(d.zone.offsetSeconds))
	}
	if err != nil {
		return DateTime{}, err
	}
	seconds, nanoseconds, err := addEpochDuration(calendarResult.epochSecond, int64(calendarResult.nanosecond), duration.seconds, int64(duration.nanoseconds))
	if err != nil {
		return DateTime{}, err
	}
	if d.zone.kind == NamedZone {
		return DateTimeFromEpochWithDatabase(seconds, nanoseconds, d.zone.name, database)
	}
	return restoreDateTime(seconds, nanoseconds, d.zone)
}

func addEpochDuration(epochSecond, nanosecond, seconds, additionalNanoseconds int64) (int64, int64, error) {
	resultSeconds, err := checkedAdd(epochSecond, seconds)
	if err != nil {
		return 0, 0, fmt.Errorf("%w: add date-time seconds", ErrOverflow)
	}
	nanosecond, err = checkedAdd(nanosecond, additionalNanoseconds)
	if err != nil {
		return 0, 0, fmt.Errorf("%w: add date-time nanoseconds", ErrOverflow)
	}
	carry := floorDiv(nanosecond, nanosecondsPerSecond)
	resultSeconds, err = checkedAdd(resultSeconds, carry)
	if err != nil {
		return 0, 0, fmt.Errorf("%w: normalize date-time seconds", ErrOverflow)
	}
	return resultSeconds, floorMod(nanosecond, nanosecondsPerSecond), nil
}

// Subtract subtracts an M23 duration.
func (d DateTime) Subtract(duration Duration) (DateTime, error) {
	negated, err := duration.Negate()
	if err != nil {
		return DateTime{}, err
	}
	return d.Add(negated)
}

// Compare implements M23 DateTime order: instant, offset west-to-east, then
// timezone identifier. A fixed offset sorts before a named zone on a full tie.
func (d DateTime) Compare(other DateTime) int {
	if comparison := compareInt64(d.epochSecond, other.epochSecond); comparison != 0 {
		return comparison
	}
	if comparison := compareInt64(int64(d.nanosecond), int64(other.nanosecond)); comparison != 0 {
		return comparison
	}
	if comparison := compareInt64(int64(d.zone.offsetSeconds), int64(other.zone.offsetSeconds)); comparison != 0 {
		return comparison
	}
	if d.zone.kind != other.zone.kind {
		if d.zone.kind < other.zone.kind {
			return -1
		}
		return 1
	}
	return strings.Compare(d.zone.name, other.zone.name)
}

// Equal reports equality of the instant and exact timezone representation.
func (d DateTime) Equal(other DateTime) bool { return d == other }

func (d DateTime) String() string {
	result := d.LocalDateTime().String() + d.Offset()
	if d.zone.kind == NamedZone {
		result += "[" + d.zone.name + "]"
	}
	return result
}

// MarshalText emits the canonical parseable M23 representation.
func (d DateTime) MarshalText() ([]byte, error) {
	if _, err := restoreDateTime(d.epochSecond, int64(d.nanosecond), d.zone); err != nil {
		return nil, err
	}
	return []byte(d.String()), nil
}

// MarshalJSON emits the canonical temporal string.
func (d DateTime) MarshalJSON() ([]byte, error) {
	text, err := d.MarshalText()
	if err != nil {
		return nil, err
	}
	return json.Marshal(string(text))
}
