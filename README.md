# sheets

`sheets` is a fast, daemonless, temporal property graph for tracking work by
humans and autonomous agents. One portable Go binary provides an
OpenCypher-compatible read/write engine, a machine-friendly CLI, a polished
terminal UI, concurrent SQLite persistence, and every committed graph state.

## Why it is different

- Every work item is a schema-free node. A task can become a parent at any
  time, and `CHILD` chains have no application depth limit.
- Every atomic write batch creates one revision. Reads can select the current
  graph, an exact revision, or a wall-clock instant.
- Independent processes cooperate through SQLite WAL. There is no daemon,
  socket, lockfile, or synchronization service.
- Cypher is the one graph language for reads and writes. JSON is used only for
  parameter values and structured output.
- `body` is a first-class Markdown value in the database, so editable mirror
  files can never drift from graph state.
- A revision-aware in-process cache and property/label indexes keep repeated
  queries fast without compromising external-process visibility.

## Quick start

Build the binary without installing it:

```sh
go build -o bin/sheets ./cmd/sheets
```

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
sheets query --output json \
  'MATCH (parent)-[r:CHILD]->(task) RETURN parent.title, task.title, r.position'
```

Open the interactive workspace:

```sh
sheets tui
```

Sheets walks upward from the current directory (or `-C PATH`) and uses the
nearest `.sheets` project, including in nested repositories.

## Interfaces

The CLI includes `init`, `root`, read-only `query`, atomic read/write `exec`,
`history`, `status`, and `tui`. Query text can be inline, read from a file, or
read from stdin; parameters can be a JSON object/file plus repeatable
`name=JSON` values. Results support aligned tables, JSON, and streaming JSONL.

The TUI provides responsive Outline, Graph, Query, and History workspaces,
full node/edge/Markdown inspection, search, historical read-only navigation,
and Cypher-backed create/edit/reparent/delete flows. It polls only the current
revision token, never a daemon or filesystem watcher.

## Documentation

- [Architecture and invariants](docs/architecture.md)
- [CLI reference](docs/cli.md)
- [Cypher dialect and examples](docs/cypher.md)
- [TUI guide](docs/tui.md)
- [Dependency decisions](docs/dependencies.md)
- [Benchmarks](docs/performance.md)
- [Development and release workflow](CONTRIBUTING.md)

Go 1.25.8 or newer is required to build from source. Release builds are static
and do not require Go, SQLite, or another runtime on the target machine.

