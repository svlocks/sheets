package store

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/svlocks/sheets/internal/domain"
)

func TestRevisionPaginationOrdersAndCursorBindings(t *testing.T) {
	ctx := context.Background()
	database := openTestStore(t, filepath.Join(t.TempDir(), "graph.db"))
	t.Cleanup(func() { _ = database.Close() })
	seedRevisionFixture(t, database, 12)

	descending, page, err := database.ListRevisionPage(ctx, domain.RevisionPage{
		Limit: 3, Order: domain.RevisionOrderDescending,
	})
	if err != nil {
		t.Fatal(err)
	}
	assertRevisionNumbers(t, descending, 12, 11, 10)
	if page.Next == "" || page.Next == base64.RawURLEncoding.EncodeToString([]byte("10")) {
		t.Fatalf("descending next cursor is not opaque: %q", page.Next)
	}

	older, next, err := database.ListRevisionPage(ctx, domain.RevisionPage{
		Limit: 3, Cursor: page.Next, Order: domain.RevisionOrderDescending,
	})
	if err != nil {
		t.Fatal(err)
	}
	assertRevisionNumbers(t, older, 9, 8, 7)
	if next.Next == "" {
		t.Fatal("older page unexpectedly ended")
	}

	ascending, ascendingPage, err := database.ListRevisionPage(ctx, domain.RevisionPage{
		Limit: 3, Order: domain.RevisionOrderAscending,
	})
	if err != nil {
		t.Fatal(err)
	}
	assertRevisionNumbers(t, ascending, 1, 2, 3)
	if ascendingPage.Next == "" {
		t.Fatal("ascending page unexpectedly ended")
	}
	if _, _, err := database.ListRevisionPage(ctx, domain.RevisionPage{
		Limit: 3, Cursor: page.Next, Order: domain.RevisionOrderAscending,
	}); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("cross-order cursor error = %v", err)
	}

	legacyCursor := base64.RawURLEncoding.EncodeToString([]byte("3"))
	legacy, _, err := database.ListRevisions(ctx, domain.Page{Limit: 2, After: legacyCursor})
	if err != nil {
		t.Fatalf("legacy ascending cursor: %v", err)
	}
	assertRevisionNumbers(t, legacy, 4, 5)
}

func TestDescendingRevisionPaginationIsStableUnderNewerCommits(t *testing.T) {
	ctx := context.Background()
	database := openTestStore(t, filepath.Join(t.TempDir(), "graph.db"))
	t.Cleanup(func() { _ = database.Close() })
	seedRevisionFixture(t, database, 10)

	first, page, err := database.ListRevisionPage(ctx, domain.RevisionPage{
		Limit: 3, Order: domain.RevisionOrderDescending,
	})
	if err != nil {
		t.Fatal(err)
	}
	assertRevisionNumbers(t, first, 10, 9, 8)
	insertRevisionFixture(t, database, 11, 12)

	second, _, err := database.ListRevisionPage(ctx, domain.RevisionPage{
		Limit: 3, Cursor: page.Next, Order: domain.RevisionOrderDescending,
	})
	if err != nil {
		t.Fatal(err)
	}
	assertRevisionNumbers(t, second, 7, 6, 5)

	refreshed, _, err := database.ListRevisionPage(ctx, domain.RevisionPage{
		Limit: 3, Order: domain.RevisionOrderDescending,
	})
	if err != nil {
		t.Fatal(err)
	}
	assertRevisionNumbers(t, refreshed, 12, 11, 10)
}

func TestRevisionCursorSurvivesStoreReopen(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "graph.db")
	database := openTestStore(t, path)
	var id domain.EntityID
	if _, err := database.Write(ctx, RevisionMeta{}, func(transaction *WriteTx) error {
		node, err := transaction.CreateNode(NodeInput{Body: "one"})
		id = node.ID
		return err
	}); err != nil {
		t.Fatal(err)
	}
	updated := "two"
	if _, err := database.Write(ctx, RevisionMeta{}, func(transaction *WriteTx) error {
		_, err := transaction.UpdateNode(id, NodeUpdate{Body: &updated})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	first, page, err := database.ListRevisionPage(ctx, domain.RevisionPage{Limit: 1, Order: domain.RevisionOrderDescending})
	if err != nil || len(first) != 1 || first[0].Revision != 2 || page.Next == "" {
		t.Fatalf("first page = %#v, %#v, %v", first, page, err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	database = openTestStore(t, path)
	t.Cleanup(func() { _ = database.Close() })
	second, page, err := database.ListRevisionPage(ctx, domain.RevisionPage{
		Limit: 1, Cursor: page.Next, Order: domain.RevisionOrderDescending,
	})
	if err != nil || len(second) != 1 || second[0].Revision != 1 || page.Next != "" {
		t.Fatalf("reopened page = %#v, %#v, %v", second, page, err)
	}
}

func TestRevisionCursorRejectsMalformedAndMismatchedPayloads(t *testing.T) {
	ctx := context.Background()
	database := openTestStore(t, filepath.Join(t.TempDir(), "graph.db"))
	t.Cleanup(func() { _ = database.Close() })
	seedRevisionFixture(t, database, 2)

	valid := encodeRevisionCursor(domain.RevisionOrderDescending, 2)
	tampered := valid[:len(valid)-1] + "A"
	if tampered == valid {
		tampered = valid[:len(valid)-1] + "B"
	}
	cases := map[string]string{
		"garbage":           "not-a-cursor",
		"truncated":         valid[:len(valid)-1],
		"checksum mismatch": tampered,
		"oversized":         strings.Repeat("x", maxRevisionCursorLength+1),
		"multiple segments": valid + ".extra",
		"legacy new API":    base64.RawURLEncoding.EncodeToString([]byte("1")),
		"wrong schema": encodeRevisionCursorPayload(revisionCursorPayload{
			Version: revisionCursorVersion, Schema: schemaVersion + 1, Generation: expectedSchemaFingerprint,
			Order: domain.RevisionOrderDescending.String(), Predicate: revisionPredicateFingerprint, Boundary: 2,
		}),
		"wrong generation": encodeRevisionCursorPayload(revisionCursorPayload{
			Version: revisionCursorVersion, Schema: schemaVersion, Generation: "different",
			Order: domain.RevisionOrderDescending.String(), Predicate: revisionPredicateFingerprint, Boundary: 2,
		}),
		"wrong predicate": encodeRevisionCursorPayload(revisionCursorPayload{
			Version: revisionCursorVersion, Schema: schemaVersion, Generation: expectedSchemaFingerprint,
			Order: domain.RevisionOrderDescending.String(), Predicate: fingerprintRevisionCursorPart("other"), Boundary: 2,
		}),
		"zero boundary": encodeRevisionCursorPayload(revisionCursorPayload{
			Version: revisionCursorVersion, Schema: schemaVersion, Generation: expectedSchemaFingerprint,
			Order: domain.RevisionOrderDescending.String(), Predicate: revisionPredicateFingerprint, Boundary: 0,
		}),
	}
	for name, cursor := range cases {
		t.Run(name, func(t *testing.T) {
			_, _, err := database.ListRevisionPage(ctx, domain.RevisionPage{
				Limit: 1, Cursor: cursor, Order: domain.RevisionOrderDescending,
			})
			if !errors.Is(err, ErrInvalidArgument) {
				t.Fatalf("error = %v", err)
			}
		})
	}

	if _, err := decodeRevisionCursor(valid, domain.RevisionOrderDescending, fingerprintRevisionCursorPart("other")); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("cross-predicate decode error = %v", err)
	}
}

func TestRevisionPaginationEmptyCancellationAndInvalidOrder(t *testing.T) {
	database := openTestStore(t, filepath.Join(t.TempDir(), "graph.db"))
	t.Cleanup(func() { _ = database.Close() })

	for _, order := range []domain.RevisionOrder{domain.RevisionOrderAscending, domain.RevisionOrderDescending} {
		values, page, err := database.ListRevisionPage(context.Background(), domain.RevisionPage{Order: order})
		if err != nil || len(values) != 0 || page.Next != "" {
			t.Fatalf("empty %s page = %#v, %#v, %v", order, values, page, err)
		}
	}
	if _, _, err := database.ListRevisionPage(context.Background(), domain.RevisionPage{Order: domain.RevisionOrder(99)}); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("invalid order error = %v", err)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := database.ListRevisionPage(canceled, domain.RevisionPage{Order: domain.RevisionOrderDescending}); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled error = %v", err)
	}
}

func TestRevisionPaginationHundredThousandFixture(t *testing.T) {
	ctx := context.Background()
	database := openTestStore(t, filepath.Join(t.TempDir(), "graph.db"))
	t.Cleanup(func() { _ = database.Close() })
	seedRevisionFixture(t, database, 100_000)

	page := domain.RevisionPage{Limit: maxPageSize, Order: domain.RevisionOrderDescending}
	want := domain.Revision(100_000)
	seen := 0
	for {
		values, info, err := database.ListRevisionPage(ctx, page)
		if err != nil {
			t.Fatal(err)
		}
		if len(values) == 0 {
			t.Fatal("non-terminal page was empty")
		}
		for _, value := range values {
			if value.Revision != want {
				t.Fatalf("revision[%d] = %d, want %d", seen, value.Revision, want)
			}
			want--
			seen++
		}
		if info.Next == "" {
			break
		}
		page.Cursor = info.Next
	}
	if seen != 100_000 || want != 0 {
		t.Fatalf("visited %d revisions, next expected %d", seen, want)
	}
}

func BenchmarkListRevisionPageHundredThousand(b *testing.B) {
	ctx := context.Background()
	database := openTestStore(b, filepath.Join(b.TempDir(), "graph.db"))
	b.Cleanup(func() { _ = database.Close() })
	seedRevisionFixture(b, database, 100_000)
	page := domain.RevisionPage{Limit: 100, Order: domain.RevisionOrderDescending}

	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		values, info, err := database.ListRevisionPage(ctx, page)
		if err != nil || len(values) != 100 || info.Next == "" {
			b.Fatalf("page = %d rows, next %t: %v", len(values), info.Next != "", err)
		}
	}
}

func FuzzDecodeRevisionCursor(f *testing.F) {
	valid := encodeRevisionCursor(domain.RevisionOrderDescending, 42)
	f.Add(valid, uint8(domain.RevisionOrderDescending))
	f.Add("not-a-cursor", uint8(domain.RevisionOrderAscending))
	f.Add(strings.Repeat("x", maxRevisionCursorLength+1), uint8(255))
	f.Fuzz(func(t *testing.T, cursor string, rawOrder uint8) {
		order := domain.RevisionOrder(rawOrder)
		revision, err := decodeRevisionCursor(cursor, order, revisionPredicateFingerprint)
		if err == nil {
			if !order.Valid() || revision == 0 || revision > domain.Revision(^uint64(0)>>1) {
				t.Fatalf("accepted invalid decoded state: order=%d revision=%d", order, revision)
			}
			if encodeRevisionCursor(order, revision) != cursor {
				t.Fatal("accepted a non-canonical revision cursor")
			}
		}
	})
}

func seedRevisionFixture(t testing.TB, database *Store, count int) {
	t.Helper()
	if _, err := database.db.Exec("DROP TRIGGER revisions_validate_insert"); err != nil {
		t.Fatalf("drop fixture insert trigger: %v", err)
	}
	if count > 0 {
		insertRevisionFixture(t, database, 1, count)
	}
}

func insertRevisionFixture(t testing.TB, database *Store, first, last int) {
	t.Helper()
	_, err := database.db.Exec(`
		WITH RECURSIVE fixture(revision) AS (
			SELECT ?
			UNION ALL
			SELECT revision + 1 FROM fixture WHERE revision < ?
		)
		INSERT INTO revisions(revision, committed_ns, actor, message, sealed)
		SELECT revision, revision, 'fixture', printf('revision %d', revision), 1 FROM fixture`, first, last)
	if err != nil {
		t.Fatalf("insert revisions %d..%d: %v", first, last, err)
	}
}

func assertRevisionNumbers(t *testing.T, values []domain.RevisionInfo, want ...domain.Revision) {
	t.Helper()
	if len(values) != len(want) {
		t.Fatalf("revision count = %d, want %d (%v)", len(values), len(want), values)
	}
	for index := range want {
		if values[index].Revision != want[index] {
			t.Fatalf("revision[%d] = %d, want %d (%v)", index, values[index].Revision, want[index], values)
		}
		if values[index].Message != fmt.Sprintf("revision %d", want[index]) {
			t.Fatalf("revision[%d] metadata = %#v", index, values[index])
		}
	}
}
