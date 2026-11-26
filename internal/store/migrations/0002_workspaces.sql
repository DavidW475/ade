CREATE TABLE workspaces (
    id          TEXT PRIMARY KEY,
    project_id  TEXT NOT NULL REFERENCES projects (id) ON DELETE RESTRICT,
    path        TEXT NOT NULL UNIQUE,
    branch      TEXT NOT NULL,
    base_branch TEXT NOT NULL,
    base_commit TEXT NOT NULL DEFAULT '',
    -- creating | setup | ready | setup_failed | error
    status      TEXT NOT NULL,
    error       TEXT NOT NULL DEFAULT '',
    setup_log   TEXT NOT NULL DEFAULT '',
    created_at  TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now')),
    updated_at  TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now'))
);

CREATE INDEX workspaces_project ON workspaces (project_id);
