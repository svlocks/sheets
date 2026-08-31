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

Selected: [`google/uuid`](https://pkg.go.dev/github.com/google/uuid) for UUIDv7
generation and validation. IDs remain ordinary strings at package boundaries.

## Graph language

The language frontend follows the
[`openCypher`](https://github.com/opencypher/openCypher) grammar and terminology.
The official Technology Compatibility Kit is the reference for supported
syntax and semantics. A parser dependency or generated parser is isolated
inside `internal/cypher`; storage and frontends never depend on parser-specific
types.

Cypher plans execute directly against indexed SQLite version tables. This is
more implementation work than loading snapshots into an in-memory graph, but
it avoids graph-size-dependent startup and makes historical predicates and
limits effective before materialization.

## Command line

Selected:

- [`Cobra`](https://github.com/spf13/cobra) for command/flag parsing,
  completions, and a stable library API.
- [`Fang`](https://github.com/charmbracelet/fang) for styled help, errors,
  version reporting, completions, and generated manual pages.

There is no configuration framework. Sheets has only a few process-scoped
flags, so explicit Cobra flags and standard-library environment handling are
clearer and cheaper than a global configuration registry.

## Terminal UI

Selected from Charm's v2 ecosystem:

- [`Bubble Tea`](https://github.com/charmbracelet/bubbletea) for the Elm-style
  update loop and terminal lifecycle;
- [`Bubbles`](https://github.com/charmbracelet/bubbles) for text input,
  viewport, table, list, spinner, and key-binding components;
- [`Lip Gloss`](https://github.com/charmbracelet/lipgloss) for responsive
  layout and adaptive color;
- [`Huh`](https://github.com/charmbracelet/huh) for mutation forms; and
- [`Glamour`](https://github.com/charmbracelet/glamour) for Markdown bodies.

The TUI polls only the current revision number while idle. It does not add a
filesystem watcher, background service, or private data-access path.

## Deliberately absent

- No daemon, RPC transport, or embedded web server.
- No ORM, repository generator, dependency-injection framework, or global
  service locator.
- No editable Markdown mirror and therefore no file synchronization library.
- No application-level lockfile: SQLite transactions are the concurrency
  authority.
- No migration framework at runtime: ordered SQL migrations are embedded and
  applied in the same connection that validates the schema version.

