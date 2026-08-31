# Cypher language

Sheets uses an OpenCypher-compatible dialect for both reads and mutations.
Keywords are case-insensitive; identifiers, labels, relationship types, and
property keys preserve case. Backtick-escaped identifiers are supported.

## Graph values

Nodes have a stable UUIDv7, labels, a schema-free property map, and a Markdown
body. Relationships have a stable UUIDv7, source, type, target, property map,
and an optional position.

Use standard graph functions plus two virtual properties:

- `elementId(n)` / `elementId(r)` returns the stable UUIDv7.
- `n.body` or `body(n)` reads the Markdown body. Setting/removing `n.body`
  updates the dedicated body field.
- `r.position` reads the optional sibling position. Setting it to `null` makes
  a `CHILD` relationship unordered.
- `labels(n)`, `type(r)`, `properties(x)`, `keys(x)`, `nodes(path)`,
  `relationships(path)`, `startNode(r)`, and `endNode(r)` are available.

Top-level null properties follow Cypher behavior: assigning null removes the
property. Null values remain valid inside nested lists and maps.

## Reads

The engine supports:

- `MATCH` and `OPTIONAL MATCH` with directed/undirected relationships,
  alternative types, labels, property patterns, named paths, and variable
  lengths;
- `WHERE`, three-valued boolean logic, comparison/string/regex/arithmetic
  operators, `IN`, label predicates, list indexing and slicing;
- `WITH`, `UNWIND`, `RETURN`, `DISTINCT`, `ORDER BY`, `SKIP`, and `LIMIT`;
- `UNION` and `UNION ALL`;
- `CASE`, list/map literals, list comprehensions, `all`/`any`/`none`/`single`,
  and `reduce`;
- `EXISTS` subqueries, pattern expressions, `shortestPath`, and
  `allShortestPaths`;
- aggregates including `count`, `collect`, `sum`, `avg`, `min`, `max`, sample
  and population standard deviation, and continuous/discrete percentiles;
- common scalar, string, collection, conversion, temporal, duration, and UUID
  functions; and
- `CALL`/`YIELD` for the read-only procedures `db.labels`,
  `db.relationshipTypes`, `db.propertyKeys`, `sheets.nodes`, and
  `sheets.edges`.

`EXPLAIN` returns the clause plan without executing it. `PROFILE` executes the
query; detailed per-operator timing is not currently emitted.

## Mutations

The engine supports `CREATE`, `MERGE` with `ON CREATE SET` / `ON MATCH SET`,
`SET`, `REMOVE`, `DELETE`, and `DETACH DELETE`. All matches and mutations in a
semicolon-delimited request share one graph and see earlier changes in that
request.

```cypher
MATCH (task:Task {status: 'ready'})
SET task.status = 'claimed', task.owner = $agent
WITH task
CREATE (task)-[:BLOCKED_BY]->(:Task {title: 'Integrate SDK'})
RETURN task
```

Reparenting is naturally atomic:

```cypher
MATCH ()-[old:CHILD]->(task), (parent)
WHERE elementId(task) = $task AND elementId(parent) = $parent
DELETE old
WITH task, parent
CREATE (parent)-[:CHILD {position: $position}]->(task)
RETURN task
```

SQLite serializes the writers. The `CHILD` uniqueness and cycle checks happen
inside that serialized transaction, so concurrent clients cannot both pass a
stale application-level check.

## Deliberate boundaries

Sheets does not implement database-administration syntax (`CREATE INDEX`,
constraints, users/roles), `LOAD CSV`, `USE`, `FOREACH`, planner hints,
`CALL ... IN TRANSACTIONS`, pattern comprehensions, or the newer quantified
path/boolean-label syntax. Those constructs do not describe sheets graph
work and would introduce a second schema/administration surface. Unknown
procedures fail explicitly rather than silently assuming read or write
behavior.

Historical selection is a host option (`--at-revision` / `--at-time`) rather
than a nonstandard Cypher clause, keeping query text valid and familiar.

