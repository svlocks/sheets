-- Extend the exact scalar indexes for the six durable openCypher temporal
-- kinds. SQLite cannot alter a CHECK constraint in place, so both derived
-- index tables are rebuilt inside Open's surrounding BEGIN IMMEDIATE. The
-- canonical node/edge version blobs are deliberately not rewritten.

DROP TRIGGER node_property_index_close;
DROP TRIGGER node_property_index_insert;
DROP TRIGGER node_property_index_update;
DROP TRIGGER edge_property_index_close;
DROP TRIGGER edge_property_index_insert;
DROP TRIGGER edge_property_index_update;

CREATE TABLE node_property_index_v3 (
    id         TEXT NOT NULL,
    valid_from INTEGER NOT NULL,
    valid_to   INTEGER,
    key        TEXT NOT NULL,
    kind       TEXT NOT NULL CHECK (kind IN (
        'null', 'bool', 'string', 'bytes', 'time', 'duration', 'int', 'float',
        'date', 'local_time', 'offset_time', 'local_datetime',
        'zoned_datetime', 'cypher_duration'
    )),
    value      BLOB NOT NULL,
    PRIMARY KEY (id, valid_from, key),
    FOREIGN KEY (id, valid_from)
        REFERENCES node_versions(id, valid_from) ON DELETE CASCADE,
    CHECK (valid_to IS NULL OR valid_to > valid_from)
) WITHOUT ROWID;

INSERT INTO node_property_index_v3(id, valid_from, valid_to, key, kind, value)
SELECT id, valid_from, valid_to, key, kind, value
FROM node_property_index;

DROP TABLE node_property_index;
ALTER TABLE node_property_index_v3 RENAME TO node_property_index;

CREATE INDEX node_property_lookup
    ON node_property_index(key, kind, value, id, valid_from, valid_to);

CREATE TABLE edge_property_index_v3 (
    id         TEXT NOT NULL,
    valid_from INTEGER NOT NULL,
    valid_to   INTEGER,
    key        TEXT NOT NULL,
    kind       TEXT NOT NULL CHECK (kind IN (
        'null', 'bool', 'string', 'bytes', 'time', 'duration', 'int', 'float',
        'date', 'local_time', 'offset_time', 'local_datetime',
        'zoned_datetime', 'cypher_duration'
    )),
    value      BLOB NOT NULL,
    PRIMARY KEY (id, valid_from, key),
    FOREIGN KEY (id, valid_from)
        REFERENCES edge_versions(id, valid_from) ON DELETE CASCADE,
    CHECK (valid_to IS NULL OR valid_to > valid_from)
) WITHOUT ROWID;

INSERT INTO edge_property_index_v3(id, valid_from, valid_to, key, kind, value)
SELECT id, valid_from, valid_to, key, kind, value
FROM edge_property_index;

DROP TABLE edge_property_index;
ALTER TABLE edge_property_index_v3 RENAME TO edge_property_index;

CREATE INDEX edge_property_lookup
    ON edge_property_index(key, kind, value, id, valid_from, valid_to);

CREATE TRIGGER node_property_index_close
AFTER UPDATE OF valid_to ON node_versions
BEGIN
    UPDATE node_property_index
    SET valid_to = NEW.valid_to
    WHERE id = NEW.id AND valid_from = NEW.valid_from;
END;

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
