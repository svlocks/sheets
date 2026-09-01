-- The CHILD cycle checks below are identical to v5 except that the recursive
-- step names its index. Without INDEXED BY, SQLite may satisfy the recursive
-- join through edge_versions_type_history (type=?), visiting every CHILD
-- version ever written on each CHILD insert or update, which makes bulk child
-- creation quadratic in graph size. current_edges_from (from_id, type, to_id
-- WHERE valid_to IS NULL) is the covering index this walk is designed for,
-- and INDEXED BY makes that choice deterministic across SQLite builds.

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
            FROM edge_versions AS versions INDEXED BY current_edges_from
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
            FROM edge_versions AS versions INDEXED BY current_edges_from
            JOIN descendants ON versions.from_id = descendants.id
            WHERE versions.valid_to IS NULL
              AND versions.type = 'CHILD'
              AND versions.id <> NEW.id
        )
        SELECT 1 FROM descendants WHERE id = NEW.from_id
    );
END;
