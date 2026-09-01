# Terminal UI

Run `sheets tui` anywhere beneath a sheets project. The interface is a
human-oriented view of the same temporal property graph and Cypher executor
used by the CLI; it does not start or connect to a daemon.

The header always exposes four named function-key workspaces. `Ctrl+K` opens a searchable
command palette containing every primary action, and the bottom line shows
keys for the current workspace and focus. `F10` opens complete contextual help.
This makes the interface usable without first learning a shortcut map.

## Workspaces

- **Work** is the primary workspace. It renders the complete, arbitrarily deep
  `CHILD` hierarchy as a collapsible tree. Ordered children show their integer
  position; `◇` marks an unordered child. Every root remains visible, and an
  explicit invalid section keeps cyclic or otherwise unreachable imported data
  from silently disappearing. The details pane shows the selected node's
  stable ID, validity range, hierarchy context, and explicitly bounded previews
  of labels, properties, relationships, and rendered Markdown body.
- **Relationships** is the complete edge inventory, including `CHILD` and every
  schema-free relationship type. Each row shows both endpoints, type, hierarchy
  order where relevant, and property count. The details pane includes identity,
  endpoints, validity range, and a bounded property preview. `/` uses Bubbles'
  fuzzy filtering over bounded searchable representations of IDs, endpoint
  titles, type, and properties.
- **Query** is an interactive Cypher console with multiline Cypher and JSON
  parameter editors. Read-only and write-capable execution are distinct.
  Results preserve statement order, column order, duplicate column names, and
  value types, while the table and selected-row inspector enforce explicit
  presentation budgets. The table can scroll horizontally, and `[` / `]` move
  between results from a multi-statement request. When terminal height is
  constrained, Query shows the focused section at full usable height; `Tab`
  still cycles through Cypher, parameters, results, and the selected row.
- **Timeline** lists committed revisions newest first and includes revision 0,
  the initial empty graph. Entries show time, actor, and message and can be
  filtered with `/`. Opening one reloads every graph workspace at that exact
  revision. The first 100 revisions load immediately; `o`, or moving near the
  bottom, fetches the next bounded older page. Failed pages remain retryable,
  duplicate refresh results are merged, and revision 0 appears only after the
  committed history is exhausted.

At 108 columns and above, Work, Relationships, and Timeline show their
navigator and inspector side by side. Compact terminals show one focused pane;
`Tab` swaps between the navigator and details. Terminals smaller than 44×12
show a wrapped resize message that retains both required dimensions and the
quit key instead of a corrupted layout. All rendered lines are clipped and
padded deterministically to the current terminal size.

Presentation is deliberately bounded: detail JSON is capped at 256 KiB,
Markdown body previews at 262,144 runes, and inspector collections at 1,000
items. Query tables show at most 10,000 rows, 256 columns, 100,000 cells, and 80
display columns per cell; selected-row detail is capped at 1 MiB. Explicit
omission markers distinguish presentation limits from stored values. Engine
resource-limit errors fail the operation rather than returning a partial graph.

## Live and historical state

Live mode is writable. Opening a Timeline entry switches the entire interface
to an exact, read-only snapshot. A persistent `READ-ONLY HISTORY` banner names
both the loaded revision and current live revision; mutation actions are
disabled, and `F6` returns to Live. Read-only Cypher remains available against
the selected historical snapshot.

The TUI initially reads the current revision token, pins a snapshot to that
revision, and loads nodes and relationships through one read-only
`app.Executor` request. While idle it polls only the cheap current-revision
token every two seconds. It reloads the graph and timeline only when that token
changes. In historical mode, an external write updates the displayed live
revision without replacing the snapshot being inspected. Revision metadata is
obtained through the shared application read service; there is no direct store
access, filesystem watcher, socket, or background service.
An initial graph or timeline failure remains visible with `F5` retry guidance;
a failed refresh keeps the last good view and marks it as stale in the status
line instead of falling back to a permanent loading message.

## Guided graph changes

Work provides guided forms for:

- creating a root or child node with labels, properties, Markdown body, and an
  optional sibling position;
- editing labels, schema-free properties, and Markdown body; oversized
  unchanged previews are preserved until explicitly replaced;
- moving a node to another parent, detaching it to a root, or changing between
  ordered and unordered sibling placement;
- connecting any two nodes with an arbitrary non-`CHILD` relationship and
  schema-free properties;
- deleting a node with a preview of incident relationships and newly rooted
  children.

Relationships adds property editing and confirmed deletion for the selected
edge. Use Move / order for `CHILD`; this keeps hierarchy cycle and single-parent
semantics in one explicit workflow. Relationship endpoints and type are stable
during editing and can be changed by deleting and recreating the edge.

Forms are implemented with Huh controls: text fields, multiline editors,
searchable node selectors, validation, and explicit confirmations. JSON must
be an object and is normalized before execution; overflowing integer literals
are rejected rather than rounded. Existing finite floats, byte slices,
temporals, durations, lists, and nested maps retain their durable types when an
edit form is submitted. IEEE non-finite floats, bytes, and all six exact Cypher
temporal types use the same tagged JSON envelopes as CLI output. Node `body`
and relationship `position` are reserved because their dedicated controls
carry those graph semantics;
dynamic labels, relationship types, and positions are validated. Values are
passed as Cypher parameters, while dynamic labels and relationship types are
escaped as identifiers. Deletes and write-capable console requests require
confirmation. A failed guided mutation or confirmed write-capable request
keeps the exact request available for retry, and a concurrent no-match result
is reported instead of being presented as success.
At the minimum supported terminal size, forms scroll the focused Huh field into
view rather than rendering controls outside the modal.

Every read and mutation of graph data crosses `app.Executor` as Cypher. The TUI
has no privileged graph API and performs no I/O inside the Bubble Tea update
loop. Slow commands are asynchronous, cancellable where superseded, and tagged
so stale snapshot, execution, timeline, and Markdown-render results cannot
replace newer state. Bubbles filter results are also tagged with their owning
list and filter value, so fast workspace or palette changes cannot apply an old
match set to a different component.

## Keys and mouse

Workspace navigation and the function-key commands are deliberately
non-printable and work from every focus state, including Cypher, parameter,
result, and form fields. The command palette is available whenever another
modal is not already active. Its filter updates as you type, `Enter` activates
the selected match, and `Esc` closes it in one step:

```text
F1–F4            Work, Relationships, Query, Timeline
ctrl+pgup/pgdn   previous / next workspace
ctrl+k           searchable command palette
F5               refresh the selected snapshot and timeline
F6               return to Live from historical mode
F10              contextual help; restores an interrupted modal when closed
ctrl+c           always quit
```

Workspace keys:

```text
Work
  arrows or j/k  select a visible tree row
  left/h         close a branch
  right/l        open a branch
  enter          toggle a branch
  /              find a node by title, ID, label, property, or body
  n/e/m/c/d      new, edit, move/order, connect, delete
  Tab            focus details (or return to the tree)

Relationships
  arrows or j/k  select
  /              filter
  Enter or Tab   focus details
  e/c/d          edit properties, connect nodes, delete

Query
  Tab/Shift+Tab  Cypher → parameters → results → selected row
  Ctrl+R         execute read-only (Ctrl+Enter is equivalent)
  Ctrl+X         confirm and execute a write-capable request
  [ / ]          previous / next statement result while results are focused

Timeline
  arrows or j/k  select
  /              filter revision metadata
  Enter          open the exact revision
  o              load or retry the next older page
  Tab            focus details
```

Mouse clicks switch visible workspace tabs and select visible Work rows. The
wheel scrolls the hierarchy, relationship list, timeline, query results, help,
or focused details as appropriate.

## Accessibility

Sheets adapts its palette to the reported terminal background. Set `NO_COLOR`
or run `sheets tui --no-color` to remove foreground and background color SGR
sequences. The monochrome layout retains ASCII borders, focus prefixes,
structural active-tab markers, text labels, and the input cursor's reverse-video
affordance, so color is never the sole carrier of state.
