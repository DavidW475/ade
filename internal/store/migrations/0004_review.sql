-- Review und Übernahme (M4)
-- head_commit: zuletzt ins Haupt-Repo geholter Stand von ade/<id>
ALTER TABLE workspaces ADD COLUMN head_commit TEXT NOT NULL DEFAULT '';
ALTER TABLE workspaces ADD COLUMN collected_at TEXT;
-- merge_commit: Merge-Commit im Basis-Branch nach „Mergen“
ALTER TABLE workspaces ADD COLUMN merge_commit TEXT NOT NULL DEFAULT '';
ALTER TABLE workspaces ADD COLUMN merged_at TEXT;
-- pr_url: Pull Request nach „PR erstellen“
ALTER TABLE workspaces ADD COLUMN pr_url TEXT NOT NULL DEFAULT '';
