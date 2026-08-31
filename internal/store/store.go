// Package store persists the temporal property graph in SQLite.
package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	_ "embed"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	_ "modernc.org/sqlite"
)

const schemaVersion = 1

//go:embed migrations/001_initial.sql
var initialMigration string

// ErrClosed is returned when an operation is attempted on a closed Store.
var ErrClosed = errors.New("store is closed")

// ErrInvalidArgument reports malformed input that is not a graph constraint.
var ErrInvalidArgument = errors.New("invalid store argument")

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
	db     *sql.DB
	clock  func() time.Time
	closed atomic.Bool
}

// Open opens path, configures SQLite for cooperating readers and writers, and
// applies all embedded schema migrations. path may be a filesystem path, a
// SQLite file: URI, or :memory:.
func Open(ctx context.Context, path string, opts ...Option) (*Store, error) {
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

	dsn := makeDSN(path, cfg.busyTimeout)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	db.SetMaxOpenConns(cfg.maxOpen)
	db.SetMaxIdleConns(cfg.maxOpen)

	s := &Store{db: db, clock: cfg.clock}
	if err := s.initialize(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

func makeDSN(path string, busy time.Duration) string {
	if path == ":memory:" {
		path = "file:sheets-" + randomToken() + "?mode=memory&cache=shared"
	}
	separator := "?"
	if strings.Contains(path, "?") {
		separator = "&"
	}
	ms := busy.Milliseconds()
	pragmas := url.Values{}
	pragmas.Add("_pragma", "foreign_keys(1)")
	pragmas.Add("_pragma", "busy_timeout("+strconv.FormatInt(ms, 10)+")")
	return path + separator + pragmas.Encode()
}

func randomToken() string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	return hex.EncodeToString(b[:])
}

func (s *Store) initialize(ctx context.Context) error {
	if err := s.db.PingContext(ctx); err != nil {
		return fmt.Errorf("open sqlite: %w", err)
	}
	var journal string
	if err := s.db.QueryRowContext(ctx, "PRAGMA journal_mode=WAL").Scan(&journal); err != nil {
		return fmt.Errorf("enable WAL: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, "PRAGMA foreign_keys=ON"); err != nil {
		return fmt.Errorf("enable foreign keys: %w", err)
	}

	conn, err := s.db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("acquire migration connection: %w", err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return fmt.Errorf("begin migration: %w", err)
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
	if version < 1 {
		if _, err := conn.ExecContext(ctx, initialMigration); err != nil {
			return fmt.Errorf("apply migration 1: %w", err)
		}
		if _, err := conn.ExecContext(ctx, "PRAGMA user_version=1"); err != nil {
			return fmt.Errorf("record migration 1: %w", err)
		}
	}
	if err := validateSchema(ctx, conn); err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return fmt.Errorf("commit migrations: %w", err)
	}
	committed = true
	return nil
}

func validateSchema(ctx context.Context, conn *sql.Conn) error {
	checks := []string{
		"SELECT revision, committed_ns, actor, message FROM revisions LIMIT 0",
		"SELECT id, created_revision FROM nodes LIMIT 0",
		"SELECT id, valid_from, valid_to, labels, properties, body FROM node_versions LIMIT 0",
		"SELECT id, created_revision FROM edges LIMIT 0",
		"SELECT id, valid_from, valid_to, from_id, type, to_id, position, properties FROM edge_versions LIMIT 0",
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
	var quickCheck string
	if err := conn.QueryRowContext(ctx, "PRAGMA quick_check").Scan(&quickCheck); err != nil {
		return fmt.Errorf("validate database integrity: %w", err)
	}
	if quickCheck != "ok" {
		return fmt.Errorf("validate database integrity: %s", quickCheck)
	}
	rows, err := conn.QueryContext(ctx, "PRAGMA foreign_key_check")
	if err != nil {
		return fmt.Errorf("validate database foreign keys: %w", err)
	}
	defer rows.Close()
	if rows.Next() {
		return errors.New("validate database foreign keys: existing violation")
	}
	return rows.Err()
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
