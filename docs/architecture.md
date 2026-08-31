# Architecture

## Product contract

Sheets is a local-first, daemonless temporal property graph. SQLite is the
single source of truth. Markdown bodies are values in the graph rather than
mutable filesystem mirrors. Every successful write batch creates exactly one
monotonically increasing revision; every statement in the batch either commits
at that revision or none of it commits.

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

## Packages

- `cmd/sheets`: process entry point.
- `internal/domain`: graph and revision value types shared across layers.
- `internal/project`: initialization and nearest-project discovery.
- `internal/store`: SQLite schema, temporal persistence, and transactions.
- `internal/cypher`: parser, semantic analysis, plans, and execution.
- `internal/app`: use cases shared by all frontends.
- `internal/cli`: command-line frontend and stable JSON contracts.
- `internal/tui`: Charm-based interactive frontend.

Dependencies point inward: frontends depend on `app`; `app` composes the query
engine and store; neither the domain package nor project discovery depends on a
frontend.

## Cypher and JSON

Cypher is the graph language for both reads and writes. Read-only commands
reject mutating clauses before execution. Multiple statements supplied in one
execution request share one SQLite transaction and, if they mutate, one
revision.

JSON is deliberately limited to machine-readable output and parameter values.
It is not a second mutation language. This keeps one semantic surface while
allowing agents to pass structured values without quoting them into queries.

## Performance principles

- Prepared statements and set-oriented SQL are preferred on hot paths.
- Current-state indexes are partial where that materially reduces index size.
- Listing and traversal APIs are paginated and streamable.
- Startup performs no network access and opens no background service.
- TUI refresh uses the maximum revision as a cheap invalidation token.
- Benchmarks cover point lookup, hierarchy traversal, historical reads, bulk
  atomic writes, and concurrent readers with a writer.

