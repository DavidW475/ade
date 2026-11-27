-- Freigaben (M7): Anfragen von Agent-Hooks und Egress-Proxys, die ein Mensch entscheidet
CREATE TABLE approvals (
    id           TEXT PRIMARY KEY,
    -- tool | egress
    kind         TEXT NOT NULL,
    project_id   TEXT NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
    workspace_id TEXT NOT NULL REFERENCES workspaces (id) ON DELETE CASCADE,
    -- leer bei egress
    session_id   TEXT NOT NULL DEFAULT '',
    -- Kurzbeschreibung (z. B. "Bash: go test ./..." oder "pypi.org:443")
    title        TEXT NOT NULL,
    -- Inhalt als JSON (Werkzeug und Eingabe bzw. Ziel)
    detail       TEXT NOT NULL DEFAULT '{}',
    -- sandbox: Anfrage kam aus dem Container
    sandbox      INTEGER NOT NULL DEFAULT 0,
    -- pending | approved | denied | expired | canceled
    status       TEXT NOT NULL,
    -- allow | allow_workspace | allow_project | deny (leer, solange offen)
    decision     TEXT NOT NULL DEFAULT '',
    reason       TEXT NOT NULL DEFAULT '',
    created_at   TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    expires_at   TEXT NOT NULL,
    decided_at   TEXT
);

CREATE INDEX approvals_status ON approvals (status, created_at);
CREATE INDEX approvals_session ON approvals (session_id);

-- Egress-Ausnahmen aus Freigaben: ergänzen .ade/sandbox.yaml, ohne das Repo zu ändern
CREATE TABLE egress_grants (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    project_id   TEXT NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
    -- leer: gilt für alle Workspaces des Projekts
    workspace_id TEXT NOT NULL DEFAULT '',
    -- Eintrag im Format der Allowlist (host oder host:port)
    rule         TEXT NOT NULL,
    approval_id  TEXT NOT NULL DEFAULT '',
    created_at   TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    UNIQUE (project_id, workspace_id, rule)
);
