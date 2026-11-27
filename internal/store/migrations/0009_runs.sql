-- Software-Fabrik (M9): Runs und ihre Schritte

CREATE TABLE runs (
    id           TEXT PRIMARY KEY,
    project_id   TEXT NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
    -- Workspace aller Rollen; kein Fremdschlüssel: der Run bleibt nach dem Verwerfen sichtbar
    workspace_id TEXT NOT NULL DEFAULT '',
    factory      TEXT NOT NULL,
    -- project | builtin
    source       TEXT NOT NULL DEFAULT '',
    -- Definition beim Start als JSON (Fortsetzen nach Neustart mit demselben Stand)
    definition   TEXT NOT NULL,
    inputs       TEXT NOT NULL DEFAULT '{}',
    task         TEXT NOT NULL DEFAULT '',
    sandbox      INTEGER NOT NULL DEFAULT 0,
    -- running | waiting | paused | done | failed | canceled
    status       TEXT NOT NULL,
    -- Index des aktuellen Schritts (= Anzahl Schritte, wenn fertig)
    current      INTEGER NOT NULL DEFAULT 0,
    error        TEXT NOT NULL DEFAULT '',
    budget_usd   REAL NOT NULL DEFAULT 0,
    max_duration_s INTEGER NOT NULL DEFAULT 0,
    created_at   TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    updated_at   TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    ended_at     TEXT
);

CREATE INDEX runs_project ON runs (project_id, created_at);
CREATE INDEX runs_status ON runs (status);

CREATE TABLE run_steps (
    run_id      TEXT NOT NULL REFERENCES runs (id) ON DELETE CASCADE,
    idx         INTEGER NOT NULL,
    name        TEXT NOT NULL,
    -- role | gate | action
    kind        TEXT NOT NULL,
    role        TEXT NOT NULL DEFAULT '',
    read_only   INTEGER NOT NULL DEFAULT 0,
    -- pending | running | waiting | done | failed | canceled
    status      TEXT NOT NULL DEFAULT 'pending',
    session_id  TEXT NOT NULL DEFAULT '',
    approval_id TEXT NOT NULL DEFAULT '',
    -- Artefakt der Rolle (Dateiname unter <data>/runs/<run>/)
    output      TEXT NOT NULL DEFAULT '',
    error       TEXT NOT NULL DEFAULT '',
    started_at  TEXT,
    ended_at    TEXT,
    PRIMARY KEY (run_id, idx)
);

CREATE INDEX run_steps_session ON run_steps (session_id);

-- read_only: Session einer Lese-Rolle (Workspace nur lesend: Lese-Container bzw. Schnappschuss)
ALTER TABLE sessions ADD COLUMN read_only INTEGER NOT NULL DEFAULT 0;
