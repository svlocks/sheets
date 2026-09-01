// Command benchseed builds a deterministic large benchmark fixture: a dense
// temporal property graph with controlled content selectivities for the
// 1M-node benchmark suite in internal/engine.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"maps"
	"math/rand/v2"
	"os"
	"runtime/pprof"
	"strings"
	"time"

	"github.com/svlocks/sheets/internal/domain"
	"github.com/svlocks/sheets/internal/project"
	"github.com/svlocks/sheets/internal/store"
)

var words = []string{
	"build", "payment", "system", "integrate", "database", "cache", "queue",
	"deploy", "review", "refactor", "migrate", "index", "search", "login",
	"session", "billing", "invoice", "report", "export", "import", "sync",
	"retry", "timeout", "metrics", "alert", "dashboard", "widget", "schema",
	"upgrade", "cleanup", "audit", "backfill", "throttle", "batch", "stream",
}

// markers appear in titles at fixed rates so content-scan benchmarks have
// known selectivities.
var markers = []struct {
	word string
	rate float64
}{
	{"alphaqx", 0.10},
	{"bravoqx", 0.01},
	{"charlieqx", 0.001},
	{"deltaqx", 0.0001},
}

var statuses = []string{"todo", "doing", "done", "blocked"}

func title(r *rand.Rand) string {
	var t strings.Builder
	t.WriteString(words[r.IntN(len(words))] + " " + words[r.IntN(len(words))] + " " + words[r.IntN(len(words))])
	for _, m := range markers {
		if r.Float64() < m.rate {
			t.WriteString(" " + m.word)
		}
	}
	return t.String()
}

func body(r *rand.Rand, i int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Item %d\n\nWork on %s %s.\n", i, words[r.IntN(len(words))], words[r.IntN(len(words))])
	for range 2 + r.IntN(6) {
		fmt.Fprintf(&b, "- %s the %s %s\n", words[r.IntN(len(words))], words[r.IntN(len(words))], words[r.IntN(len(words))])
	}
	if r.Float64() < 0.001 {
		b.WriteString("\ncharlieqx\n")
	}
	return b.String()
}

func label(r *rand.Rand) string {
	switch v := r.Float64(); {
	case v < 0.70:
		return "Task"
	case v < 0.90:
		return "Bug"
	case v < 0.98:
		return "Note"
	default:
		return "Epic"
	}
}

func main() {
	dir := flag.String("dir", "", "project directory to create the fixture in (required)")
	nodes := flag.Int("nodes", 1_000_000, "node count")
	extraEdges := flag.Int("extra-edges", 3_000_000, "non-CHILD edge count")
	batch := flag.Int("batch", 10_000, "entities per write batch (one revision each)")
	updates := flag.Int("updates", 1_000, "post-seed update batches for revision history depth")
	seed := flag.Uint64("seed", 1, "deterministic RNG seed")
	cpuprofile := flag.String("cpuprofile", "", "write a CPU profile to `path`")
	flag.Parse()
	if *cpuprofile != "" {
		f, err := os.Create(*cpuprofile)
		if err != nil {
			log.Fatal(err)
		}
		defer f.Close()
		if err := pprof.StartCPUProfile(f); err != nil {
			log.Fatal(err)
		}
		defer pprof.StopCPUProfile()
	}
	if *dir == "" {
		log.Fatal("-dir is required")
	}

	ctx := context.Background()
	// Discover first: Init on an existing project runs a full integrity scan,
	// which is far too slow to repeat on every continuation of a large fixture.
	proj, err := project.Discover(*dir)
	if err != nil {
		proj, err = project.Init(*dir)
	}
	if err != nil {
		log.Fatal(err)
	}
	openStart := time.Now()
	db, err := store.Open(ctx, proj.DBPath)
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()
	fmt.Printf("store opened in %.1fs\n", time.Since(openStart).Seconds())

	r := rand.New(rand.NewPCG(*seed, 0))
	start := time.Now()
	ids := make([]domain.EntityID, 0, *nodes)

	// Nodes plus their single incoming CHILD edge, batched. A random earlier
	// parent yields O(log n) expected depth.
	for done := 0; done < *nodes; {
		count := min(*batch, *nodes-done)
		_, err := db.Write(ctx, store.RevisionMeta{Actor: "benchseed", Message: "seed nodes"}, func(tx *store.WriteTx) error {
			for i := range count {
				idx := done + i
				node, err := tx.CreateNode(store.NodeInput{
					Labels: []string{label(r)},
					Properties: domain.Properties{
						"title":    title(r),
						"status":   statuses[r.IntN(len(statuses))],
						"priority": int64(r.IntN(5)),
						"assignee": fmt.Sprintf("user-%03d", r.IntN(500)),
						"rank":     int64(idx),
					},
					Body: body(r, idx),
				})
				if err != nil {
					return err
				}
				ids = append(ids, node.ID)
				if idx > 0 {
					position := int64(idx)
					parent := ids[r.IntN(idx)]
					if _, err := tx.CreateEdge(store.EdgeInput{From: parent, Type: "CHILD", To: node.ID, Position: &position}); err != nil {
						return err
					}
				}
			}
			return nil
		})
		if err != nil {
			log.Fatalf("node batch at %d: %v", done, err)
		}
		done += count
		fmt.Printf("\rnodes %d/%d (%.0fs)", done, *nodes, time.Since(start).Seconds())
	}
	fmt.Println()

	// Dense non-CHILD edges with a power-law target distribution: squaring the
	// random draw concentrates edges on low-index hub nodes.
	edgeTypes := []string{"DEPENDS_ON", "DEPENDS_ON", "RELATES_TO", "BLOCKS"}
	for done := 0; done < *extraEdges; {
		count := min(2**batch, *extraEdges-done)
		_, err := db.Write(ctx, store.RevisionMeta{Actor: "benchseed", Message: "seed edges"}, func(tx *store.WriteTx) error {
			for range count {
				from := ids[r.IntN(len(ids))]
				v := r.Float64()
				to := ids[int(v*v*float64(len(ids)))]
				if to == from {
					continue
				}
				if _, err := tx.CreateEdge(store.EdgeInput{From: from, Type: edgeTypes[r.IntN(len(edgeTypes))], To: to}); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			log.Fatalf("edge batch at %d: %v", done, err)
		}
		done += count
		fmt.Printf("\redges %d/%d (%.0fs)", done, *extraEdges, time.Since(start).Seconds())
	}
	fmt.Println()

	// Small update batches create revision history for temporal benchmarks.
	for u := range *updates {
		_, err := db.Write(ctx, store.RevisionMeta{Actor: "benchseed", Message: fmt.Sprintf("update %d", u)}, func(tx *store.WriteTx) error {
			for range 5 {
				id := ids[r.IntN(len(ids))]
				node, err := tx.GetNode(id)
				if err != nil {
					return err
				}
				props := make(domain.Properties, len(node.Properties)+1)
				maps.Copy(props, node.Properties)
				props["status"] = statuses[r.IntN(len(statuses))]
				props["touched"] = int64(u)
				if _, err := tx.UpdateNode(id, store.NodeUpdate{Properties: &props}); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			log.Fatalf("update batch %d: %v", u, err)
		}
	}
	fmt.Printf("done in %.0fs: %d nodes, %d CHILD + %d extra edges, %d update revisions\n",
		time.Since(start).Seconds(), *nodes, *nodes-1, *extraEdges, *updates)
}
