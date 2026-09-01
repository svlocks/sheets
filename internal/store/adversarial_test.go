package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/svlocks/sheets/internal/domain"
)

func TestNetZeroBatchesAndSwallowedMutationErrorDoNotCommit(t *testing.T) {
	ctx := context.Background()
	database := openTestStore(t, filepath.Join(t.TempDir(), "graph.db"))
	t.Cleanup(func() { _ = database.Close() })
	var first, second domain.Node
	if _, err := database.Write(ctx, RevisionMeta{}, func(tx *WriteTx) error {
		var err error
		first, err = tx.CreateNode(NodeInput{Body: "original"})
		if err != nil {
			return err
		}
		second, err = tx.CreateNode(NodeInput{})
		return err
	}); err != nil {
		t.Fatal(err)
	}

	result, err := database.Write(ctx, RevisionMeta{}, func(tx *WriteTx) error {
		temporary := "temporary"
		if _, err := tx.UpdateNode(first.ID, NodeUpdate{Body: &temporary}); err != nil {
			return err
		}
		original := "original"
		_, err := tx.UpdateNode(first.ID, NodeUpdate{Body: &original})
		return err
	})
	if err != nil || result.Changed || result.Revision != 1 {
		t.Fatalf("update/revert result = %#v, %v", result, err)
	}
	got, err := database.GetNode(ctx, first.ID, domain.Snapshot{})
	if err != nil || got.ValidFrom != 1 || got.Body != "original" {
		t.Fatalf("update/revert changed representation: %#v, %v", got, err)
	}

	var transientNode domain.EntityID
	result, err = database.Write(ctx, RevisionMeta{}, func(tx *WriteTx) error {
		node, err := tx.CreateNode(NodeInput{})
		if err != nil {
			return err
		}
		transientNode = node.ID
		return tx.DeleteNode(node.ID)
	})
	if err != nil || result.Changed || result.Revision != 1 {
		t.Fatalf("create/delete node result = %#v, %v", result, err)
	}
	if _, err := database.GetNode(ctx, transientNode, domain.Snapshot{}); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("transient node survived: %v", err)
	}

	result, err = database.Write(ctx, RevisionMeta{}, func(tx *WriteTx) error {
		edge, err := tx.CreateEdge(EdgeInput{From: first.ID, Type: "LINK", To: second.ID})
		if err != nil {
			return err
		}
		return tx.DeleteEdge(edge.ID)
	})
	if err != nil || result.Changed || result.Revision != 1 {
		t.Fatalf("create/delete edge result = %#v, %v", result, err)
	}

	result, err = database.Write(ctx, RevisionMeta{}, func(tx *WriteTx) error {
		_, ignored := tx.CreateNode(NodeInput{ID: "not-a-uuid"})
		if ignored == nil {
			return errors.New("invalid ID unexpectedly succeeded")
		}
		return nil
	})
	if err == nil || !errors.Is(err, ErrInvalidArgument) || result.Changed {
		t.Fatalf("swallowed mutation error = %#v, %v", result, err)
	}
	if revision, _ := database.CurrentRevision(ctx); revision != 1 {
		t.Fatalf("net-zero/error batches consumed revision %d", revision)
	}
}

func TestPropertyCodecRejectsCyclesAndLossyStringsAndDoesNotAlias(t *testing.T) {
	ctx := context.Background()
	database := openTestStore(t, filepath.Join(t.TempDir(), "graph.db"))
	t.Cleanup(func() { _ = database.Close() })

	cycle := domain.Properties{}
	cycle["self"] = cycle
	if _, err := database.Write(ctx, RevisionMeta{}, func(tx *WriteTx) error {
		_, err := tx.CreateNode(NodeInput{Properties: cycle})
		return err
	}); err == nil || !strings.Contains(err.Error(), "cyclic") {
		t.Fatalf("cyclic property error = %v", err)
	}
	invalidString := string([]byte{0xff})
	if _, err := database.Write(ctx, RevisionMeta{}, func(tx *WriteTx) error {
		_, err := tx.CreateNode(NodeInput{Properties: domain.Properties{"bad": invalidString}})
		return err
	}); err == nil || !strings.Contains(err.Error(), "UTF-8") {
		t.Fatalf("invalid UTF-8 property error = %v", err)
	}
	if _, err := database.Write(ctx, RevisionMeta{}, func(tx *WriteTx) error {
		_, err := tx.CreateNode(NodeInput{Properties: domain.Properties{invalidString: "bad key"}})
		return err
	}); err == nil || !strings.Contains(err.Error(), "UTF-8") {
		t.Fatalf("invalid UTF-8 key error = %v", err)
	}
	var tooDeep any = "leaf"
	for range maxPropertyDepth + 2 {
		tooDeep = []any{tooDeep}
	}
	if _, err := database.Write(ctx, RevisionMeta{}, func(tx *WriteTx) error {
		_, err := tx.CreateNode(NodeInput{Properties: domain.Properties{"deep": tooDeep}})
		return err
	}); err == nil || !strings.Contains(err.Error(), "nesting") {
		t.Fatalf("deep property error = %v", err)
	}

	bytesValue := []byte{1, 2, 3}
	nested := []any{domain.Properties{"bytes": bytesValue}}
	nan := math.Float64frombits(0x7ff8000000000042)
	negativeZero := math.Copysign(0, -1)
	var created domain.Node
	if _, err := database.Write(ctx, RevisionMeta{}, func(tx *WriteTx) error {
		var err error
		created, err = tx.CreateNode(NodeInput{Properties: domain.Properties{"nested": nested, "nan": nan, "negative_zero": negativeZero}})
		bytesValue[0] = 99
		nested[0].(domain.Properties)["bytes"] = []byte{8}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	returned := created.Properties["nested"].([]any)[0].(domain.Properties)["bytes"].([]byte)
	if !reflect.DeepEqual(returned, []byte{1, 2, 3}) {
		t.Fatalf("CreateNode result aliases input: %v", returned)
	}
	stored, err := database.GetNode(ctx, created.ID, domain.Snapshot{})
	if err != nil {
		t.Fatal(err)
	}
	storedBytes := stored.Properties["nested"].([]any)[0].(domain.Properties)["bytes"].([]byte)
	if !reflect.DeepEqual(storedBytes, []byte{1, 2, 3}) {
		t.Fatalf("stored properties alias input: %v", storedBytes)
	}
	if bits := math.Float64bits(stored.Properties["nan"].(float64)); bits != math.Float64bits(nan) {
		t.Fatalf("NaN payload did not round-trip: %x", bits)
	}
	if bits := math.Float64bits(stored.Properties["negative_zero"].(float64)); bits != math.Float64bits(negativeZero) {
		t.Fatalf("negative zero did not round-trip: %x", bits)
	}
	if revision, _ := database.CurrentRevision(ctx); revision != 1 {
		t.Fatalf("failed codec writes consumed revisions: %d", revision)
	}
}

func TestPropertyCodecCanonicalScalarsAndNULKeys(t *testing.T) {
	ctx := context.Background()
	database := openTestStore(t, filepath.Join(t.TempDir(), "canonical-values.db"))
	t.Cleanup(func() { _ = database.Close() })

	// JSON field ordering alone is not sufficient canonicalization: ParseInt
	// accepts this alternative spelling even though the writer emits "1".
	if _, err := decodeProperties([]byte(`{"k":"map","o":{"rank":{"k":"int","s":"+1"}}}`)); err == nil || !strings.Contains(err.Error(), "non-canonical scalar") {
		t.Fatalf("non-canonical scalar encoding error = %v", err)
	}

	key := "contains\x00nul"
	var node domain.Node
	if _, err := database.Write(ctx, RevisionMeta{}, func(tx *WriteTx) error {
		var err error
		node, err = tx.CreateNode(NodeInput{Properties: domain.Properties{key: int64(7)}})
		return err
	}); err != nil {
		t.Fatalf("create NUL-keyed property: %v", err)
	}
	view, err := database.View(ctx, domain.Snapshot{})
	if err != nil {
		t.Fatal(err)
	}
	got, _, err := view.ScanNodes(ctx, NodePredicate{Properties: domain.Properties{key: int64(7)}}, domain.Page{})
	if err != nil || len(got) != 1 || got[0].ID != node.ID {
		t.Fatalf("NUL-keyed property predicate = %#v, %v", got, err)
	}
}

func TestPropertyCodecTimeCanonicalizationAndIndexedPredicates(t *testing.T) {
	ctx := context.Background()
	database := openTestStore(t, filepath.Join(t.TempDir(), "temporal-canonical.db"))
	t.Cleanup(func() { _ = database.Close() })

	fixedOffset := time.Date(2026, time.August, 31, 12, 34, 56, 123456789, time.FixedZone("", -5*60*60))
	newYork, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	values := []struct {
		name  string
		value time.Time
	}{
		{name: "fixed", value: fixedOffset},
		{name: "utc", value: time.Date(2026, time.August, 31, 17, 34, 56, 123456789, time.UTC)},
		{name: "dst", value: time.Date(2026, time.July, 1, 12, 34, 56, 987654321, newYork)},
		{name: "standard", value: time.Date(2026, time.January, 1, 12, 34, 56, 1, newYork)},
	}
	ids := make(map[string]domain.EntityID, len(values))
	if _, err := database.Write(ctx, RevisionMeta{}, func(tx *WriteTx) error {
		for _, sample := range values {
			node, err := tx.CreateNode(NodeInput{Properties: domain.Properties{"when": sample.value, "name": sample.name}})
			if err != nil {
				return err
			}
			ids[sample.name] = node.ID
		}
		return nil
	}); err != nil {
		t.Fatalf("write timestamp properties: %v", err)
	}
	view, err := database.View(ctx, domain.Snapshot{})
	if err != nil {
		t.Fatal(err)
	}
	for _, sample := range values {
		node, err := database.GetNode(ctx, ids[sample.name], domain.Snapshot{})
		if err != nil {
			t.Fatal(err)
		}
		got, ok := node.Properties["when"].(time.Time)
		if !ok || !got.Equal(sample.value) || got.Nanosecond() != sample.value.Nanosecond() {
			t.Fatalf("%s timestamp round-trip = %#v, want %s", sample.name, node.Properties["when"], sample.value)
		}
		if sample.name == "fixed" {
			_, offset := got.Zone()
			if offset != -5*60*60 || !isCanonicalFixedZone(got.Location().String()) {
				t.Fatalf("fixed-offset location was not stabilized: %s (%d)", got.Location(), offset)
			}
		} else if got.Location().String() != sample.value.Location().String() {
			t.Fatalf("%s location = %q, want %q", sample.name, got.Location(), sample.value.Location())
		}
		canonical, err := encodeProperties(domain.Properties{"when": sample.value})
		if err != nil {
			t.Fatal(err)
		}
		decoded, err := decodeProperties(canonical)
		if err != nil {
			t.Fatalf("%s timestamp canonical decode: %v", sample.name, err)
		}
		reencoded, err := encodeProperties(decoded)
		if err != nil || !bytes.Equal(canonical, reencoded) {
			t.Fatalf("%s timestamp canonical re-encode = %q, %v", sample.name, reencoded, err)
		}
		after, err := encodeProperties(node.Properties)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := decodeProperties(after); err != nil {
			t.Fatalf("%s stored timestamp is not canonical: %v", sample.name, err)
		}
		matched, _, err := view.ScanNodes(ctx, NodePredicate{Properties: domain.Properties{"when": sample.value}}, domain.Page{})
		if err != nil || len(matched) != 1 || matched[0].ID != ids[sample.name] {
			t.Fatalf("%s timestamp predicate = %#v, %v", sample.name, matched, err)
		}
	}
}

func TestPropertyCodecAcceptsLegacyLocalTimestampEncoding(t *testing.T) {
	// Older writers marshaled a Local time directly. Local.String changes from
	// "Local" to values such as "UTC" or "America/Chicago" depending on host
	// initialization, but neither that process configuration nor its name is a
	// durable zone identity.
	legacy := time.Date(2026, time.August, 31, 12, 34, 56, 123456789, time.Local)
	canonical, err := encodeProperties(domain.Properties{"when": legacy})
	if err != nil {
		t.Fatalf("encode Local timestamp: %v", err)
	}
	var envelope encodedValue
	if err := json.Unmarshal(canonical, &envelope); err != nil {
		t.Fatal(err)
	}
	if got := envelope.Map["when"].Zone; got != "Local" {
		t.Fatalf("Local timestamp wire zone = %q, want Local", got)
	}
	if _, err := decodeProperties(canonical); err != nil {
		t.Fatalf("canonical Local timestamp decode: %v", err)
	}

	data, err := legacy.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	_, offset := legacy.Zone()
	legacyEnvelope := encodedValue{Kind: "map", Map: map[string]encodedValue{
		"when": {Kind: "time", Text: base64.StdEncoding.EncodeToString(data), Zone: "Local", Offset: offset},
	}}
	wire, err := json.Marshal(legacyEnvelope)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := decodeProperties(wire)
	if err != nil {
		t.Fatalf("legacy Local timestamp decode: %v", err)
	}
	got := decoded["when"].(time.Time)
	if !got.Equal(legacy) || got.Nanosecond() != legacy.Nanosecond() {
		t.Fatalf("legacy Local timestamp = %s, want %s", got, legacy)
	}
	if got.Location() == time.Local || got.Location().String() != "Local" {
		t.Fatalf("legacy Local timestamp location = %q (%p), want stable fixed Local distinct from process Local %p",
			got.Location(), got.Location(), time.Local)
	}
	_, gotOffset := got.Zone()
	if gotOffset != offset {
		t.Fatalf("legacy Local timestamp offset = %d, want %d", gotOffset, offset)
	}
	reencoded, err := encodeProperties(decoded)
	if err != nil || !bytes.Equal(reencoded, wire) {
		t.Fatalf("legacy Local timestamp re-encode = %s, %v; want %s", reencoded, err, wire)
	}
	tamperedTime := legacyEnvelope.Map["when"]
	tamperedTime.Offset += 60
	tampered, err := json.Marshal(encodedValue{Kind: "map", Map: map[string]encodedValue{"when": tamperedTime}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := decodeProperties(tampered); err == nil || !strings.Contains(err.Error(), "non-canonical scalar encoding") {
		t.Fatalf("mismatched legacy Local offset error = %v", err)
	}
}

func TestPropertyCodecAcceptsLegacyTimeBinaryV2NegativeOffset(t *testing.T) {
	// Go's binary time v2 stores a signed sub-minute offset remainder. Go 1.27
	// sign-extends this final byte while older toolchains interpreted it as
	// unsigned. The separately persisted offset makes both decoder behaviors
	// converge on the same stable value and canonical legacy bytes.
	wire := []byte(`{"k":"map","o":{"when":{"k":"time","s":"AgAAAA3OSmHBAAAABv/V+A==","u":-2588}}}`)
	decoded, err := decodeProperties(wire)
	if err != nil {
		t.Fatalf("legacy binary-v2 timestamp decode: %v", err)
	}
	got := decoded["when"].(time.Time)
	want := time.Date(1880, time.January, 2, 3, 4, 5, 6, time.FixedZone("", -(43*60+8)))
	_, offset := got.Zone()
	if !got.Equal(want) || got.Nanosecond() != want.Nanosecond() || offset != -(43*60+8) {
		t.Fatalf("legacy binary-v2 timestamp = %s (offset %d), want %s", got, offset, want)
	}
	reencoded, err := encodeProperties(decoded)
	if err != nil || !bytes.Equal(reencoded, wire) {
		t.Fatalf("legacy binary-v2 timestamp re-encode = %s, %v; want %s", reencoded, err, wire)
	}
}

func TestPropertyCodecAcceptsNamedPrimitiveValues(t *testing.T) {
	type namedBool bool
	type namedString string
	encoded, err := encodeProperties(domain.Properties{
		"bool":   namedBool(true),
		"string": namedString("named"),
	})
	if err != nil {
		t.Fatalf("encode named primitive values: %v", err)
	}
	decoded, err := decodeProperties(encoded)
	if err != nil {
		t.Fatalf("decode named primitive values: %v", err)
	}
	if decoded["bool"] != true || decoded["string"] != "named" {
		t.Fatalf("named primitive round-trip = %#v", decoded)
	}
}

func TestPropertyPredicateRespectsValueByteBound(t *testing.T) {
	tooLarge := strings.Repeat("x", maxPropertyBytes+1)
	if _, err := compileProperties(domain.Properties{"large": tooLarge}); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("oversized predicate error = %v", err)
	}
}

func TestRevisionMetadataRejectsInvalidUTF8WithoutAllocation(t *testing.T) {
	ctx := context.Background()
	database := openTestStore(t, filepath.Join(t.TempDir(), "metadata-utf8.db"))
	t.Cleanup(func() { _ = database.Close() })
	invalid := string([]byte{0xff})
	result, err := database.Write(ctx, RevisionMeta{Actor: invalid}, func(tx *WriteTx) error {
		_, err := tx.CreateNode(NodeInput{})
		return err
	})
	if !errors.Is(err, ErrInvalidArgument) || result.Changed {
		t.Fatalf("invalid revision metadata result = %#v, %v", result, err)
	}
	if revision, err := database.CurrentRevision(ctx); err != nil || revision != 0 {
		t.Fatalf("invalid revision metadata consumed revision %d: %v", revision, err)
	}
}

func TestGeneratedUUIDv7IDsAreUniqueAndMonotonic(t *testing.T) {
	seen := make(map[domain.EntityID]struct{}, 10_000)
	var previous domain.EntityID
	for range 10_000 {
		id, err := newUUIDv7()
		if err != nil {
			t.Fatal(err)
		}
		if err := validateID(id); err != nil {
			t.Fatalf("generated invalid UUIDv7 %q: %v", id, err)
		}
		if previous != "" && id <= previous {
			t.Fatalf("UUIDv7 sequence is not monotonic: %s then %s", previous, id)
		}
		if _, duplicate := seen[id]; duplicate {
			t.Fatalf("duplicate UUIDv7 %s", id)
		}
		seen[id] = struct{}{}
		previous = id
	}
}

func TestUUIDv7PayloadCarriesAcrossFullBytes(t *testing.T) {
	var id [16]byte
	id[6] = 0x70
	id[7] = 0x5a
	id[8] = 0xbf // UUID variant plus the final low payload value.
	for index := 9; index < len(id); index++ {
		id[index] = 0xff
	}
	if !incrementUUIDv7Payload(&id) {
		t.Fatal("payload should carry from byte 8 into byte 7")
	}
	if id[7] != 0x5b || id[8] != 0x80 {
		t.Fatalf("payload carry = byte7:%02x byte8:%02x, want 5b/80", id[7], id[8])
	}
	for index := 9; index < len(id); index++ {
		if id[index] != 0 {
			t.Fatalf("payload byte %d = %02x, want 00 after carry", index, id[index])
		}
	}
}

func TestReadViewIndexedPredicatesHistoryAndStableCursor(t *testing.T) {
	ctx := context.Background()
	database := openTestStore(t, filepath.Join(t.TempDir(), "graph.db"))
	t.Cleanup(func() { _ = database.Close() })
	nodes := make([]domain.Node, 10)
	var edge domain.Edge
	if _, err := database.Write(ctx, RevisionMeta{}, func(tx *WriteTx) error {
		for index := range nodes {
			labels := []string{"Task"}
			if index%2 == 0 {
				labels = append(labels, "Even")
			}
			var err error
			nodes[index], err = tx.CreateNode(NodeInput{Labels: labels, Properties: domain.Properties{
				"rank": int64(index), "group": int64(index % 2),
				"nested": domain.Properties{"value": int64(index)},
			}})
			if err != nil {
				return err
			}
		}
		var err error
		edge, err = tx.CreateEdge(EdgeInput{From: nodes[0].ID, Type: "BLOCKS", To: nodes[1].ID, Properties: domain.Properties{"weight": int64(7)}})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	view1, err := database.View(ctx, domain.Snapshot{})
	if err != nil || view1.Revision() != 1 {
		t.Fatalf("View = %#v, %v", view1, err)
	}
	filtered, _, err := view1.ScanNodes(ctx, NodePredicate{
		AllLabels:  []string{"Even", "Task"},
		Properties: domain.Properties{"group": int64(0), "nested": domain.Properties{"value": int64(4)}},
	}, domain.Page{Limit: 5})
	if err != nil || len(filtered) != 1 || filtered[0].ID != nodes[4].ID {
		t.Fatalf("indexed/residual filter = %#v, %v", filtered, err)
	}
	count, err := view1.CountNodes(ctx, NodePredicate{AllLabels: []string{"Even"}, Properties: domain.Properties{"group": int64(0)}})
	if err != nil || count != 5 {
		t.Fatalf("CountNodes = %d, %v", count, err)
	}
	edges, _, err := view1.ScanEdges(ctx, EdgePredicate{FromIDs: []domain.EntityID{nodes[0].ID}, Types: []string{"BLOCKS"}, Properties: domain.Properties{"weight": int64(7)}}, domain.Page{})
	if err != nil || len(edges) != 1 || edges[0].ID != edge.ID {
		t.Fatalf("ScanEdges = %#v, %v", edges, err)
	}

	firstPage, firstInfo, err := database.ListNodes(ctx, domain.Snapshot{}, NodeFilter{Labels: []string{"Task"}}, domain.Page{Limit: 3})
	if err != nil || len(firstPage) != 3 || firstInfo.Next == "" {
		t.Fatalf("first page = %d, %#v, %v", len(firstPage), firstInfo, err)
	}
	newRank := int64(100)
	if _, err := database.Write(ctx, RevisionMeta{}, func(tx *WriteTx) error {
		if _, err := tx.CreateNode(NodeInput{Labels: []string{"Task"}, Properties: domain.Properties{"rank": newRank}}); err != nil {
			return err
		}
		updated := domain.Properties{"rank": int64(50), "group": int64(0), "nested": domain.Properties{"value": int64(0)}}
		_, err := tx.UpdateNode(nodes[0].ID, NodeUpdate{Properties: &updated})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	seen := append([]domain.Node(nil), firstPage...)
	cursor := firstInfo.Next
	for cursor != "" {
		page, info, err := database.ListNodes(ctx, domain.Snapshot{}, NodeFilter{Labels: []string{"Task"}}, domain.Page{Limit: 3, After: cursor})
		if err != nil {
			t.Fatal(err)
		}
		seen = append(seen, page...)
		cursor = info.Next
	}
	if len(seen) != len(nodes) {
		t.Fatalf("current cursor mixed revisions: got %d original nodes, want %d", len(seen), len(nodes))
	}
	if _, _, err := view1.ScanNodes(ctx, NodePredicate{AllLabels: []string{"Even"}}, domain.Page{Limit: 3, After: firstInfo.Next}); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("predicate-mismatched cursor error = %v", err)
	}

	oldRank, _, err := view1.ScanNodes(ctx, NodePredicate{Properties: domain.Properties{"rank": int64(0)}}, domain.Page{})
	if err != nil || len(oldRank) != 1 || oldRank[0].ID != nodes[0].ID {
		t.Fatalf("historical property index = %#v, %v", oldRank, err)
	}
	view2, err := database.View(ctx, domain.Snapshot{})
	if err != nil || view2.Revision() != 2 {
		t.Fatalf("current view = %#v, %v", view2, err)
	}
	currentRank, _, err := view2.ScanNodes(ctx, NodePredicate{Properties: domain.Properties{"rank": int64(50)}}, domain.Page{})
	if err != nil || len(currentRank) != 1 || currentRank[0].ID != nodes[0].ID {
		t.Fatalf("current property index = %#v, %v", currentRank, err)
	}
	gotMany, err := view1.GetNodes(ctx, []domain.EntityID{nodes[7].ID, nodes[2].ID, "missing"})
	if err != nil || len(gotMany) != 2 || gotMany[0].ID != nodes[2].ID || gotMany[1].ID != nodes[7].ID {
		t.Fatalf("GetNodes = %#v, %v", gotMany, err)
	}
	if err := database.CheckIntegrity(ctx); err != nil {
		t.Fatalf("CheckIntegrity: %v", err)
	}
}

func TestMigrationBackfillsIndexesAndRollsBackOnInvalidCodec(t *testing.T) {
	ctx := context.Background()
	t.Run("backfill", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "v1.db")
		id := domain.EntityID("019945ee-ea00-7be6-a100-000000000001")
		createV1Database(t, path, id, mustProperties(t, domain.Properties{"rank": int64(9)}))
		database, err := Open(ctx, path)
		if err != nil {
			t.Fatalf("migrate: %v", err)
		}
		defer func() { _ = database.Close() }()
		view, err := database.View(ctx, domain.Snapshot{})
		if err != nil {
			t.Fatal(err)
		}
		nodes, _, err := view.ScanNodes(ctx, NodePredicate{AllLabels: []string{"Legacy"}, Properties: domain.Properties{"rank": int64(9)}}, domain.Page{})
		if err != nil || len(nodes) != 1 || nodes[0].ID != id {
			t.Fatalf("backfilled query = %#v, %v", nodes, err)
		}
	})

	t.Run("rollback", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "invalid-v1.db")
		id := domain.EntityID("019945ee-ea00-7be6-a100-000000000002")
		createV1Database(t, path, id, []byte("not-json"))
		if database, err := Open(ctx, path); err == nil {
			_ = database.Close()
			t.Fatal("migration accepted invalid property encoding")
		}
		raw := openRawSQLite(t, path)
		defer func() { _ = raw.Close() }()
		var version, derivedTable int
		if err := raw.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
			t.Fatal(err)
		}
		if err := raw.QueryRow("SELECT COUNT(*) FROM sqlite_schema WHERE name = 'node_property_index'").Scan(&derivedTable); err != nil {
			t.Fatal(err)
		}
		if version != 1 || derivedTable != 0 {
			t.Fatalf("failed migration was not atomic: version=%d derived=%d", version, derivedTable)
		}
	})

	t.Run("historical invariant rollback", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "cyclic-v1.db")
		ids := []domain.EntityID{
			"019945ee-ea00-7be6-a100-000000000011",
			"019945ee-ea00-7be6-a100-000000000012",
			"019945ee-ea00-7be6-a100-000000000013",
		}
		createV1Database(t, path, ids[0], mustProperties(t, nil))
		raw := openRawSQLite(t, path)
		labels, _, err := encodeLabels(nil)
		if err != nil {
			t.Fatal(err)
		}
		for _, id := range ids[1:] {
			if _, err := raw.Exec("INSERT INTO nodes(id, created_revision) VALUES (?, 1)", string(id)); err != nil {
				t.Fatal(err)
			}
			if _, err := raw.Exec("INSERT INTO node_versions(id, valid_from, labels, properties, body) VALUES (?, 1, ?, ?, '')", string(id), labels, mustProperties(t, nil)); err != nil {
				t.Fatal(err)
			}
		}
		for index := range ids {
			edgeID := domain.EntityID(fmt.Sprintf("019945ee-ea00-7be6-a200-%012d", index+1))
			if _, err := raw.Exec("INSERT INTO edges(id, created_revision) VALUES (?, 1)", string(edgeID)); err != nil {
				t.Fatal(err)
			}
			if _, err := raw.Exec(`INSERT INTO edge_versions(id, valid_from, from_id, type, to_id, properties)
				VALUES (?, 1, ?, 'CHILD', ?, ?)`, string(edgeID), string(ids[index]), string(ids[(index+1)%len(ids)]), mustProperties(t, nil)); err != nil {
				t.Fatal(err)
			}
		}
		if err := raw.Close(); err != nil {
			t.Fatal(err)
		}
		if database, err := Open(ctx, path); err == nil {
			_ = database.Close()
			t.Fatal("migration accepted a historical CHILD cycle")
		}
		raw = openRawSQLite(t, path)
		defer func() { _ = raw.Close() }()
		var version int
		if err := raw.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != 1 {
			t.Fatalf("failed invariant migration was not atomic: version=%d err=%v", version, err)
		}
	})
}

func TestSchemaFingerprintRejectsAlteredDefinitions(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "altered-schema.db")
	database := openTestStore(t, path)
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	raw := openRawSQLite(t, path)
	if _, err := raw.Exec(`DROP TRIGGER node_versions_validate_insert;
		CREATE TRIGGER node_versions_validate_insert BEFORE INSERT ON node_versions BEGIN SELECT 1; END`); err != nil {
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}
	if database, err := Open(ctx, path); err == nil {
		_ = database.Close()
		t.Fatal("Open accepted an altered invariant trigger")
	} else if !errors.Is(err, ErrCorrupt) {
		t.Fatalf("altered schema error = %v, want ErrCorrupt", err)
	}
}

func TestConcurrentMigrationAcrossStores(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v1.db")
	id := domain.EntityID("019945ee-ea00-7be6-a100-000000000003")
	createV1Database(t, path, id, mustProperties(t, domain.Properties{"rank": int64(1)}))
	const workers = 12
	start := make(chan struct{})
	errorsCh := make(chan error, workers)
	var wait sync.WaitGroup
	for range workers {
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			database, err := Open(context.Background(), path, WithBusyTimeout(10*time.Second))
			if err == nil {
				err = database.Close()
			}
			if err != nil {
				errorsCh <- err
			}
		}()
	}
	close(start)
	wait.Wait()
	close(errorsCh)
	for err := range errorsCh {
		t.Errorf("concurrent migration: %v", err)
	}
	database := openTestStore(t, path)
	defer func() { _ = database.Close() }()
	if revision, err := database.CurrentRevision(context.Background()); err != nil || revision != 1 {
		t.Fatalf("migrated revision = %d, %v", revision, err)
	}
}

func TestDatabaseBoundaryRejectsHistoryMutationCycleAndDanglingSeal(t *testing.T) {
	ctx := context.Background()
	database := openTestStore(t, filepath.Join(t.TempDir(), "graph.db"))
	t.Cleanup(func() { _ = database.Close() })
	var nodes [3]domain.Node
	if _, err := database.Write(ctx, RevisionMeta{}, func(tx *WriteTx) error {
		for index := range nodes {
			var err error
			nodes[index], err = tx.CreateNode(NodeInput{})
			if err != nil {
				return err
			}
		}
		if _, err := tx.CreateEdge(EdgeInput{From: nodes[0].ID, Type: "CHILD", To: nodes[1].ID}); err != nil {
			return err
		}
		_, err := tx.CreateEdge(EdgeInput{From: nodes[1].ID, Type: "CHILD", To: nodes[2].ID})
		return err
	}); err != nil {
		t.Fatal(err)
	}

	conn, err := database.db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	beginRevision := func(t *testing.T) {
		t.Helper()
		if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
			t.Fatal(err)
		}
		var previousTime int64
		if err := conn.QueryRowContext(ctx, "SELECT committed_ns FROM revisions WHERE revision = 1").Scan(&previousTime); err != nil {
			t.Fatal(err)
		}
		if _, err := conn.ExecContext(ctx, "INSERT INTO revisions(revision, committed_ns, actor, message, sealed) VALUES (2, ?, '', '', 0)", previousTime+1); err != nil {
			t.Fatal(err)
		}
	}
	rollback := func() { _, _ = conn.ExecContext(context.Background(), "ROLLBACK") }

	beginRevision(t)
	if _, err := conn.ExecContext(ctx, "UPDATE node_versions SET body = 'tampered' WHERE id = ? AND valid_from = 1", string(nodes[0].ID)); err == nil || !strings.Contains(err.Error(), "sheets_node_history") {
		rollback()
		t.Fatalf("sealed history mutation error = %v", err)
	}
	rollback()

	beginRevision(t)
	edgeID, err := newUUIDv7()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(ctx, "INSERT INTO edges(id, created_revision) VALUES (?, 2)", string(edgeID)); err != nil {
		rollback()
		t.Fatal(err)
	}
	emptyProperties := mustProperties(t, nil)
	if _, err := conn.ExecContext(ctx, `INSERT INTO edge_versions(id, valid_from, from_id, type, to_id, properties)
		VALUES (?, 2, ?, 'CHILD', ?, ?)`, string(edgeID), string(nodes[2].ID), string(nodes[0].ID), emptyProperties); err == nil || !strings.Contains(err.Error(), "sheets_child_cycle") {
		rollback()
		t.Fatalf("raw cycle error = %v", err)
	}
	rollback()

	beginRevision(t)
	if _, err := conn.ExecContext(ctx, "UPDATE node_versions SET valid_to = 2 WHERE id = ? AND valid_to IS NULL", string(nodes[0].ID)); err != nil {
		rollback()
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(ctx, "UPDATE revisions SET sealed = 1 WHERE revision = 2"); err == nil || !strings.Contains(err.Error(), "sheets_edge_endpoint") {
		rollback()
		t.Fatalf("dangling endpoint seal error = %v", err)
	}
	rollback()

	if revision, err := database.CurrentRevision(ctx); err != nil || revision != 1 {
		t.Fatalf("failed raw transactions changed revision: %d, %v", revision, err)
	}
}

func TestRawRevisionSealRequiresGraphChangeAndMaintainsScalarIndexes(t *testing.T) {
	ctx := context.Background()
	database := openTestStore(t, filepath.Join(t.TempDir(), "raw-indexes.db"))
	t.Cleanup(func() { _ = database.Close() })
	var node domain.Node
	if _, err := database.Write(ctx, RevisionMeta{}, func(tx *WriteTx) error {
		var err error
		node, err = tx.CreateNode(NodeInput{Properties: domain.Properties{"rank": int64(1)}})
		return err
	}); err != nil {
		t.Fatal(err)
	}

	conn, err := database.db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	beginRevision := func(t *testing.T) {
		t.Helper()
		if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
			t.Fatal(err)
		}
		var previousTime int64
		if err := conn.QueryRowContext(ctx, "SELECT committed_ns FROM revisions WHERE revision = 1").Scan(&previousTime); err != nil {
			t.Fatal(err)
		}
		if _, err := conn.ExecContext(ctx, "INSERT INTO revisions(revision, committed_ns, actor, message, sealed) VALUES (2, ?, '', '', 0)", previousTime+1); err != nil {
			t.Fatal(err)
		}
	}
	rollback := func() { _, _ = conn.ExecContext(context.Background(), "ROLLBACK") }

	beginRevision(t)
	if _, err := conn.ExecContext(ctx, "UPDATE revisions SET sealed = 1 WHERE revision = 2"); err == nil || !strings.Contains(err.Error(), "sheets_revision_no_change") {
		rollback()
		t.Fatalf("empty raw revision seal error = %v", err)
	}
	rollback()

	labels, _, err := encodeLabels(nil)
	if err != nil {
		t.Fatal(err)
	}
	beginRevision(t)
	if _, err := conn.ExecContext(ctx, "UPDATE node_versions SET valid_to = 2 WHERE id = ? AND valid_to IS NULL", string(node.ID)); err != nil {
		rollback()
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(ctx, `INSERT INTO node_versions(id, valid_from, labels, properties, body)
		VALUES (?, 2, ?, ?, '')`, string(node.ID), labels, mustProperties(t, domain.Properties{"rank": int64(2)})); err != nil {
		rollback()
		t.Fatal(err)
	}
	// A raw in-place edit of the active version has no Go-side repair step.
	// The SQLite trigger must replace the scalar index before it is sealed.
	if _, err := conn.ExecContext(ctx, "UPDATE node_versions SET properties = ? WHERE id = ? AND valid_from = 2", mustProperties(t, domain.Properties{"rank": int64(3)}), string(node.ID)); err != nil {
		rollback()
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(ctx, "UPDATE revisions SET sealed = 1 WHERE revision = 2"); err != nil {
		rollback()
		t.Fatalf("seal changed raw revision: %v", err)
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		t.Fatal(err)
	}

	view, err := database.View(ctx, domain.Snapshot{})
	if err != nil {
		t.Fatal(err)
	}
	matched, _, err := view.ScanNodes(ctx, NodePredicate{Properties: domain.Properties{"rank": int64(3)}}, domain.Page{})
	if err != nil || len(matched) != 1 || matched[0].ID != node.ID {
		t.Fatalf("raw scalar-index update lookup = %#v, %v", matched, err)
	}
	if err := database.CheckIntegrity(ctx); err != nil {
		t.Fatalf("raw scalar-index update integrity: %v", err)
	}
}

func TestRevisionTimeRangeAndExhaustion(t *testing.T) {
	ctx := context.Background()
	database := openTestStore(t, filepath.Join(t.TempDir(), "graph.db"))
	t.Cleanup(func() { _ = database.Close() })
	maximum := time.Unix(0, math.MaxInt64).UTC()
	if _, err := database.Write(ctx, RevisionMeta{Time: maximum}, func(tx *WriteTx) error {
		_, err := tx.CreateNode(NodeInput{})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Write(ctx, RevisionMeta{Time: maximum}, func(tx *WriteTx) error {
		_, err := tx.CreateNode(NodeInput{})
		return err
	}); err == nil || !strings.Contains(err.Error(), "timestamp space exhausted") {
		t.Fatalf("timestamp exhaustion error = %v", err)
	}
	outOfRange := time.Date(3000, 1, 1, 0, 0, 0, 0, time.UTC)
	if _, err := database.ResolveSnapshot(ctx, domain.Snapshot{Time: &outOfRange}); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("out-of-range snapshot error = %v", err)
	}
	if revision, _ := database.CurrentRevision(ctx); revision != 1 {
		t.Fatalf("timestamp failure consumed revision %d", revision)
	}
}

func TestReservedFilesystemPathAndConnectionPragmas(t *testing.T) {
	// '#' and '%' and a space are reserved in URIs but valid in filenames on
	// every supported OS. '?' is also URI-reserved but forbidden in Windows
	// filenames, so it cannot be exercised here.
	path := filepath.Join(t.TempDir(), "graph # %.db")
	database, err := Open(context.Background(), path, WithBusyTimeout(1234*time.Millisecond), WithMaxOpenConns(4))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = database.Close() }()
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("database was not created at literal path: %v", err)
	}
	connections := make([]*sql.Conn, 4)
	for index := range connections {
		connections[index], err = database.db.Conn(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		defer func(connection *sql.Conn) { _ = connection.Close() }(connections[index])
	}
	for index, connection := range connections {
		var foreignKeys, recursiveTriggers, trustedSchema, busyTimeout int
		if err := connection.QueryRowContext(context.Background(), `SELECT
			(SELECT * FROM pragma_foreign_keys),
			(SELECT * FROM pragma_recursive_triggers),
			(SELECT * FROM pragma_trusted_schema),
			(SELECT * FROM pragma_busy_timeout)`).Scan(&foreignKeys, &recursiveTriggers, &trustedSchema, &busyTimeout); err != nil {
			t.Fatal(err)
		}
		if foreignKeys != 1 || recursiveTriggers != 1 || trustedSchema != 0 || busyTimeout != 1234 {
			t.Errorf("connection %d pragmas = fk:%d recursive:%d trusted:%d busy:%d", index, foreignKeys, recursiveTriggers, trustedSchema, busyTimeout)
		}
	}
}

func TestIndexedReadQueryPlans(t *testing.T) {
	ctx := context.Background()
	database := openTestStore(t, filepath.Join(t.TempDir(), "plans.db"))
	defer func() { _ = database.Close() }()
	nodes := make([]domain.Node, 200)
	if _, err := database.Write(ctx, RevisionMeta{}, func(tx *WriteTx) error {
		for index := range nodes {
			var err error
			nodes[index], err = tx.CreateNode(NodeInput{Labels: []string{"Task", fmt.Sprintf("Bucket%d", index%10)}, Properties: domain.Properties{"rank": int64(index)}})
			if err != nil {
				return err
			}
		}
		for index := 1; index < len(nodes); index++ {
			if _, err := tx.CreateEdge(EdgeInput{From: nodes[index%20].ID, Type: "LINK", To: nodes[index].ID, Properties: domain.Properties{"rank": int64(index)}}); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := database.db.ExecContext(ctx, "ANALYZE"); err != nil {
		t.Fatal(err)
	}
	nodePlan, err := compileNodePredicate(NodePredicate{AllLabels: []string{"Bucket3"}, Properties: domain.Properties{"rank": int64(123)}})
	if err != nil {
		t.Fatal(err)
	}
	nodeQuery, nodeArgs, err := nodeSQL(nodePlan, 1, "", true, 11, false)
	if err != nil {
		t.Fatal(err)
	}
	nodeDetails := explainPlan(t, database.db, nodeQuery, nodeArgs...)
	if !strings.Contains(nodeDetails, "node_version_labels_by_label") || !strings.Contains(nodeDetails, "node_property_lookup") {
		t.Fatalf("node plan does not use selective indexes:\n%s\nquery: %s", nodeDetails, nodeQuery)
	}

	edgePlan, err := compileEdgePredicate(EdgePredicate{FromIDs: []domain.EntityID{nodes[3].ID}, Properties: domain.Properties{"rank": int64(123)}})
	if err != nil {
		t.Fatal(err)
	}
	edgeQuery, edgeArgs, err := edgeSQL(edgePlan, 1, "", true, 11, false)
	if err != nil {
		t.Fatal(err)
	}
	edgeDetails := explainPlan(t, database.db, edgeQuery, edgeArgs...)
	if !strings.Contains(edgeDetails, "edge_property_lookup") {
		t.Fatalf("edge plan does not use selective indexes:\n%s\nquery: %s", edgeDetails, edgeQuery)
	}
	endpointPlan, err := compileEdgePredicate(EdgePredicate{FromIDs: []domain.EntityID{nodes[17].ID}})
	if err != nil {
		t.Fatal(err)
	}
	endpointQuery, endpointArgs, err := edgeSQL(endpointPlan, 1, "", true, 11, false)
	if err != nil {
		t.Fatal(err)
	}
	endpointDetails := explainPlan(t, database.db, endpointQuery, endpointArgs...)
	if !strings.Contains(endpointDetails, "edge_versions_from_history") {
		t.Fatalf("endpoint plan does not use its history index:\n%s\nquery: %s", endpointDetails, endpointQuery)
	}
}

func TestLargePredicatesAvoidSQLiteJoinLimit(t *testing.T) {
	ctx := context.Background()
	database := openTestStore(t, filepath.Join(t.TempDir(), "large-predicate.db"))
	defer func() { _ = database.Close() }()

	labels := make([]string, 70)
	properties := make(domain.Properties, 70)
	for index := range labels {
		labels[index] = fmt.Sprintf("Label%03d", index)
		properties[fmt.Sprintf("property%03d", index)] = int64(index)
	}
	var wanted domain.Node
	var edge domain.Edge
	if _, err := database.Write(ctx, RevisionMeta{}, func(tx *WriteTx) error {
		var err error
		wanted, err = tx.CreateNode(NodeInput{Labels: labels, Properties: properties})
		if err != nil {
			return err
		}
		other, err := tx.CreateNode(NodeInput{})
		if err != nil {
			return err
		}
		edge, err = tx.CreateEdge(EdgeInput{From: wanted.ID, Type: "LINK", To: other.ID, Properties: properties})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	view, err := database.View(ctx, domain.Snapshot{})
	if err != nil {
		t.Fatal(err)
	}
	nodes, _, err := view.ScanNodes(ctx, NodePredicate{AllLabels: labels, Properties: properties}, domain.Page{Limit: 2})
	if err != nil || len(nodes) != 1 || nodes[0].ID != wanted.ID {
		t.Fatalf("large node predicate = %#v, %v", nodes, err)
	}
	count, err := view.CountNodes(ctx, NodePredicate{AllLabels: labels, Properties: properties})
	if err != nil || count != 1 {
		t.Fatalf("large node predicate count = %d, %v", count, err)
	}
	edges, _, err := view.ScanEdges(ctx, EdgePredicate{Properties: properties}, domain.Page{Limit: 2})
	if err != nil || len(edges) != 1 || edges[0].ID != edge.ID {
		t.Fatalf("large edge predicate = %#v, %v", edges, err)
	}
}

func TestDeepAndWideChildGraphs(t *testing.T) {
	if testing.Short() {
		t.Skip("deep graph stress test")
	}
	if raceDetectorEnabled {
		t.Skip("the full 2,500-level stress proof is covered by the ordinary suite; the race suite uses a bounded graph below")
	}
	testDeepAndWideChildGraphs(t, 2500, 2000)
}

// TestChildGraphsRaceRegression keeps the mutation, Go traversal, SQLite
// recursive-trigger, and indexed-width paths under the race detector without
// making a normal `go test -race ./...` spend five minutes instrumenting the
// 9,000 individual entity writes in the full stress proof above.
func TestChildGraphsRaceRegression(t *testing.T) {
	testDeepAndWideChildGraphs(t, 256, 128)
}

func testDeepAndWideChildGraphs(t *testing.T, depth, width int) {
	t.Helper()
	ctx := context.Background()
	database := openTestStore(t, filepath.Join(t.TempDir(), "deep.db"))
	defer func() { _ = database.Close() }()
	nodes := make([]domain.Node, depth)
	var wideRoot domain.Node
	if _, err := database.Write(ctx, RevisionMeta{}, func(tx *WriteTx) error {
		for index := range nodes {
			var err error
			nodes[index], err = tx.CreateNode(NodeInput{})
			if err != nil {
				return err
			}
		}
		for index := 1; index < len(nodes); index++ {
			if _, err := tx.CreateEdge(EdgeInput{From: nodes[index-1].ID, Type: "CHILD", To: nodes[index].ID}); err != nil {
				return err
			}
		}
		var err error
		wideRoot, err = tx.CreateNode(NodeInput{})
		if err != nil {
			return err
		}
		for range width {
			child, err := tx.CreateNode(NodeInput{})
			if err != nil {
				return err
			}
			if _, err := tx.CreateEdge(EdgeInput{From: wideRoot.ID, Type: "CHILD", To: child.ID}); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	view, err := database.View(ctx, domain.Snapshot{})
	if err != nil {
		t.Fatal(err)
	}
	count, err := view.CountEdges(ctx, EdgePredicate{FromIDs: []domain.EntityID{wideRoot.ID}, Types: []string{"CHILD"}})
	if err != nil || count != uint64(width) {
		t.Fatalf("wide hierarchy count = %d, %v", count, err)
	}
	if _, err := database.Write(ctx, RevisionMeta{}, func(tx *WriteTx) error {
		_, err := tx.CreateEdge(EdgeInput{From: nodes[len(nodes)-1].ID, Type: "CHILD", To: nodes[0].ID})
		return err
	}); err == nil || !strings.Contains(err.Error(), "child_cycle") {
		t.Fatalf("deep Go cycle validation error = %v", err)
	}

	conn, err := database.db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = conn.ExecContext(context.Background(), "ROLLBACK") }()
	var previousTime int64
	if err := conn.QueryRowContext(ctx, "SELECT committed_ns FROM revisions WHERE revision = 1").Scan(&previousTime); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(ctx, "INSERT INTO revisions(revision, committed_ns, actor, message, sealed) VALUES (2, ?, '', '', 0)", previousTime+1); err != nil {
		t.Fatal(err)
	}
	edgeID, err := newUUIDv7()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(ctx, "INSERT INTO edges(id, created_revision) VALUES (?, 2)", string(edgeID)); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(ctx, `INSERT INTO edge_versions(id, valid_from, from_id, type, to_id, properties)
		VALUES (?, 2, ?, 'CHILD', ?, ?)`, string(edgeID), string(nodes[len(nodes)-1].ID), string(nodes[0].ID), mustProperties(t, nil)); err == nil || !strings.Contains(err.Error(), "sheets_child_cycle") {
		t.Fatalf("deep SQLite cycle validation error = %v", err)
	}
}

func TestBusyWriterHonorsContextCancellation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "graph.db")
	first, err := Open(context.Background(), path, WithBusyTimeout(5*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	second, err := Open(context.Background(), path, WithBusyTimeout(5*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = first.Close(); _ = second.Close() }()
	entered := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		_, err := first.Write(context.Background(), RevisionMeta{}, func(*WriteTx) error {
			close(entered)
			<-release
			return nil
		})
		done <- err
	}()
	<-entered
	ctx, cancel := context.WithTimeout(context.Background(), 75*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, err = second.Write(ctx, RevisionMeta{}, func(*WriteTx) error { return nil })
	elapsed := time.Since(started)
	close(release)
	if firstErr := <-done; firstErr != nil {
		t.Fatal(firstErr)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("contended write error = %v", err)
	}
	if elapsed > time.Second {
		t.Fatalf("context cancellation took %s", elapsed)
	}
}

func TestMultiProcessWritersAndCrashRollback(t *testing.T) {
	if os.Getenv("SHEETS_STORE_HELPER") != "" {
		t.Skip("parent-only test")
	}
	t.Run("crash rollback", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "crash.db")
		command := storeHelperCommand(path, "crash")
		if err := command.Run(); err == nil {
			t.Fatal("crashing helper exited successfully")
		}
		database := openTestStore(t, path)
		defer func() { _ = database.Close() }()
		if revision, err := database.CurrentRevision(context.Background()); err != nil || revision != 0 {
			t.Fatalf("crashed transaction revision = %d, %v", revision, err)
		}
	})

	t.Run("writers", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "writers.db")
		const writers = 10
		commands := make([]*exec.Cmd, writers)
		for index := range commands {
			commands[index] = storeHelperCommand(path, fmt.Sprintf("writer-%d", index))
		}
		var wait sync.WaitGroup
		errorsCh := make(chan error, writers)
		for index := range commands {
			wait.Add(1)
			go func(command *exec.Cmd) {
				defer wait.Done()
				if output, err := command.CombinedOutput(); err != nil {
					errorsCh <- fmt.Errorf("%w: %s", err, output)
				}
			}(commands[index])
		}
		wait.Wait()
		close(errorsCh)
		for err := range errorsCh {
			t.Errorf("helper writer: %v", err)
		}
		database := openTestStore(t, path)
		defer func() { _ = database.Close() }()
		if revision, err := database.CurrentRevision(context.Background()); err != nil || revision != writers {
			t.Fatalf("multi-process revision = %d, %v", revision, err)
		}
	})
}

func TestStoreSubprocessHelper(t *testing.T) {
	mode := os.Getenv("SHEETS_STORE_HELPER")
	if mode == "" {
		return
	}
	path := os.Getenv("SHEETS_STORE_PATH")
	database, err := Open(context.Background(), path, WithBusyTimeout(15*time.Second))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	if mode == "crash" {
		_, _ = database.Write(context.Background(), RevisionMeta{}, func(tx *WriteTx) error {
			if _, err := tx.CreateNode(NodeInput{Body: "uncommitted"}); err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(3)
			}
			os.Exit(23)
			return nil
		})
	}
	_, err = database.Write(context.Background(), RevisionMeta{}, func(tx *WriteTx) error {
		_, err := tx.CreateNode(NodeInput{Body: mode})
		return err
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(4)
	}
	if err := database.Close(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(5)
	}
}

func storeHelperCommand(path, mode string) *exec.Cmd {
	command := exec.Command(os.Args[0], "-test.run=^TestStoreSubprocessHelper$")
	command.Env = append(os.Environ(), "SHEETS_STORE_HELPER="+mode, "SHEETS_STORE_PATH="+path)
	return command
}

func createV1Database(t *testing.T, path string, id domain.EntityID, properties []byte) {
	t.Helper()
	database := openRawSQLite(t, path)
	defer func() { _ = database.Close() }()
	if _, err := database.Exec(initialMigration); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec("PRAGMA user_version=1"); err != nil {
		t.Fatal(err)
	}
	labels, _, err := encodeLabels([]string{"Legacy"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec("INSERT INTO revisions(revision, committed_ns, actor, message) VALUES (1, 1, '', '')"); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec("INSERT INTO nodes(id, created_revision) VALUES (?, 1)", string(id)); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec("INSERT INTO node_versions(id, valid_from, labels, properties, body) VALUES (?, 1, ?, ?, '')", string(id), labels, properties); err != nil {
		t.Fatal(err)
	}
}

func openRawSQLite(t *testing.T, path string) *sql.DB {
	t.Helper()
	dsn, err := makeDSN(path, 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	database, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.Ping(); err != nil {
		_ = database.Close()
		t.Fatal(err)
	}
	return database
}

func mustProperties(t *testing.T, properties domain.Properties) []byte {
	t.Helper()
	encoded, err := encodeProperties(properties)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

func explainPlan(t *testing.T, database *sql.DB, query string, args ...any) string {
	t.Helper()
	rows, err := database.QueryContext(context.Background(), "EXPLAIN QUERY PLAN "+query, args...)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var details []string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		details = append(details, detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return strings.Join(details, "\n")
}
