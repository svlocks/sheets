# Development

Sheets requires Go 1.25.8 or newer. It has no code-generation or external
service prerequisite.

```sh
make test       # unit and integration tests
make test-race  # concurrency and frontend-model race checks
make lint       # go vet, gofmt, and whitespace checks
make bench      # storage and Cypher benchmarks
make build      # bin/sheets, without installing it
```

CI also runs the repository's pinned GolangCI-Lint standard suite from its
container image. To reproduce that gate locally without installing another
tool:

```sh
docker run --rm -v "$PWD:/app" -w /app \
  golangci/golangci-lint:v2.13.2@sha256:ba07dffad130794ae79ebaa0056809d18c0168f3f846480ffd3eb6c04578b83d \
  golangci-lint run
```

`make container-test` uses the running Docker-compatible engine (OrbStack is
supported) to run the Linux race and vet stages. The final Docker target is a
static, distroless image; it is an optional distribution format, not a daemon.

## Design rules

- Keep the domain and storage layers independent of CLI/TUI concerns.
- Add graph behavior through Cypher first. Convenience UI/CLI actions must call
  the same application boundary.
- Treat one `Store.Write` callback as the revision boundary. Never allocate a
  revision before an effective change.
- Preserve current and historical behavior in every schema migration.
- Prefer measured query-plan/index improvements over speculative caching.
- Add a parser test, engine test, and historical assertion for new Cypher
  mutation semantics.

## Releases

GoReleaser v2.18.0 builds static `darwin`, `linux`, and `windows` archives for
amd64 and arm64, injects version/commit/date metadata, normalizes archive
timestamps, and writes checksums. CI creates a complete non-publishing snapshot
on every change. Release hooks verify modules and tests but never rewrite
`go.mod`/`go.sum`. This repository intentionally does not install a development
binary into the host system.
