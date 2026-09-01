# CLI reference

## Project selection

Every command that needs graph data starts at the current directory and walks
upward until it finds the nearest valid `.sheets` directory. Use `-C PATH` to
choose another starting file or directory without changing the process working
directory.

```text
sheets init [directory]
sheets root [--metadata | --database]
sheets -C path/to/child status
```

`sheets init` creates the project marker and `.sheets/sheets.db`. It is
idempotent for a valid project and refuses corrupt or symlink-redirected
metadata layouts. Nested sheets projects are valid; discovery selects the
nearest one. The adversarial same-user pathname replacement boundary after a
pool is open is documented in [Architecture](architecture.md#temporal-storage).

## Cypher execution

`query` accepts reads only. `exec` accepts reads and writes. A file containing
multiple semicolon-separated statements is one SQLite transaction and, if any
effective mutation occurs, one revision.

```text
sheets query [cypher] [flags]
sheets exec  [cypher] [flags]

  -f, --file PATH          read Cypher from PATH; - means stdin
      --params JSON        JSON object, @file, or - for stdin
  -p, --param NAME=JSON    add one parameter; repeatable
  -o, --output FORMAT      table, json, or jsonl
      --at-revision N      select an exact historical revision
      --at-time RFC3339    select the latest revision at or before a time

exec only:
      --actor NAME         revision actor (defaults to SHEETS_ACTOR)
  -m, --message TEXT       revision message
```

Inline parameters are decoded as JSON, so strings need JSON quotes:

```sh
sheets query \
  --param 'status="ready"' \
  --param 'limit=20' \
  'MATCH (n {status: $status}) RETURN n LIMIT $limit'
```

JSON integers must fit Cypher's signed 64-bit range. Values outside it are
rejected instead of being rounded through `float64`. A query argument and
`--file` are mutually exclusive, and query text plus parameters cannot both
claim stdin. Query and parameter files/stdin are capped at 64 MiB, duplicate
JSON object keys are rejected, and cancellation interrupts input reads.

For a larger request, keep both surfaces out of shell quoting:

```sh
sheets exec --file transaction.cypher --params @params.json \
  --actor build-agent --message "claim and decompose task"
```

Historical selectors are accepted by both commands so scripts can share flag
construction, but `exec` rejects a mutating historical request before opening
a write transaction.

## Output contracts

- `table` is deterministic, escapes control characters, and includes mutation
  counters and the committed revision.
- `json` is one `{"results":[...],"revision":N}` envelope. Each result has
  ordered `columns` and positional `rows`; both are present as arrays even when
  empty.
- `jsonl` emits independently consumable `row`, `result`, `summary`, `page`,
  and `revision` records with zero-based statement indexes. Eligible read-only
  terminal projections are delivered synchronously with backpressure and do
  not retain a second complete result table. `ORDER BY`, `DISTINCT`,
  aggregation, pagination, unions, applicable procedure results, and mutations
  retain their bounded materialized path. Mutations never publish output before
  commit.

IEEE results which JSON cannot represent use tagged objects:
`{"$float":"NaN"}`, `{"$float":"+Infinity"}`, or
`{"$float":"-Infinity"}`. Bytes use `{"$bytes":"BASE64"}`. Cypher dates,
times, datetimes, and durations use type-specific envelopes containing a human
`text` form plus a base64 `binary` form that preserves the exact durable value.
These exact envelopes are accepted as parameters; `$float` remains ordinary
plain JSON input so parameters cannot create non-finite numbers accidentally.
Normal finite numbers remain JSON numbers.

Machine formats never use terminal styling. Table cells and diagnostics escape
terminal controls, and machine values are encoded rather than interpreted.
Data goes to stdout and diagnostics go to stderr. An early-closing pipe is a
successful exit with no diagnostic. A later runtime or sink error can leave a
valid prefix of JSONL rows on stdout. JSONL deliberately has no rollback or
end record, so the nonzero process status is the completion signal for that
prefix.

## History and status

```text
sheets history [--limit N] [--after CURSOR]
                [--order ascending|descending] [--output FORMAT]
sheets status  [--output FORMAT]
```

History defaults to oldest-first for compatibility; `--order descending`
returns newest-first. Both directions use opaque keyset cursors bound to the
schema, ordering, sealed-revision predicate, and page boundary. A cursor is an
integrity-checked continuation token, not an authentication credential, and is
valid only with the same order. Status reports the discovered root, current
revision, node count, and relationship count without materializing the graph.
The same timeline is available through
`CALL sheets.revisions(limit, afterCursor, order)` for graph-language-only
clients; the third argument is optional.

## TUI, help, and completions

`sheets tui` (alias `sheets ui`) launches the full-screen interface. Set
`NO_COLOR` or pass `--no-color` for a monochrome presentation.

Fang supplies styled help and `--version`; Cobra exposes shell completion
commands. Run `sheets completion --help` for the installed shell.
