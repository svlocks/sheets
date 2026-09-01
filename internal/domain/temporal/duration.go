package temporal

import (
	"encoding/json"
	"fmt"
	"math"
	"math/big"
	"strconv"
	"strings"
)

// ComponentMap is the schema-free input accepted by temporal map
// constructors. Numeric components accept all built-in integer and floating
// types, json.Number, decimal strings, and *big.Rat. Floating-point values are
// interpreted through their shortest round-trippable decimal representation,
// avoiding binary floating-point accumulation in temporal arithmetic.
type ComponentMap map[string]any

// Duration stores the three independently signed openCypher duration groups:
// calendar months, calendar days, and elapsed seconds. Nanoseconds are
// canonicalized into [0, 1e9), with a floor carry in seconds. No normalization
// ever crosses a group boundary.
type Duration struct {
	months      int64
	days        int64
	seconds     int64
	nanoseconds int32
}

// NewDuration constructs and canonicalizes a three-group duration.
func NewDuration(months, days, seconds, nanoseconds int64) (Duration, error) {
	carry := floorDiv(nanoseconds, nanosecondsPerSecond)
	remainder := floorMod(nanoseconds, nanosecondsPerSecond)
	normalizedSeconds, err := checkedAdd(seconds, carry)
	if err != nil {
		return Duration{}, fmt.Errorf("%w: normalize duration seconds", ErrOverflow)
	}
	return Duration{
		months:      months,
		days:        days,
		seconds:     normalizedSeconds,
		nanoseconds: int32(remainder),
	}, nil
}

// DurationFromComponents constructs a duration from M23 component names. A
// fractional remainder cascades to smaller groups; crossing months to days
// uses the specified average Gregorian month (146097/4800 days).
func DurationFromComponents(components ComponentMap) (Duration, error) {
	allowed := map[string]struct{}{
		"years": {}, "quarters": {}, "months": {}, "weeks": {}, "days": {},
		"hours": {}, "minutes": {}, "seconds": {}, "milliseconds": {},
		"microseconds": {}, "nanoseconds": {},
	}
	for key := range components {
		if _, ok := allowed[key]; !ok {
			return Duration{}, fmt.Errorf("%w: unknown duration component %q", ErrInvalid, key)
		}
	}
	months := new(big.Rat)
	days := new(big.Rat)
	seconds := new(big.Rat)
	for _, term := range []struct {
		name   string
		factor int64
		target *big.Rat
	}{
		{"years", 12, months}, {"quarters", 3, months}, {"months", 1, months},
		{"weeks", 7, days}, {"days", 1, days},
		{"hours", 3600, seconds}, {"minutes", 60, seconds}, {"seconds", 1, seconds},
	} {
		if value, ok := components[term.name]; ok {
			number, err := numberRat(value)
			if err != nil {
				return Duration{}, fmt.Errorf("%w: duration %s: %v", ErrInvalid, term.name, err)
			}
			term.target.Add(term.target, new(big.Rat).Mul(number, big.NewRat(term.factor, 1)))
		}
	}
	for _, term := range []struct {
		name        string
		denominator int64
	}{
		{"milliseconds", 1_000}, {"microseconds", 1_000_000}, {"nanoseconds", 1_000_000_000},
	} {
		if value, ok := components[term.name]; ok {
			number, err := numberRat(value)
			if err != nil {
				return Duration{}, fmt.Errorf("%w: duration %s: %v", ErrInvalid, term.name, err)
			}
			seconds.Add(seconds, new(big.Rat).Quo(number, big.NewRat(term.denominator, 1)))
		}
	}
	return durationFromRationals(months, days, seconds)
}

func durationFromRationals(months, days, seconds *big.Rat) (Duration, error) {
	wholeMonths, fractionalMonths := splitRat(months)
	if !wholeMonths.IsInt64() {
		return Duration{}, fmt.Errorf("%w: month component", ErrOverflow)
	}
	// One average Gregorian month is 146097/4800 days. This conversion is
	// performed only for a fractional month; integral groups remain distinct.
	allDays := new(big.Rat).Set(days)
	allDays.Add(allDays, fractionalMonths.Mul(fractionalMonths, big.NewRat(146097, 4800)))
	wholeDays, fractionalDays := splitRat(allDays)
	if !wholeDays.IsInt64() {
		return Duration{}, fmt.Errorf("%w: day component", ErrOverflow)
	}
	allSeconds := new(big.Rat).Set(seconds)
	allSeconds.Add(allSeconds, fractionalDays.Mul(fractionalDays, big.NewRat(secondsPerDay, 1)))
	totalNanoseconds := new(big.Rat).Mul(allSeconds, big.NewRat(nanosecondsPerSecond, 1))
	// Nanoseconds are the durable precision. M23 expressions that produce a
	// smaller fraction are truncated toward zero, matching integer component
	// accessors and avoiding host floating-point rounding.
	nanoseconds := new(big.Int).Quo(totalNanoseconds.Num(), totalNanoseconds.Denom())
	return durationFromTotalNanoseconds(wholeMonths.Int64(), wholeDays.Int64(), nanoseconds)
}

func splitRat(value *big.Rat) (*big.Int, *big.Rat) {
	whole := new(big.Int).Quo(value.Num(), value.Denom())
	fraction := new(big.Rat).Sub(new(big.Rat).Set(value), new(big.Rat).SetInt(whole))
	return whole, fraction
}

func durationFromTotalNanoseconds(months, days int64, total *big.Int) (Duration, error) {
	divisor := big.NewInt(nanosecondsPerSecond)
	seconds := new(big.Int)
	remainder := new(big.Int)
	seconds.QuoRem(total, divisor, remainder)
	if remainder.Sign() < 0 {
		seconds.Sub(seconds, big.NewInt(1))
		remainder.Add(remainder, divisor)
	}
	if !seconds.IsInt64() {
		return Duration{}, fmt.Errorf("%w: seconds component", ErrOverflow)
	}
	return NewDuration(months, days, seconds.Int64(), remainder.Int64())
}

// ParseDuration parses ISO unit-based durations and the M23 date-and-time
// duration form (for example P2012-02-02T14:37:21.545).
func ParseDuration(input string) (Duration, error) {
	if len(input) < 2 || input[0] != 'P' {
		return Duration{}, fmt.Errorf("%w: malformed duration %q", ErrInvalid, input)
	}
	if duration, ok, err := parseDateTimeDuration(input); ok || err != nil {
		return duration, err
	}
	components := make(ComponentMap)
	inTime := false
	fractionSeen := false
	componentSeen := false
	for index := 1; index < len(input); {
		if input[index] == 'T' {
			if inTime {
				return Duration{}, fmt.Errorf("%w: duplicate duration time separator", ErrInvalid)
			}
			inTime = true
			index++
			continue
		}
		start := index
		if input[index] == '+' || input[index] == '-' {
			index++
		}
		digits := 0
		for index < len(input) && input[index] >= '0' && input[index] <= '9' {
			index++
			digits++
		}
		hasFraction := false
		if index < len(input) && (input[index] == '.' || input[index] == ',') {
			hasFraction = true
			index++
			fractionDigits := 0
			for index < len(input) && input[index] >= '0' && input[index] <= '9' {
				index++
				fractionDigits++
			}
			if fractionDigits == 0 {
				return Duration{}, fmt.Errorf("%w: malformed duration fraction", ErrInvalid)
			}
		}
		if digits == 0 || index >= len(input) {
			return Duration{}, fmt.Errorf("%w: malformed duration component", ErrInvalid)
		}
		designator := input[index]
		index++
		name := ""
		switch designator {
		case 'Y':
			name = "years"
		case 'Q':
			name = "quarters"
		case 'M':
			if inTime {
				name = "minutes"
			} else {
				name = "months"
			}
		case 'W':
			name = "weeks"
		case 'D':
			name = "days"
		case 'H':
			if inTime {
				name = "hours"
			}
		case 'S':
			if inTime {
				name = "seconds"
			}
		}
		if name == "" {
			return Duration{}, fmt.Errorf("%w: invalid duration designator %q", ErrInvalid, designator)
		}
		if _, exists := components[name]; exists {
			return Duration{}, fmt.Errorf("%w: duplicate duration component %s", ErrInvalid, name)
		}
		text := strings.ReplaceAll(input[start:index-1], ",", ".")
		components[name] = text
		componentSeen = true
		if fractionSeen || hasFraction && index != len(input) {
			return Duration{}, fmt.Errorf("%w: only the final duration component may be fractional", ErrInvalid)
		}
		fractionSeen = hasFraction
	}
	if !componentSeen {
		return Duration{}, fmt.Errorf("%w: duration contains no components", ErrInvalid)
	}
	return DurationFromComponents(components)
}

func parseDateTimeDuration(input string) (Duration, bool, error) {
	separator := strings.IndexByte(input, 'T')
	if separator < 0 {
		return Duration{}, false, nil
	}
	dateText := input[1:separator]
	parts := strings.Split(dateText, "-")
	if len(parts) != 3 || parts[0] == "" || len(parts[1]) != 2 || len(parts[2]) != 2 {
		return Duration{}, false, nil
	}
	timeText := input[separator+1:]
	timeParts := strings.Split(timeText, ":")
	if len(timeParts) != 3 {
		return Duration{}, true, fmt.Errorf("%w: malformed date-time duration", ErrInvalid)
	}
	components := ComponentMap{
		"years": parts[0], "months": parts[1], "days": parts[2],
		"hours": timeParts[0], "minutes": timeParts[1],
		"seconds": strings.ReplaceAll(timeParts[2], ",", "."),
	}
	duration, err := DurationFromComponents(components)
	return duration, true, err
}

func numberRat(value any) (*big.Rat, error) {
	var text string
	switch number := value.(type) {
	case *big.Rat:
		if number == nil {
			return nil, fmt.Errorf("nil rational")
		}
		return new(big.Rat).Set(number), nil
	case big.Rat:
		return new(big.Rat).Set(&number), nil
	case json.Number:
		text = string(number)
	case string:
		text = number
	case int:
		return big.NewRat(int64(number), 1), nil
	case int8:
		return big.NewRat(int64(number), 1), nil
	case int16:
		return big.NewRat(int64(number), 1), nil
	case int32:
		return big.NewRat(int64(number), 1), nil
	case int64:
		return big.NewRat(number, 1), nil
	case uint:
		if uint64(number) > math.MaxInt64 {
			return nil, fmt.Errorf("unsigned integer exceeds int64")
		}
		return big.NewRat(int64(number), 1), nil
	case uint8:
		return big.NewRat(int64(number), 1), nil
	case uint16:
		return big.NewRat(int64(number), 1), nil
	case uint32:
		return big.NewRat(int64(number), 1), nil
	case uint64:
		if number > math.MaxInt64 {
			return nil, fmt.Errorf("unsigned integer exceeds int64")
		}
		return big.NewRat(int64(number), 1), nil
	case float32:
		if math.IsNaN(float64(number)) || math.IsInf(float64(number), 0) {
			return nil, fmt.Errorf("non-finite number")
		}
		text = strconv.FormatFloat(float64(number), 'g', -1, 32)
	case float64:
		if math.IsNaN(number) || math.IsInf(number, 0) {
			return nil, fmt.Errorf("non-finite number")
		}
		text = strconv.FormatFloat(number, 'g', -1, 64)
	default:
		return nil, fmt.Errorf("unsupported numeric type %T", value)
	}
	if len(text) > 1_024 {
		return nil, fmt.Errorf("numeric text exceeds 1024 bytes")
	}
	if exponentAt := strings.LastIndexAny(text, "eE"); exponentAt >= 0 {
		exponentText := text[exponentAt+1:]
		if len(exponentText) == 0 || len(exponentText) > 6 {
			return nil, fmt.Errorf("numeric exponent is out of range")
		}
		exponent, err := strconv.ParseInt(exponentText, 10, 32)
		if err != nil || exponent < -10_000 || exponent > 10_000 {
			return nil, fmt.Errorf("numeric exponent is out of range")
		}
	}
	rational, ok := new(big.Rat).SetString(text)
	if !ok {
		return nil, fmt.Errorf("parse number %q", text)
	}
	return rational, nil
}

// Months returns the integral calendar-month group.
func (d Duration) Months() int64 { return d.months }

// Days returns the integral calendar-day group.
func (d Duration) Days() int64 { return d.days }

// Seconds returns the total whole seconds in the elapsed-seconds group,
// truncated toward zero as required by the Cypher accessor.
func (d Duration) Seconds() int64 {
	if d.seconds < 0 && d.nanoseconds != 0 {
		return d.seconds + 1
	}
	return d.seconds
}

// CanonicalSeconds returns the floor-normalized seconds field used by the
// durable binary representation. Most callers want Seconds instead.
func (d Duration) CanonicalSeconds() int64 { return d.seconds }

// NanosecondsPart returns the canonical non-negative nanosecond adjustment.
func (d Duration) NanosecondsPart() int { return int(d.nanoseconds) }

// Equal reports equality of the three canonical component groups.
func (d Duration) Equal(other Duration) bool { return d == other }

// Add performs checked component-wise addition.
func (d Duration) Add(other Duration) (Duration, error) {
	months, err := checkedAdd(d.months, other.months)
	if err != nil {
		return Duration{}, fmt.Errorf("%w: add duration months", ErrOverflow)
	}
	days, err := checkedAdd(d.days, other.days)
	if err != nil {
		return Duration{}, fmt.Errorf("%w: add duration days", ErrOverflow)
	}
	seconds, err := checkedAdd(d.seconds, other.seconds)
	if err != nil {
		return Duration{}, fmt.Errorf("%w: add duration seconds", ErrOverflow)
	}
	return NewDuration(months, days, seconds, int64(d.nanoseconds)+int64(other.nanoseconds))
}

// Subtract performs checked component-wise subtraction.
func (d Duration) Subtract(other Duration) (Duration, error) {
	negated, err := other.Negate()
	if err != nil {
		return Duration{}, err
	}
	return d.Add(negated)
}

// Negate returns the exact additive inverse.
func (d Duration) Negate() (Duration, error) {
	if d.months == math.MinInt64 || d.days == math.MinInt64 {
		return Duration{}, fmt.Errorf("%w: negate duration", ErrOverflow)
	}
	total := d.secondsGroupNanoseconds()
	total.Neg(total)
	return durationFromTotalNanoseconds(-d.months, -d.days, total)
}

// Multiply scales a duration using exact rational arithmetic and cascades
// fractional components toward smaller groups.
func (d Duration) Multiply(factor any) (Duration, error) {
	ratio, err := numberRat(factor)
	if err != nil {
		return Duration{}, fmt.Errorf("%w: duration multiplier: %v", ErrInvalid, err)
	}
	return d.scale(ratio)
}

// Divide divides a duration using exact rational arithmetic.
func (d Duration) Divide(divisor any) (Duration, error) {
	ratio, err := numberRat(divisor)
	if err != nil {
		return Duration{}, fmt.Errorf("%w: duration divisor: %v", ErrInvalid, err)
	}
	if ratio.Sign() == 0 {
		return Duration{}, fmt.Errorf("%w: division by zero", ErrInvalid)
	}
	return d.scale(new(big.Rat).Inv(ratio))
}

func (d Duration) scale(factor *big.Rat) (Duration, error) {
	months := new(big.Rat).Mul(big.NewRat(d.months, 1), factor)
	days := new(big.Rat).Mul(big.NewRat(d.days, 1), factor)
	seconds := new(big.Rat).SetFrac(d.secondsGroupNanoseconds(), big.NewInt(nanosecondsPerSecond))
	seconds.Mul(seconds, factor)
	return durationFromRationals(months, days, seconds)
}

// CompareForOrder implements the special total ordering required by ORDER BY,
// using an average month of 2,629,746 seconds and a 24-hour day. It must not be
// used for relational duration operators, whose M23 result is null.
func (d Duration) CompareForOrder(other Duration) int {
	return d.orderNanoseconds().Cmp(other.orderNanoseconds())
}

func (d Duration) orderNanoseconds() *big.Int {
	result := new(big.Int).Mul(big.NewInt(d.months), big.NewInt(2_629_746*nanosecondsPerSecond))
	result.Add(result, new(big.Int).Mul(big.NewInt(d.days), big.NewInt(secondsPerDay*nanosecondsPerSecond)))
	result.Add(result, d.secondsGroupNanoseconds())
	return result
}

func (d Duration) secondsGroupNanoseconds() *big.Int {
	result := new(big.Int).Mul(big.NewInt(d.seconds), big.NewInt(nanosecondsPerSecond))
	return result.Add(result, big.NewInt(int64(d.nanoseconds)))
}

// Component returns an M23 duration accessor. Total values truncate toward
// zero and report overflow when they cannot be represented by a Cypher int.
func (d Duration) Component(name string) (int64, error) {
	switch name {
	case "years":
		return d.months / 12, nil
	case "quarters":
		return d.months / 3, nil
	case "months":
		return d.months, nil
	case "quartersOfYear":
		return d.months % 12 / 3, nil
	case "monthsOfQuarter":
		return d.months % 3, nil
	case "monthsOfYear":
		return d.months % 12, nil
	case "weeks":
		return d.days / 7, nil
	case "days":
		return d.days, nil
	case "daysOfWeek":
		return d.days % 7, nil
	}
	total := d.secondsGroupNanoseconds()
	unit := int64(0)
	switch name {
	case "hours":
		unit = 3600 * nanosecondsPerSecond
	case "minutes":
		unit = 60 * nanosecondsPerSecond
	case "seconds":
		unit = nanosecondsPerSecond
	case "milliseconds":
		unit = 1_000_000
	case "microseconds":
		unit = 1_000
	case "nanoseconds":
		unit = 1
	case "minutesOfHour":
		return quotientRemainder(total, 3600*nanosecondsPerSecond, 60*nanosecondsPerSecond)
	case "secondsOfMinute":
		return quotientRemainder(total, 60*nanosecondsPerSecond, nanosecondsPerSecond)
	case "millisecondsOfSecond":
		return quotientRemainder(total, nanosecondsPerSecond, 1_000_000)
	case "microsecondsOfSecond":
		return quotientRemainder(total, nanosecondsPerSecond, 1_000)
	case "nanosecondsOfSecond":
		return quotientRemainder(total, nanosecondsPerSecond, 1)
	default:
		return 0, fmt.Errorf("%w: unknown duration component %q", ErrInvalid, name)
	}
	value := new(big.Int).Quo(total, big.NewInt(unit))
	if !value.IsInt64() {
		return 0, fmt.Errorf("%w: duration component %s", ErrOverflow, name)
	}
	return value.Int64(), nil
}

func quotientRemainder(total *big.Int, boundary, unit int64) (int64, error) {
	remainder := new(big.Int).Rem(total, big.NewInt(boundary))
	value := remainder.Quo(remainder, big.NewInt(unit))
	if !value.IsInt64() {
		return 0, ErrOverflow
	}
	return value.Int64(), nil
}

func (d Duration) String() string {
	var result strings.Builder
	result.WriteByte('P')
	years := d.months / 12
	months := d.months % 12
	if years != 0 {
		fmt.Fprintf(&result, "%dY", years)
	}
	if months != 0 {
		fmt.Fprintf(&result, "%dM", months)
	}
	if d.days != 0 {
		fmt.Fprintf(&result, "%dD", d.days)
	}
	negative, wholeSeconds, fractionalNanoseconds := durationSecondMagnitude(d.seconds, d.nanoseconds)
	if wholeSeconds != 0 || fractionalNanoseconds != 0 {
		result.WriteByte('T')
		hours := wholeSeconds / 3600
		minutes := wholeSeconds / 60 % 60
		seconds := wholeSeconds % 60
		prefix := ""
		if negative {
			prefix = "-"
		}
		if hours != 0 {
			fmt.Fprintf(&result, "%s%dH", prefix, hours)
		}
		if minutes != 0 {
			fmt.Fprintf(&result, "%s%dM", prefix, minutes)
		}
		if seconds != 0 || fractionalNanoseconds != 0 {
			result.WriteString(prefix)
			result.WriteString(strconv.FormatUint(seconds, 10))
			if fractionalNanoseconds != 0 {
				fraction := strings.TrimRight(fmt.Sprintf("%09d", fractionalNanoseconds), "0")
				result.WriteByte('.')
				result.WriteString(fraction)
			}
			result.WriteByte('S')
		}
	}
	if result.Len() == 1 {
		return "PT0S"
	}
	return result.String()
}

func durationSecondMagnitude(seconds int64, nanoseconds int32) (bool, uint64, uint32) {
	if seconds >= 0 {
		return false, uint64(seconds), uint32(nanoseconds)
	}
	if nanoseconds == 0 {
		return true, uint64(-(seconds + 1)) + 1, 0
	}
	return true, uint64(-(seconds + 1)), uint32(nanosecondsPerSecond - int64(nanoseconds))
}

// MarshalText emits the canonical parseable M23 representation.
func (d Duration) MarshalText() ([]byte, error) { return []byte(d.String()), nil }

// MarshalJSON emits the canonical temporal string.
func (d Duration) MarshalJSON() ([]byte, error) { return json.Marshal(d.String()) }
