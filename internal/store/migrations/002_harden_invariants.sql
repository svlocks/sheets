-- A revision is mutable only inside the transaction that creates it. Existing
-- version-1 revisions predate sealing and are already committed.
ALTER TABLE revisions
    ADD COLUMN sealed INTEGER NOT NULL DEFAULT 1 CHECK (sealed IN (0, 1));

CREATE UNIQUE INDEX one_unsealed_revision
    ON revisions(sealed) WHERE sealed = 0;

-- Labels are normalized into an indexed side table. The canonical label blob
-- remains on node_versions so old snapshots retain a compact, self-contained
-- representation.
CREATE TABLE node_version_labels (
    id         TEXT NOT NULL,
    valid_from INTEGER NOT NULL,
    label      TEXT NOT NULL,
    PRIMARY KEY (id, valid_from, label),
    FOREIGN KEY (id, valid_from)
        REFERENCES node_versions(id, valid_from) ON DELETE CASCADE
) WITHOUT ROWID;

INSERT INTO node_version_labels(id, valid_from, label)
SELECT versions.id, versions.valid_from, labels.value
FROM node_versions AS versions, json_each(CAST(versions.labels AS TEXT)) AS labels
WHERE json_type(CAST(versions.labels AS TEXT)) = 'array';

CREATE INDEX node_version_labels_by_label
    ON node_version_labels(label, id, valid_from);
CREATE INDEX node_versions_by_close
    ON node_versions(valid_to, id) WHERE valid_to IS NOT NULL;

-- Only top-level scalar property values are indexed. The tagged canonical
-- value bytes preserve exact type identity (including float bit patterns).
CREATE TABLE node_property_index (
    id         TEXT NOT NULL,
    valid_from INTEGER NOT NULL,
    valid_to   INTEGER,
    key        TEXT NOT NULL,
    kind       TEXT NOT NULL CHECK (kind IN ('null', 'bool', 'string', 'bytes', 'time', 'duration', 'int', 'float')),
    value      BLOB NOT NULL,
    PRIMARY KEY (id, valid_from, key),
    FOREIGN KEY (id, valid_from)
        REFERENCES node_versions(id, valid_from) ON DELETE CASCADE,
    CHECK (valid_to IS NULL OR valid_to > valid_from)
) WITHOUT ROWID;

CREATE INDEX node_property_lookup
    ON node_property_index(key, kind, value, id, valid_from, valid_to);

CREATE TABLE edge_property_index (
    id         TEXT NOT NULL,
    valid_from INTEGER NOT NULL,
    valid_to   INTEGER,
    key        TEXT NOT NULL,
    kind       TEXT NOT NULL CHECK (kind IN ('null', 'bool', 'string', 'bytes', 'time', 'duration', 'int', 'float')),
    value      BLOB NOT NULL,
    PRIMARY KEY (id, valid_from, key),
    FOREIGN KEY (id, valid_from)
        REFERENCES edge_versions(id, valid_from) ON DELETE CASCADE,
    CHECK (valid_to IS NULL OR valid_to > valid_from)
) WITHOUT ROWID;

CREATE INDEX edge_property_lookup
    ON edge_property_index(key, kind, value, id, valid_from, valid_to);

CREATE INDEX current_edges_from_page
    ON edge_versions(from_id, id) WHERE valid_to IS NULL;
CREATE INDEX current_edges_to_page
    ON edge_versions(to_id, id) WHERE valid_to IS NULL;
CREATE INDEX current_edges_type_page
    ON edge_versions(type, id) WHERE valid_to IS NULL;
CREATE INDEX edge_versions_from_history
    ON edge_versions(from_id, id, valid_from, valid_to);
CREATE INDEX edge_versions_to_history
    ON edge_versions(to_id, id, valid_from, valid_to);
CREATE INDEX edge_versions_type_history
    ON edge_versions(type, id, valid_from, valid_to);
CREATE INDEX edge_versions_by_close
    ON edge_versions(valid_to, id) WHERE valid_to IS NOT NULL;

-- Revisions are gap-free, have strictly increasing times, and become immutable
-- when sealed immediately before COMMIT.
CREATE TRIGGER revisions_validate_insert
BEFORE INSERT ON revisions
BEGIN
    SELECT RAISE(ABORT, 'sheets_revision_sequence')
    WHERE typeof(NEW.revision) <> 'integer'
       OR NEW.revision <= 0
       OR NEW.revision <> COALESCE((SELECT MAX(revision) FROM revisions), 0) + 1;
    SELECT RAISE(ABORT, 'sheets_revision_time')
    WHERE typeof(NEW.committed_ns) <> 'integer'
       OR EXISTS (
            SELECT 1 FROM revisions
            WHERE committed_ns >= NEW.committed_ns
       );
    SELECT RAISE(ABORT, 'sheets_revision_metadata')
    WHERE typeof(NEW.actor) <> 'text'
       OR typeof(NEW.message) <> 'text'
       OR NEW.sealed <> 0;
END;

CREATE TRIGGER revisions_validate_update
BEFORE UPDATE ON revisions
BEGIN
    SELECT RAISE(ABORT, 'sheets_revision_immutable')
    WHERE OLD.sealed <> 0
       OR NEW.sealed <> 1
       OR NEW.revision IS NOT OLD.revision
       OR NEW.committed_ns IS NOT OLD.committed_ns
       OR NEW.actor IS NOT OLD.actor
       OR NEW.message IS NOT OLD.message;
    SELECT RAISE(ABORT, 'sheets_edge_endpoint')
    WHERE EXISTS (
        SELECT 1 FROM node_versions AS closed
        WHERE closed.valid_to = NEW.revision
          AND NOT EXISTS (
              SELECT 1 FROM node_versions AS replacement
              WHERE replacement.id = closed.id AND replacement.valid_to IS NULL
          )
          AND EXISTS (
              SELECT 1 FROM edge_versions AS edge
              WHERE edge.valid_to IS NULL
                AND (edge.from_id = closed.id OR edge.to_id = closed.id)
          )
    ) OR EXISTS (
        SELECT 1 FROM edge_versions AS edge
        WHERE edge.valid_from = NEW.revision AND edge.valid_to IS NULL
          AND (
              NOT EXISTS (
                  SELECT 1 FROM node_versions AS source
                  WHERE source.id = edge.from_id AND source.valid_to IS NULL
              )
              OR NOT EXISTS (
                  SELECT 1 FROM node_versions AS target
                  WHERE target.id = edge.to_id AND target.valid_to IS NULL
              )
          )
    );
    SELECT RAISE(ABORT, 'sheets_revision_no_change')
    WHERE NOT EXISTS (
        WITH touched(id) AS (
            SELECT id FROM node_versions
            WHERE valid_from = NEW.revision OR valid_to = NEW.revision
            GROUP BY id
        )
        SELECT 1
        FROM touched
        LEFT JOIN node_versions AS before
          ON before.id = touched.id
         AND before.valid_from <= NEW.revision - 1
         AND (before.valid_to IS NULL OR before.valid_to > NEW.revision - 1)
        LEFT JOIN node_versions AS after
          ON after.id = touched.id
         AND after.valid_from <= NEW.revision
         AND (after.valid_to IS NULL OR after.valid_to > NEW.revision)
        WHERE (before.id IS NULL) <> (after.id IS NULL)
           OR before.labels IS NOT after.labels
           OR before.properties IS NOT after.properties
           OR before.body IS NOT after.body
    ) AND NOT EXISTS (
        WITH touched(id) AS (
            SELECT id FROM edge_versions
            WHERE valid_from = NEW.revision OR valid_to = NEW.revision
            GROUP BY id
        )
        SELECT 1
        FROM touched
        LEFT JOIN edge_versions AS before
          ON before.id = touched.id
         AND before.valid_from <= NEW.revision - 1
         AND (before.valid_to IS NULL OR before.valid_to > NEW.revision - 1)
        LEFT JOIN edge_versions AS after
          ON after.id = touched.id
         AND after.valid_from <= NEW.revision
         AND (after.valid_to IS NULL OR after.valid_to > NEW.revision)
        WHERE (before.id IS NULL) <> (after.id IS NULL)
           OR before.from_id IS NOT after.from_id
           OR before.type IS NOT after.type
           OR before.to_id IS NOT after.to_id
           OR before.position IS NOT after.position
           OR before.properties IS NOT after.properties
    );
END;

CREATE TRIGGER revisions_validate_delete
BEFORE DELETE ON revisions
BEGIN
    SELECT RAISE(ABORT, 'sheets_revision_immutable')
    WHERE OLD.sealed <> 0
       OR OLD.revision <> (SELECT MAX(revision) FROM revisions);
END;

-- Identity rows may only be created or removed by their active, unsealed
-- revision. Stable identities from committed history are immutable.
CREATE TRIGGER nodes_validate_insert
BEFORE INSERT ON nodes
BEGIN
    SELECT RAISE(ABORT, 'sheets_entity_id')
    WHERE typeof(NEW.id) <> 'text'
       OR length(NEW.id) <> 36
       OR NEW.id <> lower(NEW.id)
       OR substr(NEW.id, 9, 1) <> '-'
       OR substr(NEW.id, 14, 1) <> '-'
       OR substr(NEW.id, 19, 1) <> '-'
       OR substr(NEW.id, 24, 1) <> '-'
       OR length(replace(NEW.id, '-', '')) <> 32
       OR replace(NEW.id, '-', '') GLOB '*[^0-9a-f]*'
       OR substr(NEW.id, 15, 1) <> '7'
       OR substr(NEW.id, 20, 1) NOT GLOB '[89ab]';
    SELECT RAISE(ABORT, 'sheets_revision_inactive')
    WHERE NEW.created_revision <> (SELECT MAX(revision) FROM revisions)
       OR COALESCE((SELECT sealed FROM revisions WHERE revision = NEW.created_revision), 1) <> 0;
END;

CREATE TRIGGER nodes_validate_update
BEFORE UPDATE ON nodes
BEGIN
    SELECT RAISE(ABORT, 'sheets_identity_immutable');
END;

CREATE TRIGGER nodes_validate_delete
BEFORE DELETE ON nodes
BEGIN
    SELECT RAISE(ABORT, 'sheets_identity_immutable')
    WHERE OLD.created_revision <> (SELECT MAX(revision) FROM revisions)
       OR COALESCE((SELECT sealed FROM revisions WHERE revision = OLD.created_revision), 1) <> 0;
END;

CREATE TRIGGER edges_validate_insert
BEFORE INSERT ON edges
BEGIN
    SELECT RAISE(ABORT, 'sheets_entity_id')
    WHERE typeof(NEW.id) <> 'text'
       OR length(NEW.id) <> 36
       OR NEW.id <> lower(NEW.id)
       OR substr(NEW.id, 9, 1) <> '-'
       OR substr(NEW.id, 14, 1) <> '-'
       OR substr(NEW.id, 19, 1) <> '-'
       OR substr(NEW.id, 24, 1) <> '-'
       OR length(replace(NEW.id, '-', '')) <> 32
       OR replace(NEW.id, '-', '') GLOB '*[^0-9a-f]*'
       OR substr(NEW.id, 15, 1) <> '7'
       OR substr(NEW.id, 20, 1) NOT GLOB '[89ab]';
    SELECT RAISE(ABORT, 'sheets_revision_inactive')
    WHERE NEW.created_revision <> (SELECT MAX(revision) FROM revisions)
       OR COALESCE((SELECT sealed FROM revisions WHERE revision = NEW.created_revision), 1) <> 0;
END;

CREATE TRIGGER edges_validate_update
BEFORE UPDATE ON edges
BEGIN
    SELECT RAISE(ABORT, 'sheets_identity_immutable');
END;

CREATE TRIGGER edges_validate_delete
BEFORE DELETE ON edges
BEGIN
    SELECT RAISE(ABORT, 'sheets_identity_immutable')
    WHERE OLD.created_revision <> (SELECT MAX(revision) FROM revisions)
       OR COALESCE((SELECT sealed FROM revisions WHERE revision = OLD.created_revision), 1) <> 0;
END;

-- Node histories are append-only half-open intervals. Content may be edited in
-- place only when the version belongs to the active revision.
CREATE TRIGGER node_versions_validate_insert
BEFORE INSERT ON node_versions
BEGIN
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

CREATE TRIGGER node_versions_validate_update
BEFORE UPDATE ON node_versions
BEGIN
    SELECT RAISE(ABORT, 'sheets_node_history')
    WHERE NEW.id IS NOT OLD.id
       OR NEW.valid_from IS NOT OLD.valid_from
       OR NOT (
            (OLD.valid_to IS NULL
             AND NEW.valid_to = (SELECT MAX(revision) FROM revisions)
             AND COALESCE((SELECT sealed FROM revisions WHERE revision = NEW.valid_to), 1) = 0
             AND NEW.labels IS OLD.labels
             AND NEW.properties IS OLD.properties
             AND NEW.body IS OLD.body)
            OR
            (OLD.valid_from = (SELECT MAX(revision) FROM revisions)
             AND COALESCE((SELECT sealed FROM revisions WHERE revision = OLD.valid_from), 1) = 0
             AND NEW.valid_to IS OLD.valid_to
             AND NEW.valid_to IS NULL)
       );
END;

CREATE TRIGGER node_versions_validate_delete
BEFORE DELETE ON node_versions
BEGIN
    SELECT RAISE(ABORT, 'sheets_node_history')
    WHERE OLD.valid_from <> (SELECT MAX(revision) FROM revisions)
       OR COALESCE((SELECT sealed FROM revisions WHERE revision = OLD.valid_from), 1) <> 0;
END;

CREATE TRIGGER node_version_labels_insert
AFTER INSERT ON node_versions
BEGIN
    INSERT INTO node_version_labels(id, valid_from, label)
    SELECT NEW.id, NEW.valid_from, value
    FROM json_each(CAST(NEW.labels AS TEXT))
    WHERE json_type(CAST(NEW.labels AS TEXT)) = 'array';
END;

CREATE TRIGGER node_version_labels_update
AFTER UPDATE OF labels ON node_versions
BEGIN
    DELETE FROM node_version_labels
    WHERE id = OLD.id AND valid_from = OLD.valid_from;
    INSERT INTO node_version_labels(id, valid_from, label)
    SELECT NEW.id, NEW.valid_from, value
    FROM json_each(CAST(NEW.labels AS TEXT))
    WHERE json_type(CAST(NEW.labels AS TEXT)) = 'array';
END;

CREATE TRIGGER node_property_index_close
AFTER UPDATE OF valid_to ON node_versions
BEGIN
    UPDATE node_property_index
    SET valid_to = NEW.valid_to
    WHERE id = NEW.id AND valid_from = NEW.valid_from;
END;

-- Scalar indexes are derived at the SQLite boundary as well as by the Go
-- writer.  This keeps an otherwise valid raw/import write from silently
-- producing incorrect indexed reads.  The Go writer's explicit replacement is
-- intentionally retained as a deterministic repair path for older databases
-- while a migration is in progress.
CREATE TRIGGER node_property_index_insert
AFTER INSERT ON node_versions
BEGIN
    INSERT INTO node_property_index(id, valid_from, valid_to, key, kind, value)
    SELECT NEW.id, NEW.valid_from, NEW.valid_to,
           properties.key,
           json_extract(properties.value, '$.k'),
           CAST(properties.value AS BLOB)
    FROM json_each(CAST(NEW.properties AS TEXT), '$.o') AS properties
    WHERE json_extract(properties.value, '$.k') NOT IN ('map', 'list');
END;

CREATE TRIGGER node_property_index_update
AFTER UPDATE OF properties ON node_versions
BEGIN
    DELETE FROM node_property_index
    WHERE id = NEW.id AND valid_from = NEW.valid_from;
    INSERT INTO node_property_index(id, valid_from, valid_to, key, kind, value)
    SELECT NEW.id, NEW.valid_from, NEW.valid_to,
           properties.key,
           json_extract(properties.value, '$.k'),
           CAST(properties.value AS BLOB)
    FROM json_each(CAST(NEW.properties AS TEXT), '$.o') AS properties
    WHERE json_extract(properties.value, '$.k') NOT IN ('map', 'list');
END;

-- Edge histories have the same append-only rules and additionally enforce
-- current endpoints and CHILD acyclicity at the SQLite boundary.
CREATE TRIGGER edge_versions_validate_insert
BEFORE INSERT ON edge_versions
BEGIN
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

CREATE TRIGGER edge_versions_validate_update
BEFORE UPDATE ON edge_versions
BEGIN
    SELECT RAISE(ABORT, 'sheets_edge_history')
    WHERE NEW.id IS NOT OLD.id
       OR NEW.valid_from IS NOT OLD.valid_from
       OR NOT (
            (OLD.valid_to IS NULL
             AND NEW.valid_to = (SELECT MAX(revision) FROM revisions)
             AND COALESCE((SELECT sealed FROM revisions WHERE revision = NEW.valid_to), 1) = 0
             AND NEW.from_id IS OLD.from_id
             AND NEW.type IS OLD.type
             AND NEW.to_id IS OLD.to_id
             AND NEW.position IS OLD.position
             AND NEW.properties IS OLD.properties)
            OR
            (OLD.valid_from = (SELECT MAX(revision) FROM revisions)
             AND COALESCE((SELECT sealed FROM revisions WHERE revision = OLD.valid_from), 1) = 0
             AND NEW.valid_to IS OLD.valid_to
             AND NEW.valid_to IS NULL)
       );
END;

CREATE TRIGGER edge_versions_validate_update_graph
BEFORE UPDATE OF from_id, type, to_id, position, properties ON edge_versions
WHEN NEW.valid_to IS NULL
BEGIN
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

CREATE TRIGGER edge_versions_validate_delete
BEFORE DELETE ON edge_versions
BEGIN
    SELECT RAISE(ABORT, 'sheets_edge_history')
    WHERE OLD.valid_from <> (SELECT MAX(revision) FROM revisions)
       OR COALESCE((SELECT sealed FROM revisions WHERE revision = OLD.valid_from), 1) <> 0;
END;

CREATE TRIGGER edge_property_index_close
AFTER UPDATE OF valid_to ON edge_versions
BEGIN
    UPDATE edge_property_index
    SET valid_to = NEW.valid_to
    WHERE id = NEW.id AND valid_from = NEW.valid_from;
END;

CREATE TRIGGER edge_property_index_insert
AFTER INSERT ON edge_versions
BEGIN
    INSERT INTO edge_property_index(id, valid_from, valid_to, key, kind, value)
    SELECT NEW.id, NEW.valid_from, NEW.valid_to,
           properties.key,
           json_extract(properties.value, '$.k'),
           CAST(properties.value AS BLOB)
    FROM json_each(CAST(NEW.properties AS TEXT), '$.o') AS properties
    WHERE json_extract(properties.value, '$.k') NOT IN ('map', 'list');
END;

CREATE TRIGGER edge_property_index_update
AFTER UPDATE OF properties ON edge_versions
BEGIN
    DELETE FROM edge_property_index
    WHERE id = NEW.id AND valid_from = NEW.valid_from;
    INSERT INTO edge_property_index(id, valid_from, valid_to, key, kind, value)
    SELECT NEW.id, NEW.valid_from, NEW.valid_to,
           properties.key,
           json_extract(properties.value, '$.k'),
           CAST(properties.value AS BLOB)
    FROM json_each(CAST(NEW.properties AS TEXT), '$.o') AS properties
    WHERE json_extract(properties.value, '$.k') NOT IN ('map', 'list');
END;
