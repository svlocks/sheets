# Architecture

## Product contract

Sheets is a local-first, daemonless temporal property graph. SQLite is the
single source of truth. Markdown bodies are values in the graph rather than
mutable filesystem mirrors. Every successful batch with an effective mutation
creates exactly one monotonically increasing revision; a successful no-op
creates none. Every mutation in the batch either commits at that revision or
none of it commits.

The same application services power the CLI and TUI. The TUI has no privileged
operations, and every mutation it performs can also be expressed in Cypher or
through a CLI convenience command.

## Graph model

A node contains a stable UUIDv7 identifier, zero or more labels, a property
map, and a Markdown body. An edge contains a stable UUIDv7 identifier, source
and target node identifiers, a type, an optional integer position, and a
property map.

`CHILD` is the only structural edge type. It has two additional invariants:

1. A node has at most one incoming `CHILD` edge.
2. Adding a `CHILD` edge cannot introduce a cycle.

The optional position orders siblings. A null position is explicitly
unordered. All other edge types remain schema-free.

## Temporal storage

Logical entities have stable identifiers while each committed version has a
half-open validity range `[valid_from, valid_to)`. The current version has a
null `valid_to`. Deletion closes the current version without adding a tombstone
row. Revisions store wall-clock time and optional actor/message metadata.

Historical reads select versions whose validity range contains a requested
revision. Time-based reads resolve to the latest revision at or before that
instant. Writes are accepted only against the current graph.

SQLite runs in WAL mode with a bounded busy timeout. Readers use ordinary read
transactions. A writer reserves the write transaction, validates all requested
changes against one consistent current state, allocates one revision, writes
new versions, and commits atomically. SQLite serializes writers while allowing
independent processes to read concurrently.

The embedded schema is currently version 5, with fingerprint
`3be39b39c67594a6142d3167c7952c2c799d93c3b4d449de833695fb7e52c110`.
Before upgrading an existing v1–v4 schema, migrations validate that version's
exact fingerprint and source values;
v2 and later also validate derived-index provenance. Migration DDL and
`user_version` commit in one transaction. Go admission and decode
paths enforce size, depth, cardinality, canonical form, and UTF-8. SQLite
triggers independently enforce source and derived-index byte/cardinality limits
for direct SQL; migrations and `CheckIntegrity` detect representation or UTF-8
corruption which raw SQL can still introduce. Exact ceilings are listed in
[the Cypher guide](cypher.md).

Sheets assumes that the project database path is not maliciously replaced by
another process running as the same OS user while a connection pool is open.
SQLite and `database/sql` cannot portably bind every lazily opened connection
and its WAL/SHM sidecars to one file identity. Closing that adversarial
same-user pathname race requires a pinned, fd-relative custom VFS. A permanently
single connection would mitigate the pool's split-identity case but sacrifice
read concurrency and would not by itself bind validation, open, and sidecars
against every replacement race. Neither guarantee is implied by normal
multi-process concurrency.

## Packages

- `cmd/sheets`: process entry point.
- `internal/domain`: graph and revision value types shared across layers.
- `internal/project`: initialization and nearest-project discovery.
- `internal/store`: SQLite schema, temporal persistence, and transactions.
- `internal/cypher`: generated M23 grammar frontend, CST binder, and syntax
  tree.
- `internal/engine`: semantic evaluation, graph execution, and snapshot
  indexes.
- `internal/app`: use cases shared by all frontends.
- `internal/cli`: command-line frontend and stable JSON contracts.
- `internal/tui`: Charm-based interactive frontend.

Dependencies point inward: frontends depend on `app` boundaries, and the
process entry point wires the store and engine implementations to them. Neither
the domain package nor project discovery depends on a frontend.

## Cypher and JSON

Cypher is the graph language for both reads and writes. Read-only commands
reject mutating clauses before execution. Multiple statements supplied in one
execution request share one SQLite transaction and, if they mutate, one
revision.

JSON is deliberately limited to machine-readable output and parameter values.
It is not a second mutation language. This keeps one semantic surface while
allowing agents to pass structured values without quoting them into queries.
The engine delivers eligible read-only terminal projections through an
internal synchronous Start/Row/End event boundary with backpressure; the JSONL
renderer emits only its documented public records. Operators whose semantics
require a complete bounded set, and every mutation, remain materialized;
mutation output is published only after commit.

## Performance principles

- Prepared statements and set-oriented SQL are preferred on storage hot paths.
- Current-state indexes are partial where that materially reduces index size.
- Exact revision snapshots are immutable, revision-checked, and indexed in
  memory for repeated graph matching and traversal.
- Revision history uses opaque keyset cursors bound to the schema, order,
  predicate, and page boundary, and supports ascending or descending pages.
  CLI JSONL streams eligible terminal
  projections without retaining a second result table.
- Query row, relationship-expansion, working-entity, and working-byte budgets
  fail explicitly instead of silently truncating a result.
- Startup performs no network access and opens no background service.
- TUI refresh uses the maximum revision as a cheap invalidation token.
- Benchmarks cover point lookup, hierarchy traversal, historical reads, bulk
  atomic writes, and concurrent readers with a writer.
