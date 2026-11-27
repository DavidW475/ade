-- Egress-Events der Sandbox-Proxys (M6)
CREATE TABLE egress_events (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    workspace_id TEXT NOT NULL REFERENCES workspaces (id) ON DELETE CASCADE,
    ts           TEXT NOT NULL,
    -- connect | http | api
    kind         TEXT NOT NULL,
    method       TEXT NOT NULL DEFAULT '',
    host         TEXT NOT NULL,
    port         INTEGER NOT NULL DEFAULT 0,
    path         TEXT NOT NULL DEFAULT '',
    provider     TEXT NOT NULL DEFAULT '',
    allowed      INTEGER NOT NULL,
    reason       TEXT NOT NULL DEFAULT '',
    status       INTEGER NOT NULL DEFAULT 0,
    bytes_out    INTEGER NOT NULL DEFAULT 0,
    bytes_in     INTEGER NOT NULL DEFAULT 0,
    duration_ms  INTEGER NOT NULL DEFAULT 0,
    error        TEXT NOT NULL DEFAULT ''
);

CREATE INDEX egress_events_workspace ON egress_events (workspace_id, id);
