# Cypher frontend and conformance evidence

Evidence date: 2026-09-01.

Sheets is not certified as openCypher-compatible. Its production syntax
frontend is generated from the official openCypher 9 M23 grammar, followed by
an explicit binder into sheets's existing AST. Grammar-recognized constructs
that the binder cannot represent faithfully fail with a located
`UnsupportedFeatureError`; there is no second handwritten parser that can
reinterpret them.

## Pinned sources and generation

The frontend and conformance inventory use these exact inputs:

- [openCypher 9 M23 grammar](https://s3.amazonaws.com/artifacts.opencypher.org/M23/Cypher.g4),
  SHA-256 `044d58feaccb263f2ec75f181f0f3153e8715b5013fc691d21da22592a58d62a`;
- [openCypher 9 M23 TCK](https://s3.amazonaws.com/artifacts.opencypher.org/M23/tck-M23.zip),
  SHA-256 `6deb4acffb301c926cb0811e11b2422704cad2e48fc0a42e40c401a7ee1fba49`;
- the `binary-tree-1` and `binary-tree-2` named-graph setup queries from the
  official `1.0.0-M23` tag (commit
  `007895aff5f33097d67b2e48a0a2babd6bd18590`), SHA-256
  `fbcada6966edb9e2d66b1a11a4f8a4c906a9da6afd622640ab08c686962d42da`
  and `923bcaf5686ea9051f46ad5c440a286381fd111daad5dbd6377aa3edd7dbfc4c`;
- ANTLR 4.13.1 complete JAR, SHA-256
  `bc13a9c57a8dd7d5196888211e5ede657cb64a3ce968608697e4f668251a8487`;
- `github.com/antlr4-go/antlr/v4` version `v4.13.1`; and
- the generator container
  `eclipse-temurin:17.0.16_8-jre-noble@sha256:88665729998a41823f35092c95d574581719371a0f21f62d31a9ccf506b199c6`.

`make cypher-fetch` downloads and verifies the source artifacts into the
ignored `.cache/opencypher/M23` directory. `make cypher-generate` applies the
reviewable compatibility patch in `tools/cypher/Cypher.extensions.patch` and
runs ANTLR in the pinned container. The derived grammar checksum is
`110c3dc3b70718166caf1042f92844527e889e9c80de92fa9fc3d79c2e74a1cf`.
`make cypher-generate-check` regenerates into a separate directory and requires
the committed generated Go files to match byte-for-byte.

Generated source provenance, the openCypher naming notice, and the applicable
Apache-2.0 license are kept in `internal/cypher/parsergen`.

## Frontend architecture

The generated lexer performs host-level, semicolon-delimited document
splitting, so semicolons inside strings, escaped identifiers, or comments are
not treated as separators. Each segment is then parsed as one official grammar
query. The CST binder creates the executor-facing AST directly, preserves
source spans, and performs capability checks before execution. The old
handwritten lexer and parser are absent from the production parse path.

The M23 grammar is deliberately extended for syntax that sheets supported
before this migration: `EXPLAIN`/`PROFILE`, `CALL { ... }`, MATCH-style
`EXISTS`, `reduce`, `NOT IN`, `!=`, `=~`, `OFFSET`, `YIELD *`, reserved-word
parameter names such as `$skip`, and the historical `AS all` alias. These
extensions are local compatibility behavior and are not represented as
official openCypher coverage. Administration syntax and newer quantified-path
syntax are not added.

The release ZIP contains the feature files but omits the named-graph setup
queries referenced by 19 scenarios. `make cypher-fetch` therefore retrieves
those two files from the exact official tag commit and verifies them
independently. They are not synthesized by sheets.

## TCK frontend inventory

`make cypher-tck` verifies every pinned input, expands every Examples row in
all 276 Scenario Outlines, executes the representable semantic subset, and
emits one machine-readable record for every concrete scenario. IDs are stable:
ordinary scenarios use `feature::scenario`, while outline examples add a
one-based `::example[001]` suffix. Background steps and tags are preserved.
Selected exact IDs and frontend classifications are pinned in
`tools/cypher/capabilities.json` and tied to local Go tests.

| Classification | Count |
| --- | ---: |
| Feature files | 220 |
| Scenario definitions | 1,615 |
| Scenario Outline definitions | 276 |
| Concrete scenario instances | 3,897 |
| Bound by the frontend | 3,818 |
| Typed unsupported | 51 |
| Parse rejected | 28 |

All 51 explicit capability rejections require external procedures from the TCK
adapter catalog; sheets deliberately has no user-procedure plugin runtime. The
28 parse rejections are negative grammar/numeric scenarios whose expected
outcome is rejection. Every instance remains present in the report; none is
silently dropped.

## Semantic runner evidence

Each bound scenario receives a fresh temporary Store and Engine. The runner
applies Background/setup queries, the two official named graphs, and scalar,
list, map, and null parameters; executes main and control queries; normalizes
TCK graph/path values and the M23 adapter's string representation of temporal
values; respects ordered/unordered rows and unordered list elements; compares
observable node, relationship, property, and label effects; and checks only
error categories for which sheets exposes stable evidence. It uses a
non-executing `EXPLAIN` validation pass to distinguish compile-time from
runtime errors and verifies the TCK rule that expected errors leave no
observable side effects.

The 2026-09-01 reproducible run reports:

| Semantic status | Count |
| --- | ---: |
| Pass | 3,817 |
| Semantic failure | 0 |
| Harness unsupported | 1 |
| Typed unsupported (frontend) | 51 |
| Parse rejected (frontend) | 28 |
| Silent skips | 0 |

The single harness-unsupported case is
`expressions/graph/Graph5.feature::[2] Single-labels expression on relationships`,
explicitly tagged `@ignore` by M23. Scenarios requiring custom procedures are
classified earlier as typed unsupported because sheets has no such procedure
catalog.
All 1,004 selected temporal scenarios pass. The complete report is
byte-identical on the host, Debian/linux-amd64, and Alpine/linux-arm64 even when
host timezone inputs are poisoned; its SHA-256 is
`586b61b41813729d622afc2bb726256e7411af06987f4dc948bcf43fa65055cb`.
`make cypher-tck-check` independently runs the report twice and requires exact
JSON equality.

Named-zone semantics come from IANA 2023c's default main profile
(`PACKRATDATA=` and `PACKRATLIST=`), the profile matched by M23's historical
fixtures. Both IANA source tarballs, the digest-pinned build image, and the
resulting archive are authenticated; amd64 and arm64 generation reproduce
zoneinfo SHA-256
`3fe2fe0c5897093e4965480de18722eabc224a1b7ac4dcb1ceb6943d62c01efe`.

## Interpretation and remaining boundaries

Zero semantic failures means every frontend-bound scenario executed by this
pinned runner produced the expected rows, side effects, or error category. It
does not certify sheets, cover syntax absent from M23, or provide arbitrary
external procedures. Administration commands, map projections, `LOAD CSV`,
`FOREACH`, procedure transactions, and newer quantified-path syntax remain
outside the documented surface.

The runner compares stable error categories rather than vendor-specific error
text. Read-only JSONL can stream an eligible terminal projection, but blocking
operators and intermediate graph matching retain explicitly bounded working
sets. Conformance and runtime architecture are therefore stated separately;
neither is used to imply unbounded execution.
