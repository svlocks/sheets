package engine

// Large-fixture benchmarks. They run only when SHEETS_BENCH_FIXTURE points at
// a database produced by tools/benchseed, and are skipped otherwise:
//
//	go run ./tools/benchseed -dir /path/to/fixture
//	SHEETS_BENCH_FIXTURE=/path/to/fixture/.sheets/sheets.db \
//	  go test -run '^$' -bench Fixture -benchmem ./internal/engine
//
// Queries aggregate or anchor selectively so results stay inside the
// 100k-row budget; the point is which engine regime each shape lands in
// (indexed working set, content scan, or full-snapshot fallback).

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/svlocks/sheets/internal/app"
	"github.com/svlocks/sheets/internal/domain"
	"github.com/svlocks/sheets/internal/store"
)

func fixtureEngine(b *testing.B) *Engine {
	b.Helper()
	path := os.Getenv("SHEETS_BENCH_FIXTURE")
	if path == "" {
		b.Skip("SHEETS_BENCH_FIXTURE not set")
	}
	database, err := store.Open(context.Background(), path)
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = database.Close() })
	executor, err := New(database)
	if err != nil {
		b.Fatal(err)
	}
	return executor
}

func runFixtureQuery(b *testing.B, request app.ExecuteRequest, primed bool) {
	b.Helper()
	executor := fixtureEngine(b)
	request.ReadOnly = true
	if primed {
		primeQuery(b, executor, request)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if _, err := executor.Execute(context.Background(), request); err != nil {
			b.Fatal(err)
		}
	}
}

// fixtureTitle anchors point lookups: rank is unique, so exactly one node
// matches, but the engine must still find it among a million.
const anchorQuery = "MATCH (n {rank: $rank}) RETURN n.title"

func BenchmarkFixturePointMatch(b *testing.B) {
	runFixtureQuery(b, app.ExecuteRequest{
		Query:  anchorQuery,
		Params: map[string]any{"rank": int64(500_000)},
	}, true)
}

func BenchmarkFixturePointMatchCold(b *testing.B) {
	runFixtureQuery(b, app.ExecuteRequest{
		Query:  anchorQuery,
		Params: map[string]any{"rank": int64(500_000)},
	}, false)
}

func BenchmarkFixtureSingleHop(b *testing.B) {
	runFixtureQuery(b, app.ExecuteRequest{
		Query:  "MATCH (n {rank: $rank})-[:CHILD]->(c) RETURN count(c)",
		Params: map[string]any{"rank": int64(1_000)},
	}, true)
}

func BenchmarkFixtureBoundedDescent(b *testing.B) {
	runFixtureQuery(b, app.ExecuteRequest{
		Query:  "MATCH (n {rank: $rank})-[:CHILD*1..4]->(d) RETURN count(d)",
		Params: map[string]any{"rank": int64(0)},
	}, true)
}

// Hub node: benchseed's power-law target distribution concentrates incoming
// DEPENDS_ON/RELATES_TO/BLOCKS edges on the lowest ranks.
func BenchmarkFixtureHubExpansion(b *testing.B) {
	runFixtureQuery(b, app.ExecuteRequest{
		Query:  "MATCH (h {rank: $rank})<-[:DEPENDS_ON]-(n) RETURN count(n)",
		Params: map[string]any{"rank": int64(0)},
	}, true)
}

func benchmarkContentScan(b *testing.B, marker string) {
	runFixtureQuery(b, app.ExecuteRequest{
		Query:  "MATCH (n) WHERE n.title CONTAINS $marker RETURN count(n)",
		Params: map[string]any{"marker": marker},
	}, true)
}

func BenchmarkFixtureContentScanRare(b *testing.B)   { benchmarkContentScan(b, "deltaqx") }   // ~0.01%
func BenchmarkFixtureContentScanMedium(b *testing.B) { benchmarkContentScan(b, "charlieqx") } // ~0.1%
func BenchmarkFixtureContentScanCommon(b *testing.B) { benchmarkContentScan(b, "alphaqx") }   // ~10%

func BenchmarkFixtureBodyScan(b *testing.B) {
	runFixtureQuery(b, app.ExecuteRequest{
		Query:  "MATCH (n) WHERE n.body CONTAINS $marker RETURN count(n)",
		Params: map[string]any{"marker": "charlieqx"},
	}, true)
}

func BenchmarkFixtureStatusAggregation(b *testing.B) {
	runFixtureQuery(b, app.ExecuteRequest{
		Query: "MATCH (n:Task) RETURN n.status, count(*) ORDER BY n.status",
	}, true)
}

func BenchmarkFixtureHistoricalPointMatch(b *testing.B) {
	revision := domain.Revision(200)
	runFixtureQuery(b, app.ExecuteRequest{
		Query:    anchorQuery,
		Params:   map[string]any{"rank": int64(500_000)},
		Snapshot: domain.Snapshot{Revision: &revision},
	}, true)
}

// TestFixtureRegimes is a one-shot, non-benchmark probe that reports which
// engine regime each suite query takes and its single-run latency. Run with:
//
//	SHEETS_BENCH_FIXTURE=... go test -run TestFixtureRegimes -v ./internal/engine
func TestFixtureRegimes(t *testing.T) {
	path := os.Getenv("SHEETS_BENCH_FIXTURE")
	if path == "" {
		t.Skip("SHEETS_BENCH_FIXTURE not set")
	}
	database, err := store.Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	executor, err := New(database)
	if err != nil {
		t.Fatal(err)
	}
	queries := []struct {
		name  string
		query string
		params map[string]any
	}{
		{"point", anchorQuery, map[string]any{"rank": int64(500_000)}},
		{"scan-rare", "MATCH (n) WHERE n.title CONTAINS $marker RETURN count(n)", map[string]any{"marker": "deltaqx"}},
		{"scan-common", "MATCH (n) WHERE n.title CONTAINS $marker RETURN count(n)", map[string]any{"marker": "alphaqx"}},
		{"aggregate", "MATCH (n:Task) RETURN n.status, count(*) ORDER BY n.status", nil},
		{"hub-3hop", "MATCH (h {rank: $rank})<-[:DEPENDS_ON*1..3]-(n) RETURN count(DISTINCT n)", map[string]any{"rank": int64(0)}},
	}
	for _, q := range queries {
		result, err := executor.Execute(context.Background(), app.ExecuteRequest{Query: q.query, Params: q.params, ReadOnly: true})
		if err != nil {
			t.Logf("%s: ERROR %v", q.name, err)
			continue
		}
		t.Logf("%s: %d rows, first=%v", q.name, len(result.Results[0].Rows), first(result))
	}
}

func first(result app.BatchResult) any {
	if len(result.Results) == 0 || len(result.Results[0].Rows) == 0 || len(result.Results[0].Rows[0]) == 0 {
		return nil
	}
	return fmt.Sprintf("%v", result.Results[0].Rows[0][0])
}
