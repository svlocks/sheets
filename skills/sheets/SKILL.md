---
name: sheets
description: Query and mutate a sheets project — a local, daemonless temporal property graph with a Cypher CLI. Use when the working directory (or an ancestor) contains a .sheets directory, or when asked to record, query, or organize graph-structured work with sheets.
version: 0.1.0
---

# Using sheets

sheets stores a temporal property graph in SQLite inside the nearest
`.sheets` directory (found by walking up from the current directory, like
git). One binary, no daemon; concurrent processes are safe. Every effective
write batch commits atomically as one revision, and any past revision can be
read back.

This skill teaches the mechanics only. What the graph *means* — what is
tracked, which labels, properties, and edge types are used, and any workflow
rules — is decided per project by its users and agents. Discover existing
conventions from the graph itself (labels, property keys, edge types below)
before inventing new ones, and follow any project-specific instructions.

## Commands

```sh
sheets init [dir]      # create a project (.sheets directory)
sheets root            # print the nearest project root
sheets query 'CYPHER'  # read-only; mutating clauses are rejected
sheets exec  'CYPHER'  # read/write; one atomic revision per effective batch
sheets history         # revision log (--limit, --order descending, --after CURSOR)
sheets status          # root, current revision, node/edge counts
```

Shared flags: `-C PATH` starts discovery elsewhere; `-o json|jsonl|table`
selects output (default `table`; always use `json` or `jsonl` for machine
reads); `-f FILE` reads the query from a file (`-` for stdin — prefer this
for queries with tricky quoting).

Parameters: pass values as JSON parameters instead of splicing them into
query text: `-p 'name=JSON'` (repeatable) or `--params '{"a":1}'` /
`--params @file.json`, then reference `$name` in the query.

Attribution: on `exec`, set `--actor NAME` (or `SHEETS_ACTOR`) and
`-m 'message'` so revisions record who did what and why.

## Data model

- Nodes: zero or more labels, a schema-free property map, and a first-class
  Markdown `body` (read/write it as the `body` property). IDs are stable UUIDs
  via `elementId(n)`.
- Edges: one type, a property map, and an optional integer `position`
  (set via the edge property map) that orders siblings.
- `CHILD` is the only constrained edge type: at most one incoming `CHILD`
  per node and no cycles — it forms hierarchies. All other edge types are
  free-form.

## Cypher

The dialect is openCypher (M23 grammar). Reads support `MATCH`/`OPTIONAL
MATCH`, `WHERE`, `WITH`, `UNWIND`, `ORDER BY`/`SKIP`/`LIMIT`, `UNION`,
aggregates, list/pattern comprehensions, `EXISTS` subqueries, `CALL { ... }`
subqueries, and `shortestPath`. Mutations support `CREATE`, `MERGE` (with `ON
CREATE SET`/`ON MATCH SET`), `SET`, `REMOVE`, `DELETE`, `DETACH DELETE`.
Unsupported constructs fail with an explicit error, never silently.

```sh
# Create a small hierarchy atomically (one revision)
sheets exec -m "plan payments" -p 'body="# Payments\n\nNotes."' '
  CREATE (p:Task {title: "Build payments", status: "todo", body: $body})
         -[:CHILD {position: 0}]->(:Task {title: "Integrate SDK", status: "todo"})
  RETURN elementId(p)'

# Read for machines
sheets query -o json 'MATCH (n:Task {status: "todo"}) RETURN elementId(n), n.title'

# Update by id
sheets exec -m "claim" -p 'id="UUID"' -p 'agent="me"' '
  MATCH (n) WHERE elementId(n) = $id SET n.status = "doing", n.owner = $agent RETURN n.status'

# Traverse a hierarchy (always bound variable-length patterns)
sheets query -o json '
  MATCH (root:Task {title: "Build payments"})-[:CHILD*1..5]->(d)
  RETURN elementId(d), d.title ORDER BY d.title'

# Discover this project's conventions
sheets query -o json 'CALL db.labels()'
sheets query -o json 'CALL db.relationshipTypes()'
sheets query -o json 'CALL db.propertyKeys()'
```

Semicolon-separated statements in one `exec` run in one transaction and
commit as one revision; later statements see earlier changes. A write that
changes nothing creates no revision.

## Time travel

```sh
sheets query --at-revision 42 'MATCH (n) RETURN count(n)'
sheets query --at-time 2026-01-15T12:00:00Z 'MATCH (n) RETURN count(n)'
sheets history --order descending --limit 20 -o json
```

Historical reads are read-only; writes always target the current graph.

## Output contract

`-o json` prints one `{"results":[{"columns":[...],"rows":[...]}],"revision":N}`
envelope; rows are positional arrays matching `columns`. `-o jsonl` streams
independent `row`/`summary`/`revision` records for large results — a nonzero
exit status means the emitted rows are a valid prefix, not the complete
result. Data goes to stdout, diagnostics to stderr.

## Budgets and errors

Queries share a 100,000-row budget, 1,000,000 relationship expansions, and a
200,000-entity/64 MiB working set. Exceeding one is an explicit error, never a
truncated result: narrow with labels, property predicates, finite bounds
(`*1..4`), `LIMIT`, or aggregate instead of returning rows. Read errors from
stderr and adjust the query; `EXPLAIN <query>` validates and shows the plan
without executing.
