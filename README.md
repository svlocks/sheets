# sheets

`sheets` is a fast, daemonless, temporal property graph for tracking work by
humans and autonomous agents. One portable Go binary provides an
openCypher-M23-derived read/write engine, a machine-friendly CLI, a polished
terminal UI, concurrent SQLite persistence, and every committed graph state.

## Why it is different

- Every work item is a schema-free node. A task can become a parent at any
  time, and `CHILD` chains have no application depth limit.
- Every atomic batch that makes an effective change creates one revision.
  Reads can select the current graph, an exact revision, or a wall-clock
  instant; successful no-op writes create no revision.
- Independent processes cooperate through SQLite WAL. There is no daemon,
  socket, application lockfile, or synchronization service.
- Cypher is the one graph language for reads and writes. Its generated frontend
  is tied to the official M23 grammar and a reproducible 3,897-scenario TCK
  report. JSON is used only for parameter values and structured output.
- `body` is a first-class Markdown value in the database, so editable mirror
  files can never drift from graph state.
- Indexed temporal scans accelerate common cold node queries; a
  revision-aware in-process graph cache keeps arbitrary repeated traversals
  fast without compromising external-process visibility.

## Build or install

Sheets is a regular executable, not a library that is imported into another
Go program. From a checkout, build it without installing:

```sh
go build -o bin/sheets ./cmd/sheets
```

Run it as `bin/sheets`, or install the same executable into your configured
Go binary directory (`GOBIN`, otherwise `GOPATH/bin`):

```sh
go install ./cmd/sheets
```

Release archives contain the standalone `sheets` binary and need no Go or
SQLite installation.

## Quick start

Initialize a project and create a hierarchy atomically:

```sh
bin/sheets init .
bin/sheets exec --message "plan payments" '
  CREATE (payments:Task {title: "Build payment system", body: "# Payments"})
         -[:CHILD {position: 0}]->(:Task {title: "Integrate SDK"}),
         (payments)-[:CHILD {position: 1}]->(:Task {title: "Connect database"})
  RETURN payments
'
```

Run a machine-readable query from any descendant directory:

```sh
bin/sheets query --output json \
  'MATCH (parent)-[r:CHILD]->(task) RETURN parent.title, task.title, r.position'
```

Open the interactive workspace:

```sh
bin/sheets tui
```

Sheets walks upward from the current directory (or `-C PATH`) and uses the
nearest `.sheets` project, including in nested repositories.

## Interfaces

The CLI includes `init`, `root`, read-only `query`, atomic read/write `exec`,
`history`, `status`, and `tui`. Query text can be inline, read from a file, or
read from stdin; parameters can be a JSON object/file plus repeatable
`name=JSON` values. Results support aligned tables, JSON, and row-streaming
JSONL; writes remain commit-before-output atomic.

The TUI provides responsive Work hierarchy, Relationships, Query, and Timeline
workspaces, explicitly bounded node/edge/Markdown previews, fuzzy search,
unmistakable historical read-only navigation, and guided Cypher-backed graph
changes. It polls only the current revision token, never a daemon or filesystem
watcher.

## Documentation

- [Architecture and invariants](docs/architecture.md)
- [CLI reference](docs/cli.md)
- [Cypher dialect and examples](docs/cypher.md)
- [Language conformance evidence and frontend provenance](docs/cypher-conformance.md)
- [TUI guide](docs/tui.md)
- [Dependency decisions](docs/dependencies.md)
- [Benchmarks](docs/performance.md)
- [Development and release workflow](CONTRIBUTING.md)

Go 1.25.14 or newer is required to build from source. Release builds are static
and do not require Go, SQLite, or another runtime on the target machine.
