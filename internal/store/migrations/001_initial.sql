CREATE TABLE revisions (
    revision     INTEGER PRIMARY KEY,
    committed_ns INTEGER NOT NULL,
    actor        TEXT NOT NULL,
    message      TEXT NOT NULL
);

CREATE INDEX revisions_by_time
    ON revisions(committed_ns, revision);

CREATE TABLE nodes (
    id               TEXT PRIMARY KEY,
    created_revision INTEGER NOT NULL REFERENCES revisions(revision)
);

CREATE TABLE node_versions (
    id         TEXT NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
    valid_from INTEGER NOT NULL REFERENCES revisions(revision),
    valid_to   INTEGER REFERENCES revisions(revision),
    labels     BLOB NOT NULL,
    properties BLOB NOT NULL,
    body       TEXT NOT NULL,
    PRIMARY KEY (id, valid_from),
    CHECK (valid_to IS NULL OR valid_to > valid_from)
);

CREATE UNIQUE INDEX one_current_node_version
    ON node_versions(id) WHERE valid_to IS NULL;
CREATE INDEX node_versions_at_revision
    ON node_versions(valid_from, valid_to, id);

CREATE TABLE edges (
    id               TEXT PRIMARY KEY,
    created_revision INTEGER NOT NULL REFERENCES revisions(revision)
);

CREATE TABLE edge_versions (
    id         TEXT NOT NULL REFERENCES edges(id) ON DELETE CASCADE,
    valid_from INTEGER NOT NULL REFERENCES revisions(revision),
    valid_to   INTEGER REFERENCES revisions(revision),
    from_id    TEXT NOT NULL REFERENCES nodes(id),
    type       TEXT NOT NULL,
    to_id      TEXT NOT NULL REFERENCES nodes(id),
    position   INTEGER,
    properties BLOB NOT NULL,
    PRIMARY KEY (id, valid_from),
    CHECK (valid_to IS NULL OR valid_to > valid_from),
    CHECK (length(type) > 0),
    CHECK (type = 'CHILD' OR position IS NULL)
);

CREATE UNIQUE INDEX one_current_edge_version
    ON edge_versions(id) WHERE valid_to IS NULL;
CREATE INDEX edge_versions_at_revision
    ON edge_versions(valid_from, valid_to, id);
CREATE INDEX current_edges_from
    ON edge_versions(from_id, type, to_id) WHERE valid_to IS NULL;
CREATE INDEX current_edges_to
    ON edge_versions(to_id, type, from_id) WHERE valid_to IS NULL;
CREATE UNIQUE INDEX one_current_child_parent
    ON edge_versions(to_id) WHERE valid_to IS NULL AND type = 'CHILD';
