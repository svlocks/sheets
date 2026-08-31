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
nearest one.

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
  ordered `columns` and positional `rows`, so duplicate or unusual Cypher
  column names remain representable.
- `jsonl` emits independently consumable `row`, `result`, `summary`, `page`,
  and `revision` records with zero-based statement indexes.

## History and status

```text
sheets history [--limit N] [--after CURSOR] [--output FORMAT]
sheets status  [--output FORMAT]
```

History is ordered from oldest to newest and uses opaque keyset cursors. Status
reports the discovered root, current revision, node count, and relationship
count.

## TUI, help, and completions

`sheets tui` (alias `sheets ui`) launches the full-screen interface. Set
`NO_COLOR` or pass `--no-color` for a monochrome presentation.

Fang supplies styled help and `--version`; Cobra exposes shell completion
commands. Run `sheets completion --help` for the installed shell.
