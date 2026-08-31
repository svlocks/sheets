# Performance

Sheets benchmarks the storage boundary and full Cypher engine. Run them with:

```sh
go test -run '^$' -bench . -benchmem ./internal/store ./internal/engine
```

Representative results on an Apple M2 Pro (`darwin/arm64`, Go 1.26.1) during
the August 2026 implementation pass:

| Workload | Time/op | Allocation/op |
|---|---:|---:|
| Warm indexed point match, 1,000-node graph | 0.106 ms | 17.8 KB |
| Warm four-level hierarchy traversal, 1,000 nodes | 0.130 ms | 51.9 KB |
| Warm indexed point match, 10,000-node graph | 1.23 ms | 162 KB |
| Cold full snapshot + point match, 1,000 nodes | 6.75 ms | 5.59 MB |
| Cold full snapshot + point match, 10,000 nodes | 69.2 ms | 58.8 MB |
| SQLite atomic batch creating 25 nodes | 0.74 ms | 166 KB |
| Historical list at revision 500 (500 nodes) | 0.89 ms | 569 KB |

“Warm” means the long-running engine (notably the TUI or several operations in
one process) has cached the current revision. Every request still checks the
database revision, so another process's commit invalidates that cache. “Cold”
includes reading and decoding the full snapshot, which is the conservative
path used by a new CLI process for an arbitrary query.

The cache is never the concurrency authority. Writes use `BEGIN IMMEDIATE`,
re-read their base revision inside SQLite, and clone a cache only if that exact
revision still matches. Current graphs are indexed in memory by label and
property value; historical graphs receive the same indexes after loading.

CI runs all packages under the race detector. The OrbStack Linux build runs
the race suite and `go vet` inside `golang:1.25-bookworm`, then builds a static
binary into a distroless image. Benchmark numbers are evidence, not a stable
service-level guarantee; hardware, filesystem, graph shape, and property size
matter.

