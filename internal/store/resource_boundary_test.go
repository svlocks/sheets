package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/svlocks/sheets/internal/domain"
)

var sharedLimitText struct {
	once sync.Once
	text string
}

func resourceLimitText() string {
	sharedLimitText.once.Do(func() {
		sharedLimitText.text = strings.Repeat("x", domain.MaxCanonicalPropertyBytes+1)
	})
	return sharedLimitText.text
}

func TestCodecExactCollectionAndDerivedBoundaries(t *testing.T) {
	text := resourceLimitText()

	t.Run("label rows", func(t *testing.T) {
		labels := make([]string, domain.MaxLabelsPerNode+1)
		for index := range labels {
			labels[index] = fmt.Sprintf("L%05d", index)
		}
		if _, _, err := encodeLabels(labels[:domain.MaxLabelsPerNode]); err != nil {
			t.Fatalf("exact label row limit: %v", err)
		}
		if _, _, err := encodeLabels(labels); !errors.Is(err, domain.ErrResourceLimit) {
			t.Fatalf("label row limit+1 error = %v", err)
		}
	})

	t.Run("label bytes and multibyte", func(t *testing.T) {
		// 256 lengths 65,281..65,536 plus 32,640 bytes sum to exactly
		// 16 MiB. Replace the short ASCII label with the same byte length
		// of multibyte text to exercise byte, rather than rune, accounting.
		labels := make([]string, 0, 258)
		labels = append(labels, strings.Repeat("é", 16_320))
		for length := domain.MaxLabelBytes - 255; length <= domain.MaxLabelBytes; length++ {
			labels = append(labels, text[:length])
		}
		encoded, normalized, err := encodeLabels(labels)
		if err != nil {
			t.Fatalf("exact derived label byte limit: %v", err)
		} else if got := totalStringBytes(normalized); got != domain.MaxDerivedLabelBytesPerVersion {
			t.Fatalf("derived label bytes = %d", got)
		}
		decoded, err := decodeLabels(encoded)
		if err != nil || totalStringBytes(decoded) != domain.MaxDerivedLabelBytesPerVersion {
			t.Fatalf("decode exact derived label bytes: %d, %v", totalStringBytes(decoded), err)
		}
		labels = append(labels, "y")
		if _, _, err := encodeLabels(labels); !errors.Is(err, domain.ErrResourceLimit) {
			t.Fatalf("derived label byte limit+1 error = %v", err)
		}
	})

	t.Run("indexed property rows", func(t *testing.T) {
		properties := make(domain.Properties, domain.MaxIndexedPropertiesPerVersion+1)
		for index := 0; index <= domain.MaxIndexedPropertiesPerVersion; index++ {
			properties[fmt.Sprintf("p%05d", index)] = int64(index)
		}
		delete(properties, fmt.Sprintf("p%05d", domain.MaxIndexedPropertiesPerVersion))
		if _, err := encodeProperties(properties); err != nil {
			t.Fatalf("exact indexed-property row limit: %v", err)
		}
		properties[fmt.Sprintf("p%05d", domain.MaxIndexedPropertiesPerVersion)] = int64(0)
		if _, err := encodeProperties(properties); !errors.Is(err, domain.ErrResourceLimit) {
			t.Fatalf("indexed-property row limit+1 error = %v", err)
		}
	})

	t.Run("indexed property bytes", func(t *testing.T) {
		constant := canonicalEncodedValueSize(encodedValue{Kind: "string", Text: "x"}) - 1
		firstLength := domain.MaxPropertyScalarBytes
		secondLength := domain.MaxDerivedPropertyBytesPerVersion - 2 - int(2*constant) - firstLength
		if secondLength <= 0 || secondLength > domain.MaxPropertyScalarBytes {
			t.Fatalf("invalid exact-boundary construction: %d", secondLength)
		}
		properties := domain.Properties{
			"a": text[:firstLength],
			"b": text[:secondLength],
		}
		encoded, err := encodeProperties(properties)
		if err != nil {
			t.Fatalf("exact derived property byte limit: %v", err)
		}
		decoded, err := decodeProperties(encoded)
		if err != nil || len(decoded["a"].(string))+len(decoded["b"].(string)) != firstLength+secondLength {
			t.Fatalf("decode exact derived property bytes: %v", err)
		}
		properties["b"] = text[:secondLength+1]
		if _, err := encodeProperties(properties); !errors.Is(err, domain.ErrResourceLimit) {
			t.Fatalf("derived property byte limit+1 error = %v", err)
		}
	})

	t.Run("property values and depth precharge", func(t *testing.T) {
		exactValues := make([]int8, domain.MaxPropertyValues-2) // root map + list + items
		if err := preflightPropertyInput(domain.Properties{"items": exactValues}); err != nil {
			t.Fatalf("exact property value count: %v", err)
		}
		tooManyValues := make([]int8, domain.MaxPropertyValues-1)
		if err := preflightPropertyInput(domain.Properties{"items": tooManyValues}); !errors.Is(err, domain.ErrResourceLimit) {
			t.Fatalf("property value count+1 error = %v", err)
		}

		exactDepth := nestedProperties(domain.MaxPropertyDepth - 1)
		exactDepthEncoding, err := encodeProperties(exactDepth)
		if err != nil {
			t.Fatalf("exact property depth: %v", err)
		}
		if _, err := decodeProperties(exactDepthEncoding); err != nil {
			t.Fatalf("decode exact property depth: %v", err)
		}
		tooDeep := nestedProperties(domain.MaxPropertyDepth)
		if _, err := encodeProperties(tooDeep); !errors.Is(err, domain.ErrResourceLimit) {
			t.Fatalf("property depth+1 error = %v", err)
		}
	})
}

func TestDecodePreflightRejectsAmplificationBeforeMaterialization(t *testing.T) {
	overScalar := `{"k":"map","o":{"value":{"k":"string","s":"` +
		resourceLimitText()[:domain.MaxPropertyScalarBytes+1] + `"}}}`
	if err := preflightEncodedProperties([]byte(overScalar)); !errors.Is(err, domain.ErrResourceLimit) {
		t.Fatalf("oversized scalar preflight error = %v", err)
	}

	// Literal '<' occupies one input byte but encoding/json's canonical form
	// spells it as six. The input envelope fits; its canonical expansion does
	// not, and must be rejected before unmarshalling the tagged tree.
	escapeCount := domain.MaxCanonicalPropertyBytes/6 + 1
	escapeAmplified := `{"k":"map","o":{"value":{"k":"string","s":"` +
		strings.Repeat("<", escapeCount) + `"}}}`
	if len(escapeAmplified) > domain.MaxCanonicalPropertyBytes {
		t.Fatalf("test input unexpectedly exceeds source envelope: %d", len(escapeAmplified))
	}
	if err := preflightEncodedProperties([]byte(escapeAmplified)); !errors.Is(err, domain.ErrResourceLimit) {
		t.Fatalf("canonical expansion preflight error = %v", err)
	}
}

func TestNormalWritesRejectLimitsBeforeRevisionAllocation(t *testing.T) {
	ctx := context.Background()
	database := openTestStore(t, filepath.Join(t.TempDir(), "normal-boundaries.db"))
	defer func() { _ = database.Close() }()

	exactMessage := strings.Repeat("é", domain.MaxRevisionMessageBytes/2)
	result, err := database.Write(ctx, RevisionMeta{Message: exactMessage}, func(tx *WriteTx) error {
		_, createErr := tx.CreateNode(NodeInput{Body: "message boundary"})
		return createErr
	})
	if err != nil || !result.Changed || result.Revision != 1 {
		t.Fatalf("exact multibyte revision message = %#v, %v", result, err)
	}
	tooLongMessage := exactMessage + "x"
	result, err = database.Write(ctx, RevisionMeta{Message: tooLongMessage}, func(tx *WriteTx) error {
		_, createErr := tx.CreateNode(NodeInput{Body: "must roll back"})
		return createErr
	})
	if !errors.Is(err, domain.ErrResourceLimit) || result.Changed {
		t.Fatalf("revision message limit+1 = %#v, %v", result, err)
	}

	labels := make([]string, domain.MaxLabelsPerNode+1)
	for index := range labels {
		labels[index] = fmt.Sprintf("L%05d", index)
	}
	result, err = database.Write(ctx, RevisionMeta{}, func(tx *WriteTx) error {
		_, createErr := tx.CreateNode(NodeInput{Labels: labels})
		return createErr
	})
	if !errors.Is(err, domain.ErrResourceLimit) || result.Changed {
		t.Fatalf("label count limit+1 = %#v, %v", result, err)
	}

	properties := make(domain.Properties, domain.MaxIndexedPropertiesPerVersion+1)
	for index := 0; index <= domain.MaxIndexedPropertiesPerVersion; index++ {
		properties[fmt.Sprintf("p%05d", index)] = int64(index)
	}
	result, err = database.Write(ctx, RevisionMeta{}, func(tx *WriteTx) error {
		_, createErr := tx.CreateNode(NodeInput{Properties: properties})
		return createErr
	})
	if !errors.Is(err, domain.ErrResourceLimit) || result.Changed {
		t.Fatalf("property index count limit+1 = %#v, %v", result, err)
	}

	var updateTarget domain.Node
	created, err := database.Write(ctx, RevisionMeta{}, func(tx *WriteTx) error {
		var createErr error
		updateTarget, createErr = tx.CreateNode(NodeInput{Body: "duplicate-label update"})
		return createErr
	})
	if err != nil {
		t.Fatal(err)
	}
	duplicates := make([]string, domain.MaxLabelsPerNode+1)
	for index := range duplicates {
		duplicates[index] = "same"
	}
	result, err = database.Write(ctx, RevisionMeta{}, func(tx *WriteTx) error {
		_, updateErr := tx.UpdateNode(updateTarget.ID, NodeUpdate{Labels: &duplicates})
		return updateErr
	})
	if !errors.Is(err, domain.ErrResourceLimit) || result.Changed {
		t.Fatalf("duplicate-label update limit = %#v, %v", result, err)
	}
	if revision, revisionErr := database.CurrentRevision(ctx); revisionErr != nil || revision != created.Revision {
		t.Fatalf("failed inputs consumed revision %d, want %d: %v", revision, created.Revision, revisionErr)
	}
}

func TestSQLiteDerivedTablesEnforceExactRowAndByteBudgets(t *testing.T) {
	if raceDetectorEnabled {
		t.Skip("the exact 4,096-row/32 MiB SQLite stress proof runs in the ordinary suite; race keeps the bounded raw-trigger regression")
	}
	ctx := context.Background()
	database := openTestStore(t, filepath.Join(t.TempDir(), "derived-boundaries.db"))
	defer func() { _ = database.Close() }()
	connection, err := database.db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		_, _ = connection.ExecContext(context.Background(), "ROLLBACK")
		_ = connection.Close()
	}()
	if _, err := connection.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		t.Fatal(err)
	}
	if _, err := connection.ExecContext(ctx, `INSERT INTO revisions
		(revision, committed_ns, actor, message, sealed) VALUES (1, 1, '', '', 0)`); err != nil {
		t.Fatal(err)
	}

	ids := []domain.EntityID{
		"019945ee-ea00-7be6-a100-000000000321",
		"019945ee-ea00-7be6-a100-000000000322",
		"019945ee-ea00-7be6-a100-000000000323",
		"019945ee-ea00-7be6-a100-000000000324",
		"019945ee-ea00-7be6-a100-000000000325",
		"019945ee-ea00-7be6-a100-000000000326",
	}
	emptyLabels, _, err := encodeLabels(nil)
	if err != nil {
		t.Fatal(err)
	}
	emptyProperties, err := encodeProperties(nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range ids {
		if _, err := connection.ExecContext(ctx, "INSERT INTO nodes(id, created_revision) VALUES (?, 1)", string(id)); err != nil {
			t.Fatal(err)
		}
		if _, err := connection.ExecContext(ctx, `INSERT INTO node_versions
			(id, valid_from, labels, properties, body) VALUES (?, 1, ?, ?, '')`,
			string(id), emptyLabels, emptyProperties); err != nil {
			t.Fatal(err)
		}
	}
	edges := []domain.EntityID{
		"019945ee-ea00-7be6-a100-000000000327",
		"019945ee-ea00-7be6-a100-000000000328",
	}
	for _, id := range edges {
		if _, err := connection.ExecContext(ctx, "INSERT INTO edges(id, created_revision) VALUES (?, 1)", string(id)); err != nil {
			t.Fatal(err)
		}
		if _, err := connection.ExecContext(ctx, `INSERT INTO edge_versions
			(id, valid_from, from_id, type, to_id, properties)
			VALUES (?, 1, ?, 'LINK', ?, ?)`, string(id), string(ids[0]), string(ids[1]), emptyProperties); err != nil {
			t.Fatal(err)
		}
	}

	if _, err := connection.ExecContext(ctx, `WITH RECURSIVE sequence(value) AS (
		VALUES(0) UNION ALL SELECT value + 1 FROM sequence WHERE value + 1 < ?
	)
	INSERT INTO node_version_labels(id, valid_from, label)
	SELECT ?, 1, printf('L%05d', value) FROM sequence`,
		domain.MaxLabelsPerNode, string(ids[2])); err != nil {
		t.Fatalf("exact label rows: %v", err)
	}
	if _, err := connection.ExecContext(ctx, `INSERT INTO node_version_labels(id, valid_from, label)
		VALUES (?, 1, 'overflow')`, string(ids[2])); err == nil ||
		!strings.Contains(err.Error(), "sheets_resource_limit_derived_labels") {
		t.Fatalf("label row limit+1 error = %v", err)
	}

	if _, err := connection.ExecContext(ctx, `WITH RECURSIVE sequence(value) AS (
		VALUES(0) UNION ALL SELECT value + 1 FROM sequence WHERE value + 1 < ?
	)
	INSERT INTO node_version_labels(id, valid_from, label)
	SELECT ?, 1, printf('%0*d', ?, value) FROM sequence`,
		domain.MaxDerivedLabelBytesPerVersion/domain.MaxLabelBytes,
		string(ids[3]), domain.MaxLabelBytes); err != nil {
		t.Fatalf("exact label bytes: %v", err)
	}
	assertDerivedGroupBytes(t, connection, "node_version_labels", ids[3], domain.MaxDerivedLabelBytesPerVersion)
	if _, err := connection.ExecContext(ctx, `INSERT INTO node_version_labels(id, valid_from, label)
		VALUES (?, 1, 'y')`, string(ids[3])); err == nil ||
		!strings.Contains(err.Error(), "sheets_resource_limit_derived_labels") {
		t.Fatalf("label byte limit+1 error = %v", err)
	}

	canonicalScalar := []byte(`{"k":"int","s":"0"}`)
	insertProperties := func(table string, id domain.EntityID) {
		if _, err := connection.ExecContext(ctx, `WITH RECURSIVE sequence(value) AS (
			VALUES(0) UNION ALL SELECT value + 1 FROM sequence WHERE value + 1 < ?
		)
		INSERT INTO `+table+`(id, valid_from, key, kind, value)
		SELECT ?, 1, printf('p%05d', value), 'int', ? FROM sequence`,
			domain.MaxIndexedPropertiesPerVersion, string(id), canonicalScalar); err != nil {
			t.Fatalf("%s exact rows: %v", table, err)
		}
		if _, err := connection.ExecContext(ctx, `INSERT INTO `+table+`
			(id, valid_from, key, kind, value) VALUES (?, 1, 'overflow', 'int', ?)`,
			string(id), canonicalScalar); err == nil ||
			!strings.Contains(err.Error(), "sheets_resource_limit_derived_properties") {
			t.Fatalf("%s row limit+1 error = %v", table, err)
		}
	}
	insertProperties("node_property_index", ids[4])
	insertProperties("edge_property_index", edges[0])

	insertExactPropertyBytes := func(table string, id domain.EntityID) {
		halfValueBytes := domain.MaxDerivedPropertyBytesPerVersion/2 - 1 // one-byte key fills each half exactly
		for _, key := range []string{"a", "b"} {
			if _, err := connection.ExecContext(ctx, `INSERT INTO `+table+`
				(id, valid_from, key, kind, value) VALUES (?, 1, ?, 'bytes', zeroblob(?))`,
				string(id), key, halfValueBytes); err != nil {
				t.Fatalf("%s exact bytes: %v", table, err)
			}
		}
		assertDerivedGroupBytes(t, connection, table, id, domain.MaxDerivedPropertyBytesPerVersion)
		if _, err := connection.ExecContext(ctx, `INSERT INTO `+table+`
			(id, valid_from, key, kind, value) VALUES (?, 1, 'c', 'null', zeroblob(0))`, string(id)); err == nil ||
			!strings.Contains(err.Error(), "sheets_resource_limit_derived_properties") {
			t.Fatalf("%s byte limit+1 error = %v", table, err)
		}
		if _, err := connection.ExecContext(ctx, `UPDATE `+table+`
			SET value=zeroblob(?) WHERE id=? AND valid_from=1 AND key='a'`,
			halfValueBytes+1, string(id)); err == nil ||
			!strings.Contains(err.Error(), "sheets_resource_limit_derived_properties") {
			t.Fatalf("%s byte update limit+1 error = %v", table, err)
		}
	}
	insertExactPropertyBytes("node_property_index", ids[5])
	insertExactPropertyBytes("edge_property_index", edges[1])
}

func TestSQLiteSourceUpdatesEnforceNestedAndDerivedBoundaries(t *testing.T) {
	if raceDetectorEnabled {
		t.Skip("the exact 4,096-row/16 MiB SQLite JSON stress proof runs in the ordinary suite; race keeps bounded source-update regressions")
	}
	ctx := context.Background()
	database := openTestStore(t, filepath.Join(t.TempDir(), "source-update-boundaries.db"))
	defer func() { _ = database.Close() }()
	connection, err := database.db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		_, _ = connection.ExecContext(context.Background(), "ROLLBACK")
		_ = connection.Close()
	}()
	if _, err := connection.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		t.Fatal(err)
	}
	if _, err := connection.ExecContext(ctx, `INSERT INTO revisions
		(revision, committed_ns, actor, message, sealed) VALUES (1, 1, '', '', 0)`); err != nil {
		t.Fatal(err)
	}
	nodes := []domain.EntityID{
		"019945ee-ea00-7be6-a100-000000000331",
		"019945ee-ea00-7be6-a100-000000000332",
	}
	edge := domain.EntityID("019945ee-ea00-7be6-a100-000000000333")
	emptyLabels, _, _ := encodeLabels(nil)
	emptyProperties, _ := encodeProperties(nil)
	for _, id := range nodes {
		if _, err := connection.ExecContext(ctx, "INSERT INTO nodes(id, created_revision) VALUES (?, 1)", string(id)); err != nil {
			t.Fatal(err)
		}
		if _, err := connection.ExecContext(ctx, `INSERT INTO node_versions
			(id, valid_from, labels, properties, body) VALUES (?, 1, ?, ?, '')`,
			string(id), emptyLabels, emptyProperties); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := connection.ExecContext(ctx, "INSERT INTO edges(id, created_revision) VALUES (?, 1)", string(edge)); err != nil {
		t.Fatal(err)
	}
	if _, err := connection.ExecContext(ctx, `INSERT INTO edge_versions
		(id, valid_from, from_id, type, to_id, properties)
		VALUES (?, 1, ?, 'LINK', ?, ?)`, string(edge), string(nodes[0]), string(nodes[1]), emptyProperties); err != nil {
		t.Fatal(err)
	}

	exactDepth, err := encodeProperties(nestedProperties(domain.MaxPropertyDepth - 1))
	if err != nil {
		t.Fatal(err)
	}
	tooDeep, err := json.Marshal(nestedEncodedProperties(domain.MaxPropertyDepth))
	if err != nil {
		t.Fatal(err)
	}
	for _, update := range []struct {
		table string
		id    domain.EntityID
	}{
		{table: "node_versions", id: nodes[0]},
		{table: "edge_versions", id: edge},
	} {
		if _, err := connection.ExecContext(ctx, "UPDATE "+update.table+" SET properties=? WHERE id=? AND valid_from=1",
			exactDepth, string(update.id)); err != nil {
			t.Fatalf("%s exact depth update: %v", update.table, err)
		}
		if _, err := connection.ExecContext(ctx, "UPDATE "+update.table+" SET properties=? WHERE id=? AND valid_from=1",
			tooDeep, string(update.id)); err == nil || !strings.Contains(err.Error(), "sheets_resource_limit_property_shape") {
			t.Fatalf("%s depth+1 update error = %v", update.table, err)
		}
	}

	text := resourceLimitText()
	exactKeyProperties, err := encodeProperties(domain.Properties{
		"outer": domain.Properties{text[:domain.MaxPropertyKeyBytes]: int64(1)},
	})
	if err != nil {
		t.Fatal(err)
	}
	tooLongKeyProperties, err := json.Marshal(encodedValue{Kind: "map", Map: map[string]encodedValue{
		"outer": {Kind: "map", Map: map[string]encodedValue{
			text[:domain.MaxPropertyKeyBytes+1]: {Kind: "int", Text: "1"},
		}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	exactStringProperties, err := encodeProperties(domain.Properties{
		"outer": domain.Properties{"value": text[:domain.MaxPropertyScalarBytes]},
	})
	if err != nil {
		t.Fatal(err)
	}
	tooLongStringProperties, err := json.Marshal(encodedValue{Kind: "map", Map: map[string]encodedValue{
		"outer": {Kind: "map", Map: map[string]encodedValue{
			"value": {Kind: "string", Text: text[:domain.MaxPropertyScalarBytes+1]},
		}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	for _, update := range []struct {
		table string
		id    domain.EntityID
	}{
		{table: "node_versions", id: nodes[0]},
		{table: "edge_versions", id: edge},
	} {
		if _, err := connection.ExecContext(ctx, "UPDATE "+update.table+" SET properties=? WHERE id=? AND valid_from=1",
			exactKeyProperties, string(update.id)); err != nil {
			t.Fatalf("%s exact nested key update: %v", update.table, err)
		}
		if _, err := connection.ExecContext(ctx, "UPDATE "+update.table+" SET properties=? WHERE id=? AND valid_from=1",
			tooLongKeyProperties, string(update.id)); err == nil || !strings.Contains(err.Error(), "sheets_resource_limit_property_key") {
			t.Fatalf("%s nested key limit+1 error = %v", update.table, err)
		}
		if _, err := connection.ExecContext(ctx, "UPDATE "+update.table+" SET properties=? WHERE id=? AND valid_from=1",
			exactStringProperties, string(update.id)); err != nil {
			t.Fatalf("%s exact nested string update: %v", update.table, err)
		}
		if _, err := connection.ExecContext(ctx, "UPDATE "+update.table+" SET properties=? WHERE id=? AND valid_from=1",
			tooLongStringProperties, string(update.id)); err == nil || !strings.Contains(err.Error(), "sheets_resource_limit_property_string") {
			t.Fatalf("%s nested string limit+1 error = %v", update.table, err)
		}
	}

	labels := make([]string, domain.MaxLabelsPerNode+1)
	for index := range labels {
		labels[index] = fmt.Sprintf("L%05d", index)
	}
	exactLabels, err := json.Marshal(labels[:domain.MaxLabelsPerNode])
	if err != nil {
		t.Fatal(err)
	}
	tooManyLabels, err := json.Marshal(labels)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := connection.ExecContext(ctx, "UPDATE node_versions SET labels=? WHERE id=? AND valid_from=1",
		exactLabels, string(nodes[0])); err != nil {
		t.Fatalf("exact raw label count: %v", err)
	}
	if _, err := connection.ExecContext(ctx, "UPDATE node_versions SET labels=? WHERE id=? AND valid_from=1",
		tooManyLabels, string(nodes[0])); err == nil || !strings.Contains(err.Error(), "sheets_resource_limit_derived_labels") {
		t.Fatalf("raw label count+1 error = %v", err)
	}

	topScalars := make(map[string]encodedValue, domain.MaxIndexedPropertiesPerVersion+1)
	for index := 0; index <= domain.MaxIndexedPropertiesPerVersion; index++ {
		topScalars[fmt.Sprintf("p%05d", index)] = encodedValue{Kind: "int", Text: "0"}
	}
	delete(topScalars, fmt.Sprintf("p%05d", domain.MaxIndexedPropertiesPerVersion))
	exactProperties, err := json.Marshal(encodedValue{Kind: "map", Map: topScalars})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := connection.ExecContext(ctx, "UPDATE edge_versions SET properties=? WHERE id=? AND valid_from=1",
		exactProperties, string(edge)); err != nil {
		t.Fatalf("exact raw indexed property count: %v", err)
	}
	topScalars[fmt.Sprintf("p%05d", domain.MaxIndexedPropertiesPerVersion)] = encodedValue{Kind: "int", Text: "0"}
	tooManyProperties, err := json.Marshal(encodedValue{Kind: "map", Map: topScalars})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := connection.ExecContext(ctx, "UPDATE edge_versions SET properties=? WHERE id=? AND valid_from=1",
		tooManyProperties, string(edge)); err == nil || !strings.Contains(err.Error(), "sheets_resource_limit_derived_properties") {
		t.Fatalf("raw indexed property count+1 error = %v", err)
	}
}

func assertDerivedGroupBytes(t *testing.T, connection *sql.Conn, table string, id domain.EntityID, want int) {
	t.Helper()
	column := "length(CAST(key AS BLOB)) + length(CAST(value AS BLOB))"
	if table == "node_version_labels" {
		column = "length(CAST(label AS BLOB))"
	}
	var got int
	if err := connection.QueryRowContext(context.Background(), `SELECT COALESCE(SUM(`+column+`), 0)
		FROM `+table+` WHERE id=? AND valid_from=1`, string(id)).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("%s derived bytes = %d, want %d", table, got, want)
	}
}

func nestedEncodedProperties(mapDepth int) encodedValue {
	value := encodedValue{Kind: "int", Text: "1"}
	for range mapDepth {
		value = encodedValue{Kind: "map", Map: map[string]encodedValue{"nested": value}}
	}
	return encodedValue{Kind: "map", Map: map[string]encodedValue{"root": value}}
}

func nestedProperties(mapDepth int) domain.Properties {
	var value any = int64(1)
	for range mapDepth {
		value = domain.Properties{"nested": value}
	}
	return domain.Properties{"root": value}
}

func totalStringBytes(values []string) int {
	total := 0
	for _, value := range values {
		total += len(value)
	}
	return total
}

func FuzzResourceCodecPreflights(f *testing.F) {
	f.Add([]byte(`{"k":"map","o":{"rank":{"k":"int","s":"1"}}}`), false)
	f.Add([]byte(`{"k":"map","o":{"nested":{"k":"list","a":[{"k":"null"}]}}}`), false)
	f.Add([]byte(`{"k":"map","o":{"bad":{"k":"string","s":"\xff"}}}`), false)
	f.Add([]byte(`["Alpha","雪"]`), true)
	f.Add([]byte(`["duplicate","duplicate"]`), true)
	f.Fuzz(func(t *testing.T, data []byte, labels bool) {
		if len(data) > 1<<20 {
			t.Skip()
		}
		if labels {
			_, _ = decodeLabels(data)
			return
		}
		_, _ = decodeProperties(data)
	})
}
