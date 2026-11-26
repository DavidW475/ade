import { useQuery } from "@tanstack/react-query";
import { Link, useParams } from "react-router";
import { api, isBusy } from "../api";
import { SessionTable, StartAgent } from "../components/agents";
import { IdeButton } from "../components/ide";
import { DiffView, ReviewPanel } from "../components/review";
import { useSessions } from "../sessions";
import { LineStats, Notice, WorkspaceStatusBadge } from "../components/ui";
import { errorMessage, formatTime, shortCommit } from "../format";

export function WorkspacePage() {
  const { id = "" } = useParams();
  const ws = useQuery({
    queryKey: ["workspace", id],
    queryFn: () => api.workspace(id),
    refetchInterval: (q) => (q.state.data && isBusy(q.state.data.status) ? 1500 : false),
  });
  const project = useQuery({
    queryKey: ["project", ws.data?.project_id],
    queryFn: () => api.project(ws.data!.project_id),
    enabled: !!ws.data,
  });
  const hasTree = ws.data && ws.data.status !== "creating" && ws.data.status !== "error";
  const diff = useQuery({
    queryKey: ["diff", id],
    // Nur die Dateiliste; Inhalte lädt der Diff-Editor je Datei.
    queryFn: () => api.diff(id, false),
    enabled: !!hasTree,
    refetchInterval: 5000,
  });
  const sessions = useSessions({ workspace_id: id });

  if (ws.isPending) return <p className="muted">Lade Workspace …</p>;
  if (ws.isError) return <Notice tone="bad">{errorMessage(ws.error)}</Notice>;
  const w = ws.data;

  return (
    <>
      <div className="crumbs">
        <Link to="/">Projekte</Link> / <Link to={`/projects/${w.project_id}`}>{project.data?.name ?? w.project_id}</Link> /{" "}
        {w.id}
      </div>
      <div className="pagehead">
        <h1 className="mono">{w.branch}</h1>
        <div className="actions">
          <IdeButton workspaceId={w.id} disabled={!hasTree} />
        </div>
      </div>

      <dl className="facts panel">
        <dt>Status</dt>
        <dd>
          <WorkspaceStatusBadge status={w.status} />
        </dd>
        <dt>Pfad</dt>
        <dd className="mono">{w.path}</dd>
        <dt>Basis</dt>
        <dd className="mono">
          {w.base_branch} @ {shortCommit(w.base_commit)}
        </dd>
        <dt>Angelegt</dt>
        <dd>{formatTime(w.created_at)}</dd>
      </dl>

      {w.error && (
        <Notice tone={w.status === "setup_failed" ? "warn" : "bad"} title="Fehler">
          {w.error}
        </Notice>
      )}
      {w.setup_log && (
        <details className="panel" open={w.status === "setup_failed"}>
          <summary>Setup-Ausgabe</summary>
          <pre className="log">{w.setup_log}</pre>
        </details>
      )}

      <div className="sectionhead">
        <h2>Agenten in diesem Workspace</h2>
        <StartAgent
          projectId={w.project_id}
          workspaceId={w.id}
          defaultAdapter={project.data?.config.adapter}
          disabled={w.status === "creating" || w.status === "error"}
        />
      </div>
      {sessions.data?.length ? (
        <SessionTable sessions={sessions.data} showWorkspace={false} />
      ) : (
        <p className="muted">Noch keine Agenten in diesem Workspace.</p>
      )}

      <div className="sectionhead">
        <h2>Änderungen gegenüber {w.base_branch}</h2>
        {diff.data && diff.data.files?.length ? (
          <span>
            {diff.data.files.length} {diff.data.files.length === 1 ? "Datei" : "Dateien"}{" "}
            <LineStats additions={diff.data.additions} deletions={diff.data.deletions} />
          </span>
        ) : null}
      </div>
      <ReviewPanel
        workspace={w}
        projectName={project.data?.name ?? w.project_id}
        repoPath={project.data?.repo_path}
        sessions={sessions.data}
        hasChanges={!!diff.data?.files?.length || (!!w.head_commit && w.head_commit !== w.base_commit)}
      />
      {!hasTree ? (
        <p className="muted">Kein Arbeitsverzeichnis vorhanden.</p>
      ) : diff.isPending ? (
        <p className="muted">Berechne Diff …</p>
      ) : diff.isError ? (
        <Notice tone="bad">{errorMessage(diff.error)}</Notice>
      ) : !diff.data.files?.length ? (
        <div className="empty">
          <p>Keine Änderungen.</p>
          <p className="muted">
            Änderungen im Workspace (<code>{w.path}</code>) erscheinen hier automatisch.
          </p>
        </div>
      ) : (
        <DiffView workspaceId={w.id} diff={diff.data} />
      )}
    </>
  );
}
