# Performance

Sheets benchmarks the storage boundary and full Cypher engine. Run them with:

```sh
go test -run '^$' -bench . -benchmem ./internal/store ./internal/engine
```

Representative results on an Apple M2 Pro (`darwin/arm64`, Go 1.25.14) on
2026-09-01:

| Workload | Time/op | Allocated bytes/op |
|---|---:|---:|
| Graph-free scalar query with a 10,000-node database | 0.0670 ms | 19.0 KB |
| Primed indexed point match, 1,000 nodes | 0.243 ms | 146.6 KB |
| Primed indexed point match, 10,000 nodes | 0.342 ms | 147.3 KB |
| Cold indexed single-hop working set, 1,000 nodes | 0.714 ms | 286.2 KB |
| Cold indexed single-hop working set, 10,000 nodes | 3.31 ms | 286.2 KB |
| Primed bounded four-level hierarchy traversal, 1,000 nodes | 2.40 ms | 1.09 MB |
| Materialized 10,000-row scalar result | 9.37 ms | 22.35 MB |
| Streamed 10,000-row scalar result | 2.88 ms | 4.68 MB |
| Indexed storage point lookup, 10,000 nodes | 0.286 ms | 14.2 KB |
| SQLite atomic batch creating 25 nodes | 13.0 ms | 212.7 KB |
| Historical list at revision 500 (500 nodes) | 2.31 ms | 1.94 MB |
| One descending page in 100,000 revisions | 0.0649 ms | 23.7 KB |

Database creation is outside the timed region. “Primed” means one execution on
the same request, runtime, and SQLite connection is outside the timed region.
The indexed point and bounded hierarchy shapes still build a small,
store-selected working graph; priming does not imply a complete snapshot cache.
Their scaling and the constant allocation of the cold single-hop working set
show that these safe shapes avoid full-graph materialization. The cold
10,000-node time still includes the SQLite/index scan needed to select that
working graph. Scalar queries and `sheets.revisions` resolve a revision
without loading graph entities; `status` uses storage counts.

When a planner fallback does cache a complete current revision, every request
checks the database revision, so another process's commit invalidates that
cache. Unsupported or correlated shapes take this conservative full-snapshot
path; cold time and memory remain linear in the complete graph before traversal
work begins.

The cache is never the concurrency authority. Writes use a serialized SQLite
transaction, re-read their base revision inside SQLite, and clone a cache only
if that exact revision still matches. Current and historical materialized
graphs receive the same label, property, and adjacency indexes.

Read-only JSONL uses synchronous Start/Row/End events and natural writer
backpressure. For the measured 10,000-row projection it removes about 79% of
allocated bytes and 46% of allocations. This is not described as a fully pull-
based engine: matching retains bounded intermediate rows, and `ORDER BY`,
`DISTINCT`, aggregation, pagination, unions, applicable procedure results,
table/JSON/TUI output, and mutations still require materialization. Planner
fallbacks can also materialize an exact complete graph snapshot. Query row,
working-set, and path-expansion limits make those costs explicit rather than
silently truncating results.

Durable resource validation adds roughly 15% to the small nested-property
codec benchmark and about 4% to the 25-node write benchmark versus the prior
schema, while reducing that write's allocations from roughly 3,369 to 2,418.
The trade is intentional: large values are token-preflighted before
materialization and SQLite independently prevents source or derived-index
amplification. Encoded property blobs at or below 64 KiB use an allocation-free
structural-depth preflight followed by ordinary, size-dependent decoding;
larger blobs use streaming token preflight before materialization.

CI runs all packages under the race detector. The pinned Linux container build
runs the race suite and `go vet`, then builds a static binary into a pinned
distroless runtime. Benchmark numbers are evidence, not a stable service-level
guarantee; hardware, filesystem, graph shape, and property size matter.
