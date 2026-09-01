package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/svlocks/sheets/internal/domain"
)

func TestMigrationPreflightsEachHistoricalVersionBeforeMutation(t *testing.T) {
	t.Run("v1 source amplification before backfill", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "v1-amplification.db")
		id := domain.EntityID("019945ee-ea00-7be6-a100-000000000301")
		createV1Database(t, path, id, mustProperties(t, nil))
		labels := make([]string, domain.MaxLabelsPerNode+1)
		for index := range labels {
			labels[index] = fmt.Sprintf("L%05d", index)
		}
		encoded, err := json.Marshal(labels)
		if err != nil {
			t.Fatal(err)
		}
		raw := openRawSQLite(t, path)
		if _, err := raw.Exec("UPDATE node_versions SET labels=?", encoded); err != nil {
			t.Fatal(err)
		}
		if err := raw.Close(); err != nil {
			t.Fatal(err)
		}

		assertMigrationRejectedAtVersion(t, path, 1, "node_version_labels")
	})

	t.Run("v2 derived amplification before table rebuild", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "v2-amplification.db")
		id := domain.EntityID("019945ee-ea00-7be6-a100-000000000302")
		createV2Database(t, path, id, mustProperties(t, nil))
		raw := openRawSQLite(t, path)
		value := []byte(`{"k":"int","s":"0"}`)
		if _, err := raw.Exec(`WITH RECURSIVE sequence(value) AS (
			VALUES(0) UNION ALL SELECT value + 1 FROM sequence WHERE value < ?
		)
		INSERT INTO node_property_index(id, valid_from, key, kind, value)
		SELECT ?, 1, printf('p%05d', value), 'int', ? FROM sequence`,
			domain.MaxIndexedPropertiesPerVersion, string(id), value); err != nil {
			t.Fatal(err)
		}
		if err := raw.Close(); err != nil {
			t.Fatal(err)
		}

		assertMigrationRejectedAtVersion(t, path, 2, "node_property_index_v3")
	})

	t.Run("v2 corrupt derived data before table rebuild", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "v2-corrupt-derived.db")
		id := domain.EntityID("019945ee-ea00-7be6-a100-000000000303")
		createV2Database(t, path, id, mustProperties(t, domain.Properties{"rank": int64(1)}))
		raw := openRawSQLite(t, path)
		if _, err := raw.Exec(`UPDATE node_property_index
			SET value=x'7b226b223a22696e74222c2273223a2232227d'
			WHERE id=? AND key='rank'`, string(id)); err != nil {
			t.Fatal(err)
		}
		if err := raw.Close(); err != nil {
			t.Fatal(err)
		}

		assertMigrationRejectedAtVersion(t, path, 2, "node_property_index_v3")
	})

	t.Run("v4 source amplification before v5 triggers", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "v4-amplification.db")
		id := domain.EntityID("019945ee-ea00-7be6-a100-000000000304")
		properties := make(map[string]encodedValue, domain.MaxIndexedPropertiesPerVersion+1)
		for index := 0; index <= domain.MaxIndexedPropertiesPerVersion; index++ {
			properties[fmt.Sprintf("p%05d", index)] = encodedValue{Kind: "int", Text: "0"}
		}
		encoded, err := json.Marshal(encodedValue{Kind: "map", Map: properties})
		if err != nil {
			t.Fatal(err)
		}
		createV1Database(t, path, id, encoded)
		raw := openRawSQLite(t, path)
		migrateRawToV4(t, raw)
		if err := raw.Close(); err != nil {
			t.Fatal(err)
		}

		assertMigrationRejectedAtVersion(t, path, 4, "node_versions_derived_limits_insert")
	})
}

func TestMigrationRejectsAlteredV4SchemaBeforeV5(t *testing.T) {
	path := filepath.Join(t.TempDir(), "altered-v4.db")
	id := domain.EntityID("019945ee-ea00-7be6-a100-000000000305")
	createV1Database(t, path, id, mustProperties(t, nil))
	raw := openRawSQLite(t, path)
	migrateRawToV4(t, raw)
	const trigger = "node_versions_resource_limits_insert"
	var definition string
	if err := raw.QueryRow("SELECT sql FROM sqlite_schema WHERE name=?", trigger).Scan(&definition); err != nil {
		t.Fatal(err)
	}
	altered := strings.Replace(definition, "BEGIN", "BEGIN\n    -- altered v4", 1)
	if _, err := raw.Exec("DROP TRIGGER " + trigger); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(altered); err != nil {
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}

	if opened, err := Open(context.Background(), path); err == nil {
		_ = opened.Close()
		t.Fatal("v5 migration repaired an altered v4 schema")
	} else if !errors.Is(err, ErrCorrupt) {
		t.Fatalf("migration error = %v, want ErrCorrupt", err)
	}
	raw = openRawSQLite(t, path)
	defer func() { _ = raw.Close() }()
	var version, v5Objects int
	var after string
	if err := raw.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		t.Fatal(err)
	}
	if err := raw.QueryRow("SELECT sql FROM sqlite_schema WHERE name=?", trigger).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if err := raw.QueryRow("SELECT COUNT(*) FROM sqlite_schema WHERE name='node_versions_derived_limits_insert'").Scan(&v5Objects); err != nil {
		t.Fatal(err)
	}
	if version != 4 || v5Objects != 0 || after != altered {
		t.Fatalf("altered v4 changed: version=%d v5_objects=%d\n%s", version, v5Objects, after)
	}
}

func TestDerivedResourceMigrationCrashRollbackAndConcurrentOpen(t *testing.T) {
	t.Run("crash rollback", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "crash-v5.db")
		id := domain.EntityID("019945ee-ea00-7be6-a100-000000000306")
		createV1Database(t, path, id, mustProperties(t, nil))
		raw := openRawSQLite(t, path)
		migrateRawToV4(t, raw)
		if err := raw.Close(); err != nil {
			t.Fatal(err)
		}

		command := exec.Command(os.Args[0], "-test.run=^TestDerivedResourceMigrationCrashHelper$")
		command.Env = append(os.Environ(), "SHEETS_DERIVED_MIGRATION_CRASH="+path)
		if output, err := command.CombinedOutput(); err == nil {
			t.Fatalf("crashing migration helper succeeded: %s", output)
		}

		database, err := Open(context.Background(), path)
		if err != nil {
			t.Fatalf("recover after v5 migration crash: %v", err)
		}
		defer func() { _ = database.Close() }()
		if got := storeSchemaFingerprint(t, database); got != expectedSchemaFingerprint {
			t.Fatalf("recovered fingerprint = %s", got)
		}
	})

	t.Run("concurrent open", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "concurrent-v5.db")
		id := domain.EntityID("019945ee-ea00-7be6-a100-000000000307")
		createV1Database(t, path, id, mustProperties(t, nil))
		raw := openRawSQLite(t, path)
		migrateRawToV4(t, raw)
		if err := raw.Close(); err != nil {
			t.Fatal(err)
		}

		const processes = 4
		start := make(chan struct{})
		errorsByOpen := make(chan error, processes)
		var wait sync.WaitGroup
		for range processes {
			wait.Add(1)
			go func() {
				defer wait.Done()
				<-start
				database, err := Open(context.Background(), path)
				if err == nil {
					err = database.Close()
				}
				errorsByOpen <- err
			}()
		}
		close(start)
		wait.Wait()
		close(errorsByOpen)
		for err := range errorsByOpen {
			if err != nil {
				t.Fatalf("concurrent v5 open: %v", err)
			}
		}
		raw = openRawSQLite(t, path)
		defer func() { _ = raw.Close() }()
		var version int
		if err := raw.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != schemaVersion {
			t.Fatalf("schema version = %d, %v", version, err)
		}
	})
}

func TestDerivedResourceMigrationCrashHelper(t *testing.T) {
	path := os.Getenv("SHEETS_DERIVED_MIGRATION_CRASH")
	if path == "" {
		return
	}
	dsn, err := makeDSN(path, 0)
	if err != nil {
		os.Exit(2)
	}
	database, err := sql.Open("sqlite", dsn)
	if err != nil {
		os.Exit(3)
	}
	database.SetMaxOpenConns(1)
	connection, err := database.Conn(context.Background())
	if err != nil {
		os.Exit(4)
	}
	if _, err := connection.ExecContext(context.Background(), "BEGIN IMMEDIATE"); err != nil {
		os.Exit(5)
	}
	if _, err := connection.ExecContext(context.Background(), derivedResourceLimitMigration); err != nil {
		os.Exit(6)
	}
	if _, err := connection.ExecContext(context.Background(), "PRAGMA user_version=5"); err != nil {
		os.Exit(7)
	}
	os.Exit(42)
}

func TestOpenPoolPathSwapCanObserveTwoValidDatabases(t *testing.T) {
	if os.Getenv("SHEETS_RUN_PATH_SWAP_REPRO") != "1" {
		t.Skip("set SHEETS_RUN_PATH_SWAP_REPRO=1 to run the known pathname/VFS security-boundary reproducer")
	}
	if runtime.GOOS == "windows" {
		t.Skip("Windows does not permit the same atomic symlink replacement while SQLite holds the target open")
	}
	ctx := context.Background()
	directory := t.TempDir()
	firstPath := filepath.Join(directory, "first.db")
	secondPath := filepath.Join(directory, "second.db")
	createRevision := func(path, body string, count int) {
		database := openTestStore(t, path)
		for index := 0; index < count; index++ {
			_, err := database.Write(ctx, RevisionMeta{}, func(tx *WriteTx) error {
				_, createErr := tx.CreateNode(NodeInput{Body: fmt.Sprintf("%s-%d", body, index)})
				return createErr
			})
			if err != nil {
				t.Fatal(err)
			}
		}
		if err := database.Close(); err != nil {
			t.Fatal(err)
		}
	}
	createRevision(firstPath, "first", 1)
	createRevision(secondPath, "second", 2)

	linkPath := filepath.Join(directory, "current.db")
	if err := os.Symlink(firstPath, linkPath); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	database, err := Open(ctx, linkPath, WithMaxOpenConns(2))
	if err != nil {
		t.Fatal(err)
	}
	firstConnection, err := database.db.Conn(ctx)
	if err != nil {
		_ = database.Close()
		t.Fatal(err)
	}
	var firstRevision int64
	if err := firstConnection.QueryRowContext(ctx, "SELECT COALESCE(MAX(revision), 0) FROM revisions").Scan(&firstRevision); err != nil {
		_ = firstConnection.Close()
		_ = database.Close()
		t.Fatal(err)
	}
	replacement := filepath.Join(directory, "replacement.db")
	if err := os.Symlink(secondPath, replacement); err != nil {
		_ = firstConnection.Close()
		_ = database.Close()
		t.Fatal(err)
	}
	if err := os.Rename(replacement, linkPath); err != nil {
		_ = firstConnection.Close()
		_ = database.Close()
		t.Fatal(err)
	}
	secondConnection, err := database.db.Conn(ctx)
	if err != nil {
		_ = firstConnection.Close()
		_ = database.Close()
		t.Fatal(err)
	}
	var secondRevision int64
	if err := secondConnection.QueryRowContext(ctx, "SELECT COALESCE(MAX(revision), 0) FROM revisions").Scan(&secondRevision); err != nil {
		_ = secondConnection.Close()
		_ = firstConnection.Close()
		_ = database.Close()
		t.Fatal(err)
	}
	if err := secondConnection.Close(); err != nil {
		t.Fatal(err)
	}
	if err := firstConnection.Close(); err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	if firstRevision != 1 || secondRevision != 2 {
		t.Fatalf("path swap revisions = %d then %d; reproducer did not reach both valid databases", firstRevision, secondRevision)
	}
}

func assertMigrationRejectedAtVersion(t *testing.T, path string, wantVersion int, absentObject string) {
	t.Helper()
	if opened, err := Open(context.Background(), path); err == nil {
		_ = opened.Close()
		t.Fatalf("migration from version %d accepted invalid resource amplification", wantVersion)
	} else if !errors.Is(err, ErrCorrupt) {
		t.Fatalf("migration error = %v, want ErrCorrupt", err)
	}
	raw := openRawSQLite(t, path)
	defer func() { _ = raw.Close() }()
	var version, objects int
	if err := raw.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		t.Fatal(err)
	}
	if err := raw.QueryRow("SELECT COUNT(*) FROM sqlite_schema WHERE name=?", absentObject).Scan(&objects); err != nil {
		t.Fatal(err)
	}
	if version != wantVersion || objects != 0 {
		t.Fatalf("failed migration persisted state: version=%d %s=%d", version, absentObject, objects)
	}
}

func migrateRawToV4(t *testing.T, database *sql.DB) {
	t.Helper()
	migrateRawToV3(t, database)
	if _, err := database.Exec(resourceLimitMigration); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec("PRAGMA user_version=4"); err != nil {
		t.Fatal(err)
	}
}
