package engine

import (
	"context"
	"strings"
	"testing"

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
		var actual any = row[index]
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
