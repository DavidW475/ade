-- Token-Nutzung und Kosten (M8)

-- Aufgelöste Modell-ID, wenn model ein Alias ist
ALTER TABLE sessions ADD COLUMN model_id TEXT NOT NULL DEFAULT '';
-- Summen der Nutzung (Tabelle usage)
ALTER TABLE sessions ADD COLUMN input_tokens INTEGER NOT NULL DEFAULT 0;
ALTER TABLE sessions ADD COLUMN output_tokens INTEGER NOT NULL DEFAULT 0;
ALTER TABLE sessions ADD COLUMN cache_read_tokens INTEGER NOT NULL DEFAULT 0;
ALTER TABLE sessions ADD COLUMN cache_write_tokens INTEGER NOT NULL DEFAULT 0;
-- unpriced: Tokens eines Modells ohne bekannten Preis gezählt (Kosten unvollständig)
ALTER TABLE sessions ADD COLUMN unpriced INTEGER NOT NULL DEFAULT 0;
-- metered: der Egress-Proxy misst die Nutzung (Sandbox mit Secret-Injection), die
-- Kostenmeldungen der CLI zählen dann nicht
ALTER TABLE sessions ADD COLUMN metered INTEGER NOT NULL DEFAULT 0;
-- Budget je Session in USD (0: keins)
ALTER TABLE sessions ADD COLUMN budget_usd REAL NOT NULL DEFAULT 0;
-- Nach Freigabe eines überschrittenen Monatsbudgets: bis zu diesen Monatskosten des Projekts
-- darf die Session weiterlaufen
ALTER TABLE sessions ADD COLUMN monthly_allow_usd REAL NOT NULL DEFAULT 0;

-- Ein Eintrag je gemeldeter Nutzung. Hängt am Projekt, nicht am Workspace: Monatsbudgets
-- zählen auch verworfene Workspaces mit.
CREATE TABLE usage (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    ts           TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    project_id   TEXT NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
    workspace_id TEXT NOT NULL,
    -- leer: keiner Session zuzuordnen (z. B. Terminal ohne Agent)
    session_id   TEXT NOT NULL DEFAULT '',
    -- proxy | cli
    source       TEXT NOT NULL,
    provider     TEXT NOT NULL DEFAULT '',
    model        TEXT NOT NULL DEFAULT '',
    input_tokens       INTEGER NOT NULL DEFAULT 0,
    output_tokens      INTEGER NOT NULL DEFAULT 0,
    cache_read_tokens  INTEGER NOT NULL DEFAULT 0,
    cache_write_tokens INTEGER NOT NULL DEFAULT 0,
    cost_usd     REAL NOT NULL DEFAULT 0,
    priced       INTEGER NOT NULL DEFAULT 1
);

CREATE INDEX usage_project ON usage (project_id, ts);
CREATE INDEX usage_workspace ON usage (workspace_id);
CREATE INDEX usage_session ON usage (session_id);
