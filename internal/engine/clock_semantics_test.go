package engine

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/svlocks/sheets/internal/app"
	"github.com/svlocks/sheets/internal/cypher"
	"github.com/svlocks/sheets/internal/domain/temporal"
)

func TestQueryClocksHaveTransactionStatementAndRealtimeLifetimes(t *testing.T) {
	document, err := cypher.Parse(`
RETURN datetime.transaction() AS tx,
       datetime.statement() AS statement,
       datetime.realtime() AS realtime1,
       datetime.realtime() AS realtime2;
RETURN datetime.transaction() AS tx,
       datetime.statement() AS statement`)
	if err != nil {
		t.Fatal(err)
	}
	times := []time.Time{
		time.Unix(100, 1).UTC(), // transaction
		time.Unix(200, 2).UTC(), // first statement
		time.Unix(300, 3).UTC(), // first realtime call
		time.Unix(400, 4).UTC(), // second realtime call
		time.Unix(500, 5).UTC(), // second statement
	}
	next := 0
	clock := func() time.Time {
		if next >= len(times) {
			t.Fatalf("clock called more than %d times", len(times))
		}
		value := times[next]
		next++
		return value
	}
	batch, err := executeDocumentWithClock(
		context.Background(), document, newMemoryGraph(1, nil, nil, nil), nil, nil, clock,
	)
	if err != nil {
		t.Fatal(err)
	}
	if next != len(times) {
		t.Fatalf("clock calls = %d, want %d", next, len(times))
	}
	want := [][]time.Time{
		{times[0], times[1], times[2], times[3]},
		{times[0], times[4]},
	}
	for resultIndex, result := range batch.Results {
		for columnIndex, value := range result.Rows[0] {
			got, ok := value.(temporal.DateTime)
			if !ok {
				t.Fatalf("result %d column %d = %#v", resultIndex, columnIndex, value)
			}
			expected := want[resultIndex][columnIndex]
			if got.EpochSecond() != expected.Unix() || got.Nanosecond() != expected.Nanosecond() {
				t.Fatalf("result %d column %d = %s, want %s", resultIndex, columnIndex, got, expected)
			}
		}
	}
}

func TestQueryClocksApplyExplicitTimezoneAtTheSelectedInstant(t *testing.T) {
	instant := time.Date(2024, time.March, 10, 12, 30, 0, 123456789, time.UTC)
	document, err := cypher.Parse(`
RETURN datetime.statement('America/New_York').hour AS nyHour,
       datetime.statement('America/New_York').timezone AS nyZone,
       datetime.statement('America/New_York').offsetSeconds AS nyOffset,
       time.statement('+05:30').hour AS offsetHour,
       time.statement('+05:30').offsetSeconds AS offsetSeconds,
       localdatetime.transaction('America/New_York').hour AS localHour,
       date.realtime('Pacific/Kiritimati').day AS nextDay`)
	if err != nil {
		t.Fatal(err)
	}
	batch, err := executeDocumentWithClock(
		context.Background(), document, newMemoryGraph(1, nil, nil, nil), nil, nil,
		func() time.Time { return instant },
	)
	if err != nil {
		t.Fatal(err)
	}
	want := []any{int64(8), "America/New_York", int64(-4 * 60 * 60), int64(18), int64(5*60*60 + 30*60), int64(8), int64(11)}
	if got := batch.Results[0].Rows[0]; !reflect.DeepEqual(got, want) {
		t.Fatalf("timezone clock row = %#v, want %#v", got, want)
	}
}

func TestQueryClockNullAndTimezoneErrors(t *testing.T) {
	executor, _ := testEngine(t)
	result := execute(t, executor, `
RETURN date.transaction(null), date.statement(null), date.realtime(null),
       localtime.transaction(null), localtime.statement(null), localtime.realtime(null),
       time.transaction(null), time.statement(null), time.realtime(null),
       localdatetime.transaction(null), localdatetime.statement(null), localdatetime.realtime(null),
       datetime.transaction(null), datetime.statement(null), datetime.realtime(null)`, nil)
	if got, want := result.Results[0].Rows[0], make([]any, 15); !reflect.DeepEqual(got, want) {
		t.Fatalf("null clock row = %#v, want %#v", got, want)
	}

	_, err := executor.Execute(context.Background(), app.ExecuteRequest{Query: "RETURN datetime.statement('Not/A_Zone')"})
	if err == nil || !strings.Contains(err.Error(), "timezone") {
		t.Fatalf("invalid timezone error = %v", err)
	}
}
