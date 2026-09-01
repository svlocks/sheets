# Cypher language

Sheets uses one Cypher-inspired graph language for reads and writes. It is a
deliberately documented subset aligned with selected openCypher 9 behavior; it
is not a complete or certified openCypher implementation. Unsupported syntax
is rejected rather than accepted with an invented meaning. See
[the conformance evidence](cypher-conformance.md) for the pinned upstream
sources, generated-frontend architecture, and exact TCK inventory limits.

Keywords are case-insensitive. Identifiers, labels, relationship types, and
property keys preserve case. Backtick identifiers and parameter names are
supported, including numeric parameters such as `$1`. Integers are signed
64-bit values; decimal/exponent literals are IEEE-754 doubles. Hexadecimal
(`0x`) and octal (`0o`) integer literals are accepted.

## Graph values

Nodes have a stable UUIDv7, labels, a schema-free property map, and a Markdown
body. Relationships have a stable UUIDv7, source, type, target, property map,
and optional position.

- `elementId(n)` / `elementId(r)` returns the stable UUIDv7.
- `n.body` or `body(n)` reads the Markdown body. Setting/removing `n.body`
  updates the dedicated body field.
- `r.position` reads the optional sibling position. Setting it to `null` makes
  a `CHILD` relationship unordered.
- `labels`, `type`, `properties`, `keys`, `nodes`, `relationships`,
  `startNode`, and `endNode` expose graph structure.

Assigning a top-level property to `null` removes it. Null remains valid inside
nested lists and maps. Equality, membership, list/map equality, and boolean
operators use three-valued logic. Ordering predicates return null for values
which are not comparable; result sorting has a deterministic total order and
places null last ascending and first descending.

## Reads

The supported read surface includes:

- `MATCH` and `OPTIONAL MATCH` with directed/undirected relationships,
  alternative relationship types, labels, property patterns, named paths,
  and legacy variable lengths;
- `WHERE`, chained comparisons, string/regex/arithmetic operators, `IN`, list
  indexing/slicing, label predicates, `CASE`, and list/map literals;
- `WITH`, `UNWIND`, `RETURN`, `DISTINCT`, `ORDER BY`, `SKIP`/`OFFSET`, and
  `LIMIT`, including the openCypher `WITH ... ORDER BY ... WHERE ...` order;
- `UNION` / `UNION ALL` with exactly matching column names;
- list comprehensions, `all`/`any`/`none`/`single`, and `reduce`;
- `EXISTS` subqueries, pattern expressions, `shortestPath`, and
  `allShortestPaths`;
- `count`, `collect`, `sum`, `avg`, `min`, `max`, standard deviations, and
  continuous/discrete percentiles; and
- the scalar, string, collection, conversion, temporal, duration, and UUID
  functions exercised in `internal/engine` tests.

Nested aggregate calls are rejected. An aggregate and an unaggregated variable
cannot occur inside the same projection expression; project the grouping key
as a separate item. `RETURN *, count(*)` is supported and groups by the
complete input row.

`CALL { ... }` imports outer variables only through an initial `WITH`, does not
leak subquery-local variables, supports `UNION`, and treats a subquery without
a result-producing final clause as a unit subquery. Newer scope-clause syntax,
`CALL ... IN TRANSACTIONS`, and external procedures are not supported.

Read-only built-in procedures are:

```text
db.labels()
db.relationshipTypes()
db.propertyKeys()
sheets.nodes()
sheets.edges()
sheets.revisions([limit [, afterCursor]])
```

`sheets.revisions` yields `revision`, `time`, `actor`, `message`, and `next`.
The default limit is 100, the maximum is 1,000, and `next` is the opaque cursor
to pass as the second argument.

`EXPLAIN` returns the clause list without executing it. `PROFILE` executes the
query but does not yet emit per-operator timing.

## Mutations

`CREATE`, `MERGE` with `ON CREATE SET` / `ON MATCH SET`, `SET`, `REMOVE`,
`DELETE`, and `DETACH DELETE` are supported. A semicolon-delimited request is
one SQLite transaction and one revision when it makes an effective change;
later statements see earlier changes.

```cypher
MATCH (task:Task {status: 'ready'})
SET task.status = 'claimed', task.owner = $agent
WITH task
CREATE (task)-[:BLOCKED_BY]->(:Task {title: 'Integrate SDK'})
RETURN task
```

`CREATE` relationships must be directed and have exactly one type. `CREATE`
and `MERGE` do not accept variable-length relationships; `MERGE` also rejects
null top-level pattern properties. Decorating an already-bound node in a
`CREATE` pattern is rejected—use `SET`—so labels/properties are never silently
ignored.

SQLite serializes writers. `CHILD` parent uniqueness and cycle checks run in
that transaction, so independent clients cannot both commit from a stale
application check.

## Resource limits and deliberate boundaries

A query shares a budget of 1,000,000 traversed relationship expansions and
checks context cancellation inside scans and walks. Relationship trails cannot
reuse an edge, including across adjacent pattern segments. This makes path
explosion an explicit error; it is not a guarantee that every broad path query
will finish. Add type/property predicates and finite upper bounds.

Temporal constructors return Go `time.Time` values and duration uses bounded
nanoseconds. This does not preserve every distinct openCypher temporal type,
calendar-month duration, or timezone identifier. Paths, nodes, and
relationships cannot be stored as property values.

Sheets explicitly does not implement administration/schema syntax, `USE`,
`LOAD CSV`, `FOREACH`, planner hints, pattern comprehensions, map projections,
new quantified-path/boolean-label syntax, user-defined procedures, or
procedure transactions. Historical selection remains a host option
(`--at-revision` / `--at-time`) rather than a custom language clause.
