-- Docker-Sandbox (M5)
-- sandbox: Agent, Setup und IDE laufen im Container des Workspaces
ALTER TABLE workspaces ADD COLUMN sandbox INTEGER NOT NULL DEFAULT 0;
-- sandbox_image: Image, mit dem der Container angelegt wurde
ALTER TABLE workspaces ADD COLUMN sandbox_image TEXT NOT NULL DEFAULT '';
