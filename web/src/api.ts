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

export type SessionStatus = "starting" | "running" | "idle" | "done" | "failed" | "stopped" | "interrupted";

export interface Capabilities {
  terminal: boolean;
  input: boolean;
  resume: boolean;
}

export interface Session {
  id: string;
  workspace_id: string;
  project_id: string;
  adapter: string;
  task?: string;
  command?: string[];
  model?: string;
  permission_mode?: string;
  status: SessionStatus;
  error?: string;
  exit_code?: number;
  resume_id?: string;
  cost_usd: number;
  created_at: string;
  updated_at: string;
  ended_at?: string;
  capabilities: Capabilities;
  live: boolean;
  resumable: boolean;
}

export interface AdapterInfo {
  name: string;
  capabilities: Capabilities;
}

export interface Adapters {
  adapters: AdapterInfo[];
  permission_modes: string[];
  default: string;
}

export interface StartAgentRequest {
  project_id: string;
  workspace_id?: string;
  adapter?: string;
  task?: string;
  command?: string[];
  model?: string;
  permission_mode?: string;
}

// Event-Payloads (internal/agent/adapter/events.go, Schema in event.schema.json).
export interface StatusPayload {
  status: SessionStatus;
  reason?: string;
  exit_code?: number;
  model?: string;
}
export interface MessagePayload {
  role: "user" | "assistant";
  text: string;
}
export interface ThinkingPayload {
  text: string;
}
export interface ToolCallPayload {
  id: string;
  tool: string;
  input?: unknown;
  parent_id?: string;
}
export interface ToolResultPayload {
  id: string;
  output: string;
  is_error?: boolean;
  truncated?: boolean;
  parent_id?: string;
}
export interface CostPayload {
  turn_usd: number;
  total_usd: number;
  input_tokens?: number;
  output_tokens?: number;
  cache_read_tokens?: number;
  cache_write_tokens?: number;
  duration_ms?: number;
  turns?: number;
}
export interface ErrorPayload {
  message: string;
}

export type SessionEvent =
  | EventOf<"status", StatusPayload>
  | EventOf<"message", MessagePayload>
  | EventOf<"thinking", ThinkingPayload>
  | EventOf<"tool_call", ToolCallPayload>
  | EventOf<"tool_result", ToolResultPayload>
  | EventOf<"cost", CostPayload>
  | EventOf<"error", ErrorPayload>;

interface EventOf<T extends string, P> {
  seq: number;
  session_id: string;
  ts: string;
  type: T;
  payload: P;
}

/** Ein Event, dessen Typ das Dashboard (noch) nicht kennt. */
export interface UnknownEvent {
  seq: number;
  session_id: string;
  ts: string;
  type: string;
  payload: unknown;
}

export type IdeState = "stopped" | "installing" | "starting" | "running" | "error";

export interface IdeStatus {
  state: IdeState;
  error?: string;
  started_at?: string;
  connections: number;
  url: string;
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
  adapters: () => request<Adapters>("GET", "/api/adapters"),
  startAgent: (req: StartAgentRequest) => request<Session>("POST", "/api/agents", req),
  sessions: (filter: { project_id?: string; workspace_id?: string; active?: boolean; limit?: number } = {}) => {
    const q = new URLSearchParams();
    if (filter.project_id) q.set("project_id", filter.project_id);
    if (filter.workspace_id) q.set("workspace_id", filter.workspace_id);
    if (filter.active) q.set("active", "true");
    if (filter.limit) q.set("limit", String(filter.limit));
    const qs = q.toString();
    return request<Session[]>("GET", `/api/sessions${qs ? `?${qs}` : ""}`);
  },
  session: (id: string) => request<Session>("GET", `/api/sessions/${encodeURIComponent(id)}`),
  sendInput: (id: string, text: string) => request<Session>("POST", `/api/sessions/${encodeURIComponent(id)}/input`, { text }),
  stopSession: (id: string) => request<Session>("POST", `/api/sessions/${encodeURIComponent(id)}/stop`),
  ideStatus: (workspaceId: string) => request<IdeStatus>("GET", `/api/workspaces/${encodeURIComponent(workspaceId)}/ide`),
  startIde: (workspaceId: string) => request<IdeStatus>("POST", `/api/workspaces/${encodeURIComponent(workspaceId)}/ide`),
  stopIde: (workspaceId: string) => request<void>("DELETE", `/api/workspaces/${encodeURIComponent(workspaceId)}/ide`),
  /** Liefert die Anmelde-URL der IDE-Subdomain (Einmal-Ticket, ADR-004). */
  ideTicket: (workspaceId: string) =>
    request<{ url: string }>("POST", `/api/workspaces/${encodeURIComponent(workspaceId)}/ide/ticket`),
};

/** IDEs in diesen Zuständen werden gleich bereit – öfter abfragen. */
export function ideBusy(state: IdeState): boolean {
  return state === "installing" || state === "starting";
}

/** Sessions in diesen Zuständen haben einen Prozess (oder bekommen gleich einen). */
export function isActive(status: SessionStatus): boolean {
  return status === "starting" || status === "running" || status === "idle";
}

/** URL eines WebSockets auf dem Daemon (gleicher Host, daher gleiche Cookies und Origin). */
export function wsURL(path: string): string {
  const proto = window.location.protocol === "https:" ? "wss:" : "ws:";
  return `${proto}//${window.location.host}${path}`;
}

/** Workspaces in diesen Zuständen ändern sich gleich – öfter abfragen. */
export function isBusy(status: WorkspaceStatus): boolean {
  return status === "creating" || status === "setup";
}
