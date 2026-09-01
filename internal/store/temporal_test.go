package store

import (
	"context"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/svlocks/sheets/internal/domain"
	"github.com/svlocks/sheets/internal/domain/temporal"
)

func TestTemporalCodecRoundTripGoldenTagsAndLegacyValues(t *testing.T) {
	values := temporalValues(t)
	legacyTime := time.Date(2025, 3, 4, 5, 6, 7, 8, time.FixedZone("legacy", 90*60))
	legacyDuration := 5*time.Minute + 7
	properties := domain.Properties{
		"values": domain.Properties{
			"list": []any{values["date"], values["datetime"], values["duration"]},
		},
		"date": values["date"], "local_time": values["local_time"],
		"offset_time": values["offset_time"], "local_datetime": values["local_datetime"],
		"datetime": values["datetime"], "duration": values["duration"],
		"legacy_time": legacyTime, "legacy_duration": legacyDuration,
	}
	encoded, err := encodeProperties(properties)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := decodeProperties(encoded)
	if err != nil {
		t.Fatal(err)
	}
	reencoded, err := encodeProperties(decoded)
	if err != nil || !reflect.DeepEqual(encoded, reencoded) {
		t.Fatalf("temporal codec was not canonical: %v\n%s\n%s", err, encoded, reencoded)
	}
	for key, expected := range properties {
		if key == "values" {
			continue
		}
		if !reflect.DeepEqual(decoded[key], expected) {
			t.Errorf("decoded %s = %#v (%T), want %#v (%T)", key, decoded[key], decoded[key], expected, expected)
		}
	}
	entries, err := scalarProperties(properties)
	if err != nil {
		t.Fatal(err)
	}
	kinds := make(map[string]string, len(entries))
	for _, entry := range entries {
		kinds[entry.key] = entry.kind
	}
	wantKinds := map[string]string{
		"date": "date", "local_time": "local_time", "offset_time": "offset_time",
		"local_datetime": "local_datetime", "datetime": "zoned_datetime",
		"duration": "cypher_duration", "legacy_time": "time", "legacy_duration": "duration",
	}
	if !reflect.DeepEqual(kinds, wantKinds) {
		t.Fatalf("scalar temporal tags = %#v, want %#v", kinds, wantKinds)
	}
	date := values["date"].(temporal.Date)
	datePayload, _ := date.MarshalBinary()
	dateScalar, err := (&encodeState{visiting: make(map[encodeReference]struct{})}).encodeValue(date, 0)
	if err != nil {
		t.Fatal(err)
	}
	if dateScalar.Text != "AQAAB8AKCw==" || dateScalar.Text != base64.StdEncoding.EncodeToString(datePayload) {
		t.Fatalf("date codec golden = %#v payload=%x", dateScalar, datePayload)
	}
	// Corrupting a version byte must fail before the value reaches graph state.
	datePayload[0]++
	corrupt := []byte(fmt.Sprintf(`{"k":"map","o":{"date":{"k":"date","s":%q}}}`, base64.StdEncoding.EncodeToString(datePayload)))
	if _, err := decodeProperties(corrupt); err == nil || !strings.Contains(err.Error(), "date") {
		t.Fatalf("corrupt temporal payload error = %v", err)
	}
}

func TestTemporalCodecRejectsOversizedPayloadBeforeDecode(t *testing.T) {
	called := false
	text := strings.Repeat("A", base64.StdEncoding.EncodedLen(20+65535)+4)
	_, err := decodeTemporalBinary(text, "zoned_datetime", 20, 20+65535, func([]byte) (any, error) {
		called = true
		return nil, nil
	})
	if err == nil || called {
		t.Fatalf("oversized temporal decode error=%v decoder-called=%t", err, called)
	}
}

func TestTemporalPropertiesIndexedAcrossHistoryAndKinds(t *testing.T) {
	ctx := context.Background()
	database := openTestStore(t, filepath.Join(t.TempDir(), "temporal.db"))
	t.Cleanup(func() { _ = database.Close() })
	values := temporalValues(t)
	var node domain.Node
	if _, err := database.Write(ctx, RevisionMeta{}, func(tx *WriteTx) error {
		var err error
		node, err = tx.CreateNode(NodeInput{Properties: domain.Properties{
			"date": values["date"], "local_time": values["local_time"],
			"offset_time": values["offset_time"], "local_datetime": values["local_datetime"],
			"datetime": values["datetime"], "duration": values["duration"],
			"date_text": values["date"].(temporal.Date).String(),
		}})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	view, err := database.View(ctx, domain.Snapshot{})
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"date", "local_time", "offset_time", "local_datetime", "datetime", "duration"} {
		matched, _, err := view.ScanNodes(ctx, NodePredicate{Properties: domain.Properties{key: values[key]}}, domain.Page{})
		if err != nil || len(matched) != 1 || matched[0].ID != node.ID {
			t.Errorf("indexed %s match = %#v, %v", key, matched, err)
		}
	}
	dateString := values["date"].(temporal.Date).String()
	if matched, _, err := view.ScanNodes(ctx, NodePredicate{Properties: domain.Properties{"date": dateString}}, domain.Page{}); err != nil || len(matched) != 0 {
		t.Fatalf("string collided with Date scalar key: %#v, %v", matched, err)
	}
	newDate, _ := temporal.ParseDate("2026-08-31")
	if _, err := database.Write(ctx, RevisionMeta{}, func(tx *WriteTx) error {
		properties := domain.Properties{"date": newDate}
		_, err := tx.UpdateNode(node.ID, NodeUpdate{Properties: &properties})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	revisionOne := domain.Revision(1)
	historical, err := database.View(ctx, domain.Snapshot{Revision: &revisionOne})
	if err != nil {
		t.Fatal(err)
	}
	matched, _, err := historical.ScanNodes(ctx, NodePredicate{Properties: domain.Properties{"date": values["date"]}}, domain.Page{})
	if err != nil || len(matched) != 1 || matched[0].ID != node.ID {
		t.Fatalf("historical temporal index = %#v, %v", matched, err)
	}
	if err := database.CheckIntegrity(ctx); err != nil {
		t.Fatalf("temporal index integrity: %v", err)
	}
}

func TestV2TemporalMigrationPreservesLegacyRowsAndIsAtomic(t *testing.T) {
	ctx := context.Background()
	legacyTime := time.Date(2022, 7, 8, 9, 10, 11, 12, time.FixedZone("legacy", -5*3600))
	legacyDuration := -37*time.Minute + 9
	legacyProperties := domain.Properties{"when": legacyTime, "elapsed": legacyDuration, "rank": int64(4)}

	t.Run("preserve", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "v2.db")
		id := domain.EntityID("019945ee-ea00-7be6-a100-000000000101")
		createV2Database(t, path, id, mustProperties(t, legacyProperties))
		raw := openRawSQLite(t, path)
		var beforeKind string
		var beforeValue []byte
		if err := raw.QueryRow("SELECT kind, value FROM node_property_index WHERE id=? AND key='when'", string(id)).Scan(&beforeKind, &beforeValue); err != nil {
			t.Fatal(err)
		}
		_ = raw.Close()
		database, err := Open(ctx, path)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = database.Close() }()
		node, err := database.GetNode(ctx, id, domain.Snapshot{})
		if err != nil || !node.Properties["when"].(time.Time).Equal(legacyTime) || node.Properties["elapsed"].(time.Duration) != legacyDuration {
			t.Fatalf("legacy values after v3 = %#v, %v", node.Properties, err)
		}
		raw = openRawSQLite(t, path)
		defer func() { _ = raw.Close() }()
		var version int
		var afterKind string
		var afterValue []byte
		if err := raw.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
			t.Fatal(err)
		}
		if err := raw.QueryRow("SELECT kind, value FROM node_property_index WHERE id=? AND key='when'", string(id)).Scan(&afterKind, &afterValue); err != nil {
			t.Fatal(err)
		}
		if version != 3 || beforeKind != "time" || afterKind != beforeKind || !reflect.DeepEqual(afterValue, beforeValue) {
			t.Fatalf("legacy scalar row changed: version=%d before=%s/%s after=%s/%s", version, beforeKind, beforeValue, afterKind, afterValue)
		}
	})

	t.Run("rollback", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "v2-invalid-schema.db")
		id := domain.EntityID("019945ee-ea00-7be6-a100-000000000102")
		createV2Database(t, path, id, mustProperties(t, legacyProperties))
		raw := openRawSQLite(t, path)
		if _, err := raw.Exec("DROP TRIGGER nodes_validate_update"); err != nil {
			t.Fatal(err)
		}
		_ = raw.Close()
		if database, err := Open(ctx, path); err == nil {
			_ = database.Close()
			t.Fatal("v3 migration accepted an invalid pre-existing schema")
		} else if !errors.Is(err, ErrCorrupt) {
			t.Fatalf("v3 rollback error = %v, want ErrCorrupt", err)
		}
		raw = openRawSQLite(t, path)
		defer func() { _ = raw.Close() }()
		assertV2Schema(t, raw)
	})
}

func TestV2TemporalMigrationCrashAndConcurrentOpen(t *testing.T) {
	t.Run("crash", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "v2-crash.db")
		id := domain.EntityID("019945ee-ea00-7be6-a100-000000000103")
		createV2Database(t, path, id, mustProperties(t, domain.Properties{"rank": int64(1)}))
		command := exec.Command(os.Args[0], "-test.run=^TestTemporalMigrationCrashHelper$")
		command.Env = append(os.Environ(), "SHEETS_TEMPORAL_MIGRATION_CRASH="+path)
		if output, err := command.CombinedOutput(); err == nil {
			t.Fatalf("crashing migration helper succeeded: %s", output)
		}
		raw := openRawSQLite(t, path)
		assertV2Schema(t, raw)
		_ = raw.Close()
		database, err := Open(context.Background(), path)
		if err != nil {
			t.Fatalf("recover after migration crash: %v", err)
		}
		_ = database.Close()
	})

	t.Run("concurrent", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "v2-concurrent.db")
		id := domain.EntityID("019945ee-ea00-7be6-a100-000000000104")
		createV2Database(t, path, id, mustProperties(t, domain.Properties{"rank": int64(1)}))
		const workers = 8
		start := make(chan struct{})
		errorsCh := make(chan error, workers)
		var wait sync.WaitGroup
		for range workers {
			wait.Add(1)
			go func() {
				defer wait.Done()
				<-start
				database, err := Open(context.Background(), path, WithBusyTimeout(10*time.Second))
				if err == nil {
					err = database.Close()
				}
				if err != nil {
					errorsCh <- err
				}
			}()
		}
		close(start)
		wait.Wait()
		close(errorsCh)
		for err := range errorsCh {
			t.Errorf("concurrent v3 migration: %v", err)
		}
		raw := openRawSQLite(t, path)
		defer func() { _ = raw.Close() }()
		var version int
		if err := raw.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != 3 {
			t.Fatalf("concurrent migration version=%d, %v", version, err)
		}
	})
}

func TestTemporalMigrationCrashHelper(t *testing.T) {
	path := os.Getenv("SHEETS_TEMPORAL_MIGRATION_CRASH")
	if path == "" {
		return
	}
	dsn, err := makeDSN(path, 10*time.Second)
	if err != nil {
		os.Exit(2)
	}
	database, err := sql.Open("sqlite", dsn)
	if err != nil {
		os.Exit(3)
	}
	if _, err := database.Exec("BEGIN IMMEDIATE"); err != nil {
		os.Exit(4)
	}
	if _, err := database.Exec(temporalMigration); err != nil {
		os.Exit(5)
	}
	if _, err := database.Exec("PRAGMA user_version=3"); err != nil {
		os.Exit(6)
	}
	os.Exit(31)
}

func TestTemporalIndexedEqualityAcrossProcess(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cross-process.db")
	command := exec.Command(os.Args[0], "-test.run=^TestTemporalWriterHelper$")
	command.Env = append(os.Environ(), "SHEETS_TEMPORAL_WRITER="+path)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("temporal writer helper: %v: %s", err, output)
	}
	database := openTestStore(t, path)
	defer func() { _ = database.Close() }()
	date, _ := temporal.ParseDate("2026-08-31")
	view, err := database.View(context.Background(), domain.Snapshot{})
	if err != nil {
		t.Fatal(err)
	}
	nodes, _, err := view.ScanNodes(context.Background(), NodePredicate{Properties: domain.Properties{"date": date}}, domain.Page{})
	if err != nil || len(nodes) != 1 {
		t.Fatalf("cross-process temporal lookup = %#v, %v", nodes, err)
	}
}

func TestTemporalWriterHelper(t *testing.T) {
	path := os.Getenv("SHEETS_TEMPORAL_WRITER")
	if path == "" {
		return
	}
	database, err := Open(context.Background(), path, WithBusyTimeout(10*time.Second))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	date, _ := temporal.ParseDate("2026-08-31")
	_, err = database.Write(context.Background(), RevisionMeta{}, func(tx *WriteTx) error {
		_, err := tx.CreateNode(NodeInput{Properties: domain.Properties{"date": date}})
		return err
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(3)
	}
	if err := database.Close(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(4)
	}
}

func BenchmarkTemporalCodec(b *testing.B) {
	values := temporalValues(b)
	properties := domain.Properties{
		"date": values["date"], "local_time": values["local_time"],
		"offset_time": values["offset_time"], "local_datetime": values["local_datetime"],
		"datetime": values["datetime"], "duration": values["duration"],
	}
	encoded, err := encodeProperties(properties)
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.Run("encode", func(b *testing.B) {
		for b.Loop() {
			if _, err := encodeProperties(properties); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("decode", func(b *testing.B) {
		for b.Loop() {
			if _, err := decodeProperties(encoded); err != nil {
				b.Fatal(err)
			}
		}
	})
}

type testingTB interface {
	Helper()
	Fatal(...any)
}

func temporalValues(tb testingTB) map[string]any {
	tb.Helper()
	date, err := temporal.ParseDate("1984-10-11")
	if err != nil {
		tb.Fatal(err)
	}
	localTime, err := temporal.ParseLocalTime("12:31:14.645876123")
	if err != nil {
		tb.Fatal(err)
	}
	offsetTime, err := temporal.ParseTime("12:31:14.645876123+01:00")
	if err != nil {
		tb.Fatal(err)
	}
	localDateTime := temporal.NewLocalDateTime(date, localTime)
	dateTime, err := temporal.NewDateTime(localDateTime, "Europe/Stockholm")
	if err != nil {
		tb.Fatal(err)
	}
	duration, err := temporal.NewDuration(-7, 14, -4, 500_000_000)
	if err != nil {
		tb.Fatal(err)
	}
	return map[string]any{
		"date": date, "local_time": localTime, "offset_time": offsetTime,
		"local_datetime": localDateTime, "datetime": dateTime, "duration": duration,
	}
}

func createV2Database(t *testing.T, path string, id domain.EntityID, properties []byte) {
	t.Helper()
	createV1Database(t, path, id, properties)
	database := openRawSQLite(t, path)
	defer func() { _ = database.Close() }()
	if _, err := database.Exec(invariantMigration); err != nil {
		t.Fatal(err)
	}
	connection, err := database.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := backfillPropertyIndexes(context.Background(), connection); err != nil {
		_ = connection.Close()
		t.Fatal(err)
	}
	if err := connection.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec("PRAGMA user_version=2"); err != nil {
		t.Fatal(err)
	}
}

func assertV2Schema(t *testing.T, database *sql.DB) {
	t.Helper()
	var version int
	var definition string
	if err := database.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		t.Fatal(err)
	}
	if err := database.QueryRow("SELECT sql FROM sqlite_schema WHERE name='node_property_index'").Scan(&definition); err != nil {
		t.Fatal(err)
	}
	if version != 2 || strings.Contains(definition, "'date'") {
		t.Fatalf("migration was not rolled back: version=%d definition=%s", version, definition)
	}
}
