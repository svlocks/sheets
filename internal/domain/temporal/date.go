package temporal

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// Date is an astronomical-year date in the proleptic Gregorian calendar. Its
// zero value is the valid date 0000-01-01.
type Date struct {
	year  int32
	month uint8
	day   uint8
}

// NewDate validates and constructs a proleptic Gregorian date.
func NewDate(year int64, month, day int) (Date, error) {
	if year < MinYear || year > MaxYear {
		return Date{}, fmt.Errorf("%w: year %d outside [%d,%d]", ErrInvalid, year, MinYear, MaxYear)
	}
	if month < 1 || month > 12 {
		return Date{}, fmt.Errorf("%w: month %d outside [1,12]", ErrInvalid, month)
	}
	maximum := daysInMonth(year, month)
	if day < 1 || day > maximum {
		return Date{}, fmt.Errorf("%w: day %d outside [1,%d] for %d-%02d", ErrInvalid, day, maximum, year, month)
	}
	// Store one-based fields with a one-unit bias so Date's Go zero value is a
	// useful, valid value rather than an invalid sentinel.
	return Date{year: int32(year), month: uint8(month - 1), day: uint8(day - 1)}, nil
}

// DateFromOrdinal constructs a date from an astronomical year and one-based
// ordinal day.
func DateFromOrdinal(year int64, ordinalDay int) (Date, error) {
	maximum := 365
	if isLeapYear(year) {
		maximum++
	}
	if ordinalDay < 1 || ordinalDay > maximum {
		return Date{}, fmt.Errorf("%w: ordinal day %d outside [1,%d]", ErrInvalid, ordinalDay, maximum)
	}
	start, err := NewDate(year, 1, 1)
	if err != nil {
		return Date{}, err
	}
	return start.AddDays(int64(ordinalDay - 1))
}

// DateFromWeek constructs an ISO week date. Week one is the week containing
// January 4 and Monday is day one.
func DateFromWeek(weekYear int64, week, dayOfWeek int) (Date, error) {
	if dayOfWeek < 1 || dayOfWeek > 7 {
		return Date{}, fmt.Errorf("%w: day of week %d outside [1,7]", ErrInvalid, dayOfWeek)
	}
	if week < 1 || week > weeksInISOYear(weekYear) {
		return Date{}, fmt.Errorf("%w: week %d is invalid for ISO week-year %d", ErrInvalid, week, weekYear)
	}
	jan4, err := NewDate(weekYear, 1, 4)
	if err != nil {
		return Date{}, err
	}
	monday, err := jan4.AddDays(int64(1 - jan4.WeekDay()))
	if err != nil {
		return Date{}, err
	}
	return monday.AddDays(int64((week-1)*7 + dayOfWeek - 1))
}

// DateFromQuarter constructs a date from a calendar quarter and a one-based
// day within that quarter.
func DateFromQuarter(year int64, quarter, dayOfQuarter int) (Date, error) {
	if quarter < 1 || quarter > 4 {
		return Date{}, fmt.Errorf("%w: quarter %d outside [1,4]", ErrInvalid, quarter)
	}
	startMonth := (quarter-1)*3 + 1
	start, err := NewDate(year, startMonth, 1)
	if err != nil {
		return Date{}, err
	}
	maximum := daysInMonth(year, startMonth) + daysInMonth(year, startMonth+1) + daysInMonth(year, startMonth+2)
	if dayOfQuarter < 1 || dayOfQuarter > maximum {
		return Date{}, fmt.Errorf("%w: day of quarter %d outside [1,%d]", ErrInvalid, dayOfQuarter, maximum)
	}
	return start.AddDays(int64(dayOfQuarter - 1))
}

// ParseDate parses the calendar, ordinal, or ISO week forms accepted by the
// M23 temporal constructors. Missing lower-order components default to one.
func ParseDate(input string) (Date, error) {
	if input == "" {
		return Date{}, fmt.Errorf("%w: empty date", ErrInvalid)
	}
	year, rest, err := splitDateYear(input)
	if err != nil {
		return Date{}, err
	}
	if rest == "" {
		return NewDate(year, 1, 1)
	}
	extended := strings.HasPrefix(rest, "-")
	body := rest
	if extended {
		body = rest[1:]
		if body == "" || body[0] == '-' {
			return Date{}, fmt.Errorf("%w: malformed extended date %q", ErrInvalid, input)
		}
	}
	if strings.HasPrefix(body, "W") {
		weekText := body[1:]
		if extended {
			if len(weekText) == 4 && weekText[2] == '-' {
				weekText = weekText[:2] + weekText[3:]
			} else if len(weekText) != 2 {
				return Date{}, fmt.Errorf("%w: malformed ISO week date %q", ErrInvalid, input)
			}
		} else if len(weekText) != 2 && len(weekText) != 3 || strings.Contains(weekText, "-") {
			return Date{}, fmt.Errorf("%w: malformed ISO week date %q", ErrInvalid, input)
		}
		week, err := parseUnsigned(weekText[:2], "week")
		if err != nil {
			return Date{}, err
		}
		day := 1
		if len(weekText) == 3 {
			day, err = parseUnsigned(weekText[2:], "day of week")
			if err != nil {
				return Date{}, err
			}
		}
		return DateFromWeek(year, week, day)
	}
	if strings.HasPrefix(body, "Q") {
		quarterText := body[1:]
		if extended {
			if len(quarterText) >= 3 && quarterText[1] == '-' {
				quarterText = quarterText[:1] + quarterText[2:]
			} else if len(quarterText) != 1 {
				return Date{}, fmt.Errorf("%w: malformed quarter date %q", ErrInvalid, input)
			}
		}
		if len(quarterText) < 1 || len(quarterText) > 3 || strings.Contains(quarterText, "-") {
			return Date{}, fmt.Errorf("%w: malformed quarter date %q", ErrInvalid, input)
		}
		quarter, err := parseUnsigned(quarterText[:1], "quarter")
		if err != nil {
			return Date{}, err
		}
		day := 1
		if len(quarterText) > 1 {
			day, err = parseUnsigned(quarterText[1:], "day of quarter")
			if err != nil {
				return Date{}, err
			}
		}
		return DateFromQuarter(year, quarter, day)
	}
	digits := body
	if extended {
		switch {
		case len(body) == 2, len(body) == 3:
		case len(body) == 5 && body[2] == '-':
			digits = body[:2] + body[3:]
		default:
			return Date{}, fmt.Errorf("%w: malformed extended date %q", ErrInvalid, input)
		}
	} else if strings.Contains(body, "-") {
		return Date{}, fmt.Errorf("%w: malformed basic date %q", ErrInvalid, input)
	}
	switch len(digits) {
	case 2:
		month, err := parseUnsigned(digits, "month")
		if err != nil {
			return Date{}, err
		}
		return NewDate(year, month, 1)
	case 3:
		ordinal, err := parseUnsigned(digits, "ordinal day")
		if err != nil {
			return Date{}, err
		}
		return DateFromOrdinal(year, ordinal)
	case 4:
		month, err := parseUnsigned(digits[:2], "month")
		if err != nil {
			return Date{}, err
		}
		day, err := parseUnsigned(digits[2:], "day")
		if err != nil {
			return Date{}, err
		}
		return NewDate(year, month, day)
	default:
		return Date{}, fmt.Errorf("%w: malformed date %q", ErrInvalid, input)
	}
}

func splitDateYear(input string) (int64, string, error) {
	start := 0
	if input[0] == '+' || input[0] == '-' {
		start = 1
	}
	end := start
	for end < len(input) && input[end] >= '0' && input[end] <= '9' {
		end++
	}
	if end-start < 4 {
		return 0, "", fmt.Errorf("%w: date year must have at least four digits", ErrInvalid)
	}
	// Unsigned basic forms have exactly four year digits. Extended years must
	// carry the sign required by ISO 8601 and the M23 proposal.
	if start == 0 && end > 4 {
		end = 4
	}
	year, err := strconv.ParseInt(input[:end], 10, 64)
	if err != nil {
		return 0, "", fmt.Errorf("%w: parse year: %v", ErrInvalid, err)
	}
	if year < MinYear || year > MaxYear {
		return 0, "", fmt.Errorf("%w: year outside durable range", ErrInvalid)
	}
	return year, input[end:], nil
}

func parseUnsigned(input, component string) (int, error) {
	if input == "" {
		return 0, fmt.Errorf("%w: missing %s", ErrInvalid, component)
	}
	for _, character := range input {
		if character < '0' || character > '9' {
			return 0, fmt.Errorf("%w: malformed %s %q", ErrInvalid, component, input)
		}
	}
	value, err := strconv.Atoi(input)
	if err != nil {
		return 0, fmt.Errorf("%w: parse %s: %v", ErrInvalid, component, err)
	}
	return value, nil
}

// Year returns the astronomical year number.
func (d Date) Year() int64 { return int64(d.year) }

// Month returns the month of year in [1,12].
func (d Date) Month() int { return int(d.month) + 1 }

// Day returns the day of month.
func (d Date) Day() int { return int(d.day) + 1 }

// Quarter returns the quarter of year in [1,4].
func (d Date) Quarter() int { return int(d.month)/3 + 1 }

// OrdinalDay returns the one-based day of year.
func (d Date) OrdinalDay() int {
	return int(d.EpochDay()-daysFromCivil(int64(d.year), 1, 1)) + 1
}

// WeekDay returns the ISO weekday, where Monday is one and Sunday is seven.
func (d Date) WeekDay() int { return int(floorMod(d.EpochDay()+3, 7)) + 1 }

// ISOWeek returns the ISO week-year and week number.
func (d Date) ISOWeek() (int64, int) {
	thursday, _ := d.AddDays(int64(4 - d.WeekDay()))
	weekYear := thursday.Year()
	jan4, _ := NewDate(weekYear, 1, 4)
	weekOne, _ := jan4.AddDays(int64(1 - jan4.WeekDay()))
	return weekYear, int((d.EpochDay()-weekOne.EpochDay())/7) + 1
}

// Week returns the ISO week number.
func (d Date) Week() int { _, week := d.ISOWeek(); return week }

// WeekYear returns the ISO week-year.
func (d Date) WeekYear() int64 { year, _ := d.ISOWeek(); return year }

// DayOfQuarter returns the one-based day within the calendar quarter.
func (d Date) DayOfQuarter() int {
	start, _ := NewDate(d.Year(), (d.Quarter()-1)*3+1, 1)
	return int(d.EpochDay()-start.EpochDay()) + 1
}

// EpochDay returns the number of civil days since 1970-01-01.
func (d Date) EpochDay() int64 { return daysFromCivil(int64(d.year), d.Month(), d.Day()) }

// AddDays performs checked calendar-day arithmetic.
func (d Date) AddDays(days int64) (Date, error) {
	epochDay, err := checkedAdd(d.EpochDay(), days)
	if err != nil {
		return Date{}, err
	}
	year, month, day := civilFromDays(epochDay)
	return NewDate(year, month, day)
}

// AddMonths performs checked calendar-month arithmetic and clamps a day that
// does not exist in the destination month to that month's final day.
func (d Date) AddMonths(months int64) (Date, error) {
	base, err := checkedMul(int64(d.year), 12)
	if err != nil {
		return Date{}, err
	}
	base, err = checkedAdd(base, int64(d.Month()-1))
	if err != nil {
		return Date{}, err
	}
	target, err := checkedAdd(base, months)
	if err != nil {
		return Date{}, err
	}
	year := floorDiv(target, 12)
	month := int(floorMod(target, 12)) + 1
	day := d.Day()
	if maximum := daysInMonth(year, month); day > maximum {
		day = maximum
	}
	return NewDate(year, month, day)
}

// Compare returns -1, 0, or 1 in civil chronological order.
func (d Date) Compare(other Date) int {
	return compareInt64(d.EpochDay(), other.EpochDay())
}

// Equal reports exact Date equality.
func (d Date) Equal(other Date) bool { return d == other }

func (d Date) String() string {
	return formatYear(int64(d.year)) + fmt.Sprintf("-%02d-%02d", d.Month(), d.Day())
}

// MarshalText emits the canonical parseable M23 representation.
func (d Date) MarshalText() ([]byte, error) {
	if _, err := NewDate(d.Year(), d.Month(), d.Day()); err != nil {
		return nil, err
	}
	return []byte(d.String()), nil
}

// MarshalJSON emits the canonical temporal string. Application protocols that
// need type preservation should wrap this string with an explicit type tag.
func (d Date) MarshalJSON() ([]byte, error) {
	text, err := d.MarshalText()
	if err != nil {
		return nil, err
	}
	return json.Marshal(string(text))
}

func formatYear(year int64) string {
	if year >= 0 && year <= 9999 {
		return fmt.Sprintf("%04d", year)
	}
	if year < 0 {
		magnitude := -year
		return fmt.Sprintf("-%04d", magnitude)
	}
	return "+" + strconv.FormatInt(year, 10)
}

func isLeapYear(year int64) bool {
	return floorMod(year, 4) == 0 && (floorMod(year, 100) != 0 || floorMod(year, 400) == 0)
}

func daysInMonth(year int64, month int) int {
	switch month {
	case 2:
		if isLeapYear(year) {
			return 29
		}
		return 28
	case 4, 6, 9, 11:
		return 30
	default:
		return 31
	}
}

func weeksInISOYear(year int64) int {
	jan1, err := NewDate(year, 1, 1)
	if err != nil {
		return 0
	}
	weekday := jan1.WeekDay()
	if weekday == 4 || weekday == 3 && isLeapYear(year) {
		return 53
	}
	return 52
}

// daysFromCivil and civilFromDays are the integer proleptic-Gregorian
// algorithms described by Howard Hinnant, shifted to the Unix epoch. Their
// intermediates remain comfortably inside int64 for the documented year range.
func daysFromCivil(year int64, month, day int) int64 {
	yearForEra := year
	if month <= 2 {
		yearForEra--
	}
	era := floorDiv(yearForEra, 400)
	yearOfEra := yearForEra - era*400
	adjustedMonth := int64(month)
	if month > 2 {
		adjustedMonth -= 3
	} else {
		adjustedMonth += 9
	}
	dayOfYear := (153*adjustedMonth+2)/5 + int64(day) - 1
	dayOfEra := yearOfEra*365 + yearOfEra/4 - yearOfEra/100 + dayOfYear
	return era*146097 + dayOfEra - 719468
}

func civilFromDays(epochDay int64) (int64, int, int) {
	value := epochDay + 719468
	era := floorDiv(value, 146097)
	dayOfEra := value - era*146097
	yearOfEra := (dayOfEra - dayOfEra/1460 + dayOfEra/36524 - dayOfEra/146096) / 365
	year := yearOfEra + era*400
	dayOfYear := dayOfEra - (365*yearOfEra + yearOfEra/4 - yearOfEra/100)
	monthPrime := (5*dayOfYear + 2) / 153
	day := int(dayOfYear-(153*monthPrime+2)/5) + 1
	month := int(monthPrime)
	if month < 10 {
		month += 3
	} else {
		month -= 9
	}
	if month <= 2 {
		year++
	}
	return year, month, day
}
