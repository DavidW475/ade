// Types and calls of the ADE API. The types mirror the Go structs in internal/.

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
  budget?: { session_usd?: number; monthly_usd?: number };
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
  /** Effective .ade/sandbox.yaml (with defaults). */
  sandbox?: { image?: string; resources: { memory: string; cpus: number; pids: number }; egress?: Egress };
  sandbox_error?: string;
  /** Effective approval rules (empty for an invalid project.yaml: everything needs an approval). */
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
  /** State of ade/<id> most recently fetched into the project repository. */
  head_commit?: string;
  collected_at?: string;
  merge_commit?: string;
  merged_at?: string;
  pr_url?: string;
  /** Agent, setup and IDE run in the workspace's Docker container. */
  sandbox: boolean;
  sandbox_image?: string;
}

/** GET /api/sandbox */
export interface SandboxInfo {
  available: boolean;
  error?: string;
  docker_version?: string;
  /** Detected container runtime, e.g. “Docker (rootless)” or “Podman (rootless)”. */
  runtime?: string;
  image: string;
  /** Default for new workspaces (defaults.sandbox). */
  default: boolean;
  /** Network isolation of new containers: internal network, internet only through the egress proxy. */
  network: NetworkMode;
  /** Available egress.presets with their hosts. */
  presets: Record<string, string[]>;
  /** Presets without an egress section in .ade/sandbox.yaml. */
  default_presets: string[];
}

/** proxy: only through the egress proxy; open: container from M5 with a normal network. */
export type NetworkMode = "proxy" | "open";

/** The egress section in .ade/sandbox.yaml. */
export interface Egress {
  presets: string[] | null;
  allow: string[] | null;
  deny: string[] | null;
  /** deny: block right away; ask: the request waits for an approval in the dashboard. */
  on_blocked: "deny" | "ask";
  /** No egress section: the defaults apply. */
  default: boolean;
}

/** GET /api/workspaces/{id}/egress */
export interface WorkspaceEgress {
  events: EgressEvent[];
  hosts: EgressHost[];
  policy?: Egress & { effective_allow: string[] | null; error?: string; providers: string[] };
  /** Exceptions from approvals (project-wide and for this workspace). */
  grants: EgressGrant[];
}

/** Egress exception from an approval; extends .ade/sandbox.yaml without changing the repository. */
export interface EgressGrant {
  id: number;
  project_id: string;
  /** Empty: applies to all workspaces of the project. */
  workspace_id?: string;
  rule: string;
  approval_id?: string;
  created_at: string;
}

export type ApprovalStatus = "pending" | "approved" | "denied" | "expired" | "canceled";
export type ApprovalDecision = "allow" | "allow_workspace" | "allow_project" | "deny";

/** Approval request (an agent's tool call, a blocked network target or an exceeded budget). */
export interface Approval {
  id: string;
  kind: "tool" | "egress" | "budget" | "gate";
  project_id: string;
  workspace_id: string;
  session_id?: string;
  title: string;
  detail: ToolApprovalDetail | EgressApprovalDetail | BudgetApprovalDetail | GateApprovalDetail;
  sandbox: boolean;
  status: ApprovalStatus;
  decision?: ApprovalDecision;
  reason?: string;
  created_at: string;
  expires_at: string;
  decided_at?: string;
}

export interface BudgetApprovalDetail {
  scope: "session" | "monthly";
  limit_usd: number;
  spent_usd: number;
  /** Amount by which “Allow” raises the budget. */
  step_usd: number;
}

/** Gate of a factory run (gate: human). */
export interface GateApprovalDetail {
  run_id: string;
  factory: string;
  step: string;
  task?: string;
  artifacts?: string[];
  /** Denying jumps back to this step (empty: denying cancels the run). */
  on_deny?: string;
  iteration?: number;
  max_iterations?: number;
  /** Changed files under .github/workflows/, .github/actions/ or .ade/. */
  sensitive?: string[];
  /** Approval before pull_request without a gate, only because of sensitive changes. */
  unattended?: boolean;
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

/** Rules per environment (approvals in .ade/project.yaml). */
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

/** One side of a file in the diff editor (base commit or workspace). */
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

export type SessionStatus = "starting" | "running" | "idle" | "done" | "failed" | "stopped" | "interrupted" | "paused";

export interface Capabilities {
  terminal: boolean;
  input: boolean;
  resume: boolean;
  /** The CLI asks ADE before tool calls (approvals). */
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
  /** Resolved model ID if model is an alias. */
  model_id?: string;
  input_tokens: number;
  output_tokens: number;
  cache_read_tokens: number;
  cache_write_tokens: number;
  /** No price was known for some of the tokens. */
  unpriced?: boolean;
  /** The egress proxy measures the usage. */
  metered?: boolean;
  /** Budget of the session in USD (missing: none). */
  budget_usd?: number;
  /** Role of a factory (e.g. planner) or feedback. */
  role?: string;
  /** Read-only role: workspace read-only (reader container or snapshot). */
  read_only?: boolean;
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

export interface ModelAlias {
  name: string;
  provider: string;
  id: string;
}

export interface Adapters {
  adapters: AdapterInfo[];
  permission_modes: string[];
  /** Values for permission_mode per adapter. */
  modes?: Record<string, string[]>;
  /** Provider per adapter. */
  providers?: Record<string, string>;
  models?: ModelAlias[];
  default: string;
}

export interface UsageTotal {
  cost_usd: number;
  input_tokens: number;
  output_tokens: number;
  cache_read_tokens: number;
  cache_write_tokens: number;
  unpriced?: boolean;
  count: number;
}

export interface Usage {
  total: UsageTotal;
  month: UsageTotal;
  month_start: string;
  monthly_budget_usd?: number;
  projects?: Record<string, UsageTotal>;
  workspaces?: Record<string, UsageTotal>;
  prices_as_of: string;
}

export interface StartAgentRequest {
  project_id: string;
  workspace_id?: string;
  adapter?: string;
  task?: string;
  command?: string[];
  model?: string;
  permission_mode?: string;
  /** Only for a new workspace; without it defaults.sandbox applies. */
  sandbox?: boolean;
  /** Budget of the session in USD (without it: budget.session_usd, 0: none). */
  budget_usd?: number;
}

// Event payloads (internal/agent/adapter/events.go, schema in event.schema.json).
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
  model?: string;
  /** ADE computed the costs from tokens and the price table. */
  estimated?: boolean;
  /** Measured by the egress proxy. */
  metered?: boolean;
  budget_usd?: number;
}
/** Usage of a request measured by the egress proxy (not in the timeline). */
export interface UsagePayload {
  provider?: string;
  model?: string;
  input_tokens: number;
  output_tokens: number;
  cost_usd: number;
  priced: boolean;
  total_usd: number;
  budget_usd?: number;
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
  | EventOf<"usage", UsagePayload>
  | EventOf<"error", ErrorPayload>;

interface EventOf<T extends string, P> {
  seq: number;
  session_id: string;
  ts: string;
  type: T;
  payload: P;
}

/** An event whose type the dashboard does not know (yet). */
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

// --- Software factory (M9) ---

export type RunStatus = "running" | "waiting" | "paused" | "done" | "failed" | "canceled";
export type StepStatus = "pending" | "running" | "waiting" | "done" | "failed" | "canceled" | "skipped" | "looped";
export type StepKind = "role" | "gate" | "action" | "decision";

export interface RunStep {
  /** Position in the flow. */
  index: number;
  /** Row of the execution (unique within the run). */
  row: number;
  /** Pass (1, 2, … after jumps back). */
  iteration: number;
  /** For members of a parallel group, the position of the first member. */
  group?: number;
  name: string;
  kind: StepKind;
  role?: string;
  read_only?: boolean;
  status: StepStatus;
  session_id?: string;
  approval_id?: string;
  /** Artifact of the role (its last answer). */
  output?: string;
  error?: string;
  /** Correction rounds for an invalid structured artifact. */
  fixes?: number;
  started_at?: string;
  ended_at?: string;
}

export interface Run {
  id: string;
  project_id: string;
  workspace_id?: string;
  factory: string;
  source?: "project" | "builtin";
  inputs: Record<string, string>;
  task?: string;
  sandbox: boolean;
  status: RunStatus;
  /** Index of the current step (= number of steps when done). */
  current: number;
  error?: string;
  budget_usd?: number;
  max_duration_ns?: number;
  created_at: string;
  updated_at: string;
  ended_at?: string;
  /** The latest execution per position. */
  steps: RunStep[];
  /** Earlier passes (loops). */
  history?: RunStep[];
  cost_usd: number;
  /** Trigger (empty: by hand), reference (e.g. issue URL), closed issue. */
  trigger?: string;
  trigger_ref?: string;
  closes?: number;
  artifacts: { name: string; step: string; role: string; size: number; iterations?: number[] }[];
  pr_url?: string;
  workspace_gone?: boolean;
  /** Condition and jump back from the definition per position (like steps). */
  plan?: StepPlan[];
}

export interface StepPlan {
  /** Condition: otherwise skipped (for a decision: jump back if true). */
  when?: string;
  /** Target of a jump back (decision: goto, gate: on_deny.goto). */
  goto?: string;
  max_iterations?: number;
  on_exhausted?: string;
}

// --- Triggers (M10) ---

export interface TriggerView {
  name: string;
  factory: string;
  inputs?: Record<string, string>;
  sandbox?: boolean;
  allow_unbudgeted?: boolean;
  github_issues?: { label: string; authors?: string[]; interval_ns?: number; comment?: boolean };
  schedule?: { cron: string; catch_up?: boolean };
  kind: "github_issues" | "schedule";
  state: { paused: boolean; last_check?: string; last_fire?: string; last_error?: string };
  /** Next poll or next date (missing for paused triggers). */
  next?: string;
  runs: { trigger: string; key: string; run_id: string; reported: boolean; created_at: string }[];
}

export interface FactoryInput {
  type: string;
  required?: boolean;
  description?: string;
}

/** GET /api/factories: own (.ade/factories) and built-in factories. */
export interface FactorySummary {
  name: string;
  description?: string;
  source: "project" | "builtin";
  path: string;
  inputs?: Record<string, FactoryInput>;
  steps?: { name: string; kind: StepKind; role?: string; read_only?: boolean }[];
  error?: string;
}

export interface StartRunRequest {
  project_id: string;
  factory: string;
  task?: string;
  inputs?: Record<string, string>;
  sandbox?: boolean;
}

export function runActive(status: RunStatus): boolean {
  return status === "running" || status === "waiting" || status === "paused";
}

async function requestText(path: string): Promise<string> {
  const res = await fetch(path);
  if (!res.ok) {
    let message = `HTTP ${res.status}`;
    try {
      const data = (await res.json()) as { error?: string };
      if (data.error) message = data.error;
    } catch {
      // Not JSON.
    }
    throw new ApiError(res.status, message);
  }
  return res.text();
}

/** Error of an API response with HTTP status and the daemon's message. */
export class ApiError extends Error {
  readonly status: number;
  /** Further fields of the error response (e.g. conflicts when merging). */
  readonly details: Record<string, unknown>;
  constructor(status: number, message: string, details: Record<string, unknown> = {}) {
    super(message);
    this.status = status;
    this.details = details;
  }
}

/** Files with conflicts from a failed merge response. */
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
      // Not JSON – the status line is enough.
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

/** Reports whether the browser has a valid ADE session. */
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
  usage: (projectId?: string) =>
    request<Usage>("GET", `/api/usage${projectId ? `?project_id=${encodeURIComponent(projectId)}` : ""}`),
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
  factories: (projectId: string) => request<FactorySummary[]>("GET", `/api/factories?project_id=${encodeURIComponent(projectId)}`),
  runs: (filter: { project_id?: string; workspace_id?: string; active?: boolean; limit?: number } = {}) => {
    const q = new URLSearchParams();
    if (filter.project_id) q.set("project_id", filter.project_id);
    if (filter.workspace_id) q.set("workspace_id", filter.workspace_id);
    if (filter.active) q.set("active", "1");
    if (filter.limit) q.set("limit", String(filter.limit));
    const qs = q.toString();
    return request<Run[]>("GET", `/api/runs${qs ? `?${qs}` : ""}`);
  },
  run: (id: string) => request<Run>("GET", `/api/runs/${encodeURIComponent(id)}`),
  startRun: (req: StartRunRequest) => request<Run>("POST", "/api/runs", req),
  cancelRun: (id: string) => request<Run>("POST", `/api/runs/${encodeURIComponent(id)}/cancel`),
  artifact: (id: string, name: string, iteration?: number) =>
    requestText(`/api/runs/${encodeURIComponent(id)}/artifacts/${encodeURIComponent(name)}${iteration ? `?iteration=${iteration}` : ""}`),
  triggers: (projectId: string) => request<TriggerView[]>("GET", `/api/projects/${encodeURIComponent(projectId)}/triggers`),
  pauseTrigger: (projectId: string, name: string, paused: boolean) =>
    request<TriggerView>("POST", `/api/projects/${encodeURIComponent(projectId)}/triggers/${encodeURIComponent(name)}/${paused ? "pause" : "resume"}`),
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
  /** Returns the login URL of the IDE subdomain (one-time ticket, ADR-004). */
  ideTicket: (workspaceId: string) =>
    request<{ url: string }>("POST", `/api/workspaces/${encodeURIComponent(workspaceId)}/ide/ticket`),
};

/** IDEs in these states are about to be ready – poll more often. */
export function ideBusy(state: IdeState): boolean {
  return state === "installing" || state === "starting";
}

/** Sessions in these states have a process (or are about to get one). */
export function isActive(status: SessionStatus): boolean {
  return status === "starting" || status === "running" || status === "idle";
}

/** URL of a WebSocket on the daemon (same host, therefore same cookies and origin). */
export function wsURL(path: string): string {
  const proto = window.location.protocol === "https:" ? "wss:" : "ws:";
  return `${proto}//${window.location.host}${path}`;
}

/** Workspaces in these states are about to change – poll more often. */
export function isBusy(status: WorkspaceStatus): boolean {
  return status === "creating" || status === "setup";
}
