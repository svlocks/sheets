package temporal

import (
	"encoding/binary"
	"fmt"
	"math"
	"unicode/utf8"
)

const binaryVersion byte = 1

// MarshalBinary returns the versioned canonical durable Date payload.
func (d Date) MarshalBinary() ([]byte, error) {
	if _, err := NewDate(d.Year(), d.Month(), d.Day()); err != nil {
		return nil, err
	}
	result := make([]byte, 7)
	result[0] = binaryVersion
	binary.BigEndian.PutUint32(result[1:5], uint32(d.year))
	result[5] = byte(d.Month())
	result[6] = byte(d.Day())
	return result, nil
}

// DateFromBinary decodes a canonical durable Date payload.
func DateFromBinary(data []byte) (Date, error) {
	if len(data) != 7 || data[0] != binaryVersion {
		return Date{}, fmt.Errorf("%w: invalid date binary payload", ErrInvalid)
	}
	return NewDate(int64(int32(binary.BigEndian.Uint32(data[1:5]))), int(data[5]), int(data[6]))
}

// MarshalBinary returns the versioned canonical durable LocalTime payload.
func (t LocalTime) MarshalBinary() ([]byte, error) {
	if _, err := localTimeFromNanoOfDay(t.nanoOfDay); err != nil {
		return nil, err
	}
	result := make([]byte, 9)
	result[0] = binaryVersion
	binary.BigEndian.PutUint64(result[1:], uint64(t.nanoOfDay))
	return result, nil
}

// LocalTimeFromBinary decodes a canonical durable LocalTime payload.
func LocalTimeFromBinary(data []byte) (LocalTime, error) {
	if len(data) != 9 || data[0] != binaryVersion {
		return LocalTime{}, fmt.Errorf("%w: invalid local-time binary payload", ErrInvalid)
	}
	value := binary.BigEndian.Uint64(data[1:])
	if value > math.MaxInt64 {
		return LocalTime{}, fmt.Errorf("%w: local-time binary payload overflows", ErrInvalid)
	}
	return localTimeFromNanoOfDay(int64(value))
}

// MarshalBinary returns the versioned canonical durable Time payload.
func (t Time) MarshalBinary() ([]byte, error) {
	if _, err := NewTime(t.local, int(t.offsetSeconds)); err != nil {
		return nil, err
	}
	result := make([]byte, 13)
	result[0] = binaryVersion
	binary.BigEndian.PutUint64(result[1:9], uint64(t.local.nanoOfDay))
	binary.BigEndian.PutUint32(result[9:13], uint32(t.offsetSeconds))
	return result, nil
}

// TimeFromBinary decodes a canonical durable offset-Time payload.
func TimeFromBinary(data []byte) (Time, error) {
	if len(data) != 13 || data[0] != binaryVersion {
		return Time{}, fmt.Errorf("%w: invalid offset-time binary payload", ErrInvalid)
	}
	nanoOfDay := binary.BigEndian.Uint64(data[1:9])
	if nanoOfDay > math.MaxInt64 {
		return Time{}, fmt.Errorf("%w: offset-time binary payload overflows", ErrInvalid)
	}
	local, err := localTimeFromNanoOfDay(int64(nanoOfDay))
	if err != nil {
		return Time{}, err
	}
	return NewTime(local, int(int32(binary.BigEndian.Uint32(data[9:13]))))
}

// MarshalBinary returns the versioned canonical durable LocalDateTime payload.
func (d LocalDateTime) MarshalBinary() ([]byte, error) {
	if _, err := NewDate(d.date.Year(), d.date.Month(), d.date.Day()); err != nil {
		return nil, err
	}
	if _, err := localTimeFromNanoOfDay(d.time.nanoOfDay); err != nil {
		return nil, err
	}
	result := make([]byte, 15)
	result[0] = binaryVersion
	binary.BigEndian.PutUint32(result[1:5], uint32(d.date.year))
	result[5] = byte(d.date.Month())
	result[6] = byte(d.date.Day())
	binary.BigEndian.PutUint64(result[7:15], uint64(d.time.nanoOfDay))
	return result, nil
}

// LocalDateTimeFromBinary decodes a canonical durable LocalDateTime payload.
func LocalDateTimeFromBinary(data []byte) (LocalDateTime, error) {
	if len(data) != 15 || data[0] != binaryVersion {
		return LocalDateTime{}, fmt.Errorf("%w: invalid local-date-time binary payload", ErrInvalid)
	}
	date, err := NewDate(int64(int32(binary.BigEndian.Uint32(data[1:5]))), int(data[5]), int(data[6]))
	if err != nil {
		return LocalDateTime{}, err
	}
	nanoOfDay := binary.BigEndian.Uint64(data[7:15])
	if nanoOfDay > math.MaxInt64 {
		return LocalDateTime{}, fmt.Errorf("%w: local-date-time binary payload overflows", ErrInvalid)
	}
	localTime, err := localTimeFromNanoOfDay(int64(nanoOfDay))
	if err != nil {
		return LocalDateTime{}, err
	}
	return NewLocalDateTime(date, localTime), nil
}

// MarshalBinary returns the versioned canonical durable DateTime payload. The
// payload stores the instant, zone kind, resolved offset, and exact zone ID.
func (d DateTime) MarshalBinary() ([]byte, error) {
	if _, err := restoreDateTime(d.epochSecond, int64(d.nanosecond), d.zone); err != nil {
		return nil, err
	}
	if len(d.zone.name) > math.MaxUint16 {
		return nil, fmt.Errorf("%w: timezone identifier is too long", ErrInvalid)
	}
	result := make([]byte, 20+len(d.zone.name))
	result[0] = binaryVersion
	binary.BigEndian.PutUint64(result[1:9], uint64(d.epochSecond))
	binary.BigEndian.PutUint32(result[9:13], uint32(d.nanosecond))
	result[13] = byte(d.zone.kind)
	binary.BigEndian.PutUint32(result[14:18], uint32(d.zone.offsetSeconds))
	binary.BigEndian.PutUint16(result[18:20], uint16(len(d.zone.name)))
	copy(result[20:], d.zone.name)
	return result, nil
}

// DateTimeFromBinary decodes a canonical durable DateTime payload without
// consulting the current timezone database.
func DateTimeFromBinary(data []byte) (DateTime, error) {
	if len(data) < 20 || data[0] != binaryVersion {
		return DateTime{}, fmt.Errorf("%w: invalid date-time binary payload", ErrInvalid)
	}
	nameLength := int(binary.BigEndian.Uint16(data[18:20]))
	if len(data) != 20+nameLength || !utf8.Valid(data[20:]) {
		return DateTime{}, fmt.Errorf("%w: malformed date-time timezone payload", ErrInvalid)
	}
	kind := ZoneKind(data[13])
	offset := int(int32(binary.BigEndian.Uint32(data[14:18])))
	name := string(data[20:])
	nanosecond := int64(binary.BigEndian.Uint32(data[9:13]))
	if nanosecond >= nanosecondsPerSecond {
		return DateTime{}, fmt.Errorf("%w: non-canonical date-time nanosecond", ErrInvalid)
	}
	var zone Zone
	var err error
	switch kind {
	case OffsetZone:
		if name != "" {
			return DateTime{}, fmt.Errorf("%w: fixed zone carries a name", ErrInvalid)
		}
		zone, err = FixedZone(offset)
	case NamedZone:
		zone, err = ResolvedNamedZone(name, offset)
	default:
		return DateTime{}, fmt.Errorf("%w: unknown timezone kind %d", ErrInvalid, kind)
	}
	if err != nil {
		return DateTime{}, err
	}
	return restoreDateTime(
		int64(binary.BigEndian.Uint64(data[1:9])),
		nanosecond,
		zone,
	)
}

// MarshalBinary returns the versioned canonical durable Duration payload.
func (d Duration) MarshalBinary() ([]byte, error) {
	if d.nanoseconds < 0 || int64(d.nanoseconds) >= nanosecondsPerSecond {
		return nil, fmt.Errorf("%w: non-canonical duration nanoseconds", ErrInvalid)
	}
	result := make([]byte, 29)
	result[0] = binaryVersion
	binary.BigEndian.PutUint64(result[1:9], uint64(d.months))
	binary.BigEndian.PutUint64(result[9:17], uint64(d.days))
	binary.BigEndian.PutUint64(result[17:25], uint64(d.seconds))
	binary.BigEndian.PutUint32(result[25:29], uint32(d.nanoseconds))
	return result, nil
}

// DurationFromBinary decodes a canonical durable Duration payload.
func DurationFromBinary(data []byte) (Duration, error) {
	if len(data) != 29 || data[0] != binaryVersion {
		return Duration{}, fmt.Errorf("%w: invalid duration binary payload", ErrInvalid)
	}
	nanoseconds := int64(binary.BigEndian.Uint32(data[25:29]))
	if nanoseconds >= nanosecondsPerSecond {
		return Duration{}, fmt.Errorf("%w: non-canonical duration binary payload", ErrInvalid)
	}
	return NewDuration(
		int64(binary.BigEndian.Uint64(data[1:9])),
		int64(binary.BigEndian.Uint64(data[9:17])),
		int64(binary.BigEndian.Uint64(data[17:25])),
		nanoseconds,
	)
}
