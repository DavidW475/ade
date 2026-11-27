-- Software-Fabrik v2 (M10)

-- fixes: Korrektur-Runden einer Rolle, deren strukturiertes Artefakt ungültig war
ALTER TABLE run_steps ADD COLUMN fixes INTEGER NOT NULL DEFAULT 0;
