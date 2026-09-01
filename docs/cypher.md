# Cypher language

Sheets uses one graph language for reads and writes. Its syntax frontend is
generated from the official openCypher 9 M23 grammar plus documented local
extensions. Every frontend-bound scenario executed by the pinned M23 runner
passes. This is reproducible evidence, not certification or a claim about
later Cypher revisions. Unsupported syntax is rejected rather than
accepted with an invented meaning. See
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
- pattern comprehensions, `EXISTS` subqueries, pattern expressions,
  `shortestPath`, and `allShortestPaths`;
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
sheets.revisions([limit [, afterCursor [, order]]])
```

`sheets.revisions` yields `revision`, `time`, `actor`, `message`, and `next`.
The default limit is 100, the maximum is 1,000, and `next` is the opaque cursor
to pass as the second argument. `order` accepts `ascending`/`asc` or
`descending`/`desc`; a cursor is bound to its order and cannot be reused in the
other direction.

`EXPLAIN` validates without executing and reports planner operators, predicate
pushdowns, and any explicit fallback. `PROFILE` executes the query but does not
yet emit per-operator timing.

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

A query shares a 100,000-row evaluator budget and a budget of 1,000,000
traversed relationship expansions. Indexed pull plans additionally cap their
working set at 200,000 entities and 64 MiB. All limits and context cancellation
are checked inside scans and walks. Relationship trails cannot reuse an edge,
including across adjacent pattern segments. Exceeding a budget is an explicit
error, never a silently partial result. Broad path queries can still be
expensive; add type/property predicates and finite upper bounds.

Sheets preserves six distinct Cypher temporal types (`date`, `localtime`,
offset `time`, `localdatetime`, zoned `datetime`, and calendar-aware
`duration`) as exact durable values. Named-zone construction uses the embedded,
checksum-pinned IANA 2023c main profile and never consults host `TZ` or
`ZONEINFO`; each zoned value also retains its resolved offset so historical
formatting and equality stay stable if rules are deliberately upgraded later.
Paths, nodes, and relationships cannot be stored as property values.

Sheets's mutation APIs preflight durable values before opening a write
transaction and before encoding large structures. SQLite triggers independently
recheck source and derived size/cardinality limits:

| Value | Limit |
| --- | ---: |
| Canonical property map, encoded labels, or Markdown body | 64 MiB each |
| One property string or byte string | 16 MiB |
| Revision message | 1 MiB |
| Actor, label, relationship type, property key, or timezone name | 64 KiB |
| Property nesting / total values | 128 levels / 1,000,000 |
| Labels or indexed scalar properties per entity version | 4,096 each |
| Derived label / property-index payload per entity version | 16 MiB / 32 MiB |

The 64 MiB property source remains useful for large nested, non-indexed data;
the separate derived limits prevent a valid source value from amplifying into
an unbounded SQLite B-tree. The Go mutation boundary rejects invalid UTF-8;
invalid text injected through raw SQLite is reported by migration and store
integrity validation because SQLite itself does not enforce UTF-8 text encoding.

Sheets explicitly does not implement administration/schema syntax, `USE`,
`LOAD CSV`, `FOREACH`, planner hints, map projections, newer
quantified-path/boolean-label syntax, user-defined procedures, or procedure
transactions. The 51 typed-unsupported M23 scenarios all depend on external
TCK procedures. Historical selection remains a host option (`--at-revision` /
`--at-time`) rather than a custom language clause.
