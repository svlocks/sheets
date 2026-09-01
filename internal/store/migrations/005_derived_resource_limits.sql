-- Bound amplification into durable lookup B-trees independently of the
-- canonical source envelopes. Keep these constants synchronized with
-- internal/domain/limits.go:
--   4096 rows, 16 MiB of label text, 32 MiB of property key/value bytes.
--
-- Every source trigger starts with the cheap envelope predicate because
-- SQLite does not define the order of triggers with the same timing/event.

CREATE TRIGGER node_versions_derived_limits_insert
BEFORE INSERT ON node_versions
BEGIN
    SELECT RAISE(ABORT, 'sheets_resource_limit_node_envelope')
    WHERE length(CAST(NEW.labels AS BLOB)) > 67108864
       OR length(CAST(NEW.properties AS BLOB)) > 67108864
       OR length(CAST(NEW.body AS BLOB)) > 67108864;
    SELECT RAISE(ABORT, 'sheets_resource_limit_derived_labels')
    WHERE CASE
        WHEN json_valid(CAST(NEW.labels AS TEXT)) <> 1 THEN 0
        WHEN json_type(CAST(NEW.labels AS TEXT)) <> 'array' THEN 0
        ELSE json_array_length(CAST(NEW.labels AS TEXT)) > 4096
          OR COALESCE((
                SELECT SUM(length(CAST(label.value AS BLOB)))
                FROM json_each(CAST(NEW.labels AS TEXT)) AS label
             ), 0) > 16777216
    END;
    SELECT RAISE(ABORT, 'sheets_resource_limit_derived_properties')
    WHERE CASE
        WHEN json_valid(CAST(NEW.properties AS TEXT)) <> 1 THEN 0
        ELSE EXISTS (
            SELECT 1
            FROM json_each(CAST(NEW.properties AS TEXT), '$.o') AS property
            WHERE json_extract(property.value, '$.k') NOT IN ('map', 'list')
            GROUP BY 1
            HAVING COUNT(*) > 4096
                OR COALESCE(SUM(
                    length(CAST(property.key AS BLOB))
                    + length(CAST(property.value AS BLOB))
                ), 0) > 33554432
        )
    END;
END;

CREATE TRIGGER node_versions_derived_limits_update
BEFORE UPDATE OF labels, properties, body ON node_versions
BEGIN
    SELECT RAISE(ABORT, 'sheets_resource_limit_node_envelope')
    WHERE length(CAST(NEW.labels AS BLOB)) > 67108864
       OR length(CAST(NEW.properties AS BLOB)) > 67108864
       OR length(CAST(NEW.body AS BLOB)) > 67108864;
    SELECT RAISE(ABORT, 'sheets_resource_limit_derived_labels')
    WHERE CASE
        WHEN json_valid(CAST(NEW.labels AS TEXT)) <> 1 THEN 0
        WHEN json_type(CAST(NEW.labels AS TEXT)) <> 'array' THEN 0
        ELSE json_array_length(CAST(NEW.labels AS TEXT)) > 4096
          OR COALESCE((
                SELECT SUM(length(CAST(label.value AS BLOB)))
                FROM json_each(CAST(NEW.labels AS TEXT)) AS label
             ), 0) > 16777216
    END;
    SELECT RAISE(ABORT, 'sheets_resource_limit_derived_properties')
    WHERE CASE
        WHEN json_valid(CAST(NEW.properties AS TEXT)) <> 1 THEN 0
        ELSE EXISTS (
            SELECT 1
            FROM json_each(CAST(NEW.properties AS TEXT), '$.o') AS property
            WHERE json_extract(property.value, '$.k') NOT IN ('map', 'list')
            GROUP BY 1
            HAVING COUNT(*) > 4096
                OR COALESCE(SUM(
                    length(CAST(property.key AS BLOB))
                    + length(CAST(property.value AS BLOB))
                ), 0) > 33554432
        )
    END;
END;

CREATE TRIGGER edge_versions_derived_limits_insert
BEFORE INSERT ON edge_versions
BEGIN
    SELECT RAISE(ABORT, 'sheets_resource_limit_edge_envelope')
    WHERE length(CAST(NEW.type AS BLOB)) > 65536
       OR length(CAST(NEW.properties AS BLOB)) > 67108864;
    SELECT RAISE(ABORT, 'sheets_resource_limit_derived_properties')
    WHERE CASE
        WHEN json_valid(CAST(NEW.properties AS TEXT)) <> 1 THEN 0
        ELSE EXISTS (
            SELECT 1
            FROM json_each(CAST(NEW.properties AS TEXT), '$.o') AS property
            WHERE json_extract(property.value, '$.k') NOT IN ('map', 'list')
            GROUP BY 1
            HAVING COUNT(*) > 4096
                OR COALESCE(SUM(
                    length(CAST(property.key AS BLOB))
                    + length(CAST(property.value AS BLOB))
                ), 0) > 33554432
        )
    END;
END;

CREATE TRIGGER edge_versions_derived_limits_update
BEFORE UPDATE OF type, properties ON edge_versions
BEGIN
    SELECT RAISE(ABORT, 'sheets_resource_limit_edge_envelope')
    WHERE length(CAST(NEW.type AS BLOB)) > 65536
       OR length(CAST(NEW.properties AS BLOB)) > 67108864;
    SELECT RAISE(ABORT, 'sheets_resource_limit_derived_properties')
    WHERE CASE
        WHEN json_valid(CAST(NEW.properties AS TEXT)) <> 1 THEN 0
        ELSE EXISTS (
            SELECT 1
            FROM json_each(CAST(NEW.properties AS TEXT), '$.o') AS property
            WHERE json_extract(property.value, '$.k') NOT IN ('map', 'list')
            GROUP BY 1
            HAVING COUNT(*) > 4096
                OR COALESCE(SUM(
                    length(CAST(property.key AS BLOB))
                    + length(CAST(property.value AS BLOB))
                ), 0) > 33554432
        )
    END;
END;

CREATE TRIGGER node_version_labels_derived_limits_insert
BEFORE INSERT ON node_version_labels
BEGIN
    SELECT RAISE(ABORT, 'sheets_resource_limit_label')
    WHERE length(CAST(NEW.label AS BLOB)) > 65536
       OR instr(NEW.label, char(0)) <> 0;
    SELECT RAISE(ABORT, 'sheets_resource_limit_derived_labels')
    WHERE (SELECT COUNT(*) FROM node_version_labels
           WHERE id = NEW.id AND valid_from = NEW.valid_from) >= 4096
       OR COALESCE((
            SELECT SUM(length(CAST(label AS BLOB)))
            FROM node_version_labels
            WHERE id = NEW.id AND valid_from = NEW.valid_from
          ), 0) + length(CAST(NEW.label AS BLOB)) > 16777216;
END;

CREATE TRIGGER node_version_labels_derived_limits_update
BEFORE UPDATE OF id, valid_from, label ON node_version_labels
BEGIN
    SELECT RAISE(ABORT, 'sheets_resource_limit_label')
    WHERE length(CAST(NEW.label AS BLOB)) > 65536
       OR instr(NEW.label, char(0)) <> 0;
    SELECT RAISE(ABORT, 'sheets_resource_limit_derived_labels')
    WHERE (SELECT COUNT(*) FROM node_version_labels
           WHERE id = NEW.id AND valid_from = NEW.valid_from
             AND NOT (id IS OLD.id AND valid_from IS OLD.valid_from AND label IS OLD.label)) >= 4096
       OR COALESCE((
            SELECT SUM(length(CAST(label AS BLOB)))
            FROM node_version_labels
            WHERE id = NEW.id AND valid_from = NEW.valid_from
              AND NOT (id IS OLD.id AND valid_from IS OLD.valid_from AND label IS OLD.label)
          ), 0) + length(CAST(NEW.label AS BLOB)) > 16777216;
END;

CREATE TRIGGER node_property_index_derived_limits_insert
BEFORE INSERT ON node_property_index
BEGIN
    SELECT RAISE(ABORT, 'sheets_resource_limit_property_index')
    WHERE length(CAST(NEW.key AS BLOB)) > 65536
       OR length(CAST(NEW.value AS BLOB)) > 33554432;
    SELECT RAISE(ABORT, 'sheets_resource_limit_derived_properties')
    WHERE (SELECT COUNT(*) FROM node_property_index
           WHERE id = NEW.id AND valid_from = NEW.valid_from) >= 4096
       OR COALESCE((
            SELECT SUM(length(CAST(key AS BLOB)) + length(CAST(value AS BLOB)))
            FROM node_property_index
            WHERE id = NEW.id AND valid_from = NEW.valid_from
          ), 0) + length(CAST(NEW.key AS BLOB)) + length(CAST(NEW.value AS BLOB)) > 33554432;
END;

CREATE TRIGGER node_property_index_derived_limits_update
BEFORE UPDATE OF id, valid_from, key, value ON node_property_index
BEGIN
    SELECT RAISE(ABORT, 'sheets_resource_limit_property_index')
    WHERE length(CAST(NEW.key AS BLOB)) > 65536
       OR length(CAST(NEW.value AS BLOB)) > 33554432;
    SELECT RAISE(ABORT, 'sheets_resource_limit_derived_properties')
    WHERE (SELECT COUNT(*) FROM node_property_index
           WHERE id = NEW.id AND valid_from = NEW.valid_from
             AND NOT (id IS OLD.id AND valid_from IS OLD.valid_from AND key IS OLD.key)) >= 4096
       OR COALESCE((
            SELECT SUM(length(CAST(key AS BLOB)) + length(CAST(value AS BLOB)))
            FROM node_property_index
            WHERE id = NEW.id AND valid_from = NEW.valid_from
              AND NOT (id IS OLD.id AND valid_from IS OLD.valid_from AND key IS OLD.key)
          ), 0) + length(CAST(NEW.key AS BLOB)) + length(CAST(NEW.value AS BLOB)) > 33554432;
END;

CREATE TRIGGER edge_property_index_derived_limits_insert
BEFORE INSERT ON edge_property_index
BEGIN
    SELECT RAISE(ABORT, 'sheets_resource_limit_property_index')
    WHERE length(CAST(NEW.key AS BLOB)) > 65536
       OR length(CAST(NEW.value AS BLOB)) > 33554432;
    SELECT RAISE(ABORT, 'sheets_resource_limit_derived_properties')
    WHERE (SELECT COUNT(*) FROM edge_property_index
           WHERE id = NEW.id AND valid_from = NEW.valid_from) >= 4096
       OR COALESCE((
            SELECT SUM(length(CAST(key AS BLOB)) + length(CAST(value AS BLOB)))
            FROM edge_property_index
            WHERE id = NEW.id AND valid_from = NEW.valid_from
          ), 0) + length(CAST(NEW.key AS BLOB)) + length(CAST(NEW.value AS BLOB)) > 33554432;
END;

CREATE TRIGGER edge_property_index_derived_limits_update
BEFORE UPDATE OF id, valid_from, key, value ON edge_property_index
BEGIN
    SELECT RAISE(ABORT, 'sheets_resource_limit_property_index')
    WHERE length(CAST(NEW.key AS BLOB)) > 65536
       OR length(CAST(NEW.value AS BLOB)) > 33554432;
    SELECT RAISE(ABORT, 'sheets_resource_limit_derived_properties')
    WHERE (SELECT COUNT(*) FROM edge_property_index
           WHERE id = NEW.id AND valid_from = NEW.valid_from
             AND NOT (id IS OLD.id AND valid_from IS OLD.valid_from AND key IS OLD.key)) >= 4096
       OR COALESCE((
            SELECT SUM(length(CAST(key AS BLOB)) + length(CAST(value AS BLOB)))
            FROM edge_property_index
            WHERE id = NEW.id AND valid_from = NEW.valid_from
              AND NOT (id IS OLD.id AND valid_from IS OLD.valid_from AND key IS OLD.key)
          ), 0) + length(CAST(NEW.key AS BLOB)) + length(CAST(NEW.value AS BLOB)) > 33554432;
END;
