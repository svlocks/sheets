package engine

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/svlocks/sheets/internal/app"
	"github.com/svlocks/sheets/internal/domain"
	"github.com/svlocks/sheets/internal/domain/temporal"
)

func TestEngineTemporalDSTEpochCalendarAndNullSemantics(t *testing.T) {
	executor, _ := testEngine(t)
	result := execute(t, executor, `
RETURN datetime('2024-03-10T02:30[America/New_York]') AS gap,
       datetime('2024-11-03T01:30[America/New_York]') AS early,
       datetime('2024-11-03T01:30-05:00[America/New_York]') AS late,
       duration.inSeconds(datetime('2024-11-03T01:30[America/New_York]'),
                          datetime('2024-11-03T01:30-05:00[America/New_York]')) AS overlap,
       datetime.fromepoch(-1, 500000000) AS negativeEpoch,
       datetime.fromepoch(-1, 500000000).epochMillis AS negativeEpochMillis,
       date('2024-01-31') + duration({months:1}) AS calendarMonth,
       date('2024-01-31') + duration({days:30}) AS calendarDays,
       duration({months:1}) = duration({days:30}) AS unlikeGroups,
       duration.inSeconds(localtime(), localtime()) AS stableClock,
       duration.between(null, null) AS nullBetween`, nil)
	row := result.Results[0].Rows[0]
	want := []any{
		"2024-03-10T03:30-04:00[America/New_York]",
		"2024-11-03T01:30-04:00[America/New_York]",
		"2024-11-03T01:30-05:00[America/New_York]",
		"PT1H",
		"1969-12-31T23:59:59.5Z",
		int64(-500),
		"2024-02-29",
		"2024-03-01",
		false,
		"PT0S",
		nil,
	}
	for index, expected := range want {
		actual := row[index]
		switch value := actual.(type) {
		case temporal.Date:
			actual = value.String()
		case temporal.DateTime:
			actual = value.String()
		case temporal.Duration:
			actual = value.String()
		}
		if actual != expected {
			t.Errorf("column %d = %#v, want %#v", index, actual, expected)
		}
	}
}

func TestEngineTemporalStatementClockIsStableAcrossPipelines(t *testing.T) {
	executor, _ := testEngine(t)
	result := execute(t, executor, `
CALL { RETURN datetime() AS inner }
UNWIND range(1, 100) AS ignored
WITH inner, collect(datetime()) AS observed
CREATE (n:Clock {first:inner, second:datetime()})
RETURN inner = head(observed) AS subquery,
       size([value IN observed WHERE value = inner]) AS aggregateRows,
       n.first = n.second AS mutation,
       timestamp() = datetime().epochMillis AS timestamp`, nil)
	want := []any{true, int64(100), true, true}
	if got := result.Results[0].Rows; len(got) != 1 {
		t.Fatalf("statement clock rows = %#v", got)
	} else {
		for index := range want {
			if got[0][index] != want[index] {
				t.Errorf("statement clock column %d = %#v, want %#v", index, got[0][index], want[index])
			}
		}
	}
}

func TestEngineTemporalNegativeFractionalDurationAccessorsUseCanonicalRemainder(t *testing.T) {
	executor, _ := testEngine(t)
	result := execute(t, executor, `
RETURN duration('PT-0.5S').seconds AS seconds,
       duration('PT-0.5S').milliseconds AS milliseconds,
       duration('PT-0.5S').microseconds AS microseconds,
       duration('PT-0.5S').nanoseconds AS nanoseconds,
       duration('PT-0.5S').millisecondsOfSecond AS millisecondsOfSecond,
       duration('PT-0.5S').microsecondsOfSecond AS microsecondsOfSecond,
       duration('PT-0.5S').nanosecondsOfSecond AS nanosecondsOfSecond`, nil)
	want := []any{int64(-1), int64(-500), int64(-500_000), int64(-500_000_000), int64(500), int64(500_000), int64(500_000_000)}
	if got := result.Results[0].Rows; len(got) != 1 {
		t.Fatalf("rows = %#v", got)
	} else {
		for index := range want {
			if got[0][index] != want[index] {
				t.Errorf("column %d = %#v, want %#v", index, got[0][index], want[index])
			}
		}
	}
}

func TestEngineDateArithmeticPreservesOfficialFractionalDurationCarry(t *testing.T) {
	executor, _ := testEngine(t)
	result := execute(t, executor, `
WITH date({year: 1984, month: 10, day: 11}) AS x,
     duration({years: 12.5, months: 5.5, days: 14.5, hours: 16.5,
               minutes: 12.5, seconds: 70.5, nanoseconds: 3}) AS d
RETURN x + d AS sum, x - d AS diff`, nil)
	got := result.Results[0].Rows
	if len(got) != 1 {
		t.Fatalf("rows = %#v", got)
	}
	sum, sumOK := got[0][0].(temporal.Date)
	diff, diffOK := got[0][1].(temporal.Date)
	if !sumOK || sum.String() != "1997-10-11" || !diffOK || diff.String() != "1971-10-12" {
		t.Fatalf("date fractional-duration arithmetic = %#v", got[0])
	}
}

func TestEngineTemporalLegacyBridgesPreserveExactEqualityAndGrouping(t *testing.T) {
	executor, _ := testEngine(t)
	instant := time.Date(2024, 1, 1, 0, 0, 0, 123, time.UTC)
	sameInstantDifferentOffset := time.Date(2023, 12, 31, 19, 0, 0, 123, time.FixedZone("legacy-fixed", -5*60*60))
	params := map[string]any{
		"utc":      instant,
		"offset":   sameInstantDifferentOffset,
		"duration": time.Hour,
		"times":    []time.Time{instant},
	}

	equality := execute(t, executor, `
RETURN $utc = datetime('2024-01-01T00:00:00.000000123Z') AS utcBridge,
       $offset = datetime('2024-01-01T00:00:00.000000123Z') AS offsetVsUTC,
       $utc = $offset AS legacyExactZones,
       datetime('2024-01-01T00:00:00.000000123Z') =
         datetime('2023-12-31T19:00:00.000000123-05:00') AS exactZones,
       $duration = duration('PT1H') AS durationBridge`, params)
	wantEquality := []any{true, false, false, false, true}
	if got := equality.Results[0].Rows; len(got) != 1 {
		t.Fatalf("equality rows = %#v", got)
	} else {
		for index := range wantEquality {
			if got[0][index] != wantEquality[index] {
				t.Errorf("equality column %d = %#v, want %#v", index, got[0][index], wantEquality[index])
			}
		}
	}

	grouped := execute(t, executor, `
UNWIND [$utc, datetime('2024-01-01T00:00:00.000000123Z')] AS value
RETURN value, count(*) AS occurrences`, params)
	if got := grouped.Results[0].Rows; len(got) != 1 || got[0][1] != int64(2) {
		t.Fatalf("legacy/exact datetime groups = %#v", got)
	}

	distinct := execute(t, executor, `
UNWIND [$duration, duration('PT1H')] AS value
RETURN count(DISTINCT value) AS distinctDurations`, params)
	if got := distinct.Results[0].Rows; len(got) != 1 || got[0][0] != int64(1) {
		t.Fatalf("legacy/exact duration distinct = %#v", got)
	}

	nested := execute(t, executor, `
UNWIND [$times, [datetime('2024-01-01T00:00:00.000000123Z')]] AS value
RETURN count(DISTINCT value) AS distinctLists`, params)
	if got := nested.Results[0].Rows; len(got) != 1 || got[0][0] != int64(1) {
		t.Fatalf("nested legacy/exact datetime distinct = %#v", got)
	}
}

func TestEngineTemporalLegacyPropertiesRoundTripWithExactSemantics(t *testing.T) {
	executor, _ := testEngine(t)
	legacyTime := time.Date(2022, 7, 8, 9, 10, 11, 12, time.FixedZone("legacy", -5*60*60))
	legacyDuration := -37*time.Minute + 9*time.Nanosecond
	created := execute(t, executor, `
CREATE (:Legacy {key:'value', at:$at, elapsed:$elapsed})`, map[string]any{
		"at": legacyTime, "elapsed": legacyDuration,
	})
	if created.Revision == nil {
		t.Fatal("legacy temporal create produced no revision")
	}

	result := execute(t, executor, `
MATCH (n:Legacy {at:datetime('2022-07-08T09:10:11.000000012-05:00'),
                 elapsed:duration('PT-36M-59.999999991S')})
RETURN n.at.year AS year, n.at.timezone AS timezone, toString(n.at) AS at,
       n.elapsed.minutes AS minutes, n.elapsed.seconds AS seconds,
       toString(n.elapsed) AS elapsed,
       toString(n.at + n.elapsed) AS shifted`, nil)
	want := []any{
		int64(2022), "-05:00", "2022-07-08T09:10:11.000000012-05:00",
		int64(-36), int64(-2220), "PT-36M-59.999999991S",
		"2022-07-08T08:33:11.000000021-05:00",
	}
	if got := result.Results[0].Rows; len(got) != 1 {
		t.Fatalf("legacy temporal rows = %#v", got)
	} else {
		for index := range want {
			if got[0][index] != want[index] {
				t.Errorf("legacy temporal column %d = %#v, want %#v", index, got[0][index], want[index])
			}
		}
	}

	historical, err := executor.Execute(context.Background(), app.ExecuteRequest{
		Query:    "MATCH (n:Legacy {key:'value'}) RETURN n.at = $at, n.elapsed = $elapsed",
		Params:   map[string]any{"at": legacyTime, "elapsed": legacyDuration},
		Snapshot: domain.Snapshot{Revision: created.Revision},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := historical.Results[0].Rows; len(got) != 1 || got[0][0] != true || got[0][1] != true {
		t.Fatalf("historical legacy temporal row = %#v", got)
	}
}

func TestEngineTemporalOverflowIsAnError(t *testing.T) {
	executor, _ := testEngine(t)
	for _, query := range []string{
		"RETURN duration('P9223372036854775807M') + duration('P1M')",
		"RETURN datetime('+999999999-12-31T23:59:59.999999999Z') + duration('PT0.000000001S')",
	} {
		_, err := executor.Execute(context.Background(), app.ExecuteRequest{Query: query})
		if err == nil || !strings.Contains(strings.ToLower(err.Error()), "overflow") && !strings.Contains(err.Error(), "outside") {
			t.Errorf("Execute(%q) error = %v, want checked range failure", query, err)
		}
	}
}

func TestEngineTemporalRuntimeErrorsRollbackMutations(t *testing.T) {
	queries := []string{
		"CREATE (:Temporal {value:date('not-a-date')})",
		"CREATE (:Temporal {value:duration('PT1S') / 0})",
		"CREATE (:Temporal {value:datetime('+999999999-12-31T23:59:59.999999999Z') + duration('PT0.000000001S')})",
	}
	for _, query := range queries {
		t.Run(query, func(t *testing.T) {
			executor, database := testEngine(t)
			if _, err := executor.Execute(context.Background(), app.ExecuteRequest{Query: "EXPLAIN " + query}); err != nil {
				t.Fatalf("EXPLAIN rejected runtime temporal error: %v", err)
			}
			if _, err := executor.Execute(context.Background(), app.ExecuteRequest{Query: query}); err == nil {
				t.Fatal("temporal mutation unexpectedly succeeded")
			}
			revision, err := database.CurrentRevision(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if revision != 0 {
				t.Fatalf("revision after failed temporal mutation = %d", revision)
			}
			snapshot, err := executor.Snapshot(context.Background(), domain.Snapshot{})
			if err != nil {
				t.Fatal(err)
			}
			if len(snapshot.Nodes) != 0 || len(snapshot.Edges) != 0 {
				t.Fatalf("failed temporal mutation left graph state: %#v", snapshot)
			}
		})
	}
}

func TestEngineTemporalHistoricalRoundTripAndIndexedResidual(t *testing.T) {
	executor, _ := testEngine(t)
	created := execute(t, executor, `
CREATE (n:Temporal {
  key:'exact',
  at:datetime('2024-11-03T01:30-05:00[America/New_York]'),
  values:[date('2024-02-29'), localtime('12:34:56.123456789'),
          time('12:34:56.123456789-05:00'), duration('P1M2DT3H')]
})`, nil)
	if created.Revision == nil {
		t.Fatal("create did not produce a revision")
	}
	execute(t, executor, "MATCH (n:Temporal {key:'exact'}) SET n.at = datetime('2025-01-01T00:00Z')", nil)

	historical, err := executor.Execute(context.Background(), app.ExecuteRequest{
		Query: `MATCH (n:Temporal {at:datetime('2024-11-03T01:30-05:00[America/New_York]')})
RETURN n.at, n.values`,
		Snapshot: domain.Snapshot{Revision: created.Revision},
	})
	if err != nil {
		t.Fatal(err)
	}
	rows := historical.Results[0].Rows
	if len(rows) != 1 || len(rows[0]) != 2 {
		t.Fatalf("historical rows = %#v", rows)
	}
	at, ok := rows[0][0].(temporal.DateTime)
	if !ok || at.String() != "2024-11-03T01:30-05:00[America/New_York]" {
		t.Fatalf("historical datetime = %#v", rows[0][0])
	}
	values, ok := rows[0][1].([]any)
	if !ok || len(values) != 4 {
		t.Fatalf("historical temporal list = %#v", rows[0][1])
	}
	if _, ok := values[0].(temporal.Date); !ok {
		t.Errorf("date type = %T", values[0])
	}
	if _, ok := values[1].(temporal.LocalTime); !ok {
		t.Errorf("local time type = %T", values[1])
	}
	if _, ok := values[2].(temporal.Time); !ok {
		t.Errorf("time type = %T", values[2])
	}
	if _, ok := values[3].(temporal.Duration); !ok {
		t.Errorf("duration type = %T", values[3])
	}
}
