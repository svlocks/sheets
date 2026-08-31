# sheets

`sheets` is a daemonless, temporal property-graph work sheets designed for
humans and autonomous agents. A single Go binary provides:

- an OpenCypher-compatible query and mutation interface;
- ergonomic CLI commands for common graph operations;
- a full-screen terminal UI built on the Charm ecosystem;
- atomic, concurrent updates backed by an embedded SQLite database; and
- revision-addressable history for every committed graph state.

Every sheets project owns a `.sheets/sheets.db` database. Commands locate
the nearest project by walking from the current directory toward the
filesystem root, matching Git's project-discovery behavior.

The repository is under active construction. See
[`docs/architecture.md`](docs/architecture.md) for the implementation contract.

