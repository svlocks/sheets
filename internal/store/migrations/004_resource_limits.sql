-- Bound each durable value without limiting graph cardinality. Keep these byte
-- constants synchronized with internal/domain/limits.go. length(CAST(... AS
-- BLOB)) is intentional: SQLite length(TEXT) stops at an embedded NUL.

CREATE TRIGGER revisions_resource_limits_insert
BEFORE INSERT ON revisions
BEGIN
    SELECT RAISE(ABORT, 'sheets_resource_limit_revision_actor')
    WHERE length(CAST(NEW.actor AS BLOB)) > 65536;
    SELECT RAISE(ABORT, 'sheets_resource_limit_revision_message')
    WHERE length(CAST(NEW.message AS BLOB)) > 1048576;
END;

CREATE TRIGGER node_versions_resource_limits_insert
BEFORE INSERT ON node_versions
BEGIN
    SELECT RAISE(ABORT, 'sheets_resource_limit_node_envelope')
    WHERE length(CAST(NEW.labels AS BLOB)) > 67108864
       OR length(CAST(NEW.properties AS BLOB)) > 67108864
       OR length(CAST(NEW.body AS BLOB)) > 67108864;
    SELECT RAISE(ABORT, 'sheets_resource_limit_label')
    WHERE EXISTS (
        SELECT 1 FROM json_each(CAST(NEW.labels AS TEXT)) AS label
        WHERE length(CAST(label.value AS BLOB)) > 65536
           OR instr(label.value, char(0)) <> 0
    );
    SELECT RAISE(ABORT, 'sheets_resource_limit_label_count')
    WHERE COALESCE(json_array_length(CAST(NEW.labels AS TEXT)), 0) > 1000000;
    SELECT RAISE(ABORT, 'sheets_resource_limit_property_shape')
    WHERE EXISTS (
        WITH RECURSIVE
        tree(id, parent, key, type) AS MATERIALIZED (
            SELECT id, parent, key, type
            FROM json_tree(CAST(NEW.properties AS TEXT))
        ),
        property_values(id, depth) AS (
            SELECT id, 0 FROM tree WHERE parent IS NULL
            UNION ALL
            SELECT child.id, property_values.depth + 1
            FROM property_values
            JOIN tree AS container
              ON container.parent = property_values.id
             AND container.key IN ('a', 'o')
            JOIN tree AS child
              ON child.parent = container.id
             AND child.type = 'object'
        )
        SELECT 1
        FROM (SELECT COUNT(*) AS value_count, MAX(depth) AS maximum_depth FROM property_values)
        WHERE value_count > 1000000 OR maximum_depth > 128
    );
    SELECT RAISE(ABORT, 'sheets_resource_limit_property_key')
    WHERE EXISTS (
        SELECT 1
        FROM json_tree(CAST(NEW.properties AS TEXT)) AS property
        JOIN json_tree(CAST(NEW.properties AS TEXT)) AS container
          ON container.id = property.parent
        WHERE container.key = 'o'
          AND container.type = 'object'
          AND length(CAST(property.key AS BLOB)) > 65536
    );
    SELECT RAISE(ABORT, 'sheets_resource_limit_property_string')
    WHERE EXISTS (
        SELECT 1
        FROM json_tree(CAST(NEW.properties AS TEXT)) AS payload
        JOIN json_tree(CAST(NEW.properties AS TEXT)) AS kind
          ON kind.parent = payload.parent
        WHERE payload.key = 's'
          AND payload.type = 'text'
          AND kind.key = 'k'
          AND kind.value = 'string'
          AND length(CAST(payload.value AS BLOB)) > 16777216
    );
    SELECT RAISE(ABORT, 'sheets_resource_limit_property_bytes')
    WHERE EXISTS (
        SELECT 1
        FROM json_tree(CAST(NEW.properties AS TEXT)) AS payload
        JOIN json_tree(CAST(NEW.properties AS TEXT)) AS kind
          ON kind.parent = payload.parent
        WHERE payload.key = 's'
          AND payload.type = 'text'
          AND kind.key = 'k'
          AND kind.value = 'bytes'
          AND (length(CAST(payload.value AS BLOB)) > 22369624
               OR (length(CAST(payload.value AS BLOB)) = 22369624
                   AND substr(payload.value, -2) <> '=='))
    );
    SELECT RAISE(ABORT, 'sheets_resource_limit_time_zone')
    WHERE EXISTS (
        SELECT 1
        FROM json_tree(CAST(NEW.properties AS TEXT)) AS payload
        JOIN json_tree(CAST(NEW.properties AS TEXT)) AS kind
          ON kind.parent = payload.parent
        WHERE payload.key = 'z'
          AND payload.type = 'text'
          AND kind.key = 'k'
          AND kind.value = 'time'
          AND length(CAST(payload.value AS BLOB)) > 65536
    );
END;

CREATE TRIGGER node_versions_resource_limits_update
BEFORE UPDATE OF labels, properties, body ON node_versions
BEGIN
    SELECT RAISE(ABORT, 'sheets_resource_limit_node_envelope')
    WHERE length(CAST(NEW.labels AS BLOB)) > 67108864
       OR length(CAST(NEW.properties AS BLOB)) > 67108864
       OR length(CAST(NEW.body AS BLOB)) > 67108864;
    SELECT RAISE(ABORT, 'sheets_resource_limit_label')
    WHERE EXISTS (
        SELECT 1 FROM json_each(CAST(NEW.labels AS TEXT)) AS label
        WHERE length(CAST(label.value AS BLOB)) > 65536
           OR instr(label.value, char(0)) <> 0
    );
    SELECT RAISE(ABORT, 'sheets_resource_limit_label_count')
    WHERE COALESCE(json_array_length(CAST(NEW.labels AS TEXT)), 0) > 1000000;
    SELECT RAISE(ABORT, 'sheets_resource_limit_property_shape')
    WHERE EXISTS (
        WITH RECURSIVE
        tree(id, parent, key, type) AS MATERIALIZED (
            SELECT id, parent, key, type
            FROM json_tree(CAST(NEW.properties AS TEXT))
        ),
        property_values(id, depth) AS (
            SELECT id, 0 FROM tree WHERE parent IS NULL
            UNION ALL
            SELECT child.id, property_values.depth + 1
            FROM property_values
            JOIN tree AS container
              ON container.parent = property_values.id
             AND container.key IN ('a', 'o')
            JOIN tree AS child
              ON child.parent = container.id
             AND child.type = 'object'
        )
        SELECT 1
        FROM (SELECT COUNT(*) AS value_count, MAX(depth) AS maximum_depth FROM property_values)
        WHERE value_count > 1000000 OR maximum_depth > 128
    );
    SELECT RAISE(ABORT, 'sheets_resource_limit_property_key')
    WHERE EXISTS (
        SELECT 1
        FROM json_tree(CAST(NEW.properties AS TEXT)) AS property
        JOIN json_tree(CAST(NEW.properties AS TEXT)) AS container
          ON container.id = property.parent
        WHERE container.key = 'o'
          AND container.type = 'object'
          AND length(CAST(property.key AS BLOB)) > 65536
    );
    SELECT RAISE(ABORT, 'sheets_resource_limit_property_string')
    WHERE EXISTS (
        SELECT 1
        FROM json_tree(CAST(NEW.properties AS TEXT)) AS payload
        JOIN json_tree(CAST(NEW.properties AS TEXT)) AS kind
          ON kind.parent = payload.parent
        WHERE payload.key = 's'
          AND payload.type = 'text'
          AND kind.key = 'k'
          AND kind.value = 'string'
          AND length(CAST(payload.value AS BLOB)) > 16777216
    );
    SELECT RAISE(ABORT, 'sheets_resource_limit_property_bytes')
    WHERE EXISTS (
        SELECT 1
        FROM json_tree(CAST(NEW.properties AS TEXT)) AS payload
        JOIN json_tree(CAST(NEW.properties AS TEXT)) AS kind
          ON kind.parent = payload.parent
        WHERE payload.key = 's'
          AND payload.type = 'text'
          AND kind.key = 'k'
          AND kind.value = 'bytes'
          AND (length(CAST(payload.value AS BLOB)) > 22369624
               OR (length(CAST(payload.value AS BLOB)) = 22369624
                   AND substr(payload.value, -2) <> '=='))
    );
    SELECT RAISE(ABORT, 'sheets_resource_limit_time_zone')
    WHERE EXISTS (
        SELECT 1
        FROM json_tree(CAST(NEW.properties AS TEXT)) AS payload
        JOIN json_tree(CAST(NEW.properties AS TEXT)) AS kind
          ON kind.parent = payload.parent
        WHERE payload.key = 'z'
          AND payload.type = 'text'
          AND kind.key = 'k'
          AND kind.value = 'time'
          AND length(CAST(payload.value AS BLOB)) > 65536
    );
END;

CREATE TRIGGER edge_versions_resource_limits_insert
BEFORE INSERT ON edge_versions
BEGIN
    SELECT RAISE(ABORT, 'sheets_resource_limit_edge_envelope')
    WHERE length(CAST(NEW.type AS BLOB)) > 65536
       OR length(CAST(NEW.properties AS BLOB)) > 67108864;
    SELECT RAISE(ABORT, 'sheets_resource_limit_property_shape')
    WHERE EXISTS (
        WITH RECURSIVE
        tree(id, parent, key, type) AS MATERIALIZED (
            SELECT id, parent, key, type
            FROM json_tree(CAST(NEW.properties AS TEXT))
        ),
        property_values(id, depth) AS (
            SELECT id, 0 FROM tree WHERE parent IS NULL
            UNION ALL
            SELECT child.id, property_values.depth + 1
            FROM property_values
            JOIN tree AS container
              ON container.parent = property_values.id
             AND container.key IN ('a', 'o')
            JOIN tree AS child
              ON child.parent = container.id
             AND child.type = 'object'
        )
        SELECT 1
        FROM (SELECT COUNT(*) AS value_count, MAX(depth) AS maximum_depth FROM property_values)
        WHERE value_count > 1000000 OR maximum_depth > 128
    );
    SELECT RAISE(ABORT, 'sheets_resource_limit_property_key')
    WHERE EXISTS (
        SELECT 1
        FROM json_tree(CAST(NEW.properties AS TEXT)) AS property
        JOIN json_tree(CAST(NEW.properties AS TEXT)) AS container
          ON container.id = property.parent
        WHERE container.key = 'o'
          AND container.type = 'object'
          AND length(CAST(property.key AS BLOB)) > 65536
    );
    SELECT RAISE(ABORT, 'sheets_resource_limit_property_string')
    WHERE EXISTS (
        SELECT 1
        FROM json_tree(CAST(NEW.properties AS TEXT)) AS payload
        JOIN json_tree(CAST(NEW.properties AS TEXT)) AS kind
          ON kind.parent = payload.parent
        WHERE payload.key = 's'
          AND payload.type = 'text'
          AND kind.key = 'k'
          AND kind.value = 'string'
          AND length(CAST(payload.value AS BLOB)) > 16777216
    );
    SELECT RAISE(ABORT, 'sheets_resource_limit_property_bytes')
    WHERE EXISTS (
        SELECT 1
        FROM json_tree(CAST(NEW.properties AS TEXT)) AS payload
        JOIN json_tree(CAST(NEW.properties AS TEXT)) AS kind
          ON kind.parent = payload.parent
        WHERE payload.key = 's'
          AND payload.type = 'text'
          AND kind.key = 'k'
          AND kind.value = 'bytes'
          AND (length(CAST(payload.value AS BLOB)) > 22369624
               OR (length(CAST(payload.value AS BLOB)) = 22369624
                   AND substr(payload.value, -2) <> '=='))
    );
    SELECT RAISE(ABORT, 'sheets_resource_limit_time_zone')
    WHERE EXISTS (
        SELECT 1
        FROM json_tree(CAST(NEW.properties AS TEXT)) AS payload
        JOIN json_tree(CAST(NEW.properties AS TEXT)) AS kind
          ON kind.parent = payload.parent
        WHERE payload.key = 'z'
          AND payload.type = 'text'
          AND kind.key = 'k'
          AND kind.value = 'time'
          AND length(CAST(payload.value AS BLOB)) > 65536
    );
END;

CREATE TRIGGER edge_versions_resource_limits_update
BEFORE UPDATE OF type, properties ON edge_versions
BEGIN
    SELECT RAISE(ABORT, 'sheets_resource_limit_edge_envelope')
    WHERE length(CAST(NEW.type AS BLOB)) > 65536
       OR length(CAST(NEW.properties AS BLOB)) > 67108864;
    SELECT RAISE(ABORT, 'sheets_resource_limit_property_shape')
    WHERE EXISTS (
        WITH RECURSIVE
        tree(id, parent, key, type) AS MATERIALIZED (
            SELECT id, parent, key, type
            FROM json_tree(CAST(NEW.properties AS TEXT))
        ),
        property_values(id, depth) AS (
            SELECT id, 0 FROM tree WHERE parent IS NULL
            UNION ALL
            SELECT child.id, property_values.depth + 1
            FROM property_values
            JOIN tree AS container
              ON container.parent = property_values.id
             AND container.key IN ('a', 'o')
            JOIN tree AS child
              ON child.parent = container.id
             AND child.type = 'object'
        )
        SELECT 1
        FROM (SELECT COUNT(*) AS value_count, MAX(depth) AS maximum_depth FROM property_values)
        WHERE value_count > 1000000 OR maximum_depth > 128
    );
    SELECT RAISE(ABORT, 'sheets_resource_limit_property_key')
    WHERE EXISTS (
        SELECT 1
        FROM json_tree(CAST(NEW.properties AS TEXT)) AS property
        JOIN json_tree(CAST(NEW.properties AS TEXT)) AS container
          ON container.id = property.parent
        WHERE container.key = 'o'
          AND container.type = 'object'
          AND length(CAST(property.key AS BLOB)) > 65536
    );
    SELECT RAISE(ABORT, 'sheets_resource_limit_property_string')
    WHERE EXISTS (
        SELECT 1
        FROM json_tree(CAST(NEW.properties AS TEXT)) AS payload
        JOIN json_tree(CAST(NEW.properties AS TEXT)) AS kind
          ON kind.parent = payload.parent
        WHERE payload.key = 's'
          AND payload.type = 'text'
          AND kind.key = 'k'
          AND kind.value = 'string'
          AND length(CAST(payload.value AS BLOB)) > 16777216
    );
    SELECT RAISE(ABORT, 'sheets_resource_limit_property_bytes')
    WHERE EXISTS (
        SELECT 1
        FROM json_tree(CAST(NEW.properties AS TEXT)) AS payload
        JOIN json_tree(CAST(NEW.properties AS TEXT)) AS kind
          ON kind.parent = payload.parent
        WHERE payload.key = 's'
          AND payload.type = 'text'
          AND kind.key = 'k'
          AND kind.value = 'bytes'
          AND (length(CAST(payload.value AS BLOB)) > 22369624
               OR (length(CAST(payload.value AS BLOB)) = 22369624
                   AND substr(payload.value, -2) <> '=='))
    );
    SELECT RAISE(ABORT, 'sheets_resource_limit_time_zone')
    WHERE EXISTS (
        SELECT 1
        FROM json_tree(CAST(NEW.properties AS TEXT)) AS payload
        JOIN json_tree(CAST(NEW.properties AS TEXT)) AS kind
          ON kind.parent = payload.parent
        WHERE payload.key = 'z'
          AND payload.type = 'text'
          AND kind.key = 'k'
          AND kind.value = 'time'
          AND length(CAST(payload.value AS BLOB)) > 65536
    );
END;

CREATE TRIGGER node_version_labels_resource_limits_insert
BEFORE INSERT ON node_version_labels
BEGIN
    SELECT RAISE(ABORT, 'sheets_resource_limit_label')
    WHERE length(CAST(NEW.label AS BLOB)) > 65536
       OR instr(NEW.label, char(0)) <> 0;
END;

CREATE TRIGGER node_version_labels_resource_limits_update
BEFORE UPDATE OF label ON node_version_labels
BEGIN
    SELECT RAISE(ABORT, 'sheets_resource_limit_label')
    WHERE length(CAST(NEW.label AS BLOB)) > 65536
       OR instr(NEW.label, char(0)) <> 0;
END;

CREATE TRIGGER node_property_index_resource_limits_insert
BEFORE INSERT ON node_property_index
BEGIN
    SELECT RAISE(ABORT, 'sheets_resource_limit_property_index')
    WHERE length(CAST(NEW.key AS BLOB)) > 65536
       OR length(CAST(NEW.value AS BLOB)) > 67108864;
END;

CREATE TRIGGER node_property_index_resource_limits_update
BEFORE UPDATE OF key, value ON node_property_index
BEGIN
    SELECT RAISE(ABORT, 'sheets_resource_limit_property_index')
    WHERE length(CAST(NEW.key AS BLOB)) > 65536
       OR length(CAST(NEW.value AS BLOB)) > 67108864;
END;

CREATE TRIGGER edge_property_index_resource_limits_insert
BEFORE INSERT ON edge_property_index
BEGIN
    SELECT RAISE(ABORT, 'sheets_resource_limit_property_index')
    WHERE length(CAST(NEW.key AS BLOB)) > 65536
       OR length(CAST(NEW.value AS BLOB)) > 67108864;
END;

CREATE TRIGGER edge_property_index_resource_limits_update
BEFORE UPDATE OF key, value ON edge_property_index
BEGIN
    SELECT RAISE(ABORT, 'sheets_resource_limit_property_index')
    WHERE length(CAST(NEW.key AS BLOB)) > 65536
       OR length(CAST(NEW.value AS BLOB)) > 67108864;
END;

-- SQLite does not specify the order of triggers sharing the same timing and
-- event. Rebuild the v2 triggers that parse JSON so every possible first
-- trigger performs the cheap envelope check before invoking JSON functions.
DROP TRIGGER node_versions_validate_insert;
CREATE TRIGGER node_versions_validate_insert
BEFORE INSERT ON node_versions
BEGIN
    SELECT RAISE(ABORT, 'sheets_resource_limit_node_envelope')
    WHERE length(CAST(NEW.labels AS BLOB)) > 67108864
       OR length(CAST(NEW.properties AS BLOB)) > 67108864
       OR length(CAST(NEW.body AS BLOB)) > 67108864;
    SELECT RAISE(ABORT, 'sheets_node_version_shape')
    WHERE NEW.valid_to IS NOT NULL
       OR typeof(NEW.id) <> 'text'
       OR typeof(NEW.valid_from) <> 'integer'
       OR typeof(NEW.labels) <> 'blob'
       OR json_valid(CAST(NEW.labels AS TEXT)) <> 1
       OR json_type(CAST(NEW.labels AS TEXT)) NOT IN ('array', 'null')
       OR (json_type(CAST(NEW.labels AS TEXT)) = 'array' AND EXISTS (
            SELECT 1 FROM json_each(CAST(NEW.labels AS TEXT))
            WHERE type <> 'text'
       ))
       OR typeof(NEW.properties) <> 'blob'
       OR json_valid(CAST(NEW.properties AS TEXT)) <> 1
       OR json_extract(CAST(NEW.properties AS TEXT), '$.k') NOT IN ('map', 'null')
       OR typeof(NEW.body) <> 'text';
    SELECT RAISE(ABORT, 'sheets_revision_inactive')
    WHERE NEW.valid_from <> (SELECT MAX(revision) FROM revisions)
       OR COALESCE((SELECT sealed FROM revisions WHERE revision = NEW.valid_from), 1) <> 0;
    SELECT RAISE(ABORT, 'sheets_node_history')
    WHERE NOT (
        (NOT EXISTS (SELECT 1 FROM node_versions WHERE id = NEW.id)
         AND NEW.valid_from = (SELECT created_revision FROM nodes WHERE id = NEW.id))
        OR
        (EXISTS (
            SELECT 1 FROM node_versions
            WHERE id = NEW.id AND valid_to = NEW.valid_from
         )
         AND NOT EXISTS (
            SELECT 1 FROM node_versions
            WHERE id = NEW.id AND valid_to IS NULL
         ))
    );
END;

DROP TRIGGER edge_versions_validate_insert;
CREATE TRIGGER edge_versions_validate_insert
BEFORE INSERT ON edge_versions
BEGIN
    SELECT RAISE(ABORT, 'sheets_resource_limit_edge_envelope')
    WHERE length(CAST(NEW.type AS BLOB)) > 65536
       OR length(CAST(NEW.properties AS BLOB)) > 67108864;
    SELECT RAISE(ABORT, 'sheets_edge_version_shape')
    WHERE NEW.valid_to IS NOT NULL
       OR typeof(NEW.id) <> 'text'
       OR typeof(NEW.valid_from) <> 'integer'
       OR typeof(NEW.from_id) <> 'text'
       OR typeof(NEW.type) <> 'text'
       OR length(NEW.type) = 0
       OR instr(NEW.type, char(0)) <> 0
       OR typeof(NEW.to_id) <> 'text'
       OR (NEW.position IS NOT NULL AND typeof(NEW.position) <> 'integer')
       OR (NEW.type <> 'CHILD' AND NEW.position IS NOT NULL)
       OR typeof(NEW.properties) <> 'blob'
       OR json_valid(CAST(NEW.properties AS TEXT)) <> 1
       OR json_extract(CAST(NEW.properties AS TEXT), '$.k') NOT IN ('map', 'null');
    SELECT RAISE(ABORT, 'sheets_revision_inactive')
    WHERE NEW.valid_from <> (SELECT MAX(revision) FROM revisions)
       OR COALESCE((SELECT sealed FROM revisions WHERE revision = NEW.valid_from), 1) <> 0;
    SELECT RAISE(ABORT, 'sheets_edge_history')
    WHERE NOT (
        (NOT EXISTS (SELECT 1 FROM edge_versions WHERE id = NEW.id)
         AND NEW.valid_from = (SELECT created_revision FROM edges WHERE id = NEW.id))
        OR
        (EXISTS (
            SELECT 1 FROM edge_versions
            WHERE id = NEW.id AND valid_to = NEW.valid_from
         )
         AND NOT EXISTS (
            SELECT 1 FROM edge_versions
            WHERE id = NEW.id AND valid_to IS NULL
         ))
    );
    SELECT RAISE(ABORT, 'sheets_edge_endpoint')
    WHERE NOT EXISTS (
            SELECT 1 FROM node_versions
            WHERE id = NEW.from_id AND valid_to IS NULL
          )
       OR NOT EXISTS (
            SELECT 1 FROM node_versions
            WHERE id = NEW.to_id AND valid_to IS NULL
          );
    SELECT RAISE(ABORT, 'sheets_child_cycle')
    WHERE NEW.type = 'CHILD' AND EXISTS (
        WITH RECURSIVE descendants(id) AS (
            SELECT NEW.to_id
            UNION
            SELECT versions.to_id
            FROM edge_versions AS versions
            JOIN descendants ON versions.from_id = descendants.id
            WHERE versions.valid_to IS NULL
              AND versions.type = 'CHILD'
              AND versions.id <> NEW.id
        )
        SELECT 1 FROM descendants WHERE id = NEW.from_id
    );
END;

DROP TRIGGER edge_versions_validate_update_graph;
CREATE TRIGGER edge_versions_validate_update_graph
BEFORE UPDATE OF from_id, type, to_id, position, properties ON edge_versions
WHEN NEW.valid_to IS NULL
BEGIN
    SELECT RAISE(ABORT, 'sheets_resource_limit_edge_envelope')
    WHERE length(CAST(NEW.type AS BLOB)) > 65536
       OR length(CAST(NEW.properties AS BLOB)) > 67108864;
    SELECT RAISE(ABORT, 'sheets_edge_version_shape')
    WHERE typeof(NEW.from_id) <> 'text'
       OR typeof(NEW.type) <> 'text'
       OR length(NEW.type) = 0
       OR instr(NEW.type, char(0)) <> 0
       OR typeof(NEW.to_id) <> 'text'
       OR (NEW.position IS NOT NULL AND typeof(NEW.position) <> 'integer')
       OR (NEW.type <> 'CHILD' AND NEW.position IS NOT NULL)
       OR typeof(NEW.properties) <> 'blob'
       OR json_valid(CAST(NEW.properties AS TEXT)) <> 1
       OR json_extract(CAST(NEW.properties AS TEXT), '$.k') NOT IN ('map', 'null');
    SELECT RAISE(ABORT, 'sheets_edge_endpoint')
    WHERE NOT EXISTS (
            SELECT 1 FROM node_versions
            WHERE id = NEW.from_id AND valid_to IS NULL
          )
       OR NOT EXISTS (
            SELECT 1 FROM node_versions
            WHERE id = NEW.to_id AND valid_to IS NULL
          );
    SELECT RAISE(ABORT, 'sheets_child_cycle')
    WHERE NEW.type = 'CHILD' AND EXISTS (
        WITH RECURSIVE descendants(id) AS (
            SELECT NEW.to_id
            UNION
            SELECT versions.to_id
            FROM edge_versions AS versions
            JOIN descendants ON versions.from_id = descendants.id
            WHERE versions.valid_to IS NULL
              AND versions.type = 'CHILD'
              AND versions.id <> NEW.id
        )
        SELECT 1 FROM descendants WHERE id = NEW.from_id
    );
END;
