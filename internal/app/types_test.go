package app

import (
	"encoding/json"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/svlocks/sheets/internal/domain"
	"github.com/svlocks/sheets/internal/domain/temporal"
)

func TestExecuteRequestValidate(t *testing.T) {
	revision := domain.Revision(1)
	now := time.Now()
	tests := []struct {
		name    string
		request ExecuteRequest
		wantErr bool
	}{
		{name: "valid", request: ExecuteRequest{Query: "RETURN 1"}},
		{name: "empty", request: ExecuteRequest{}, wantErr: true},
		{name: "whitespace", request: ExecuteRequest{Query: " \n\t"}, wantErr: true},
		{
			name: "two snapshots",
			request: ExecuteRequest{
				Query:    "RETURN 1",
				Snapshot: domain.Snapshot{Revision: &revision, Time: &now},
			},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.request.Validate(); (got != nil) != tt.wantErr {
				t.Fatalf("Validate() error = %v, wantErr %v", got, tt.wantErr)
			}
		})
	}
}

func TestExactJSONValuesRoundTripWithoutMutation(t *testing.T) {
	date, _ := temporal.ParseDate("1984-10-11")
	localTime, _ := temporal.ParseLocalTime("12:31:14.645876123")
	offsetTime, _ := temporal.ParseTime("12:31:14.645876123+01:00")
	localDateTime := temporal.NewLocalDateTime(date, localTime)
	dateTime, _ := temporal.NewDateTime(localDateTime, "Europe/Stockholm")
	duration, _ := temporal.NewDuration(-7, 14, -4, 500_000_000)
	legacyTime := time.Date(2026, 8, 31, 9, 10, 11, 12, time.FixedZone("legacy", -5*3600))
	extremeLegacyTime := time.Date(-1, 1, 2, 3, 4, 5, 6, time.FixedZone("historical", 53*60+28))
	legacyDuration := -90*time.Minute + 7
	original := domain.Properties{
		"date": date, "local_time": localTime, "offset_time": offsetTime,
		"local_datetime": localDateTime, "zoned_datetime": dateTime,
		"cypher_duration": duration, "legacy_time": legacyTime,
		"legacy_extreme_time": extremeLegacyTime,
		"legacy_duration":     legacyDuration, "bytes": []byte{0, 1, 2, 255},
		"nested": []any{math.NaN(), domain.Node{ID: "n", Properties: domain.Properties{"date": date}}},
	}
	encoded := JSONValue(original)
	data, err := json.Marshal(encoded)
	if err != nil {
		t.Fatal(err)
	}
	for _, tag := range []string{
		`"$date"`, `"$local_time"`, `"$offset_time"`, `"$local_datetime"`,
		`"$zoned_datetime"`, `"$cypher_duration"`, `"$legacy_time"`,
		`"$legacy_duration"`, `"$bytes"`, `"$float"`,
	} {
		if !strings.Contains(string(data), tag) {
			t.Errorf("exact JSON lacks %s: %s", tag, data)
		}
	}
	if _, ok := original["date"].(temporal.Date); !ok {
		t.Fatalf("JSONValue mutated caller date to %T", original["date"])
	}
	node := original["nested"].([]any)[1].(domain.Node)
	if _, ok := node.Properties["date"].(temporal.Date); !ok {
		t.Fatalf("JSONValue mutated nested node property to %T", node.Properties["date"])
	}

	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.UseNumber()
	var raw any
	if err := decoder.Decode(&raw); err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeTaggedJSONValue(raw, true)
	if err != nil {
		t.Fatal(err)
	}
	values := decoded.(map[string]any)
	for _, key := range []string{"date", "local_time", "offset_time", "local_datetime", "zoned_datetime", "cypher_duration"} {
		if !reflect.DeepEqual(values[key], original[key]) {
			t.Errorf("%s round trip = %#v (%T), want %#v (%T)", key, values[key], values[key], original[key], original[key])
		}
	}
	if got := values["legacy_time"].(time.Time); !got.Equal(legacyTime) || got.Location().String() != legacyTime.Location().String() {
		t.Errorf("legacy time round trip = %s (%s)", got, got.Location())
	}
	if got := values["legacy_extreme_time"].(time.Time); !got.Equal(extremeLegacyTime) || got.Location().String() != extremeLegacyTime.Location().String() {
		t.Errorf("extreme legacy time round trip = %s (%s)", got, got.Location())
	}
	if values["legacy_duration"] != legacyDuration || !reflect.DeepEqual(values["bytes"], original["bytes"]) {
		t.Errorf("legacy values = %#v / %#v", values["legacy_duration"], values["bytes"])
	}
	if value := values["nested"].([]any)[0].(float64); !math.IsNaN(value) {
		t.Errorf("tagged NaN round trip = %#v", value)
	}
}

func TestExactJSONEnvelopeValidationAndDuplicateKeys(t *testing.T) {
	date, _ := temporal.ParseDate("2026-08-31")
	envelope := JSONValue(date).(map[string]any)
	payload := envelope["$date"].(exactTemporalJSON)
	payload.Text = "2026-09-01"
	if _, err := DecodeTaggedJSONValue(map[string]any{"$date": map[string]any{
		"text": payload.Text, "binary": payload.Binary,
	}}, true); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("mismatched temporal envelope error = %v", err)
	}
	if _, err := DecodeTaggedJSONValue(map[string]any{"$date": map[string]any{
		"text": "2026-08-31", "binary": strings.Repeat("A", 1<<20),
	}}, true); err == nil || !strings.Contains(err.Error(), "length") {
		t.Fatalf("oversized temporal envelope error = %v", err)
	}
	plain, err := DecodeTaggedJSONValue(map[string]any{"$float": "NaN"}, false)
	if err != nil || !reflect.DeepEqual(plain, map[string]any{"$float": "NaN"}) {
		t.Fatalf("plain parameter map = %#v, %v", plain, err)
	}
	for _, input := range []string{
		`{"x":1,"\u0078":2}`,
		`{"x":{"text":"a","text":"b"}}`,
		`[{"x":1,"x":2}]`,
	} {
		if err := RejectDuplicateJSONKeys([]byte(input)); err == nil {
			t.Errorf("duplicate JSON %s was accepted", input)
		}
	}
}

func TestSummaryChanged(t *testing.T) {
	if (Summary{}).Changed() {
		t.Fatal("zero summary should not report a change")
	}
	if !(Summary{NodesCreated: 1}).Changed() {
		t.Fatal("created node should report a change")
	}
	if !(Summary{NodesCreated: ^uint64(0), NodesUpdated: 1}).Changed() {
		t.Fatal("counter overflow must not hide a change")
	}
}
