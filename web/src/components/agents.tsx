import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useState } from "react";
import { Link, useNavigate } from "react-router";
import { api, type Session, type StartAgentRequest } from "../api";
import { errorMessage, formatTime, formatUSD } from "../format";
import { sessionTitle } from "../sessions";
import { sandboxDefault, useSandboxInfo } from "../sandbox";
import { SandboxBadge, SandboxToggle } from "./sandbox";
import { Notice, SessionStatusBadge } from "./ui";

export function SessionTable({ sessions, showWorkspace = true }: { sessions: Session[]; showWorkspace?: boolean }) {
  return (
    <div className="tablewrap">
      <table>
        <thead>
          <tr>
            <th>Agent</th>
            <th>Status</th>
            <th>Adapter</th>
            {showWorkspace && <th>Workspace</th>}
            <th>Costs</th>
            <th>Started</th>
          </tr>
        </thead>
        <tbody>
          {sessions.map((s) => (
            <tr key={s.id}>
              <td className="wrap">
                <Link to={`/sessions/${s.id}`}>{sessionTitle(s)}</Link>
              </td>
              <td>
                <SessionStatusBadge status={s.status} terminal={s.capabilities.terminal} />
              </td>
              <td className="muted">{s.adapter}</td>
              {showWorkspace && (
                <td>
                  <Link to={`/workspaces/${s.workspace_id}`} className="mono">
                    {s.workspace_id}
                  </Link>
                </td>
              )}
              <td className="muted">
                {s.capabilities.terminal && !s.cost_usd ? "–" : formatUSD(s.cost_usd)}
                {s.budget_usd ? ` / ${formatUSD(s.budget_usd)}` : ""}
                {s.unpriced ? " *" : ""}
              </td>
              <td className="muted">{formatTime(s.created_at)}</td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}

const adapterLabels: Record<string, string> = {
  claude: "claude – Claude Code",
  codex: "codex – Codex CLI (OpenAI)",
  pty: "pty – terminal (any command)",
};

/**
 * Form “Start agent”. With workspaceId the agent runs in this workspace, otherwise a new one
 * is created (or an existing one chosen).
 */
export function StartAgentForm({
  projectId,
  workspaceId,
  defaultAdapter,
  onCancel,
}: {
  projectId: string;
  workspaceId?: string;
  defaultAdapter?: string;
  onCancel?: () => void;
}) {
  const qc = useQueryClient();
  const navigate = useNavigate();
  const adapters = useQuery({ queryKey: ["adapters"], queryFn: api.adapters, staleTime: 60_000 });
  const workspaces = useQuery({
    queryKey: ["workspaces", projectId],
    queryFn: () => api.workspaces(projectId),
    enabled: !workspaceId,
  });
  const [adapter, setAdapter] = useState("");
  const [task, setTask] = useState("");
  const [command, setCommand] = useState("");
  const [model, setModel] = useState("");
  const [permission, setPermission] = useState("");
  const [budget, setBudget] = useState("");
  const [ws, setWs] = useState(workspaceId ?? "");
  const sandboxInfo = useSandboxInfo();
  const [sandboxChoice, setSandbox] = useState<boolean | null>(null);
  const sandbox = sandboxChoice ?? sandboxDefault(sandboxInfo.data);
  // An existing workspace already has its mode; only new workspaces take the switch.
  const existing = useQuery({
    queryKey: ["workspace", ws],
    queryFn: () => api.workspace(ws),
    enabled: !!ws,
  });

  const chosen = adapter || defaultAdapter || adapters.data?.default || "claude";
  const info = adapters.data?.adapters.find((a) => a.name === chosen);
  const terminal = info?.capabilities.terminal ?? chosen === "pty";
  const modes = adapters.data?.modes?.[chosen] ?? (chosen === "claude" ? adapters.data?.permission_modes : undefined) ?? [];
  const defaultMode = chosen === "claude" ? "acceptEdits" : chosen === "codex" ? "workspace-write" : "";
  const mode = permission && modes.includes(permission) ? permission : defaultMode;
  const provider = adapters.data?.providers?.[chosen];
  const aliases = (adapters.data?.models ?? []).filter((m) => !provider || m.provider === provider);
  const budgetValue = budget.trim() === "" ? undefined : Number(budget.replace(",", "."));
  const budgetInvalid = budgetValue !== undefined && (!Number.isFinite(budgetValue) || budgetValue < 0);

  const start = useMutation({
    mutationFn: () => {
      const req: StartAgentRequest = { project_id: projectId, adapter: chosen };
      if (ws) req.workspace_id = ws;
      else req.sandbox = sandbox && !!sandboxInfo.data?.available;
      if (terminal) {
        if (command.trim()) req.command = ["sh", "-c", command.trim()];
      } else {
        req.task = task;
        if (model.trim()) req.model = model.trim();
        if (modes.length && mode) req.permission_mode = mode;
      }
      if (budgetValue !== undefined) req.budget_usd = budgetValue;
      return api.startAgent(req);
    },
    onSuccess: (s) => {
      void qc.invalidateQueries({ queryKey: ["sessions"] });
      void qc.invalidateQueries({ queryKey: ["workspaces", projectId] });
      void navigate(`/sessions/${s.id}`);
    },
  });

  const submit = (e: { preventDefault(): void }) => {
    e.preventDefault();
    if (!start.isPending && !budgetInvalid && (terminal || task.trim())) start.mutate();
  };

  const readyWorkspaces = workspaces.data?.filter((w) => w.status === "ready" || w.status === "setup" || w.status === "setup_failed") ?? [];

  return (
    <form className="panel startform" onSubmit={submit}>
      <div className="field">
        <label htmlFor="sa-adapter">Adapter</label>
        <select id="sa-adapter" value={chosen} onChange={(e) => setAdapter(e.target.value)}>
          {(adapters.data?.adapters ?? [{ name: chosen }]).map((a) => (
            <option key={a.name} value={a.name}>
              {adapterLabels[a.name] ?? a.name}
            </option>
          ))}
        </select>
      </div>

      {!workspaceId && (
        <div className="field">
          <label htmlFor="sa-ws">Workspace</label>
          <select id="sa-ws" value={ws} onChange={(e) => setWs(e.target.value)}>
            <option value="">New workspace</option>
            {readyWorkspaces.map((w) => (
              <option key={w.id} value={w.id}>
                {w.branch}
              </option>
            ))}
          </select>
        </div>
      )}

      <div className="field wide">
        {ws ? (
          existing.data && (
            <p className="hint">
              <SandboxBadge workspace={existing.data} />{" "}
              {existing.data.sandbox ? "Runs in this workspace's container." : "This workspace runs without a sandbox."}
            </p>
          )
        ) : (
          <SandboxToggle id="sa-sandbox" checked={sandbox} onChange={setSandbox} info={sandboxInfo.data} />
        )}
      </div>

      {terminal ? (
        <div className="field wide">
          <label htmlFor="sa-cmd">Command</label>
          <input
            id="sa-cmd"
            className="mono"
            placeholder="empty: login shell (e.g. bash)"
            value={command}
            onChange={(e) => setCommand(e.target.value)}
            spellCheck={false}
          />
          <p className="hint">Runs interactively in the workspace directory (in the sandbox: bash in the container), without a timeline.</p>
        </div>
      ) : (
        <>
          <div className="field wide">
            <label htmlFor="sa-task">Task</label>
            <textarea
              id="sa-task"
              rows={5}
              required
              placeholder="What should the agent do?"
              value={task}
              onChange={(e) => setTask(e.target.value)}
              onKeyDown={(e) => {
                if (e.key === "Enter" && (e.ctrlKey || e.metaKey)) submit(e);
              }}
            />
          </div>
          <div className="field">
            <label htmlFor="sa-model">Model</label>
            <input
              id="sa-model"
              list="sa-models"
              placeholder={aliases.length ? "alias or model ID" : "CLI default"}
              value={model}
              onChange={(e) => setModel(e.target.value)}
              spellCheck={false}
            />
            <datalist id="sa-models">
              {aliases.map((m) => (
                <option key={m.name} value={m.name}>
                  {m.id}
                </option>
              ))}
            </datalist>
          </div>
          {modes.length > 0 && (
            <div className="field">
              <label htmlFor="sa-perm">{chosen === "codex" ? "Sandbox mode (local)" : "Permissions"}</label>
              <select id="sa-perm" value={mode} onChange={(e) => setPermission(e.target.value)}>
                {modes.map((m) => (
                  <option key={m} value={m}>
                    {m}
                  </option>
                ))}
              </select>
            </div>
          )}
          {chosen === "codex" && (
            <p className="hint wide">
              Codex handles one instruction per process; follow-up instructions continue the conversation. Codex does not ask before
              tool calls (no approvals). In the sandbox it runs without its own sandbox in the container; with an API key in{" "}
              <code>providers.openai</code> the egress proxy inserts it and counts the tokens.
            </p>
          )}
          {chosen === "claude" && (
            <p className="hint wide">
              Before every tool call Claude Code asks ADE: what the rules in <code>approvals</code>{" "}
              (<code>.ade/project.yaml</code>) allow runs right away, denied calls are rejected, everything else waits for an{" "}
              <Link to="/approvals">approval</Link> in the dashboard. Default: everything allowed in the sandbox, locally only reading and
              editing files in the workspace. The permission mode only applies if the hook is not available. In the sandbox Claude Code needs an
              API key or a subscription token in the configuration (<code>providers.anthropic</code>); the egress proxy inserts it, the
              container does not see it.
            </p>
          )}
        </>
      )}

      <div className="field">
        <label htmlFor="sa-budget">Budget (USD)</label>
        <input
          id="sa-budget"
          inputMode="decimal"
          placeholder="project default"
          value={budget}
          onChange={(e) => setBudget(e.target.value)}
          aria-invalid={budgetInvalid}
        />
        <p className="hint">Empty: budget.session_usd from .ade/project.yaml; 0: no budget. Beyond it the agent pauses and waits for an approval.</p>
      </div>

      {start.isError && (
        <div className="wide">
          <Notice tone="bad">{errorMessage(start.error)}</Notice>
        </div>
      )}
      <div className="actions wide">
        {onCancel && (
          <button type="button" onClick={onCancel}>
            Cancel
          </button>
        )}
        <button type="submit" className="primary" disabled={start.isPending || budgetInvalid || (!terminal && !task.trim())}>
          {start.isPending ? "Starting …" : terminal ? "Open terminal" : "Start agent"}
        </button>
      </div>
    </form>
  );
}

/** Button that expands the start form. */
export function StartAgent(props: { projectId: string; workspaceId?: string; defaultAdapter?: string; disabled?: boolean }) {
  const [open, setOpen] = useState(false);
  if (!open)
    return (
      <button type="button" className="primary" disabled={props.disabled} onClick={() => setOpen(true)}>
        Start agent
      </button>
    );
  return (
    <div className="startwrap">
      <StartAgentForm {...props} onCancel={() => setOpen(false)} />
    </div>
  );
}
