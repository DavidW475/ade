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
            <th>Kosten</th>
            <th>Gestartet</th>
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
              <td className="muted">{s.capabilities.terminal ? "–" : formatUSD(s.cost_usd)}</td>
              <td className="muted">{formatTime(s.created_at)}</td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}

/**
 * Formular „Agent starten“. Mit workspaceId läuft der Agent in diesem Workspace, sonst wird
 * ein neuer angelegt (oder ein bestehender gewählt).
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
  const [permission, setPermission] = useState("acceptEdits");
  const [ws, setWs] = useState(workspaceId ?? "");
  const sandboxInfo = useSandboxInfo();
  const [sandboxChoice, setSandbox] = useState<boolean | null>(null);
  const sandbox = sandboxChoice ?? sandboxDefault(sandboxInfo.data);
  // Ein bestehender Workspace hat seinen Modus schon; nur neue Workspaces nehmen den Schalter.
  const existing = useQuery({
    queryKey: ["workspace", ws],
    queryFn: () => api.workspace(ws),
    enabled: !!ws,
  });

  const chosen = adapter || defaultAdapter || adapters.data?.default || "claude";
  const info = adapters.data?.adapters.find((a) => a.name === chosen);
  const terminal = info?.capabilities.terminal ?? chosen === "pty";

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
        if (chosen === "claude" && permission) req.permission_mode = permission;
      }
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
    if (!start.isPending && (terminal || task.trim())) start.mutate();
  };

  const readyWorkspaces = workspaces.data?.filter((w) => w.status === "ready" || w.status === "setup" || w.status === "setup_failed") ?? [];

  return (
    <form className="panel startform" onSubmit={submit}>
      <div className="field">
        <label htmlFor="sa-adapter">Adapter</label>
        <select id="sa-adapter" value={chosen} onChange={(e) => setAdapter(e.target.value)}>
          {(adapters.data?.adapters ?? [{ name: chosen }]).map((a) => (
            <option key={a.name} value={a.name}>
              {a.name === "claude" ? "claude – Claude Code" : a.name === "pty" ? "pty – Terminal (beliebiger Befehl)" : a.name}
            </option>
          ))}
        </select>
      </div>

      {!workspaceId && (
        <div className="field">
          <label htmlFor="sa-ws">Workspace</label>
          <select id="sa-ws" value={ws} onChange={(e) => setWs(e.target.value)}>
            <option value="">Neuer Workspace</option>
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
              {existing.data.sandbox ? "Läuft im Container dieses Workspaces." : "Dieser Workspace läuft ohne Sandbox."}
            </p>
          )
        ) : (
          <SandboxToggle id="sa-sandbox" checked={sandbox} onChange={setSandbox} info={sandboxInfo.data} />
        )}
      </div>

      {terminal ? (
        <div className="field wide">
          <label htmlFor="sa-cmd">Befehl</label>
          <input
            id="sa-cmd"
            className="mono"
            placeholder="leer: Login-Shell (z. B. bash)"
            value={command}
            onChange={(e) => setCommand(e.target.value)}
            spellCheck={false}
          />
          <p className="hint">Läuft interaktiv im Workspace-Verzeichnis (in der Sandbox: bash im Container), ohne Timeline.</p>
        </div>
      ) : (
        <>
          <div className="field wide">
            <label htmlFor="sa-task">Aufgabe</label>
            <textarea
              id="sa-task"
              rows={5}
              required
              placeholder="Was soll der Agent tun?"
              value={task}
              onChange={(e) => setTask(e.target.value)}
              onKeyDown={(e) => {
                if (e.key === "Enter" && (e.ctrlKey || e.metaKey)) submit(e);
              }}
            />
          </div>
          <div className="field">
            <label htmlFor="sa-model">Modell</label>
            <input id="sa-model" placeholder="Standard der CLI" value={model} onChange={(e) => setModel(e.target.value)} spellCheck={false} />
          </div>
          {chosen === "claude" && (
            <div className="field">
              <label htmlFor="sa-perm">Berechtigungen</label>
              <select id="sa-perm" value={permission} onChange={(e) => setPermission(e.target.value)}>
                {(adapters.data?.permission_modes ?? ["acceptEdits"]).map((m) => (
                  <option key={m} value={m}>
                    {m}
                  </option>
                ))}
              </select>
            </div>
          )}
          {chosen === "claude" && (
            <p className="hint wide">
              <code>acceptEdits</code> erlaubt Dateiänderungen; Befehle, die eine Bestätigung bräuchten, werden abgelehnt.{" "}
              <code>bypassPermissions</code> ohne Sandbox nur bei vertrauenswürdigen Repositories. In der Sandbox braucht Claude Code einen
              API-Key in der Konfiguration (<code>providers.anthropic</code>); er ist bis M6 im Container lesbar.
            </p>
          )}
        </>
      )}

      {start.isError && (
        <div className="wide">
          <Notice tone="bad">{errorMessage(start.error)}</Notice>
        </div>
      )}
      <div className="actions wide">
        {onCancel && (
          <button type="button" onClick={onCancel}>
            Abbrechen
          </button>
        )}
        <button type="submit" className="primary" disabled={start.isPending || (!terminal && !task.trim())}>
          {start.isPending ? "Starte …" : terminal ? "Terminal öffnen" : "Agent starten"}
        </button>
      </div>
    </form>
  );
}

/** Button, der das Startformular aufklappt. */
export function StartAgent(props: { projectId: string; workspaceId?: string; defaultAdapter?: string; disabled?: boolean }) {
  const [open, setOpen] = useState(false);
  if (!open)
    return (
      <button type="button" className="primary" disabled={props.disabled} onClick={() => setOpen(true)}>
        Agent starten
      </button>
    );
  return (
    <div className="startwrap">
      <StartAgentForm {...props} onCancel={() => setOpen(false)} />
    </div>
  );
}
