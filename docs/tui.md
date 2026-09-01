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
  stable ID, labels, every property, validity range, parent, ordered/unordered
  children, all non-hierarchy relationships, and rendered Markdown body.
- **Relationships** is the complete edge inventory, including `CHILD` and every
  schema-free relationship type. Each row shows both endpoints, type, hierarchy
  order where relevant, and property count. The details pane includes the full
  identity, endpoints, validity range, and property object. `/` uses Bubbles'
  fuzzy filtering over IDs, endpoint titles, type, and properties.
- **Query** is an interactive Cypher console with multiline Cypher and JSON
  parameter editors. Read-only and write-capable execution are distinct.
  Results preserve statement order, column order, duplicate column names, and
  full selected-row values. The table can scroll horizontally, and `[` / `]`
  move between results from a multi-statement request. When terminal height is
  constrained, Query shows the focused section at full usable height; `Tab`
  still cycles through Cypher, parameters, results, and the selected row.
- **Timeline** lists committed revisions newest first and includes revision 0,
  the initial empty graph. Entries show time, actor, and message and can be
  filtered with `/`. Opening one reloads every graph workspace at that exact
  revision.

At 108 columns and above, Work, Relationships, and Timeline show their
navigator and inspector side by side. Compact terminals show one focused pane;
`Tab` swaps between the navigator and details. Terminals smaller than 44×12
show a wrapped resize message that retains both required dimensions and the
quit key instead of a corrupted layout. All rendered lines are clipped and
padded deterministically to the current terminal size.

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
- editing all labels, schema-free properties, and Markdown body;
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
edit form is submitted. IEEE non-finite floats use the same explicit JSON tags
as CLI output: `{"$float":"NaN"}`, `{"$float":"+Infinity"}`, and
`{"$float":"-Infinity"}`. Node `body` and relationship `position` are
reserved because their dedicated controls carry those graph semantics;
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
