package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/svlocks/sheets/internal/domain"
)

type indexedProperty struct {
	key   string
	kind  string
	value []byte
}

type sqlExecutor interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

func scalarProperties(properties domain.Properties) ([]indexedProperty, error) {
	if len(properties) == 0 {
		return nil, nil
	}
	result := make([]indexedProperty, 0, min(len(properties), domain.MaxIndexedPropertiesPerVersion))
	payloadBytes := int64(0)
	for key, property := range properties {
		if err := domain.ValidateText("property map key", key, domain.MaxPropertyKeyBytes); err != nil {
			return nil, fmt.Errorf("index property %q: %w", key, err)
		}
		state := encodeState{visiting: make(map[encodeReference]struct{})}
		encoded, err := state.encodeValue(property, 0)
		if err != nil {
			return nil, fmt.Errorf("index property %q: %w", key, err)
		}
		if encoded.Kind == "map" || encoded.Kind == "list" {
			continue
		}
		if len(result) == domain.MaxIndexedPropertiesPerVersion {
			return nil, &domain.ResourceLimitError{
				Field: "indexed properties", Unit: "values",
				Limit: domain.MaxIndexedPropertiesPerVersion, Actual: len(result) + 1,
			}
		}
		payloadBytes += int64(len(key)) + canonicalEncodedValueSize(encoded)
		if payloadBytes > int64(domain.MaxDerivedPropertyBytesPerVersion) {
			return nil, &domain.ResourceLimitError{
				Field: "derived property index", Unit: "bytes",
				Limit: domain.MaxDerivedPropertyBytesPerVersion, Actual: int(payloadBytes),
			}
		}
		value, err := json.Marshal(encoded)
		if err != nil {
			return nil, fmt.Errorf("index property %q: %w", key, err)
		}
		result = append(result, indexedProperty{key: key, kind: encoded.Kind, value: value})
	}
	sort.Slice(result, func(i, j int) bool { return result[i].key < result[j].key })
	return result, nil
}

func replacePropertyIndex(
	ctx context.Context,
	executor sqlExecutor,
	table string,
	id domain.EntityID,
	validFrom domain.Revision,
	validTo *domain.Revision,
	properties domain.Properties,
) error {
	if table != "node_property_index" && table != "edge_property_index" {
		return fmt.Errorf("invalid property index table %q", table)
	}
	if _, err := executor.ExecContext(ctx,
		"DELETE FROM "+table+" WHERE id = ? AND valid_from = ?", string(id), int64(validFrom)); err != nil {
		return fmt.Errorf("clear %s: %w", table, err)
	}
	entries, err := scalarProperties(properties)
	if err != nil {
		return err
	}
	var end any
	if validTo != nil {
		end = int64(*validTo)
	}
	for _, entry := range entries {
		if _, err := executor.ExecContext(ctx, `INSERT INTO `+table+`
			(id, valid_from, valid_to, key, kind, value)
			VALUES (?, ?, ?, ?, ?, ?)`,
			string(id), int64(validFrom), end, entry.key, entry.kind, entry.value); err != nil {
			return fmt.Errorf("populate %s for %s property %q: %w", table, id, entry.key, err)
		}
	}
	return nil
}

func backfillPropertyIndexes(ctx context.Context, conn *sql.Conn) error {
	if err := backfillPropertyIndex(ctx, conn, "node_versions", "node_property_index"); err != nil {
		return err
	}
	return backfillPropertyIndex(ctx, conn, "edge_versions", "edge_property_index")
}

func backfillPropertyIndex(ctx context.Context, conn *sql.Conn, versions, index string) error {
	if versions != "node_versions" && versions != "edge_versions" {
		return fmt.Errorf("invalid version table %q", versions)
	}
	rows, err := conn.QueryContext(ctx,
		"SELECT id, valid_from, valid_to, properties FROM "+versions+" ORDER BY id, valid_from")
	if err != nil {
		return fmt.Errorf("read %s for property index backfill: %w", versions, err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var rawID string
		var rawFrom int64
		var rawTo sql.NullInt64
		var data []byte
		if err := rows.Scan(&rawID, &rawFrom, &rawTo, &data); err != nil {
			return fmt.Errorf("scan %s property index backfill: %w", versions, err)
		}
		properties, err := decodeProperties(data)
		if err != nil {
			return fmt.Errorf("decode %s %s properties during migration: %w", versions, rawID, err)
		}
		from := domain.Revision(rawFrom)
		var to *domain.Revision
		if rawTo.Valid {
			value := domain.Revision(rawTo.Int64)
			to = &value
		}
		if err := replacePropertyIndex(ctx, conn, index, domain.EntityID(rawID), from, to, properties); err != nil {
			return fmt.Errorf("backfill %s: %w", index, err)
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read %s for property index backfill: %w", versions, err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close %s property index backfill: %w", versions, err)
	}
	return nil
}
