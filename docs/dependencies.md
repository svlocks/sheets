# Dependency decisions

Sheets keeps its runtime dependency surface intentionally narrow. Libraries
are selected when they provide a difficult, well-tested capability; core graph
semantics remain in this repository so agents do not need to reason about
hidden framework behavior.

## Storage

Selected: [`modernc.org/sqlite`](https://pkg.go.dev/modernc.org/sqlite).

It embeds SQLite without CGO, which allows one static binary across Linux,
macOS, and Windows. SQLite's
[WAL mode](https://sqlite.org/wal.html) gives independent processes concurrent
read access while serializing the single writer required for atomic graph
revisions. The schema and SQL are handwritten; an ORM would obscure temporal
range predicates and make query-plan control harder.

Alternatives considered:

- `mattn/go-sqlite3` is mature and often fast, but requires a C toolchain and
  complicates cross-compilation.
- `ncruces/go-sqlite3` is a strong pure-Go/WASM driver. It remains a viable
  replacement if repeatable workload benchmarks beat modernc without harming
  startup or binary size.
- Ladybug/Kuzu offers native graph execution and Cypher, but its embedded mode
  does not support the independent concurrent writer processes sheets needs.
- Badger-backed graph engines take an exclusive directory lock and therefore
  do not meet the daemonless multi-process contract.

UUIDv7 entity identifiers are generated directly from `crypto/rand` and the
standard UUID bit layout. The small implementation is tested at the storage
boundary and keeps IDs ordinary strings across package boundaries.

## Graph language

Selected: the official openCypher 9 M23 grammar, ANTLR 4.13.1 for generation,
and [`antlr4-go`](https://github.com/antlr4-go/antlr) 4.13.1 at runtime.

The grammar, ANTLR JAR, named-graph fixtures, and generator image are pinned by
digest. Generated Go is committed, so building sheets does not need Java,
Docker, or the network. A small local grammar patch adds explicitly documented
sheets extensions. The binder converts the generated CST into repository-owned
AST values and returns located typed errors for recognized-but-unsupported
features; parser runtime types do not escape `internal/cypher`. Exact source
provenance and TCK evidence are in
[cypher-conformance.md](cypher-conformance.md).

The executor uses a snapshot-bound `store.GraphReader`. Scalar/timeline
queries avoid graph loading, and safe pattern shapes push label, property,
endpoint, and historical predicates into paged SQLite scans. Other shapes
materialize an explicitly bounded working graph with label/property/adjacency
indexes; long-running processes cache complete immutable snapshots behind a
cheap revision check. Eligible terminal projections stream synchronously to
JSONL, while semantically blocking operators keep their bounded materialized
path. SQLite remains the concurrency and history authority.

## Temporal rules

Selected: a repository-embedded archive compiled from checksum-pinned IANA
tzcode/tzdata 2023c using its default main profile. Generation occurs in the
digest-pinned Go 1.25.14 Bookworm image and is byte-identical on amd64 and
arm64. Runtime named-zone construction never consults the host filesystem,
`TZ`, or `ZONEINFO`; this makes temporal values and M23 evidence reproducible
on static/distroless systems. No runtime timezone package or daemon is needed.

## Command line

Selected:

- [`Cobra`](https://github.com/spf13/cobra) for command/flag parsing,
  completions, and a stable library API.
- [`Fang`](https://github.com/charmbracelet/fang) for styled help, errors,
  version reporting, and shell completion integration.

There is no configuration framework. Sheets has only a few process-scoped
flags, so explicit Cobra flags and standard-library environment handling are
clearer and cheaper than a global configuration registry.

## Terminal UI

Selected from Charm's v2 ecosystem:

- [`Bubble Tea`](https://github.com/charmbracelet/bubbletea) for the Elm-style
  update loop, declarative v2 view settings, mouse events, and terminal
  lifecycle;
- [`Bubbles`](https://github.com/charmbracelet/bubbles) for fuzzy lists, the
  result table, viewports, help, key maps, spinner, text inputs, and multiline
  editors;
- [`Lip Gloss`](https://github.com/charmbracelet/lipgloss) for responsive
  layout, ANSI-aware sizing, borders, and adaptive color;
- [`Glamour`](https://github.com/charmbracelet/glamour) for Markdown bodies;
- [`Huh`](https://github.com/charmbracelet/huh) for guided, validated mutation
  and confirmation forms.

All selected TUI packages use the Charm v2 module line and therefore share Bubble
Tea v2 messages and Lip Gloss v2 styles. Huh owns field focus, searchable
selectors, validation, and form help; the TUI root model owns overlay lifetime,
historical gating, and asynchronous execution. Relationships, Query, and
Timeline use Bubbles components rather than private list, table, viewport, or
help implementations. Work's arbitrary-depth hierarchy is a small
domain-specific flattened tree because Bubbles does not provide a tree
component; its navigation still uses Bubbles key bindings and Bubble Tea
messages.

The TUI polls only the current revision number while idle. Graph snapshots and
all mutations cross the shared Cypher executor. Revision metadata uses the
shared application read service. The frontend adds no direct store access,
filesystem watcher, background service, or private graph-data path.

## Deliberately absent

- No daemon, RPC transport, or embedded web server.
- No ORM, repository generator, dependency-injection framework, or global
  service locator.
- No editable Markdown mirror and therefore no file synchronization library.
- No application-level lockfile: SQLite transactions are the concurrency
  authority.
- No migration framework at runtime: ordered SQL migrations are embedded and
  applied in the same connection that validates the schema version.
- No dynamic procedure/plugin runtime. The only callable procedures are the
  small built-in read catalog documented with the Cypher surface.
