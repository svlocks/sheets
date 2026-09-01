// Package store persists the temporal property graph in SQLite.
package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/svlocks/sheets/internal/domain"
	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

const schemaVersion = 5

// This is the SHA-256 digest of the ordered, non-internal sqlite_schema rows
// produced by the embedded migrations. It makes routine Open detect altered
// trigger/index definitions without scanning graph data.
const expectedSchemaFingerprint = "3be39b39c67594a6142d3167c7952c2c799d93c3b4d449de833695fb7e52c110"

// Migrations verify the complete preceding schema before applying DDL. Later
// migrations replace tables and triggers, so checking only the final schema
// could otherwise silently repair an altered historical definition.
const (
	expectedV1SchemaFingerprint = "9dea9ce84b3782df2bb8bc4a3ca1e8257bb6532a84b5aae104e65e865056c116"
	expectedV2SchemaFingerprint = "868d00a7ad6e6bd2564e6c20687e1fe24af149ad4bcc6b56be00f302f75ec69c"
	expectedV3SchemaFingerprint = "ce220b74c7edd80aff1942223383af9bc3c43f580fbe1537a46fcbbaf3709b3a"
	expectedV4SchemaFingerprint = "77988fee52f43cdbcb9050836717c5f3287d6524d68f573d462f893b663ea8f0"
)

//go:embed migrations/001_initial.sql
var initialMigration string

//go:embed migrations/002_harden_invariants.sql
var invariantMigration string

//go:embed migrations/003_temporal_values.sql
var temporalMigration string

//go:embed migrations/004_resource_limits.sql
var resourceLimitMigration string

//go:embed migrations/005_derived_resource_limits.sql
var derivedResourceLimitMigration string

// ErrClosed is returned when an operation is attempted on a closed Store.
var ErrClosed = errors.New("store is closed")

// ErrInvalidArgument reports malformed input that is not a graph constraint.
var ErrInvalidArgument = errors.New("invalid store argument")

// ErrBusy means another process held a conflicting SQLite lock beyond the
// configured busy timeout. Callers may retry the entire operation.
var ErrBusy = errors.New("store is busy")

// ErrCorrupt means SQLite bytes, temporal data, derived indexes, or the schema
// do not satisfy sheets's durable format.
var ErrCorrupt = errors.New("store is corrupt or has an invalid schema")

// RevisionMeta is recorded when a Write callback first changes graph state.
// Time is normally left zero, in which case Store's clock supplies it. A
// caller-provided time is useful for imports and deterministic tests.
type RevisionMeta struct {
	Time    time.Time
	Actor   string
	Message string
}

type options struct {
	busyTimeout time.Duration
	maxOpen     int
	clock       func() time.Time
}

// Option customizes Open.
type Option func(*options) error

// WithBusyTimeout sets how long SQLite waits for another process's writer.
func WithBusyTimeout(d time.Duration) Option {
	return func(o *options) error {
		if d < 0 {
			return fmt.Errorf("%w: negative busy timeout", ErrInvalidArgument)
		}
		if d.Milliseconds() > math.MaxInt32 {
			return fmt.Errorf("%w: busy timeout exceeds SQLite's maximum", ErrInvalidArgument)
		}
		o.busyTimeout = d
		return nil
	}
}

// WithMaxOpenConns bounds the connection pool. At least two connections are
// recommended so WAL readers can proceed while a writer is active.
func WithMaxOpenConns(n int) Option {
	return func(o *options) error {
		if n < 1 {
			return fmt.Errorf("%w: max open connections must be positive", ErrInvalidArgument)
		}
		o.maxOpen = n
		return nil
	}
}

// WithClock replaces the clock used for revision timestamps.
func WithClock(clock func() time.Time) Option {
	return func(o *options) error {
		if clock == nil {
			return fmt.Errorf("%w: nil clock", ErrInvalidArgument)
		}
		o.clock = clock
		return nil
	}
}

// Store is a concurrency-safe handle to one SQLite graph database.
type Store struct {
	db          *sql.DB
	clock       func() time.Time
	busyTimeout time.Duration
	closed      atomic.Bool
}

// Open opens path, configures SQLite for cooperating readers and writers, and
// applies all embedded schema migrations. path may be a filesystem path, a
// SQLite file: URI, or :memory:.
func Open(ctx context.Context, path string, opts ...Option) (*Store, error) {
	if ctx == nil {
		return nil, fmt.Errorf("%w: nil context", ErrInvalidArgument)
	}
	if strings.TrimSpace(path) == "" {
		return nil, fmt.Errorf("%w: empty database path", ErrInvalidArgument)
	}
	cfg := options{
		busyTimeout: 5 * time.Second,
		maxOpen:     8,
		clock:       time.Now,
	}
	for _, opt := range opts {
		if opt == nil {
			continue
		}
		if err := opt(&cfg); err != nil {
			return nil, err
		}
	}

	dsn, err := makeDSN(path, cfg.busyTimeout)
	if err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	db.SetMaxOpenConns(cfg.maxOpen)
	db.SetMaxIdleConns(cfg.maxOpen)

	s := &Store{db: db, clock: cfg.clock, busyTimeout: cfg.busyTimeout}
	if err := s.initialize(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

func makeDSN(path string, busy time.Duration) (string, error) {
	if path == ":memory:" {
		path = "file:sheets-" + randomToken() + "?mode=memory&cache=shared"
	} else if !strings.HasPrefix(strings.ToLower(path), "file:") {
		absolute, err := filepath.Abs(path)
		if err != nil {
			return "", fmt.Errorf("resolve database path: %w", err)
		}
		path = (&url.URL{Scheme: "file", Path: filepath.ToSlash(absolute)}).String()
	}
	uri, err := url.Parse(path)
	if err != nil {
		return "", fmt.Errorf("parse database URI: %w", err)
	}
	if err := anchorRelativeFileURI(uri); err != nil {
		return "", err
	}
	ms := busy.Milliseconds()
	query := uri.Query()
	query.Add("_pragma", "foreign_keys(1)")
	query.Add("_pragma", "recursive_triggers(1)")
	query.Add("_pragma", "trusted_schema(0)")
	query.Add("_pragma", "busy_timeout("+strconv.FormatInt(ms, 10)+")")
	uri.RawQuery = query.Encode()
	return uri.String(), nil
}

// anchorRelativeFileURI prevents lazy connections in one sql.DB pool from
// resolving file:graph.db against different working directories after an
// os.Chdir. It deliberately does not claim to pin a pathname against symlink
// replacement; that requires a SQLite VFS capable of fd-relative opens and
// sidecar handling.
func anchorRelativeFileURI(uri *url.URL) error {
	if uri == nil || !strings.EqualFold(uri.Scheme, "file") || uri.Host != "" ||
		strings.EqualFold(uri.Query().Get("mode"), "memory") {
		return nil
	}
	var path string
	switch {
	case uri.Opaque != "":
		var err error
		path, err = url.PathUnescape(uri.Opaque)
		if err != nil {
			return fmt.Errorf("decode database URI path: %w", err)
		}
		if path == ":memory:" {
			return nil
		}
	case uri.Path != "":
		path = filepath.FromSlash(uri.Path)
	default:
		return nil
	}
	if filepath.IsAbs(path) {
		return nil
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return fmt.Errorf("resolve database URI path: %w", err)
	}
	uri.Opaque = ""
	uri.Path = filepath.ToSlash(absolute)
	uri.RawPath = ""
	return nil
}

func randomToken() string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	return hex.EncodeToString(b[:])
}

func (s *Store) initialize(ctx context.Context) (err error) {
	if err := s.db.PingContext(ctx); err != nil {
		return databaseError(ctx, "open sqlite", err)
	}
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("acquire migration connection: %w", err)
	}
	busyDisabled := false
	defer func() {
		if busyDisabled {
			err = errors.Join(err, setBusyTimeout(context.Background(), conn, s.busyTimeout))
		}
		err = errors.Join(err, conn.Close())
	}()
	if err := setBusyTimeout(ctx, conn, 0); err != nil {
		return databaseError(ctx, "disable SQLite busy handler for initialization", err)
	}
	busyDisabled = true
	journal, err := queryStringWithBusyRetry(ctx, conn, s.busyTimeout, "enable WAL", "PRAGMA journal_mode=WAL")
	if err != nil {
		return err
	}
	if !strings.EqualFold(journal, "wal") && !strings.EqualFold(journal, "memory") {
		return fmt.Errorf("enable WAL: SQLite selected journal mode %q", journal)
	}
	if _, err := conn.ExecContext(ctx, "PRAGMA foreign_keys=ON"); err != nil {
		return databaseError(ctx, "enable foreign keys", err)
	}
	if err := beginImmediate(ctx, conn, s.busyTimeout, "begin migration"); err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
		}
	}()

	var version int
	if err := conn.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}
	if version > schemaVersion {
		return fmt.Errorf("database schema version %d is newer than supported version %d", version, schemaVersion)
	}
	migrations := []string{
		initialMigration,
		invariantMigration,
		temporalMigration,
		resourceLimitMigration,
		derivedResourceLimitMigration,
	}
	migrated := version < schemaVersion
	for version < schemaVersion {
		next := version + 1
		if version != 0 {
			if err := validateMigrationSourceSchema(ctx, conn, version); err != nil {
				return fmt.Errorf("%w: before migration %d: %v", ErrCorrupt, next, err)
			}
			if err := validateMigrationSourceValues(ctx, conn, version); err != nil {
				return fmt.Errorf("%w: preflight migration %d: %v", ErrCorrupt, next, err)
			}
		}
		if _, err := conn.ExecContext(ctx, migrations[next-1]); err != nil {
			return fmt.Errorf("apply migration %d: %w", next, err)
		}
		if next == 2 {
			if err := backfillPropertyIndexes(ctx, conn); err != nil {
				return fmt.Errorf("%w: apply migration %d property indexes: %v", ErrCorrupt, next, err)
			}
		}
		if _, err := conn.ExecContext(ctx, "PRAGMA user_version="+strconv.Itoa(next)); err != nil {
			return fmt.Errorf("record migration %d: %w", next, err)
		}
		version = next
	}
	if err := validateSchema(ctx, conn, migrated); err != nil {
		return fmt.Errorf("%w: %w", ErrCorrupt, err)
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return databaseError(ctx, "commit migrations", err)
	}
	committed = true
	return nil
}

func validateMigrationSourceSchema(ctx context.Context, conn *sql.Conn, version int) error {
	expected := map[int]string{
		1: expectedV1SchemaFingerprint,
		2: expectedV2SchemaFingerprint,
		3: expectedV3SchemaFingerprint,
		4: expectedV4SchemaFingerprint,
	}[version]
	if expected == "" {
		return fmt.Errorf("no expected fingerprint for schema version %d", version)
	}
	fingerprint, err := schemaFingerprint(ctx, conn)
	if err != nil {
		return fmt.Errorf("fingerprint schema version %d: %w", version, err)
	}
	if fingerprint != expected {
		return fmt.Errorf("schema version %d has definition fingerprint %s", version, fingerprint)
	}
	return nil
}

func validateMigrationSourceValues(ctx context.Context, conn *sql.Conn, version int) error {
	var err error
	if version == 1 {
		err = validateBaseResourceEnvelopes(ctx, conn)
	} else {
		err = validateResourceEnvelopes(ctx, conn)
	}
	if err != nil {
		return err
	}
	if err := validateDerivedResourceBudgets(ctx, conn, version >= 2); err != nil {
		return err
	}
	if err := validateCanonicalGraphValues(ctx, conn); err != nil {
		return err
	}
	if version >= 2 {
		return validateDerivedIndexes(ctx, conn)
	}
	return nil
}

func validateBaseResourceEnvelopes(ctx context.Context, conn *sql.Conn) error {
	var invalid int
	err := conn.QueryRowContext(ctx, `SELECT
		EXISTS (SELECT 1 FROM revisions
			WHERE octet_length(actor) > ? OR octet_length(message) > ?)
		OR EXISTS (SELECT 1 FROM node_versions
			WHERE octet_length(labels) > ? OR octet_length(properties) > ? OR octet_length(body) > ?)
		OR EXISTS (SELECT 1 FROM edge_versions
			WHERE octet_length(type) > ? OR octet_length(properties) > ?)`,
		domain.MaxRevisionActorBytes, domain.MaxRevisionMessageBytes,
		domain.MaxEncodedLabelsBytes, domain.MaxCanonicalPropertyBytes, domain.MaxNodeBodyBytes,
		domain.MaxRelationshipTypeBytes, domain.MaxCanonicalPropertyBytes,
	).Scan(&invalid)
	if err != nil {
		return fmt.Errorf("inspect durable base value envelopes: %w", err)
	}
	if invalid != 0 {
		return errors.New("existing durable base value exceeds sheets resource limits")
	}
	return validateStoredLabelResources(ctx, conn)
}

// validateResourceEnvelopes uses SQLite-side byte counts so migrating a
// database containing an enormous raw-SQL value never copies that value into
// Go merely to discover that it violates the v4 ceiling. Nested canonical
// values are checked later by validateCanonicalGraphValues once every envelope
// is known to be bounded.
func validateResourceEnvelopes(ctx context.Context, conn *sql.Conn) error {
	var invalid int
	err := conn.QueryRowContext(ctx, `SELECT
		EXISTS (SELECT 1 FROM revisions
			WHERE octet_length(actor) > ? OR octet_length(message) > ?)
		OR EXISTS (SELECT 1 FROM node_versions
			WHERE octet_length(labels) > ? OR octet_length(properties) > ? OR octet_length(body) > ?)
		OR EXISTS (SELECT 1 FROM edge_versions
			WHERE octet_length(type) > ? OR octet_length(properties) > ?)
		OR EXISTS (SELECT 1 FROM node_version_labels
			WHERE octet_length(label) > ?)
		OR EXISTS (SELECT 1 FROM node_property_index
			WHERE octet_length(key) > ? OR octet_length(value) > ?)
		OR EXISTS (SELECT 1 FROM edge_property_index
			WHERE octet_length(key) > ? OR octet_length(value) > ?)`,
		domain.MaxRevisionActorBytes, domain.MaxRevisionMessageBytes,
		domain.MaxEncodedLabelsBytes, domain.MaxCanonicalPropertyBytes, domain.MaxNodeBodyBytes,
		domain.MaxRelationshipTypeBytes, domain.MaxCanonicalPropertyBytes,
		domain.MaxLabelBytes,
		domain.MaxPropertyKeyBytes, domain.MaxDerivedPropertyBytesPerVersion,
		domain.MaxPropertyKeyBytes, domain.MaxDerivedPropertyBytesPerVersion,
	).Scan(&invalid)
	if err != nil {
		return fmt.Errorf("inspect durable value envelopes: %w", err)
	}
	if invalid != 0 {
		return errors.New("existing durable value exceeds sheets resource limits")
	}
	return validateStoredLabelResources(ctx, conn)
}

func validateStoredLabelResources(ctx context.Context, conn *sql.Conn) error {
	var invalid int
	err := conn.QueryRowContext(ctx, `SELECT EXISTS (
		SELECT 1
		FROM node_versions AS versions
		WHERE CASE
			WHEN json_valid(CAST(versions.labels AS TEXT)) <> 1 THEN 0
			WHEN json_type(CAST(versions.labels AS TEXT)) <> 'array' THEN 0
			WHEN json_array_length(CAST(versions.labels AS TEXT)) > ? THEN 1
			WHEN COALESCE((
				SELECT SUM(octet_length(label.value))
				FROM json_each(CAST(versions.labels AS TEXT)) AS label
			), 0) > ? THEN 1
			ELSE EXISTS (
				SELECT 1 FROM json_each(CAST(versions.labels AS TEXT)) AS label
				WHERE octet_length(label.value) > ? OR instr(label.value, char(0)) <> 0
			)
		END
	)`, domain.MaxLabelsPerNode, domain.MaxDerivedLabelBytesPerVersion, domain.MaxLabelBytes).Scan(&invalid)
	if err != nil {
		return fmt.Errorf("inspect stored label resources: %w", err)
	}
	if invalid != 0 {
		return errors.New("existing labels exceed sheets resource limits")
	}
	return nil
}

// validateDerivedResourceBudgets bounds the work and B-tree amplification of
// the migration that follows. Source envelopes are authoritative; historical
// derived tables are checked too before a table-rebuild migration copies them.
// All byte/count work stays inside SQLite, after the cheap envelope preflight,
// so oversized legacy values are never materialized by the Go driver first.
func validateDerivedResourceBudgets(ctx context.Context, conn *sql.Conn, hasDerivedTables bool) error {
	var invalid int
	err := conn.QueryRowContext(ctx, `SELECT
		EXISTS (
			SELECT 1 FROM node_versions AS version
			WHERE CASE
				WHEN json_valid(CAST(version.labels AS TEXT)) <> 1 THEN 0
				WHEN json_type(CAST(version.labels AS TEXT)) <> 'array' THEN 0
				ELSE json_array_length(CAST(version.labels AS TEXT)) > ?
				  OR COALESCE((
					SELECT SUM(octet_length(label.value))
					FROM json_each(CAST(version.labels AS TEXT)) AS label
				  ), 0) > ?
			END
		)
		OR EXISTS (
			SELECT 1 FROM node_versions AS version
			WHERE CASE
				WHEN json_valid(CAST(version.properties AS TEXT)) <> 1 THEN 0
				ELSE EXISTS (
					SELECT 1 FROM (
						SELECT COUNT(*) AS row_count,
						       COALESCE(SUM(
							   octet_length(property.key)
							   + octet_length(CAST(property.value AS BLOB))
						       ), 0) AS payload_bytes
						FROM json_each(CAST(version.properties AS TEXT), '$.o') AS property
						WHERE json_extract(property.value, '$.k') NOT IN ('map', 'list')
					)
					WHERE row_count > ? OR payload_bytes > ?
				)
			END
		)
		OR EXISTS (
			SELECT 1 FROM edge_versions AS version
			WHERE CASE
				WHEN json_valid(CAST(version.properties AS TEXT)) <> 1 THEN 0
				ELSE EXISTS (
					SELECT 1 FROM (
						SELECT COUNT(*) AS row_count,
						       COALESCE(SUM(
							   octet_length(property.key)
							   + octet_length(CAST(property.value AS BLOB))
						       ), 0) AS payload_bytes
						FROM json_each(CAST(version.properties AS TEXT), '$.o') AS property
						WHERE json_extract(property.value, '$.k') NOT IN ('map', 'list')
					)
					WHERE row_count > ? OR payload_bytes > ?
				)
			END
		)`,
		domain.MaxLabelsPerNode, domain.MaxDerivedLabelBytesPerVersion,
		domain.MaxIndexedPropertiesPerVersion, domain.MaxDerivedPropertyBytesPerVersion,
		domain.MaxIndexedPropertiesPerVersion, domain.MaxDerivedPropertyBytesPerVersion,
	).Scan(&invalid)
	if err != nil {
		return fmt.Errorf("inspect canonical derived-value budgets: %w", err)
	}
	if invalid != 0 {
		return errors.New("existing canonical value exceeds sheets derived-index budgets")
	}
	if !hasDerivedTables {
		return nil
	}
	err = conn.QueryRowContext(ctx, `SELECT
		EXISTS (
			SELECT 1 FROM node_version_labels
			GROUP BY id, valid_from
			HAVING COUNT(*) > ? OR COALESCE(SUM(octet_length(label)), 0) > ?
		)
		OR EXISTS (
			SELECT 1 FROM node_property_index
			GROUP BY id, valid_from
			HAVING COUNT(*) > ? OR COALESCE(SUM(
				octet_length(key) + octet_length(CAST(value AS BLOB))
			), 0) > ?
		)
		OR EXISTS (
			SELECT 1 FROM edge_property_index
			GROUP BY id, valid_from
			HAVING COUNT(*) > ? OR COALESCE(SUM(
				octet_length(key) + octet_length(CAST(value AS BLOB))
			), 0) > ?
		)`,
		domain.MaxLabelsPerNode, domain.MaxDerivedLabelBytesPerVersion,
		domain.MaxIndexedPropertiesPerVersion, domain.MaxDerivedPropertyBytesPerVersion,
		domain.MaxIndexedPropertiesPerVersion, domain.MaxDerivedPropertyBytesPerVersion,
	).Scan(&invalid)
	if err != nil {
		return fmt.Errorf("inspect historical derived-table budgets: %w", err)
	}
	if invalid != 0 {
		return errors.New("existing derived table exceeds sheets resource budgets")
	}
	return nil
}

func validateSchema(ctx context.Context, conn *sql.Conn, deep bool) error {
	checks := []string{
		"SELECT revision, committed_ns, actor, message, sealed FROM revisions LIMIT 0",
		"SELECT id, created_revision FROM nodes LIMIT 0",
		"SELECT id, valid_from, valid_to, labels, properties, body FROM node_versions LIMIT 0",
		"SELECT id, valid_from, label FROM node_version_labels LIMIT 0",
		"SELECT id, valid_from, valid_to, key, kind, value FROM node_property_index LIMIT 0",
		"SELECT id, created_revision FROM edges LIMIT 0",
		"SELECT id, valid_from, valid_to, from_id, type, to_id, position, properties FROM edge_versions LIMIT 0",
		"SELECT id, valid_from, valid_to, key, kind, value FROM edge_property_index LIMIT 0",
	}
	expectedObjects := map[string]string{
		"revisions":                                  "table",
		"nodes":                                      "table",
		"node_versions":                              "table",
		"node_version_labels":                        "table",
		"node_property_index":                        "table",
		"edges":                                      "table",
		"edge_versions":                              "table",
		"edge_property_index":                        "table",
		"one_current_node_version":                   "index",
		"one_unsealed_revision":                      "index",
		"one_current_edge_version":                   "index",
		"one_current_child_parent":                   "index",
		"node_version_labels_by_label":               "index",
		"node_versions_by_close":                     "index",
		"node_property_lookup":                       "index",
		"edge_property_lookup":                       "index",
		"current_edges_from_page":                    "index",
		"current_edges_to_page":                      "index",
		"current_edges_type_page":                    "index",
		"edge_versions_from_history":                 "index",
		"edge_versions_to_history":                   "index",
		"edge_versions_type_history":                 "index",
		"edge_versions_by_close":                     "index",
		"revisions_validate_insert":                  "trigger",
		"revisions_validate_update":                  "trigger",
		"revisions_validate_delete":                  "trigger",
		"nodes_validate_insert":                      "trigger",
		"nodes_validate_update":                      "trigger",
		"nodes_validate_delete":                      "trigger",
		"edges_validate_insert":                      "trigger",
		"edges_validate_update":                      "trigger",
		"edges_validate_delete":                      "trigger",
		"node_versions_validate_insert":              "trigger",
		"node_versions_validate_update":              "trigger",
		"node_versions_validate_delete":              "trigger",
		"node_version_labels_insert":                 "trigger",
		"node_version_labels_update":                 "trigger",
		"edge_versions_validate_insert":              "trigger",
		"edge_versions_validate_update":              "trigger",
		"edge_versions_validate_update_graph":        "trigger",
		"edge_versions_validate_delete":              "trigger",
		"node_property_index_close":                  "trigger",
		"node_property_index_insert":                 "trigger",
		"node_property_index_update":                 "trigger",
		"edge_property_index_close":                  "trigger",
		"edge_property_index_insert":                 "trigger",
		"edge_property_index_update":                 "trigger",
		"revisions_resource_limits_insert":           "trigger",
		"node_versions_resource_limits_insert":       "trigger",
		"node_versions_resource_limits_update":       "trigger",
		"edge_versions_resource_limits_insert":       "trigger",
		"edge_versions_resource_limits_update":       "trigger",
		"node_version_labels_resource_limits_insert": "trigger",
		"node_version_labels_resource_limits_update": "trigger",
		"node_property_index_resource_limits_insert": "trigger",
		"node_property_index_resource_limits_update": "trigger",
		"edge_property_index_resource_limits_insert": "trigger",
		"edge_property_index_resource_limits_update": "trigger",
		"node_versions_derived_limits_insert":        "trigger",
		"node_versions_derived_limits_update":        "trigger",
		"edge_versions_derived_limits_insert":        "trigger",
		"edge_versions_derived_limits_update":        "trigger",
		"node_version_labels_derived_limits_insert":  "trigger",
		"node_version_labels_derived_limits_update":  "trigger",
		"node_property_index_derived_limits_insert":  "trigger",
		"node_property_index_derived_limits_update":  "trigger",
		"edge_property_index_derived_limits_insert":  "trigger",
		"edge_property_index_derived_limits_update":  "trigger",
	}
	objectArgs := make([]any, 0, len(expectedObjects))
	for name := range expectedObjects {
		objectArgs = append(objectArgs, name)
	}
	objectRows, err := conn.QueryContext(ctx,
		"SELECT name, type FROM sqlite_schema WHERE name IN ("+
			strings.TrimSuffix(strings.Repeat("?,", len(objectArgs)), ",")+")", objectArgs...)
	if err != nil {
		return fmt.Errorf("validate database schema objects: %w", err)
	}
	foundObjects := make(map[string]string, len(expectedObjects))
	for objectRows.Next() {
		var name, kind string
		if err := objectRows.Scan(&name, &kind); err != nil {
			return errors.Join(fmt.Errorf("validate database schema objects: %w", err), objectRows.Close())
		}
		foundObjects[name] = kind
	}
	if err := objectRows.Err(); err != nil {
		return errors.Join(fmt.Errorf("validate database schema objects: %w", err), objectRows.Close())
	}
	if err := objectRows.Close(); err != nil {
		return fmt.Errorf("validate database schema objects: %w", err)
	}
	for name, kind := range expectedObjects {
		if foundObjects[name] != kind {
			return fmt.Errorf("validate database schema: missing %s %s", kind, name)
		}
	}
	for _, query := range checks {
		rows, err := conn.QueryContext(ctx, query)
		if err != nil {
			return fmt.Errorf("validate database schema: %w", err)
		}
		if err := rows.Close(); err != nil {
			return fmt.Errorf("validate database schema: %w", err)
		}
	}
	fingerprint, err := schemaFingerprint(ctx, conn)
	if err != nil {
		return err
	}
	if fingerprint != expectedSchemaFingerprint {
		return fmt.Errorf("validate database schema: definition fingerprint %s does not match sheets schema", fingerprint)
	}
	var unsealed int
	if err := conn.QueryRowContext(ctx,
		"SELECT EXISTS (SELECT 1 FROM revisions INDEXED BY one_unsealed_revision WHERE sealed = 0)").Scan(&unsealed); err != nil {
		return fmt.Errorf("validate revision seals: %w", err)
	}
	if unsealed != 0 {
		return errors.New("validate revision seals: database contains an unsealed revision")
	}
	if deep {
		return validateDataIntegrity(ctx, conn)
	}
	return nil
}

func schemaFingerprint(ctx context.Context, conn *sql.Conn) (string, error) {
	type schemaObject struct {
		Type string `json:"type"`
		Name string `json:"name"`
		SQL  string `json:"sql"`
	}
	rows, err := conn.QueryContext(ctx, `SELECT type, name, COALESCE(sql, '')
		FROM sqlite_schema
		WHERE name NOT LIKE 'sqlite_%'
		ORDER BY type, name`)
	if err != nil {
		return "", fmt.Errorf("fingerprint database schema: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var objects []schemaObject
	for rows.Next() {
		var object schemaObject
		if err := rows.Scan(&object.Type, &object.Name, &object.SQL); err != nil {
			return "", fmt.Errorf("fingerprint database schema: %w", err)
		}
		objects = append(objects, object)
	}
	if err := rows.Err(); err != nil {
		return "", fmt.Errorf("fingerprint database schema: %w", err)
	}
	if err := rows.Close(); err != nil {
		return "", fmt.Errorf("fingerprint database schema: %w", err)
	}
	encoded, err := json.Marshal(objects)
	if err != nil {
		return "", fmt.Errorf("fingerprint database schema: %w", err)
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:]), nil
}

func validateDataIntegrity(ctx context.Context, conn *sql.Conn) error {
	var invalid int
	if err := conn.QueryRowContext(ctx, `
		WITH ordered AS (
			SELECT revision, committed_ns, sealed,
			       LAG(revision, 1, 0) OVER (ORDER BY revision) AS previous_revision,
			       LAG(committed_ns) OVER (ORDER BY revision) AS previous_time
			FROM revisions
		)
		SELECT EXISTS (
			SELECT 1 FROM ordered
			WHERE sealed <> 1
			   OR revision <> previous_revision + 1
			   OR (previous_time IS NOT NULL AND committed_ns <= previous_time)
		)`).Scan(&invalid); err != nil {
		return fmt.Errorf("validate revision history: %w", err)
	}
	if invalid != 0 {
		return errors.New("validate revision history: revisions are unsealed, non-contiguous, or non-monotonic")
	}
	logicalChecks := []struct {
		name  string
		query string
	}{
		{
			name: "entity version history",
			query: `WITH
				node_ordered AS (
					SELECT versions.id, versions.valid_from,
					       ROW_NUMBER() OVER (PARTITION BY versions.id ORDER BY versions.valid_from) AS ordinal,
					       LAG(versions.valid_to) OVER (PARTITION BY versions.id ORDER BY versions.valid_from) AS previous_to
					FROM node_versions AS versions
				),
				edge_ordered AS (
					SELECT versions.id, versions.valid_from,
					       ROW_NUMBER() OVER (PARTITION BY versions.id ORDER BY versions.valid_from) AS ordinal,
					       LAG(versions.valid_to) OVER (PARTITION BY versions.id ORDER BY versions.valid_from) AS previous_to
					FROM edge_versions AS versions
				)
			SELECT
				EXISTS (SELECT 1 FROM nodes WHERE NOT EXISTS (SELECT 1 FROM node_versions WHERE node_versions.id = nodes.id))
				OR EXISTS (SELECT 1 FROM edges WHERE NOT EXISTS (SELECT 1 FROM edge_versions WHERE edge_versions.id = edges.id))
				OR EXISTS (
					SELECT 1 FROM node_ordered
					JOIN nodes USING (id)
					WHERE (ordinal = 1 AND valid_from <> nodes.created_revision)
					   OR (ordinal > 1 AND valid_from IS NOT previous_to)
				)
				OR EXISTS (
					SELECT 1 FROM edge_ordered
					JOIN edges USING (id)
					WHERE (ordinal = 1 AND valid_from <> edges.created_revision)
					   OR (ordinal > 1 AND valid_from IS NOT previous_to)
				)`,
		},
		{
			name: "edge endpoint lifetime",
			query: `SELECT EXISTS (
				SELECT 1 FROM edge_versions AS edge
				WHERE NOT EXISTS (
					SELECT 1 FROM node_versions AS source
					WHERE source.id = edge.from_id
					  AND source.valid_from <= edge.valid_from
					  AND (source.valid_to IS NULL OR source.valid_to > edge.valid_from)
				)
				OR NOT EXISTS (
					SELECT 1 FROM node_versions AS target
					WHERE target.id = edge.to_id
					  AND target.valid_from <= edge.valid_from
					  AND (target.valid_to IS NULL OR target.valid_to > edge.valid_from)
				)
				OR (edge.valid_to IS NULL AND (
					NOT EXISTS (SELECT 1 FROM node_versions WHERE id = edge.from_id AND valid_to IS NULL)
					OR NOT EXISTS (SELECT 1 FROM node_versions WHERE id = edge.to_id AND valid_to IS NULL)
				))
				OR (edge.valid_to IS NOT NULL AND (
					NOT EXISTS (
						SELECT 1 FROM node_versions AS source
						WHERE source.id = edge.from_id
						  AND source.valid_from < edge.valid_to
						  AND (source.valid_to IS NULL OR source.valid_to >= edge.valid_to)
					)
					OR NOT EXISTS (
						SELECT 1 FROM node_versions AS target
						WHERE target.id = edge.to_id
						  AND target.valid_from < edge.valid_to
						  AND (target.valid_to IS NULL OR target.valid_to >= edge.valid_to)
					)
				))
			)`,
		},
		{
			name: "historical CHILD parent uniqueness",
			query: `SELECT EXISTS (
				SELECT 1
				FROM edge_versions AS first
				JOIN edge_versions AS second
				  ON second.to_id = first.to_id
				 AND second.id > first.id
				WHERE first.type = 'CHILD' AND second.type = 'CHILD'
				  AND first.valid_from < COALESCE(second.valid_to, 9223372036854775807)
				  AND second.valid_from < COALESCE(first.valid_to, 9223372036854775807)
			)`,
		},
		{
			name: "historical CHILD acyclicity",
			query: `WITH RECURSIVE child_paths(start_id, end_id, low_revision, high_revision) AS (
				SELECT from_id, to_id, valid_from, COALESCE(valid_to - 1, 9223372036854775807)
				FROM edge_versions
				WHERE type = 'CHILD'
				UNION
				SELECT paths.start_id, next.to_id,
				       MAX(paths.low_revision, next.valid_from),
				       MIN(paths.high_revision, COALESCE(next.valid_to - 1, 9223372036854775807))
				FROM child_paths AS paths
				JOIN edge_versions AS next ON next.from_id = paths.end_id
				WHERE next.type = 'CHILD'
				  AND MAX(paths.low_revision, next.valid_from)
				      <= MIN(paths.high_revision, COALESCE(next.valid_to - 1, 9223372036854775807))
			)
			SELECT EXISTS (SELECT 1 FROM child_paths WHERE start_id = end_id)`,
		},
	}
	for _, check := range logicalChecks {
		if err := conn.QueryRowContext(ctx, check.query).Scan(&invalid); err != nil {
			return fmt.Errorf("validate %s: %w", check.name, err)
		}
		if invalid != 0 {
			return fmt.Errorf("validate %s: invariant violation", check.name)
		}
	}
	if err := validateCanonicalGraphValues(ctx, conn); err != nil {
		return err
	}
	var quickCheck string
	if err := conn.QueryRowContext(ctx, "PRAGMA quick_check(1)").Scan(&quickCheck); err != nil {
		return fmt.Errorf("validate database integrity: %w", err)
	}
	if quickCheck != "ok" {
		return fmt.Errorf("validate database integrity: %s", quickCheck)
	}
	rows, err := conn.QueryContext(ctx, "PRAGMA foreign_key_check")
	if err != nil {
		return fmt.Errorf("validate database foreign keys: %w", err)
	}
	defer func() { _ = rows.Close() }()
	if rows.Next() {
		return errors.New("validate database foreign keys: existing violation")
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("validate database foreign keys: %w", err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("validate database foreign keys: %w", err)
	}
	return validateDerivedIndexes(ctx, conn)
}

func validateDerivedIndexes(ctx context.Context, conn *sql.Conn) error {
	var derivedInvalid int
	if err := conn.QueryRowContext(ctx, `
		WITH
		expected_labels(id, valid_from, label) AS (
			SELECT versions.id, versions.valid_from, labels.value
			FROM node_versions AS versions,
			     json_each(CAST(versions.labels AS TEXT)) AS labels
			WHERE json_type(CAST(versions.labels AS TEXT)) = 'array'
		),
		expected_node_properties(id, valid_from, valid_to, key, kind, value) AS (
			SELECT versions.id, versions.valid_from, versions.valid_to,
			       properties.key,
			       json_extract(properties.value, '$.k'),
			       CAST(properties.value AS BLOB)
			FROM node_versions AS versions,
			     json_each(CAST(versions.properties AS TEXT), '$.o') AS properties
			WHERE json_extract(properties.value, '$.k') NOT IN ('map', 'list')
		),
		expected_edge_properties(id, valid_from, valid_to, key, kind, value) AS (
			SELECT versions.id, versions.valid_from, versions.valid_to,
			       properties.key,
			       json_extract(properties.value, '$.k'),
			       CAST(properties.value AS BLOB)
			FROM edge_versions AS versions,
			     json_each(CAST(versions.properties AS TEXT), '$.o') AS properties
			WHERE json_extract(properties.value, '$.k') NOT IN ('map', 'list')
		)
		SELECT
			EXISTS (
				SELECT 1 FROM expected_labels AS expected
				LEFT JOIN node_version_labels AS actual USING (id, valid_from, label)
				WHERE actual.id IS NULL
			)
			OR EXISTS (
				SELECT 1 FROM node_version_labels AS actual
				LEFT JOIN expected_labels AS expected USING (id, valid_from, label)
				WHERE expected.id IS NULL
			)
			OR EXISTS (
				SELECT 1 FROM expected_node_properties AS expected
				LEFT JOIN node_property_index AS actual USING (id, valid_from, key)
				WHERE actual.id IS NULL OR actual.kind IS NOT expected.kind
				   OR actual.value IS NOT expected.value OR actual.valid_to IS NOT expected.valid_to
			)
			OR EXISTS (
				SELECT 1 FROM node_property_index AS actual
				LEFT JOIN expected_node_properties AS expected USING (id, valid_from, key)
				WHERE expected.id IS NULL
			)
			OR EXISTS (
				SELECT 1 FROM expected_edge_properties AS expected
				LEFT JOIN edge_property_index AS actual USING (id, valid_from, key)
				WHERE actual.id IS NULL OR actual.kind IS NOT expected.kind
				   OR actual.value IS NOT expected.value OR actual.valid_to IS NOT expected.valid_to
			)
			OR EXISTS (
				SELECT 1 FROM edge_property_index AS actual
				LEFT JOIN expected_edge_properties AS expected USING (id, valid_from, key)
				WHERE expected.id IS NULL
			)`).Scan(&derivedInvalid); err != nil {
		return fmt.Errorf("validate derived graph indexes: %w", err)
	}
	if derivedInvalid != 0 {
		return errors.New("validate derived graph indexes: labels or scalar properties do not match canonical versions")
	}
	return nil
}

func validateCanonicalGraphValues(ctx context.Context, conn *sql.Conn) error {
	identityTables := []string{"nodes", "edges"}
	for _, table := range identityTables {
		rows, err := conn.QueryContext(ctx, "SELECT id FROM "+table+" ORDER BY id")
		if err != nil {
			return fmt.Errorf("validate %s identities: %w", table, err)
		}
		for rows.Next() {
			var rawID string
			if err := rows.Scan(&rawID); err != nil {
				return errors.Join(fmt.Errorf("validate %s identities: %w", table, err), rows.Close())
			}
			if err := validateID(domain.EntityID(rawID)); err != nil {
				return errors.Join(fmt.Errorf("validate %s identity %q: %w", table, rawID, err), rows.Close())
			}
		}
		if err := rows.Err(); err != nil {
			return errors.Join(fmt.Errorf("validate %s identities: %w", table, err), rows.Close())
		}
		if err := rows.Close(); err != nil {
			return fmt.Errorf("validate %s identities: %w", table, err)
		}
	}

	nodeRows, err := conn.QueryContext(ctx, "SELECT id, labels, properties, body FROM node_versions ORDER BY id, valid_from")
	if err != nil {
		return fmt.Errorf("validate node values: %w", err)
	}
	for nodeRows.Next() {
		var id, body string
		var labels, properties []byte
		if err := nodeRows.Scan(&id, &labels, &properties, &body); err != nil {
			return errors.Join(fmt.Errorf("validate node values: %w", err), nodeRows.Close())
		}
		if _, err := decodeLabels(labels); err != nil {
			return errors.Join(fmt.Errorf("validate node %s labels: %w", id, err), nodeRows.Close())
		}
		if _, err := decodeProperties(properties); err != nil {
			return errors.Join(fmt.Errorf("validate node %s properties: %w", id, err), nodeRows.Close())
		}
		if err := validateNodeBody(body); err != nil {
			return errors.Join(fmt.Errorf("validate node %s body: %w", id, err), nodeRows.Close())
		}
	}
	if err := nodeRows.Err(); err != nil {
		return errors.Join(fmt.Errorf("validate node values: %w", err), nodeRows.Close())
	}
	if err := nodeRows.Close(); err != nil {
		return fmt.Errorf("validate node values: %w", err)
	}

	edgeRows, err := conn.QueryContext(ctx, "SELECT id, type, position, properties FROM edge_versions ORDER BY id, valid_from")
	if err != nil {
		return fmt.Errorf("validate edge values: %w", err)
	}
	for edgeRows.Next() {
		var id, edgeType string
		var position sql.NullInt64
		var properties []byte
		if err := edgeRows.Scan(&id, &edgeType, &position, &properties); err != nil {
			return errors.Join(fmt.Errorf("validate edge values: %w", err), edgeRows.Close())
		}
		if err := validateRelationshipType(edgeType); err != nil {
			return errors.Join(fmt.Errorf("validate edge %s: %w", id, err), edgeRows.Close())
		}
		if edgeType != "CHILD" && position.Valid {
			return errors.Join(fmt.Errorf("validate edge %s: invalid position for relationship type", id), edgeRows.Close())
		}
		if _, err := decodeProperties(properties); err != nil {
			return errors.Join(fmt.Errorf("validate edge %s properties: %w", id, err), edgeRows.Close())
		}
	}
	if err := edgeRows.Err(); err != nil {
		return errors.Join(fmt.Errorf("validate edge values: %w", err), edgeRows.Close())
	}
	if err := edgeRows.Close(); err != nil {
		return fmt.Errorf("validate edge values: %w", err)
	}

	revisionRows, err := conn.QueryContext(ctx, "SELECT revision, actor, message FROM revisions ORDER BY revision")
	if err != nil {
		return fmt.Errorf("validate revision metadata: %w", err)
	}
	for revisionRows.Next() {
		var revision int64
		var actor, message string
		if err := revisionRows.Scan(&revision, &actor, &message); err != nil {
			return errors.Join(fmt.Errorf("validate revision metadata: %w", err), revisionRows.Close())
		}
		if err := validateRevisionMeta(RevisionMeta{Actor: actor, Message: message}); err != nil {
			return errors.Join(fmt.Errorf("validate revision %d metadata: %w", revision, err), revisionRows.Close())
		}
	}
	if err := revisionRows.Err(); err != nil {
		return errors.Join(fmt.Errorf("validate revision metadata: %w", err), revisionRows.Close())
	}
	return revisionRows.Close()
}

// CheckIntegrity performs an explicit full SQLite, foreign-key, schema, and
// revision-history check against one consistent read transaction. Open does
// this automatically only while creating or migrating a database, keeping
// routine one-shot startup independent of database size.
func (s *Store) CheckIntegrity(ctx context.Context) (err error) {
	if ctx == nil {
		return fmt.Errorf("%w: nil context", ErrInvalidArgument)
	}
	if err := s.checkOpen(); err != nil {
		return err
	}
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return databaseError(ctx, "acquire integrity-check connection", err)
	}
	defer func() { err = errors.Join(err, conn.Close()) }()
	if _, err := conn.ExecContext(ctx, "BEGIN"); err != nil {
		return databaseError(ctx, "begin integrity check", err)
	}
	defer func() { _, _ = conn.ExecContext(context.Background(), "ROLLBACK") }()
	return validateSchema(ctx, conn, true)
}

// Close closes the connection pool. It is safe to call more than once.
func (s *Store) Close() error {
	if s == nil || !s.closed.CompareAndSwap(false, true) {
		return nil
	}
	return s.db.Close()
}

func (s *Store) checkOpen() error {
	if s == nil || s.closed.Load() {
		return ErrClosed
	}
	return nil
}

func databaseError(ctx context.Context, operation string, err error) error {
	if err == nil {
		return nil
	}
	if ctx != nil && ctx.Err() != nil {
		return fmt.Errorf("%s: %w", operation, ctx.Err())
	}
	if isSQLiteBusy(err) {
		return fmt.Errorf("%s: %w: %v", operation, ErrBusy, err)
	}
	if isSQLiteCorrupt(err) {
		return fmt.Errorf("%s: %w: %v", operation, ErrCorrupt, err)
	}
	return fmt.Errorf("%s: %w", operation, err)
}

func queryStringWithBusyRetry(ctx context.Context, conn *sql.Conn, timeout time.Duration, operation, query string) (string, error) {
	deadline := time.Now().Add(timeout)
	backoff := time.Millisecond
	for {
		var value string
		err := conn.QueryRowContext(ctx, query).Scan(&value)
		if err == nil {
			return value, nil
		}
		if ctx.Err() != nil || !isSQLiteBusy(err) {
			return "", databaseError(ctx, operation, err)
		}
		remaining := time.Until(deadline)
		if timeout == 0 || remaining <= 0 {
			return "", fmt.Errorf("%s: %w: %v", operation, ErrBusy, err)
		}
		wait := min(backoff, remaining)
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return "", fmt.Errorf("%s: %w", operation, ctx.Err())
		case <-timer.C:
		}
		backoff = min(backoff*2, 25*time.Millisecond)
	}
}

func sqliteBaseCode(err error) (int, bool) {
	var sqliteErr *sqlite.Error
	if !errors.As(err, &sqliteErr) {
		return 0, false
	}
	return sqliteErr.Code() & 0xff, true
}

func isSQLiteBusy(err error) bool {
	code, ok := sqliteBaseCode(err)
	return ok && (code == sqlite3.SQLITE_BUSY || code == sqlite3.SQLITE_LOCKED)
}

func isSQLiteConstraint(err error) bool {
	code, ok := sqliteBaseCode(err)
	return ok && code == sqlite3.SQLITE_CONSTRAINT
}

func isSQLiteCorrupt(err error) bool {
	code, ok := sqliteBaseCode(err)
	return ok && (code == sqlite3.SQLITE_CORRUPT || code == sqlite3.SQLITE_NOTADB)
}
