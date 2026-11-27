import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useState, type FormEvent } from "react";
import { Link, useNavigate, useParams } from "react-router";
import { api, isBusy, type Workspace } from "../api";
import { SessionTable, StartAgent } from "../components/agents";
import { SandboxBadge, SandboxToggle } from "../components/sandbox";
import { sandboxDefault, useSandboxInfo } from "../sandbox";
import { useSessions } from "../sessions";
import { LineStats, Notice, WorkspaceStatusBadge } from "../components/ui";
import { errorMessage, formatTime, formatUSD, shortCommit } from "../format";
import { MonthlyBudget, UsageAmount } from "../components/usage";
import { useUsage } from "../hooks/useUsage";
import { RunTable, StartRun } from "../components/runs";
import { useRuns } from "../runs";
import { TriggerList } from "../components/triggers";

export function ProjectPage() {
  const { id = "" } = useParams();
  const project = useQuery({ queryKey: ["project", id], queryFn: () => api.project(id) });
  const workspaces = useQuery({
    queryKey: ["workspaces", id],
    queryFn: () => api.workspaces(id),
    enabled: project.isSuccess,
    refetchInterval: (q) => (q.state.data?.some((w) => isBusy(w.status)) ? 1500 : 10_000),
  });

  const sessions = useSessions({ project_id: id, limit: 50 });
  const runs = useRuns({ project_id: id, limit: 20 });
  const usage = useUsage(id);

  if (project.isPending) return <p className="muted">Lade Projekt …</p>;
  if (project.isError) return <Notice tone="bad">{errorMessage(project.error)}</Notice>;
  const p = project.data;

  return (
    <>
      <div className="crumbs">
        <Link to="/">Projekte</Link> / {p.name}
      </div>
      <div className="pagehead">
        <h1>{p.name}</h1>
        <RemoveProject projectId={p.id} name={p.name} workspaceCount={workspaces.data?.length ?? 0} />
      </div>

      <dl className="facts panel">
        <dt>Repository</dt>
        <dd className="mono">{p.repo_path}</dd>
        <dt>Basis-Branch</dt>
        <dd>
          <code>{p.base_branch}</code>
          {p.config.base_branch && <span className="muted"> (aus .ade/project.yaml)</span>}
        </dd>
        {p.config.adapter && (
          <>
            <dt>Adapter</dt>
            <dd>{p.config.adapter}</dd>
          </>
        )}
        <dt>Kosten</dt>
        <dd>
          <UsageAmount total={usage.data?.month} empty="0.00 $" /> <span className="muted">diesen Monat</span>
          {usage.data && usage.data.total.count > usage.data.month.count && (
            <span className="muted">
              {" "}
              · gesamt <UsageAmount total={usage.data.total} />
            </span>
          )}
          {p.config.budget?.session_usd ? <span className="muted"> · Budget je Session {formatUSD(p.config.budget.session_usd)}</span> : null}
          <MonthlyBudget spent={usage.data?.month.cost_usd ?? 0} budget={p.config.budget?.monthly_usd} />
        </dd>
        <dt>Setup</dt>
        <dd>
          {p.config.setup?.length ? (
            <ul className="plain mono">
              {p.config.setup.map((c, i) => (
                <li key={i}>{c}</li>
              ))}
            </ul>
          ) : (
            <span className="muted">keine Befehle</span>
          )}
        </dd>
      </dl>

      {p.warnings?.map((w) => (
        <Notice key={w} tone="warn">
          {w}
        </Notice>
      ))}
      {p.config_error && (
        <Notice tone="bad" title="Projekt-Konfiguration ungültig">
          <code>{p.config_error}</code>
          <p>Neue Workspaces lassen sich erst nach der Korrektur anlegen.</p>
        </Notice>
      )}

      <div className="sectionhead">
        <h2>Fabrik-Runs</h2>
        <StartRun projectId={p.id} disabled={!!p.config_error} />
      </div>
      {runs.data?.length ? (
        <RunTable runs={runs.data} />
      ) : (
        <div className="empty">
          <p>Noch keine Runs.</p>
          <p className="muted">
            Ein Run führt mehrere Rollen-Agenten nacheinander aus – mitgeliefert ist die Fabrik <code>feature</code>, eigene liegen in{" "}
            <code>.ade/factories/</code>.
          </p>
        </div>
      )}

      <div className="sectionhead">
        <h2>Auslöser</h2>
      </div>
      <TriggerList projectId={p.id} />

      <div className="sectionhead">
        <h2>Agenten</h2>
        <StartAgent projectId={p.id} defaultAdapter={p.config.adapter} disabled={!!p.config_error} />
      </div>
      {sessions.data?.length ? (
        <SessionTable sessions={sessions.data} />
      ) : (
        <div className="empty">
          <p>Noch keine Agenten.</p>
          <p className="muted">Ein Agent arbeitet in einem eigenen Workspace; du siehst ihm live zu und übernimmst das Ergebnis später.</p>
        </div>
      )}

      <div className="sectionhead">
        <h2>Workspaces</h2>
        <CreateWorkspace projectId={p.id} defaultBranch={p.base_branch} disabled={!!p.config_error} />
      </div>

      {workspaces.isPending ? (
        <p className="muted">Lade Workspaces …</p>
      ) : workspaces.isError ? (
        <Notice tone="bad">{errorMessage(workspaces.error)}</Notice>
      ) : workspaces.data.length === 0 ? (
        <div className="empty">
          <p>Noch keine Workspaces.</p>
          <p className="muted">Ein Workspace ist eine eigene Arbeitskopie auf einem eigenen Branch – das Repository bleibt unberührt.</p>
        </div>
      ) : (
        <WorkspaceTable workspaces={workspaces.data} projectId={p.id} />
      )}
    </>
  );
}

function WorkspaceTable({ workspaces, projectId }: { workspaces: Workspace[]; projectId: string }) {
  return (
    <div className="tablewrap">
      <table>
        <thead>
          <tr>
            <th>Workspace</th>
            <th>Status</th>
            <th>Basis</th>
            <th>Änderungen</th>
            <th>Angelegt</th>
            <th aria-label="Aktionen" />
          </tr>
        </thead>
        <tbody>
          {workspaces.map((w) => (
            <tr key={w.id}>
              <td>
                <Link to={`/workspaces/${w.id}`} className="mono">
                  {w.branch}
                </Link>
              </td>
              <td>
                <WorkspaceStatusBadge status={w.status} /> <SandboxBadge workspace={w} />
              </td>
              <td className="mono muted">
                {w.base_branch} @ {shortCommit(w.base_commit)}
              </td>
              <td>
                {w.changes ? (
                  w.changes.files === 0 ? (
                    <span className="muted">keine</span>
                  ) : (
                    <>
                      {w.changes.files} {w.changes.files === 1 ? "Datei" : "Dateien"}{" "}
                      <LineStats additions={w.changes.additions} deletions={w.changes.deletions} />
                    </>
                  )
                ) : (
                  <span className="muted">–</span>
                )}
              </td>
              <td className="muted">{formatTime(w.created_at)}</td>
              <td className="actions">
                <DeleteWorkspace workspace={w} projectId={projectId} />
              </td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}

function CreateWorkspace({ projectId, defaultBranch, disabled }: { projectId: string; defaultBranch: string; disabled: boolean }) {
  const qc = useQueryClient();
  const [branch, setBranch] = useState("");
  const sandboxInfo = useSandboxInfo();
  const [sandboxChoice, setSandbox] = useState<boolean | null>(null);
  const sandbox = sandboxChoice ?? sandboxDefault(sandboxInfo.data);
  const create = useMutation({
    mutationFn: () =>
      api.createWorkspace(projectId, { base_branch: branch.trim() || undefined, sandbox: sandbox && !!sandboxInfo.data?.available }),
    onSuccess: () => {
      setBranch("");
      void qc.invalidateQueries({ queryKey: ["workspaces", projectId] });
    },
  });
  const submit = (e: FormEvent) => {
    e.preventDefault();
    create.mutate();
  };
  return (
    <div>
      <form className="inline-form compact" onSubmit={submit}>
        <input
          aria-label="Basis-Branch"
          className="mono"
          placeholder={defaultBranch}
          value={branch}
          onChange={(e) => setBranch(e.target.value)}
          spellCheck={false}
        />
        <button type="submit" className="primary" disabled={disabled || create.isPending}>
          {create.isPending ? "Lege an …" : "Workspace anlegen"}
        </button>
        <SandboxToggle id="cw-sandbox" checked={sandbox} onChange={setSandbox} info={sandboxInfo.data} />
      </form>
      {create.isError && <Notice tone="bad">{errorMessage(create.error)}</Notice>}
    </div>
  );
}

function DeleteWorkspace({ workspace: w, projectId }: { workspace: Workspace; projectId: string }) {
  const qc = useQueryClient();
  const del = useMutation({
    mutationFn: () => api.deleteWorkspace(w.id),
    onSuccess: () => void qc.invalidateQueries({ queryKey: ["workspaces", projectId] }),
  });
  const onClick = () => {
    const changes = w.changes?.files ? `\n\nAchtung: ${w.changes.files} geänderte Datei(en) gehen verloren.` : "";
    if (window.confirm(`Workspace ${w.branch} löschen?${changes}`)) del.mutate();
  };
  return (
    <button type="button" className="danger small" onClick={onClick} disabled={del.isPending} title={del.isError ? errorMessage(del.error) : undefined}>
      {del.isPending ? "Lösche …" : del.isError ? "Fehler – erneut" : "Löschen"}
    </button>
  );
}

function RemoveProject({ projectId, name, workspaceCount }: { projectId: string; name: string; workspaceCount: number }) {
  const qc = useQueryClient();
  const navigate = useNavigate();
  const remove = useMutation({
    mutationFn: () => api.removeProject(projectId, workspaceCount > 0),
    onSuccess: () => {
      void qc.invalidateQueries({ queryKey: ["projects"] });
      void navigate("/");
    },
  });
  const onClick = () => {
    const extra = workspaceCount > 0 ? `\n\nDabei werden ${workspaceCount} Workspace(s) gelöscht.` : "";
    if (window.confirm(`Projekt „${name}“ abmelden? Das Repository bleibt unverändert.${extra}`)) remove.mutate();
  };
  return (
    <div>
      <button type="button" className="danger" onClick={onClick} disabled={remove.isPending}>
        Projekt abmelden
      </button>
      {remove.isError && <Notice tone="bad">{errorMessage(remove.error)}</Notice>}
    </div>
  );
}
