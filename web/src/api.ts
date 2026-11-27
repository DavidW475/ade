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
  /** Wirksame .ade/sandbox.yaml (mit Voreinstellungen). */
  sandbox?: { image?: string; resources: { memory: string; cpus: number; pids: number }; egress?: Egress };
  sandbox_error?: string;
  /** Wirksame Freigabe-Regeln (bei ungültiger project.yaml leer: alles braucht eine Freigabe). */
  approvals: { sandbox: RuleSet; local: RuleSet };
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
  /** Zuletzt ins Projekt-Repo geholter Stand von ade/<id>. */
  head_commit?: string;
  collected_at?: string;
  merge_commit?: string;
  merged_at?: string;
  pr_url?: string;
  /** Agent, Setup und IDE laufen im Docker-Container des Workspaces. */
  sandbox: boolean;
  sandbox_image?: string;
}

/** GET /api/sandbox */
export interface SandboxInfo {
  available: boolean;
  error?: string;
  docker_version?: string;
  image: string;
  /** Voreinstellung für neue Workspaces (defaults.sandbox). */
  default: boolean;
  /** Netzwerk-Isolation neuer Container: internes Netz, Internet nur über den Egress-Proxy. */
  network: NetworkMode;
  /** Verfügbare egress.presets mit ihren Hosts. */
  presets: Record<string, string[]>;
  /** Presets ohne egress-Abschnitt in .ade/sandbox.yaml. */
  default_presets: string[];
}

/** proxy: nur über den Egress-Proxy; open: Container aus M5 mit normalem Netz. */
export type NetworkMode = "proxy" | "open";

/** Abschnitt egress in .ade/sandbox.yaml. */
export interface Egress {
  presets: string[] | null;
  allow: string[] | null;
  deny: string[] | null;
  /** deny: sofort blocken; ask: Anfrage wartet auf eine Freigabe im Dashboard. */
  on_blocked: "deny" | "ask";
  /** Kein egress-Abschnitt: es gelten die Voreinstellungen. */
  default: boolean;
}

/** GET /api/workspaces/{id}/egress */
export interface WorkspaceEgress {
  events: EgressEvent[];
  hosts: EgressHost[];
  policy?: Egress & { effective_allow: string[] | null; error?: string; providers: string[] };
  /** Ausnahmen aus Freigaben (projektweit und für diesen Workspace). */
  grants: EgressGrant[];
}

/** Egress-Ausnahme aus einer Freigabe; ergänzt .ade/sandbox.yaml, ohne das Repo zu ändern. */
export interface EgressGrant {
  id: number;
  project_id: string;
  /** Leer: gilt für alle Workspaces des Projekts. */
  workspace_id?: string;
  rule: string;
  approval_id?: string;
  created_at: string;
}

export type ApprovalStatus = "pending" | "approved" | "denied" | "expired" | "canceled";
export type ApprovalDecision = "allow" | "allow_workspace" | "allow_project" | "deny";

/** Freigabe-Anfrage (Werkzeug-Aufruf eines Agenten oder geblocktes Netzwerk-Ziel). */
export interface Approval {
  id: string;
  kind: "tool" | "egress";
  project_id: string;
  workspace_id: string;
  session_id?: string;
  title: string;
  detail: ToolApprovalDetail | EgressApprovalDetail;
  sandbox: boolean;
  status: ApprovalStatus;
  decision?: ApprovalDecision;
  reason?: string;
  created_at: string;
  expires_at: string;
  decided_at?: string;
}

export interface ToolApprovalDetail {
  tool: string;
  input: Record<string, unknown>;
  tool_use_id?: string;
  workdir: string;
}

export interface EgressApprovalDetail {
  host: string;
  port: number;
}

/** Regeln je Umgebung (approvals in .ade/project.yaml). */
export interface RuleSet {
  allow: string[] | null;
  ask: string[] | null;
  deny: string[] | null;
}

export interface EgressEvent {
  id: number;
  ts: string;
  kind: "connect" | "http" | "api";
  method?: string;
  host: string;
  port: number;
  path?: string;
  provider?: string;
  allowed: boolean;
  reason: string;
  status?: number;
  bytes_out: number;
  bytes_in: number;
  duration_ms: number;
  error?: string;
}

export interface EgressHost {
  host: string;
  allowed: number;
  blocked: number;
  bytes_out: number;
  bytes_in: number;
  last: string;
  reason?: string;
}

export interface SandboxContainer {
  id: string;
  name: string;
  image: string;
  state: string;
  running: boolean;
  created: string;
}

/** GET /api/workspaces/{id}/sandbox */
export interface WorkspaceSandbox {
  sandbox: boolean;
  image?: string;
  container?: SandboxContainer;
  error?: string;
  network?: NetworkMode;
}

/** Eine Seite einer Datei im Diff-Editor (Basis-Commit bzw. Workspace). */
export interface FileSide {
  exists: boolean;
  binary?: boolean;
  too_large?: boolean;
  text?: string;
}

export interface FileContents {
  path: string;
  old_path?: string;
  original: FileSide;
  modified: FileSide;
}

export interface PRDraft {
  title: string;
  body: string;
  base: string;
  head: string;
  remote: string;
  remote_url?: string;
  url?: string;
}

export interface PRRequest {
  title?: string;
  body?: string;
  draft?: boolean;
}

export interface FeedbackRequest {
  comment: string;
  adapter?: string;
  model?: string;
  permission_mode?: string;
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
  /** Die CLI fragt vor Werkzeug-Aufrufen bei ADE nach (Freigaben). */
  approvals: boolean;
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
  /** Nur für einen neuen Workspace; ohne Angabe gilt defaults.sandbox. */
  sandbox?: boolean;
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
  /** Weitere Felder der Fehlerantwort (z. B. conflicts beim Mergen). */
  readonly details: Record<string, unknown>;
  constructor(status: number, message: string, details: Record<string, unknown> = {}) {
    super(message);
    this.status = status;
    this.details = details;
  }
}

/** Dateien mit Konflikten aus einer fehlgeschlagenen Merge-Antwort. */
export function mergeConflicts(err: unknown): string[] {
  if (err instanceof ApiError && Array.isArray(err.details.conflicts)) {
    return err.details.conflicts.filter((c): c is string => typeof c === "string");
  }
  return [];
}

async function request<T>(method: string, path: string, body?: unknown): Promise<T> {
  const res = await fetch(path, {
    method,
    headers: body === undefined ? undefined : { "Content-Type": "application/json" },
    body: body === undefined ? undefined : JSON.stringify(body),
  });
  if (!res.ok) {
    let message = `HTTP ${res.status}`;
    let details: Record<string, unknown> = {};
    try {
      const data = (await res.json()) as { error?: string } & Record<string, unknown>;
      if (data.error) message = data.error;
      details = data;
    } catch {
      // Kein JSON – Statuszeile genügt.
    }
    throw new ApiError(res.status, message, details);
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
  createWorkspace: (projectId: string, opts: { base_branch?: string; sandbox?: boolean } = {}) =>
    request<Workspace>("POST", `/api/projects/${encodeURIComponent(projectId)}/workspaces`, opts),
  sandbox: () => request<SandboxInfo>("GET", "/api/sandbox"),
  workspaceSandbox: (id: string) => request<WorkspaceSandbox>("GET", `/api/workspaces/${encodeURIComponent(id)}/sandbox`),
  workspaceEgress: (id: string, blockedOnly = false) =>
    request<WorkspaceEgress>("GET", `/api/workspaces/${encodeURIComponent(id)}/egress${blockedOnly ? "?blocked=1" : ""}`),
  workspace: (id: string) => request<Workspace>("GET", `/api/workspaces/${encodeURIComponent(id)}`),
  deleteWorkspace: (id: string) => request<void>("DELETE", `/api/workspaces/${encodeURIComponent(id)}`),
  diff: (id: string, patch = true) =>
    request<Diff>("GET", `/api/workspaces/${encodeURIComponent(id)}/diff${patch ? "" : "?patch=false"}`),
  file: (id: string, path: string, oldPath?: string) => {
    const q = new URLSearchParams({ path });
    if (oldPath) q.set("old_path", oldPath);
    return request<FileContents>("GET", `/api/workspaces/${encodeURIComponent(id)}/file?${q.toString()}`);
  },
  collect: (id: string) => request<Workspace>("POST", `/api/workspaces/${encodeURIComponent(id)}/collect`),
  merge: (id: string) => request<Workspace>("POST", `/api/workspaces/${encodeURIComponent(id)}/merge`),
  prDraft: (id: string) => request<PRDraft>("GET", `/api/workspaces/${encodeURIComponent(id)}/pull-request`),
  createPR: (id: string, req: PRRequest) =>
    request<Workspace>("POST", `/api/workspaces/${encodeURIComponent(id)}/pull-request`, req),
  feedback: (id: string, req: FeedbackRequest) =>
    request<Session>("POST", `/api/workspaces/${encodeURIComponent(id)}/feedback`, req),
  adapters: () => request<Adapters>("GET", "/api/adapters"),
  approvals: (filter: { status?: ApprovalStatus; session_id?: string; workspace_id?: string; limit?: number } = {}) => {
    const q = new URLSearchParams();
    for (const [k, v] of Object.entries(filter)) if (v !== undefined && v !== "") q.set(k, String(v));
    const qs = q.toString();
    return request<Approval[]>("GET", `/api/approvals${qs ? `?${qs}` : ""}`);
  },
  decideApproval: (id: string, decision: ApprovalDecision, reason?: string) =>
    request<Approval>("POST", `/api/approvals/${encodeURIComponent(id)}`, { decision, reason }),
  addEgressGrant: (workspaceId: string, rule: string, scope: "workspace" | "project") =>
    request<EgressGrant>("POST", `/api/workspaces/${encodeURIComponent(workspaceId)}/egress/grants`, { rule, scope }),
  deleteEgressGrant: (id: number) => request<void>("DELETE", `/api/egress-grants/${id}`),
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
