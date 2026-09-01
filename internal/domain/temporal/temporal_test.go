package temporal

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"reflect"
	"testing"
)

func TestDateParsingComponentsAndArithmetic(t *testing.T) {
	if (Date{}).String() != "0000-01-01" || (LocalDateTime{}).String() != "0000-01-01T00:00" {
		t.Fatalf("zero temporal values are not canonical: %s / %s", Date{}, LocalDateTime{})
	}
	tests := map[string]string{
		"2015-07-21":  "2015-07-21",
		"20150721":    "2015-07-21",
		"2015-07":     "2015-07-01",
		"201507":      "2015-07-01",
		"2015-W30-2":  "2015-07-21",
		"2015W302":    "2015-07-21",
		"2015-W30":    "2015-07-20",
		"2015-202":    "2015-07-21",
		"2015":        "2015-01-01",
		"+10000-01":   "+10000-01-01",
		"-0001-12-31": "-0001-12-31",
	}
	for input, expected := range tests {
		value, err := ParseDate(input)
		if err != nil || value.String() != expected {
			t.Errorf("ParseDate(%q) = %q, %v; want %q", input, value, err, expected)
		}
	}
	for _, input := range []string{"2015--07", "2015-0-7", "2015-W3-02", "2015---202", "10000-01-01"} {
		if _, err := ParseDate(input); !errors.Is(err, ErrInvalid) {
			t.Errorf("ParseDate(%q) error = %v", input, err)
		}
	}
	weekDate, err := DateFromComponents(ComponentMap{"year": int64(1818), "week": int64(53)})
	if err != nil || weekDate.String() != "1818-12-28" {
		t.Fatalf("week date = %s, %v", weekDate, err)
	}
	quarterDate, err := DateFromComponents(ComponentMap{"year": int64(1984), "quarter": 3, "dayOfQuarter": 45})
	if err != nil || quarterDate.String() != "1984-08-14" || quarterDate.DayOfQuarter() != 45 {
		t.Fatalf("quarter date = %s, %v", quarterDate, err)
	}
	base, _ := ParseDate("1816-12-30")
	selected, err := DateFromComponents(ComponentMap{"date": base, "week": 2, "dayOfWeek": 3})
	if err != nil || selected.String() != "1817-01-08" {
		t.Fatalf("selected week date = %s, %v", selected, err)
	}
	january, _ := ParseDate("2011-01-31")
	oneMonth, _ := NewDuration(1, 0, 0, 0)
	got, err := january.Add(oneMonth)
	if err != nil || got.String() != "2011-02-28" {
		t.Fatalf("month clamp = %s, %v", got, err)
	}
	maximum, _ := NewDate(MaxYear, 12, 31)
	if _, err := maximum.AddDays(1); !errors.Is(err, ErrInvalid) {
		t.Fatalf("date bound error = %v", err)
	}
	maximumQuarter, err := DateFromQuarter(MaxYear, 4, 92)
	if err != nil || maximumQuarter.String() != "+999999999-12-31" {
		t.Fatalf("maximum-year quarter = %s, %v", maximumQuarter, err)
	}
	for _, year := range []int64{MinYear, -400, -1, 0, 1, 400, 1970, 2000, MaxYear} {
		for month := 1; month <= 12; month++ {
			for _, day := range []int{1, daysInMonth(year, month)} {
				value, err := NewDate(year, month, day)
				if err != nil {
					t.Fatal(err)
				}
				roundYear, roundMonth, roundDay := civilFromDays(value.EpochDay())
				if roundYear != year || roundMonth != month || roundDay != day {
					t.Fatalf("civil round trip %s -> %d-%02d-%02d", value, roundYear, roundMonth, roundDay)
				}
			}
		}
	}
}

func TestTimeParsingComponentsComparisonAndWrap(t *testing.T) {
	localTests := map[string]string{
		"21:40:32.142": "21:40:32.142",
		"214032.142":   "21:40:32.142",
		"21:40":        "21:40",
		"2140":         "21:40",
		"21":           "21:00",
	}
	for input, expected := range localTests {
		value, err := ParseLocalTime(input)
		if err != nil || value.String() != expected {
			t.Errorf("ParseLocalTime(%q) = %q, %v; want %q", input, value, err, expected)
		}
	}
	timeTests := map[string]string{
		"21:40:32.142+0100": "21:40:32.142+01:00",
		"214032.142Z":       "21:40:32.142Z",
		"2140-02":           "21:40-02:00",
		"12:34:56+02:05:59": "12:34:56+02:05:59",
	}
	for input, expected := range timeTests {
		value, err := ParseTime(input)
		if err != nil || value.String() != expected {
			t.Errorf("ParseTime(%q) = %q, %v; want %q", input, value, err, expected)
		}
	}
	for _, input := range []string{"12:00+0:100", "12:00+01::00", "12:00+1801", "12:00+19"} {
		if _, err := ParseTime(input); !errors.Is(err, ErrInvalid) {
			t.Errorf("ParseTime(%q) error = %v", input, err)
		}
	}
	composed, err := LocalTimeFromComponents(ComponentMap{
		"hour": 12, "minute": 31, "second": 14,
		"millisecond": 123, "microsecond": 456, "nanosecond": 789,
	})
	if err != nil || composed.String() != "12:31:14.123456789" {
		t.Fatalf("composed local time = %s, %v", composed, err)
	}
	left, _ := ParseTime("10:00+01:00")
	right, _ := ParseTime("09:35:14.645876123Z")
	if left.Compare(right) >= 0 {
		t.Fatalf("offset time comparison = %d, want left < right", left.Compare(right))
	}
	late, _ := ParseLocalTime("23:30")
	twoHours, _ := NewDuration(0, 9, 7200, 0)
	wrapped, err := late.Add(twoHours)
	if err != nil || wrapped.String() != "01:30" {
		t.Fatalf("wrapped local time = %s, %v", wrapped, err)
	}
}

func TestDurationConstructionParsingArithmeticAndAccessors(t *testing.T) {
	tests := []struct {
		components ComponentMap
		expected   string
	}{
		{ComponentMap{"days": 14, "hours": 16, "minutes": 12}, "P14DT16H12M"},
		{ComponentMap{"months": 5, "days": 1.5}, "P5M1DT12H"},
		{ComponentMap{"months": 0.75}, "P22DT19H51M49.5S"},
		{ComponentMap{"weeks": 2.5}, "P17DT12H"},
		{ComponentMap{"years": 12, "months": 5, "days": 14, "hours": 16, "minutes": 12, "seconds": 70}, "P12Y5M14DT16H13M10S"},
		{ComponentMap{"minutes": 1.5, "seconds": 1}, "PT1M31S"},
	}
	for _, test := range tests {
		value, err := DurationFromComponents(test.components)
		if err != nil || value.String() != test.expected {
			t.Errorf("DurationFromComponents(%v) = %s, %v; want %s", test.components, value, err, test.expected)
		}
		parsed, err := ParseDuration(test.expected)
		if err != nil || !parsed.Equal(value) {
			t.Errorf("ParseDuration(%q) = %s, %v; want %#v", test.expected, parsed, err, value)
		}
	}
	dateForm, err := ParseDuration("P2012-02-02T14:37:21.545")
	if err != nil || dateForm.String() != "P2012Y2M2DT14H37M21.545S" {
		t.Fatalf("date-form duration = %s, %v", dateForm, err)
	}
	first, _ := DurationFromComponents(ComponentMap{"years": 12, "months": 5, "days": 14, "hours": 16, "minutes": 12, "seconds": 70, "nanoseconds": 1})
	half, err := first.Divide(2)
	if err != nil || half.String() != "P6Y2M22DT13H21M8S" {
		t.Fatalf("duration / 2 = %s, %v", half, err)
	}
	doubled, err := half.Multiply(2)
	if err != nil || doubled.String() != "P12Y4M44DT26H42M16S" {
		// Scaling is intentionally non-associative across the three duration
		// groups once a fractional month has cascaded into days.
		t.Fatalf("cascaded duration * 2 = %s, %v", doubled, err)
	}
	equivalent, _ := DurationFromComponents(ComponentMap{"years": 12, "months": 5, "days": 14, "hours": 16, "minutes": 13, "seconds": 10, "nanoseconds": 1})
	if !first.Equal(equivalent) {
		t.Fatal("equivalent within-group duration components were not canonicalized")
	}
	if got, err := first.Component("nanosecondsOfSecond"); err != nil || got != 1 {
		t.Fatalf("nanosecondsOfSecond = %d, %v", got, err)
	}
	negative, _ := NewDuration(0, 0, -3, -500_000_002)
	if negative.String() != "PT-3.500000002S" {
		t.Fatalf("negative duration = %s", negative)
	}
	if negative.CanonicalSeconds() != -4 || negative.Seconds() != -3 {
		t.Fatalf("negative duration seconds canonical=%d accessor=%d", negative.CanonicalSeconds(), negative.Seconds())
	}
	if _, err := NewDuration(0, 0, math.MaxInt64, nanosecondsPerSecond); !errors.Is(err, ErrOverflow) {
		t.Fatalf("duration overflow error = %v", err)
	}
}

func TestDateTimeNamedZonesDSTAndNegativeEpoch(t *testing.T) {
	tests := map[string]string{
		"2015-07-21T21:40:32.142+0100":                    "2015-07-21T21:40:32.142+01:00",
		"2015-W30-2T214032.142Z":                          "2015-07-21T21:40:32.142Z",
		"2015-07-21T21:40:32.142[Europe/London]":          "2015-07-21T21:40:32.142+01:00[Europe/London]",
		"1818-07-21T21:40:32.142[Europe/Stockholm]":       "1818-07-21T21:40:32.142+00:53:28[Europe/Stockholm]",
		"2015-07-21T21:40:32.142+02:00[Europe/Stockholm]": "2015-07-21T21:40:32.142+02:00[Europe/Stockholm]",
	}
	for input, expected := range tests {
		value, err := ParseDateTime(input)
		if err != nil || value.String() != expected {
			t.Errorf("ParseDateTime(%q) = %q, %v; want %q", input, value, err, expected)
		}
	}
	if _, err := ParseDateTime("2015-07-21T21:40:32+01:00[Europe/Stockholm]"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("mismatched named-zone offset error = %v", err)
	}
	overlapEarly, err := ParseDateTime("2017-10-29T02:30[Europe/Stockholm]")
	if err != nil || overlapEarly.Offset() != "+02:00" {
		t.Fatalf("overlap default = %s, %v", overlapEarly, err)
	}
	overlapLate, err := ParseDateTime("2017-10-29T02:30+01:00[Europe/Stockholm]")
	if err != nil || overlapLate.EpochSecond()-overlapEarly.EpochSecond() != 3600 {
		t.Fatalf("overlap explicit = %s, %v", overlapLate, err)
	}
	gap, err := ParseDateTime("2017-03-26T02:30[Europe/Stockholm]")
	if err != nil || gap.String() != "2017-03-26T03:30+02:00[Europe/Stockholm]" {
		t.Fatalf("gap resolution = %s, %v", gap, err)
	}
	negative, err := DateTimeFromEpochMillis(-1)
	if err != nil || negative.String() != "1969-12-31T23:59:59.999Z" {
		t.Fatalf("negative epoch = %s, %v", negative, err)
	}
	if millis, err := negative.EpochMillis(); err != nil || millis != -1 {
		t.Fatalf("negative EpochMillis = %d, %v", millis, err)
	}
	before, _ := ParseDateTime("2017-10-28T23:00+02:00[Europe/Stockholm]")
	oneDay, _ := NewDuration(0, 1, 0, 0)
	after, err := before.Add(oneDay)
	if err != nil || after.String() != "2017-10-29T23:00+01:00[Europe/Stockholm]" || after.EpochSecond()-before.EpochSecond() != 25*3600 {
		t.Fatalf("DST calendar day = %s delta=%d, %v", after, after.EpochSecond()-before.EpochSecond(), err)
	}
	truncated, err := after.Truncate("week")
	if err != nil || truncated.String() != "2017-10-23T00:00+02:00[Europe/Stockholm]" {
		t.Fatalf("zoned week truncation = %s, %v", truncated, err)
	}
}

func TestCanonicalBinaryAndJSON(t *testing.T) {
	date, _ := ParseDate("1984-10-11")
	localTime, _ := ParseLocalTime("12:31:14.645876123")
	offsetTime, _ := ParseTime("12:31:14.645876123+01:00")
	localDateTime := NewLocalDateTime(date, localTime)
	dateTime, _ := NewDateTime(localDateTime, "Europe/Stockholm")
	duration, _ := NewDuration(-7, 14, -4, 500_000_000)
	tests := []struct {
		name    string
		value   any
		marshal func() ([]byte, error)
		decode  func([]byte) (any, error)
	}{
		{"date", date, date.MarshalBinary, func(data []byte) (any, error) { return DateFromBinary(data) }},
		{"local time", localTime, localTime.MarshalBinary, func(data []byte) (any, error) { return LocalTimeFromBinary(data) }},
		{"offset time", offsetTime, offsetTime.MarshalBinary, func(data []byte) (any, error) { return TimeFromBinary(data) }},
		{"local date-time", localDateTime, localDateTime.MarshalBinary, func(data []byte) (any, error) { return LocalDateTimeFromBinary(data) }},
		{"date-time", dateTime, dateTime.MarshalBinary, func(data []byte) (any, error) { return DateTimeFromBinary(data) }},
		{"duration", duration, duration.MarshalBinary, func(data []byte) (any, error) { return DurationFromBinary(data) }},
	}
	for _, test := range tests {
		data, err := test.marshal()
		if err != nil {
			t.Fatalf("marshal %s: %v", test.name, err)
		}
		decoded, err := test.decode(append([]byte(nil), data...))
		if err != nil || !reflect.DeepEqual(decoded, test.value) {
			t.Errorf("decode %s (%s) = %#v, %v; want %#v", test.name, hex.EncodeToString(data), decoded, err, test.value)
		}
		data[0]++
		if _, err := test.decode(data); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s accepted unknown binary version: %v", test.name, err)
		}
		encodedJSON, err := json.Marshal(test.value)
		if err != nil || !bytes.HasPrefix(encodedJSON, []byte{'"'}) {
			t.Errorf("JSON %s = %s, %v", test.name, encodedJSON, err)
		}
	}
	dateBytes, _ := date.MarshalBinary()
	if got := hex.EncodeToString(dateBytes); got != "01000007c00a0b" {
		t.Fatalf("date binary golden = %s", got)
	}
	localBytes, _ := localTime.MarshalBinary()
	if got := hex.EncodeToString(localBytes); got != "01000028fec2417d9b" {
		t.Fatalf("local-time binary golden = %s", got)
	}
}

func TestBinaryRejectsNonCanonicalAndComponentErrors(t *testing.T) {
	if _, err := DateFromBinary([]byte{2}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("date binary error = %v", err)
	}
	badDuration := make([]byte, 29)
	badDuration[0] = binaryVersion
	badDuration[25] = 0x3b
	badDuration[26] = 0x9a
	badDuration[27] = 0xca
	badDuration[28] = 0x00 // 1,000,000,000
	if _, err := DurationFromBinary(badDuration); !errors.Is(err, ErrInvalid) {
		t.Fatalf("noncanonical duration error = %v", err)
	}
	if _, err := DateFromComponents(ComponentMap{"year": 2024, "week": 1, "month": 1}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("mixed date representation error = %v", err)
	}
	if _, err := DurationFromComponents(ComponentMap{"days": math.NaN()}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("NaN duration error = %v", err)
	}
	if _, err := ParseLocalTime("12:34:56.1234567890"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("excess precision error = %v", err)
	}
}

func FuzzTemporalParsersDoNotPanic(f *testing.F) {
	for _, seed := range []string{"", "2015-07-21", "2015-W30-2", "21:40:32.142", "P0.75M", "2017-10-29T02:30[Europe/Stockholm]", string([]byte{0xff})} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, input string) {
		_, _ = ParseDate(input)
		_, _ = ParseLocalTime(input)
		_, _ = ParseTime(input)
		_, _ = ParseLocalDateTime(input)
		_, _ = ParseDateTime(input)
		_, _ = ParseDuration(input)
	})
}

func FuzzTemporalBinaryDecodersDoNotPanic(f *testing.F) {
	for _, seed := range [][]byte{{}, {1}, bytes.Repeat([]byte{0xff}, 64)} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, input []byte) {
		_, _ = DateFromBinary(input)
		_, _ = LocalTimeFromBinary(input)
		_, _ = TimeFromBinary(input)
		_, _ = LocalDateTimeFromBinary(input)
		_, _ = DateTimeFromBinary(input)
		_, _ = DurationFromBinary(input)
	})
}
