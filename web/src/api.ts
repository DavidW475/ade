// Typen und Aufrufe der ADE-API. Die Typen spiegeln die Go-Structs in internal/.

export interface Health {
  service: string;
  status: "ok" | "degraded";
  version: string;
  schema: number;
}

export interface ProjectConfig {
  name?: string;
  base_branch?: string;
  adapter?: string;
  setup?: string[];
}

export interface Project {
  id: string;
  name: string;
  repo_path: string;
  default_branch: string;
  base_branch: string;
  created_at: string;
  config: ProjectConfig;
  config_error?: string;
  warnings?: string[];
}

export type WorkspaceStatus = "creating" | "setup" | "ready" | "setup_failed" | "error";

export interface Workspace {
  id: string;
  project_id: string;
  path: string;
  branch: string;
  base_branch: string;
  base_commit: string;
  status: WorkspaceStatus;
  error?: string;
  setup_log?: string;
  created_at: string;
  updated_at: string;
  changes?: { files: number; additions: number; deletions: number };
  diff_error?: string;
}

export type FileStatus = "added" | "modified" | "deleted" | "renamed" | "copied" | "type_changed" | "unmerged";

export interface FileChange {
  path: string;
  old_path?: string;
  status: FileStatus;
  additions: number;
  deletions: number;
  binary: boolean;
  patch?: string;
  truncated?: boolean;
}

export interface Diff {
  base_commit: string;
  head_commit: string;
  files: FileChange[] | null;
  additions: number;
  deletions: number;
}

/** Fehler einer API-Antwort mit HTTP-Status und Meldung des Daemons. */
export class ApiError extends Error {
  readonly status: number;
  constructor(status: number, message: string) {
    super(message);
    this.status = status;
  }
}

async function request<T>(method: string, path: string, body?: unknown): Promise<T> {
  const res = await fetch(path, {
    method,
    headers: body === undefined ? undefined : { "Content-Type": "application/json" },
    body: body === undefined ? undefined : JSON.stringify(body),
  });
  if (!res.ok) {
    let message = `HTTP ${res.status}`;
    try {
      const data = (await res.json()) as { error?: string };
      if (data.error) message = data.error;
    } catch {
      // Kein JSON – Statuszeile genügt.
    }
    throw new ApiError(res.status, message);
  }
  if (res.status === 204) return undefined as T;
  return (await res.json()) as T;
}

export async function fetchHealth(): Promise<Health> {
  const res = await fetch("/api/health");
  if (!res.ok && res.status !== 503) throw new ApiError(res.status, `HTTP ${res.status}`);
  return (await res.json()) as Health;
}

/** Meldet, ob der Browser eine gültige ADE-Session hat. */
export async function fetchAuthenticated(): Promise<boolean> {
  try {
    await request("GET", "/api/session");
    return true;
  } catch (err) {
    if (err instanceof ApiError && err.status === 401) return false;
    throw err;
  }
}

export const api = {
  projects: () => request<Project[]>("GET", "/api/projects"),
  project: (id: string) => request<Project>("GET", `/api/projects/${encodeURIComponent(id)}`),
  addProject: (path: string) => request<Project>("POST", "/api/projects", { path }),
  removeProject: (id: string, force: boolean) =>
    request<void>("DELETE", `/api/projects/${encodeURIComponent(id)}${force ? "?force=true" : ""}`),
  workspaces: (projectId: string) =>
    request<Workspace[]>("GET", `/api/projects/${encodeURIComponent(projectId)}/workspaces`),
  createWorkspace: (projectId: string, baseBranch?: string) =>
    request<Workspace>(
      "POST",
      `/api/projects/${encodeURIComponent(projectId)}/workspaces`,
      baseBranch ? { base_branch: baseBranch } : {},
    ),
  workspace: (id: string) => request<Workspace>("GET", `/api/workspaces/${encodeURIComponent(id)}`),
  deleteWorkspace: (id: string) => request<void>("DELETE", `/api/workspaces/${encodeURIComponent(id)}`),
  diff: (id: string) => request<Diff>("GET", `/api/workspaces/${encodeURIComponent(id)}/diff`),
};

/** Workspaces in diesen Zuständen ändern sich gleich – öfter abfragen. */
export function isBusy(status: WorkspaceStatus): boolean {
  return status === "creating" || status === "setup";
}
