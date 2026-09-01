package store

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/svlocks/sheets/internal/domain"
)

func TestPropertyCodecResourceLimitsAndSizePreflight(t *testing.T) {
	plain := strings.Repeat("x", domain.MaxPropertyScalarBytes+1)
	if _, err := encodeProperties(domain.Properties{"value": plain[:domain.MaxPropertyScalarBytes]}); err != nil {
		t.Fatalf("exact scalar limit: %v", err)
	}
	if _, err := encodeProperties(domain.Properties{"value": plain}); !errors.Is(err, domain.ErrResourceLimit) {
		t.Fatalf("scalar limit+1 error = %v", err)
	}
	if _, err := encodeProperties(domain.Properties{"value": make([]byte, domain.MaxPropertyScalarBytes+1)}); !errors.Is(err, domain.ErrResourceLimit) {
		t.Fatalf("byte scalar limit+1 error = %v", err)
	}
	byteChunk := make([]byte, domain.MaxPropertyScalarBytes)
	if _, err := encodeProperties(domain.Properties{
		"chunks": []any{byteChunk, byteChunk, byteChunk},
	}); !errors.Is(err, domain.ErrResourceLimit) || !strings.Contains(err.Error(), "aggregate encoded property byte strings") {
		t.Fatalf("aggregate byte allocation guard error = %v", err)
	}
	reflectedItems := make([]int8, domain.MaxPropertyValues+1)
	if _, err := encodeProperties(domain.Properties{"items": reflectedItems}); !errors.Is(err, domain.ErrResourceLimit) || !strings.Contains(err.Error(), "property list") {
		t.Fatalf("reflected collection preallocation guard error = %v", err)
	}

	// Each scalar is well below its individual limit, but JSON escaping expands
	// this nested aggregate beyond the canonical 64 MiB envelope. The exact
	// size preflight rejects it before json.Marshal constructs that expansion.
	escapeHeavy := strings.Repeat("\x01", 6<<20)
	if _, err := encodeProperties(domain.Properties{
		"nested": []any{escapeHeavy, domain.Properties{"again": escapeHeavy}},
	}); !errors.Is(err, domain.ErrResourceLimit) {
		t.Fatalf("escape-amplified aggregate error = %v", err)
	}

	samples := []encodedValue{
		{Kind: "null"},
		{Kind: "bool", Bool: true},
		{Kind: "string", Text: "<line>\n雪"},
		{Kind: "list", Items: []encodedValue{{Kind: "int", Text: "7"}, {Kind: "string", Text: "\u2028"}}},
		{Kind: "map", Map: map[string]encodedValue{"a&b": {Kind: "string", Text: "quoted \" text"}}},
	}
	for _, sample := range samples {
		encoded, err := json.Marshal(sample)
		if err != nil {
			t.Fatal(err)
		}
		if got := canonicalEncodedValueSize(sample); got != int64(len(encoded)) {
			t.Fatalf("canonical size = %d, want %d for %s", got, len(encoded), encoded)
		}
	}
}

func TestMakeDSNAnchorsRelativeFileURIButPreservesMemoryURI(t *testing.T) {
	dsn, err := makeDSN("file:relative%20graph.db?mode=rwc", 0)
	if err != nil {
		t.Fatal(err)
	}
	uri, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	expected, err := filepath.Abs("relative graph.db")
	if err != nil {
		t.Fatal(err)
	}
	if uri.Opaque != "" || filepath.Clean(filepath.FromSlash(uri.Path)) != filepath.Clean(expected) {
		t.Fatalf("relative file URI was not anchored: %s", dsn)
	}

	memoryDSN, err := makeDSN("file:shared-memory?mode=memory&cache=shared", 0)
	if err != nil {
		t.Fatal(err)
	}
	memoryURI, err := url.Parse(memoryDSN)
	if err != nil {
		t.Fatal(err)
	}
	if memoryURI.Opaque != "shared-memory" || memoryURI.Query().Get("mode") != "memory" {
		t.Fatalf("named memory URI was changed: %s", memoryDSN)
	}
}

func TestStoreMutationLimitsAreTypedAndDoNotAllocateRevisions(t *testing.T) {
	ctx := context.Background()
	database := openTestStore(t, filepath.Join(t.TempDir(), "limits.db"))
	t.Cleanup(func() { _ = database.Close() })

	actor := strings.Repeat("a", domain.MaxRevisionActorBytes+1)
	result, err := database.Write(ctx, RevisionMeta{Actor: actor}, func(tx *WriteTx) error {
		_, createErr := tx.CreateNode(NodeInput{})
		return createErr
	})
	if !errors.Is(err, ErrInvalidArgument) || !errors.Is(err, domain.ErrResourceLimit) || result.Changed {
		t.Fatalf("oversized metadata result = %#v, %v", result, err)
	}

	// Metadata is lazy: if no revision is needed it is never durable input and
	// retains the established no-op behavior.
	result, err = database.Write(ctx, RevisionMeta{Actor: actor}, func(*WriteTx) error { return nil })
	if err != nil || result.Changed {
		t.Fatalf("unused oversized metadata = %#v, %v", result, err)
	}

	invalidUTF8 := string([]byte{0xff})
	_, err = database.Write(ctx, RevisionMeta{}, func(tx *WriteTx) error {
		_, createErr := tx.CreateNode(NodeInput{Body: invalidUTF8})
		return createErr
	})
	if !errors.Is(err, ErrInvalidArgument) || !errors.Is(err, domain.ErrInvalidText) {
		t.Fatalf("invalid body error = %v", err)
	}

	oversizedLabel := strings.Repeat("l", domain.MaxLabelBytes+1)
	_, err = database.Write(ctx, RevisionMeta{}, func(tx *WriteTx) error {
		_, createErr := tx.CreateNode(NodeInput{Labels: []string{oversizedLabel}})
		return createErr
	})
	if !errors.Is(err, ErrInvalidArgument) || !errors.Is(err, domain.ErrResourceLimit) {
		t.Fatalf("oversized label error = %v", err)
	}

	if revision, revisionErr := database.CurrentRevision(ctx); revisionErr != nil || revision != 0 {
		t.Fatalf("failed inputs consumed revision %d: %v", revision, revisionErr)
	}
}

func TestSQLiteResourceTriggersBlockRawSourceAndDerivedWrites(t *testing.T) {
	ctx := context.Background()
	database := openTestStore(t, filepath.Join(t.TempDir(), "raw-limits.db"))
	t.Cleanup(func() { _ = database.Close() })
	conn, err := database.db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
		_ = conn.Close()
	})
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		t.Fatal(err)
	}

	oversizedActor := strings.Repeat("a", domain.MaxRevisionActorBytes+1)
	if _, err := conn.ExecContext(ctx, `INSERT INTO revisions(revision, committed_ns, actor, message, sealed)
		VALUES (1, 1, ?, '', 0)`, oversizedActor); err == nil || !strings.Contains(err.Error(), "sheets_resource_limit_revision_actor") {
		t.Fatalf("raw oversized actor error = %v", err)
	}
	if _, err := conn.ExecContext(ctx, `INSERT INTO revisions(revision, committed_ns, actor, message, sealed)
		VALUES (1, 1, '', '', 0)`); err != nil {
		t.Fatal(err)
	}

	labels, _, err := encodeLabels([]string{"Raw"})
	if err != nil {
		t.Fatal(err)
	}
	emptyProperties, err := encodeProperties(nil)
	if err != nil {
		t.Fatal(err)
	}
	ids := []domain.EntityID{
		"019945ee-ea00-7be6-a100-000000000201",
		"019945ee-ea00-7be6-a100-000000000202",
		"019945ee-ea00-7be6-a100-000000000203",
		"019945ee-ea00-7be6-a100-000000000204",
		"019945ee-ea00-7be6-a100-000000000205",
		"019945ee-ea00-7be6-a100-000000000206",
	}
	for _, id := range ids {
		if _, err := conn.ExecContext(ctx, "INSERT INTO nodes(id, created_revision) VALUES (?, 1)", string(id)); err != nil {
			t.Fatal(err)
		}
	}

	if _, err := conn.ExecContext(ctx, `INSERT INTO node_versions(id, valid_from, labels, properties, body)
		VALUES (?, 1, ?, ?, CAST(zeroblob(?) AS TEXT))`, string(ids[0]), labels, emptyProperties, domain.MaxNodeBodyBytes+1); err == nil || !strings.Contains(err.Error(), "sheets_resource_limit_node_envelope") {
		t.Fatalf("raw oversized body error = %v", err)
	}

	oversizedKey := strings.Repeat("k", domain.MaxPropertyKeyBytes+1)
	keyProperties, err := json.Marshal(encodedValue{Kind: "map", Map: map[string]encodedValue{
		"outer": {Kind: "map", Map: map[string]encodedValue{oversizedKey: {Kind: "int", Text: "1"}}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(ctx, `INSERT INTO node_versions(id, valid_from, labels, properties, body)
		VALUES (?, 1, ?, ?, '')`, string(ids[1]), labels, keyProperties); err == nil || !strings.Contains(err.Error(), "sheets_resource_limit_property_key") {
		t.Fatalf("raw oversized nested key error = %v", err)
	}

	oversizedString := strings.Repeat("s", domain.MaxPropertyScalarBytes+1)
	stringProperties, err := json.Marshal(encodedValue{Kind: "map", Map: map[string]encodedValue{
		"outer": {Kind: "map", Map: map[string]encodedValue{"nested": {Kind: "string", Text: oversizedString}}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(ctx, `INSERT INTO node_versions(id, valid_from, labels, properties, body)
		VALUES (?, 1, ?, ?, '')`, string(ids[2]), labels, stringProperties); err == nil || !strings.Contains(err.Error(), "sheets_resource_limit_property_string") {
		t.Fatalf("raw oversized nested string error = %v", err)
	}

	encodedByteLimit := base64.StdEncoding.EncodedLen(domain.MaxPropertyScalarBytes)
	oversizedBytes := strings.Repeat("A", encodedByteLimit-1) + "=" // decodes to Max+1 bytes.
	byteProperties, err := json.Marshal(encodedValue{Kind: "map", Map: map[string]encodedValue{
		"nested": {Kind: "bytes", Text: oversizedBytes},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(ctx, `INSERT INTO node_versions(id, valid_from, labels, properties, body)
		VALUES (?, 1, ?, ?, '')`, string(ids[3]), labels, byteProperties); err == nil || !strings.Contains(err.Error(), "sheets_resource_limit_property_bytes") {
		t.Fatalf("raw oversized nested bytes error = %v", err)
	}

	validNULProperties, err := encodeProperties(domain.Properties{"contains\x00nul": "value\x00kept"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(ctx, `INSERT INTO node_versions(id, valid_from, labels, properties, body)
		VALUES (?, 1, ?, ?, ?)`, string(ids[4]), labels, validNULProperties, "body\x00kept"); err != nil {
		t.Fatalf("raw compatible NUL values: %v", err)
	}
	oversizedZoneProperties, err := json.Marshal(encodedValue{Kind: "map", Map: map[string]encodedValue{
		"when": {Kind: "time", Zone: strings.Repeat("z", domain.MaxTimeZoneNameBytes+1)},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(ctx, `INSERT INTO node_versions(id, valid_from, labels, properties, body)
		VALUES (?, 1, ?, ?, '')`, string(ids[5]), labels, oversizedZoneProperties); err == nil || !strings.Contains(err.Error(), "sheets_resource_limit_time_zone") {
		t.Fatalf("raw oversized time zone error = %v", err)
	}

	// Failed INSERT statements did not create versions, so these identities can
	// now host valid rows used to exercise UPDATE and derived-table boundaries.
	for _, id := range ids[:2] {
		if _, err := conn.ExecContext(ctx, `INSERT INTO node_versions(id, valid_from, labels, properties, body)
			VALUES (?, 1, ?, ?, '')`, string(id), labels, emptyProperties); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := conn.ExecContext(ctx, `UPDATE node_versions SET body=CAST(zeroblob(?) AS TEXT)
		WHERE id=? AND valid_from=1`, domain.MaxNodeBodyBytes+1, string(ids[0])); err == nil || !strings.Contains(err.Error(), "sheets_resource_limit_node_envelope") {
		t.Fatalf("raw oversized node UPDATE error = %v", err)
	}

	edgeID := domain.EntityID("019945ee-ea00-7be6-a100-000000000207")
	if _, err := conn.ExecContext(ctx, "INSERT INTO edges(id, created_revision) VALUES (?, 1)", string(edgeID)); err != nil {
		t.Fatal(err)
	}
	edgeProperties, err := encodeProperties(domain.Properties{"weight": int64(1)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(ctx, `INSERT INTO edge_versions(id, valid_from, from_id, type, to_id, properties)
		VALUES (?, 1, ?, 'LINK', ?, ?)`, string(edgeID), string(ids[0]), string(ids[1]), edgeProperties); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(ctx, "UPDATE edge_versions SET type=? WHERE id=? AND valid_from=1",
		strings.Repeat("t", domain.MaxRelationshipTypeBytes+1), string(edgeID)); err == nil || !strings.Contains(err.Error(), "sheets_resource_limit_edge_envelope") {
		t.Fatalf("raw oversized edge UPDATE error = %v", err)
	}
	if _, err := conn.ExecContext(ctx, "UPDATE node_version_labels SET label=? WHERE id=?", strings.Repeat("l", domain.MaxLabelBytes+1), string(ids[4])); err == nil || !strings.Contains(err.Error(), "sheets_resource_limit_label") {
		t.Fatalf("raw derived label error = %v", err)
	}
	if _, err := conn.ExecContext(ctx, "UPDATE node_property_index SET key=? WHERE id=?",
		oversizedKey, string(ids[4])); err == nil || !strings.Contains(err.Error(), "sheets_resource_limit_property_index") {
		t.Fatalf("raw derived node property error = %v", err)
	}
	if _, err := conn.ExecContext(ctx, "UPDATE edge_property_index SET value=zeroblob(?) WHERE id=?",
		domain.MaxCanonicalPropertyBytes+1, string(edgeID)); err == nil || !strings.Contains(err.Error(), "sheets_resource_limit_property_index") {
		t.Fatalf("raw derived edge property error = %v", err)
	}
}

func TestRawInvalidUTF8IsDetectedByIntegrityScan(t *testing.T) {
	ctx := context.Background()
	database := openTestStore(t, filepath.Join(t.TempDir(), "invalid-utf8.db"))
	t.Cleanup(func() { _ = database.Close() })
	conn, err := database.db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		t.Fatal(err)
	}
	id := domain.EntityID("019945ee-ea00-7be6-a100-000000000211")
	labels, _, _ := encodeLabels(nil)
	properties, _ := encodeProperties(nil)
	statements := []struct {
		query string
		args  []any
	}{
		{query: `INSERT INTO revisions(revision, committed_ns, actor, message, sealed)
			VALUES (1, 1, CAST(x'80' AS TEXT), '', 0)`},
		{query: "INSERT INTO nodes(id, created_revision) VALUES (?, 1)", args: []any{string(id)}},
		{query: `INSERT INTO node_versions(id, valid_from, labels, properties, body)
			VALUES (?, 1, ?, ?, '')`, args: []any{string(id), labels, properties}},
		{query: "UPDATE revisions SET sealed=1 WHERE revision=1"},
	}
	for _, statement := range statements {
		if _, err := conn.ExecContext(ctx, statement.query, statement.args...); err != nil {
			_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
			_ = conn.Close()
			t.Fatal(err)
		}
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		_ = conn.Close()
		t.Fatal(err)
	}
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	if err := database.CheckIntegrity(ctx); !errors.Is(err, domain.ErrInvalidText) {
		t.Fatalf("integrity error = %v", err)
	}
}

func TestV3ResourceMigrationRejectsInvalidHistoryAtomically(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name         string
		properties   []byte
		oversizeBody bool
	}{
		{
			name: "nested key",
			properties: mustJSON(t, encodedValue{Kind: "map", Map: map[string]encodedValue{
				"outer": {Kind: "map", Map: map[string]encodedValue{
					strings.Repeat("k", domain.MaxPropertyKeyBytes+1): {Kind: "int", Text: "1"},
				}},
			}}),
		},
		{name: "body envelope", properties: mustProperties(t, nil), oversizeBody: true},
	}
	for index, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "v3.db")
			id := domain.EntityID("019945ee-ea00-7be6-a100-00000000022" + string(rune('0'+index)))
			createV1Database(t, path, id, test.properties)
			raw := openRawSQLite(t, path)
			if test.oversizeBody {
				if _, err := raw.Exec("UPDATE node_versions SET body=CAST(zeroblob(?) AS TEXT)", domain.MaxNodeBodyBytes+1); err != nil {
					t.Fatal(err)
				}
			}
			migrateRawToV3(t, raw)
			if err := raw.Close(); err != nil {
				t.Fatal(err)
			}

			if opened, err := Open(ctx, path); err == nil {
				_ = opened.Close()
				t.Fatal("v4 migration accepted oversized history")
			} else if !errors.Is(err, ErrCorrupt) {
				t.Fatalf("migration error = %v, want ErrCorrupt", err)
			}

			raw = openRawSQLite(t, path)
			defer func() { _ = raw.Close() }()
			var version, v4Triggers int
			if err := raw.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
				t.Fatal(err)
			}
			if err := raw.QueryRow(`SELECT COUNT(*) FROM sqlite_schema
				WHERE type='trigger' AND name LIKE '%resource_limits%'`).Scan(&v4Triggers); err != nil {
				t.Fatal(err)
			}
			if version != 3 || v4Triggers != 0 {
				t.Fatalf("failed migration persisted state: version=%d triggers=%d", version, v4Triggers)
			}
		})
	}
}

func TestResourceMigrationSchemaFingerprintFreshAndV3(t *testing.T) {
	ctx := context.Background()
	fresh := openTestStore(t, filepath.Join(t.TempDir(), "fresh.db"))
	freshFingerprint := storeSchemaFingerprint(t, fresh)
	if err := fresh.Close(); err != nil {
		t.Fatal(err)
	}

	path := filepath.Join(t.TempDir(), "migrated.db")
	id := domain.EntityID("019945ee-ea00-7be6-a100-000000000230")
	createV1Database(t, path, id, mustProperties(t, nil))
	raw := openRawSQLite(t, path)
	migrateRawToV3(t, raw)
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}
	migrated, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	migratedFingerprint := storeSchemaFingerprint(t, migrated)
	if err := migrated.Close(); err != nil {
		t.Fatal(err)
	}
	if freshFingerprint != expectedSchemaFingerprint || migratedFingerprint != expectedSchemaFingerprint {
		t.Fatalf("schema fingerprints: fresh=%s migrated=%s expected=%s", freshFingerprint, migratedFingerprint, expectedSchemaFingerprint)
	}
}

func TestHistoricalSchemaFingerprints(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fingerprints.db")
	raw := openRawSQLite(t, path)
	defer func() { _ = raw.Close() }()
	if _, err := raw.Exec(initialMigration); err != nil {
		t.Fatal(err)
	}
	v1 := rawSchemaFingerprint(t, raw)
	if _, err := raw.Exec(invariantMigration); err != nil {
		t.Fatal(err)
	}
	v2 := rawSchemaFingerprint(t, raw)
	if _, err := raw.Exec(temporalMigration); err != nil {
		t.Fatal(err)
	}
	v3 := rawSchemaFingerprint(t, raw)
	if _, err := raw.Exec(resourceLimitMigration); err != nil {
		t.Fatal(err)
	}
	v4 := rawSchemaFingerprint(t, raw)
	if _, err := raw.Exec(derivedResourceLimitMigration); err != nil {
		t.Fatal(err)
	}
	v5 := rawSchemaFingerprint(t, raw)
	if _, err := raw.Exec(deterministicCycleIndexMigration); err != nil {
		t.Fatal(err)
	}
	v6 := rawSchemaFingerprint(t, raw)
	if v1 != expectedV1SchemaFingerprint || v2 != expectedV2SchemaFingerprint ||
		v3 != expectedV3SchemaFingerprint || v4 != expectedV4SchemaFingerprint ||
		v5 != expectedV5SchemaFingerprint || v6 != expectedSchemaFingerprint {
		t.Fatalf("historical fingerprints: v1=%s v2=%s v3=%s v4=%s v5=%s v6=%s", v1, v2, v3, v4, v5, v6)
	}
}

func rawSchemaFingerprint(t *testing.T, database *sql.DB) string {
	t.Helper()
	connection, err := database.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = connection.Close() }()
	fingerprint, err := schemaFingerprint(context.Background(), connection)
	if err != nil {
		t.Fatal(err)
	}
	return fingerprint
}

func TestResourceMigrationRejectsAlteredV3TriggersWithoutRepair(t *testing.T) {
	for index, trigger := range []string{
		"node_versions_validate_insert",
		"edge_versions_validate_insert",
		"edge_versions_validate_update_graph",
	} {
		t.Run(trigger, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "altered-v3.db")
			id := domain.EntityID("019945ee-ea00-7be6-a100-00000000024" + string(rune('0'+index)))
			createV1Database(t, path, id, mustProperties(t, nil))
			raw := openRawSQLite(t, path)
			migrateRawToV3(t, raw)
			var definition string
			if err := raw.QueryRow("SELECT sql FROM sqlite_schema WHERE type='trigger' AND name=?", trigger).Scan(&definition); err != nil {
				t.Fatal(err)
			}
			altered := strings.Replace(definition, "BEGIN", "BEGIN\n    -- deliberately altered", 1)
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
				t.Fatal("v4 migration repaired an altered v3 trigger")
			} else if !errors.Is(err, ErrCorrupt) {
				t.Fatalf("migration error = %v, want ErrCorrupt", err)
			}
			raw = openRawSQLite(t, path)
			defer func() { _ = raw.Close() }()
			var version int
			var after string
			if err := raw.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
				t.Fatal(err)
			}
			if err := raw.QueryRow("SELECT sql FROM sqlite_schema WHERE type='trigger' AND name=?", trigger).Scan(&after); err != nil {
				t.Fatal(err)
			}
			if version != 3 || after != altered {
				t.Fatalf("altered schema was changed: version=%d\n%s", version, after)
			}
		})
	}
}

func TestMigrationPreflightRejectsV1EnvelopeBeforeBackfill(t *testing.T) {
	path := filepath.Join(t.TempDir(), "oversized-v1.db")
	id := domain.EntityID("019945ee-ea00-7be6-a100-000000000260")
	createV1Database(t, path, id, mustProperties(t, nil))
	raw := openRawSQLite(t, path)
	if _, err := raw.Exec("UPDATE node_versions SET body=CAST(zeroblob(?) AS TEXT)", domain.MaxNodeBodyBytes+1); err != nil {
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}

	if opened, err := Open(context.Background(), path); err == nil {
		_ = opened.Close()
		t.Fatal("migration accepted oversized v1 body")
	} else if !errors.Is(err, ErrCorrupt) {
		t.Fatalf("migration error = %v, want ErrCorrupt", err)
	}
	raw = openRawSQLite(t, path)
	defer func() { _ = raw.Close() }()
	var version, v2Objects int
	if err := raw.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		t.Fatal(err)
	}
	if err := raw.QueryRow(`SELECT COUNT(*) FROM sqlite_schema
		WHERE name IN ('node_version_labels', 'node_property_index')`).Scan(&v2Objects); err != nil {
		t.Fatal(err)
	}
	if version != 1 || v2Objects != 0 {
		t.Fatalf("v1 preflight persisted migration state: version=%d v2_objects=%d", version, v2Objects)
	}
}

func TestMigrationRejectsAlteredV2SchemaBeforeTemporalRepair(t *testing.T) {
	path := filepath.Join(t.TempDir(), "altered-v2.db")
	id := domain.EntityID("019945ee-ea00-7be6-a100-000000000261")
	createV2Database(t, path, id, mustProperties(t, nil))
	raw := openRawSQLite(t, path)
	const trigger = "node_property_index_insert"
	var definition string
	if err := raw.QueryRow("SELECT sql FROM sqlite_schema WHERE type='trigger' AND name=?", trigger).Scan(&definition); err != nil {
		t.Fatal(err)
	}
	altered := strings.Replace(definition, "BEGIN", "BEGIN\n    -- deliberately altered v2", 1)
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
		t.Fatal("migration repaired altered v2 schema")
	} else if !errors.Is(err, ErrCorrupt) {
		t.Fatalf("migration error = %v, want ErrCorrupt", err)
	}
	raw = openRawSQLite(t, path)
	defer func() { _ = raw.Close() }()
	var version int
	var after string
	if err := raw.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		t.Fatal(err)
	}
	if err := raw.QueryRow("SELECT sql FROM sqlite_schema WHERE type='trigger' AND name=?", trigger).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if version != 2 || after != altered {
		t.Fatalf("altered v2 schema was changed: version=%d\n%s", version, after)
	}
}

func TestResourceMigrationRecoversAfterProcessCrash(t *testing.T) {
	path := filepath.Join(t.TempDir(), "crash-v3.db")
	id := domain.EntityID("019945ee-ea00-7be6-a100-000000000250")
	createV1Database(t, path, id, mustProperties(t, nil))
	raw := openRawSQLite(t, path)
	migrateRawToV3(t, raw)
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}

	command := exec.Command(os.Args[0], "-test.run=^TestResourceMigrationCrashHelper$")
	command.Env = append(os.Environ(), "SHEETS_RESOURCE_MIGRATION_CRASH="+path)
	if output, err := command.CombinedOutput(); err == nil {
		t.Fatalf("crashing migration helper succeeded: %s", output)
	}

	database, err := Open(context.Background(), path)
	if err != nil {
		t.Fatalf("recover after v4 migration crash: %v", err)
	}
	defer func() { _ = database.Close() }()
	if got := storeSchemaFingerprint(t, database); got != expectedSchemaFingerprint {
		t.Fatalf("recovered fingerprint = %s", got)
	}
}

func TestResourceMigrationCrashHelper(t *testing.T) {
	path := os.Getenv("SHEETS_RESOURCE_MIGRATION_CRASH")
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
	if _, err := database.Exec("BEGIN IMMEDIATE"); err != nil {
		os.Exit(4)
	}
	if _, err := database.Exec(resourceLimitMigration); err != nil {
		os.Exit(5)
	}
	if _, err := database.Exec("PRAGMA user_version=4"); err != nil {
		os.Exit(6)
	}
	os.Exit(41)
}

func storeSchemaFingerprint(t *testing.T, database *Store) string {
	t.Helper()
	connection, err := database.db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = connection.Close() }()
	fingerprint, err := schemaFingerprint(context.Background(), connection)
	if err != nil {
		t.Fatal(err)
	}
	return fingerprint
}

func migrateRawToV3(t *testing.T, database *sql.DB) {
	t.Helper()
	if _, err := database.Exec(invariantMigration); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(`INSERT INTO node_property_index(id, valid_from, valid_to, key, kind, value)
		SELECT versions.id, versions.valid_from, versions.valid_to, properties.key,
		       json_extract(properties.value, '$.k'), CAST(properties.value AS BLOB)
		FROM node_versions AS versions, json_each(CAST(versions.properties AS TEXT), '$.o') AS properties
		WHERE json_extract(properties.value, '$.k') NOT IN ('map', 'list')`); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(temporalMigration); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec("PRAGMA user_version=3"); err != nil {
		t.Fatal(err)
	}
}

func mustJSON(t *testing.T, value encodedValue) []byte {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}
