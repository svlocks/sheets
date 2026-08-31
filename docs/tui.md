# Terminal UI

Run `sheets tui` anywhere beneath a sheets project. The interface loads a
complete graph snapshot and revision timeline, then polls only the latest
revision number every two seconds. A changed token triggers a refresh.

## Workspaces

- **Outline** renders ordered and unordered `CHILD` hierarchy at arbitrary
  depth. Roots, orphans, collapsed branches, and malformed/cyclic imported
  data remain visible instead of disappearing.
- **Graph** renders an ANSI-safe adjacency topology with selection, horizontal
  and vertical pan, and three detail levels. Dangling relationships are shown.
- **Query** contains multiline Cypher and JSON-parameter editors plus a
  scrollable result table. Read and execute are separate actions.
- **History** opens the entire UI at an exact revision. A prominent banner and
  disabled mutation paths make historical mode read-only.

Wide terminals use navigator, main-content, and inspector panes. Compact
terminals devote the screen to the active view and expose details as an
overlay. The inspector includes stable ID, labels, every property, validity
metadata, incoming/outgoing relationships, and Glamour-rendered Markdown.

## Keys

Global:

```text
1–4              switch workspace outside Query editors
ctrl+left/right  switch workspace
ctrl+k           command palette
?                help
r                refresh
q / ctrl+c       quit
```

Outline and Graph use `j/k` or arrows to select. `/` searches across IDs,
labels, property keys/values, and titles. `i` inspects; `c`, `e`, `p`, and `d`
open create, edit, reparent, and delete workflows. Graph also uses `h/l` to
pan, page keys for vertical movement, `+/-` for detail, and `0` to reset.

Query uses `tab` / `shift+tab` to move between Cypher, parameters, and results;
`ctrl+r` or `ctrl+enter` performs a read-only query and `ctrl+x` executes a
write-capable request.

History uses `enter` to open the selected revision and `l` to return Live.
Mouse clicks select tabs/items and the wheel scrolls applicable views.

## Mutation forms

Schema-free nested properties are edited in guided JSON overlays. The TUI
validates and normalizes the form, constructs a parameterized Cypher request,
and sends it through the same `app.Executor` used by `sheets exec`. It has no
direct storage or privileged mutation path.

