-- Auslöser für Fabrik-Runs (M10): GitHub-Issues mit Label, Zeitplan

-- Zustand je Auslöser (die Definition steht in .ade/project.yaml)
CREATE TABLE trigger_state (
    project_id TEXT NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
    name       TEXT NOT NULL,
    paused     INTEGER NOT NULL DEFAULT 0,
    -- letzte Abfrage (Issues) bzw. zuletzt behandelter Termin (Zeitplan)
    last_check TEXT,
    last_fire  TEXT,
    last_error TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (project_id, name)
);

-- Ausgelöste Runs; key ist die Issue-Nummer bzw. der Termin. Ein Issue löst höchstens
-- einen Run aus, auch über Neustarts hinweg.
CREATE TABLE trigger_runs (
    project_id TEXT NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
    trigger    TEXT NOT NULL,
    key        TEXT NOT NULL,
    run_id     TEXT NOT NULL,
    -- Ende am Issue gemeldet
    reported   INTEGER NOT NULL DEFAULT 0,
    created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    PRIMARY KEY (project_id, trigger, key)
);

CREATE INDEX trigger_runs_open ON trigger_runs (reported);

-- Auslöser eines Runs (leer: von Hand), Verweis (z. B. Issue-URL), zu schließendes Issue
ALTER TABLE runs ADD COLUMN trigger TEXT NOT NULL DEFAULT '';
ALTER TABLE runs ADD COLUMN trigger_ref TEXT NOT NULL DEFAULT '';
ALTER TABLE runs ADD COLUMN closes INTEGER NOT NULL DEFAULT 0;
