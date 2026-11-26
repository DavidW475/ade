CREATE TABLE sessions (
    id          TEXT PRIMARY KEY,
    workspace_id TEXT NOT NULL REFERENCES workspaces (id) ON DELETE CASCADE,
    adapter     TEXT NOT NULL,
    task        TEXT NOT NULL DEFAULT '',
    -- Befehl als JSON-Array (nur pty-Adapter)
    command     TEXT NOT NULL DEFAULT '[]',
    model       TEXT NOT NULL DEFAULT '',
    permission_mode TEXT NOT NULL DEFAULT '',
    role        TEXT NOT NULL DEFAULT '',
    -- starting | running | idle | done | failed | stopped | interrupted
    status      TEXT NOT NULL,
    error       TEXT NOT NULL DEFAULT '',
    exit_code   INTEGER,
    -- Session-ID der Agent-CLI zum Fortsetzen (z. B. Claude Code --resume)
    resume_id   TEXT NOT NULL DEFAULT '',
    pid         INTEGER NOT NULL DEFAULT 0,
    cost_usd    REAL NOT NULL DEFAULT 0,
    created_at  TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    updated_at  TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    ended_at    TEXT
);

CREATE INDEX sessions_workspace ON sessions (workspace_id);
CREATE INDEX sessions_status ON sessions (status);

CREATE TABLE events (
    session_id TEXT NOT NULL REFERENCES sessions (id) ON DELETE CASCADE,
    seq        INTEGER NOT NULL,
    ts         TEXT NOT NULL,
    type       TEXT NOT NULL,
    payload    TEXT NOT NULL DEFAULT '{}',
    PRIMARY KEY (session_id, seq)
) WITHOUT ROWID;
