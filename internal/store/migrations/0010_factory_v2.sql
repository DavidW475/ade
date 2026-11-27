-- Software-Fabrik v2 (M10)

-- fixes: Korrektur-Runden einer Rolle, deren strukturiertes Artefakt ungültig war
ALTER TABLE run_steps ADD COLUMN fixes INTEGER NOT NULL DEFAULT 0;

-- Schleifen: Jede Ausführung eines Schritts ist eine eigene Zeile. idx bleibt die Zeilennummer,
-- pos ist die Position im Ablauf, iteration der Durchlauf (1, 2, …).
ALTER TABLE run_steps ADD COLUMN pos INTEGER NOT NULL DEFAULT 0;
UPDATE run_steps SET pos = idx;
ALTER TABLE run_steps ADD COLUMN iteration INTEGER NOT NULL DEFAULT 1;
CREATE INDEX run_steps_pos ON run_steps (run_id, pos, iteration);

-- Parallele Gruppen: grp ist die Position des ersten Mitglieds (NULL: keine Gruppe)
ALTER TABLE run_steps ADD COLUMN grp INTEGER;
