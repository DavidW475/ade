import { useMutation, useQuery } from "@tanstack/react-query";
import { Link, useNavigate, useParams } from "react-router";
import { api, isBusy, type FileChange } from "../api";
import { SessionTable, StartAgent } from "../components/agents";
import { useSessions } from "../sessions";
import { FileStatusBadge, LineStats, Notice, WorkspaceStatusBadge } from "../components/ui";
import { errorMessage, formatTime, shortCommit } from "../format";

export function WorkspacePage() {
  const { id = "" } = useParams();
  const navigate = useNavigate();
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
    queryFn: () => api.diff(id),
    enabled: !!hasTree,
    refetchInterval: 5000,
  });
  const sessions = useSessions({ workspace_id: id });
  const del = useMutation({
    mutationFn: () => api.deleteWorkspace(id),
    onSuccess: () => void navigate(ws.data ? `/projects/${ws.data.project_id}` : "/"),
  });

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
        <button
          type="button"
          className="danger"
          disabled={del.isPending}
          onClick={() => window.confirm(
              `Workspace ${w.branch} mit allen Änderungen löschen?${sessions.data?.some((x) => x.live) ? "\n\nLaufende Agenten werden dabei gestoppt." : ""}`,
            ) && del.mutate()}
        >
          Workspace löschen
        </button>
      </div>
      {del.isError && <Notice tone="bad">{errorMessage(del.error)}</Notice>}

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
        <ul className="files">
          {diff.data.files.map((f) => (
            <FileDiff key={f.path} file={f} />
          ))}
        </ul>
      )}
    </>
  );
}

function FileDiff({ file: f }: { file: FileChange }) {
  return (
    <li>
      <details>
        <summary>
          <FileStatusBadge status={f.status} />
          <span className="mono path">
            {f.old_path ? (
              <>
                {f.old_path} → {f.path}
              </>
            ) : (
              f.path
            )}
          </span>
          {f.binary ? <span className="muted">binär</span> : <LineStats additions={f.additions} deletions={f.deletions} />}
        </summary>
        {f.truncated ? (
          <p className="muted pad">Patch zu groß für die Anzeige.</p>
        ) : f.binary ? (
          <p className="muted pad">Binärdatei – kein Text-Diff.</p>
        ) : (
          <Patch text={f.patch ?? ""} />
        )}
      </details>
    </li>
  );
}

function Patch({ text }: { text: string }) {
  // Kopfzeilen (diff --git, index, ---/+++) ausblenden, Hunks einfärben.
  const lines = text.split("\n");
  const start = lines.findIndex((l) => l.startsWith("@@"));
  const body = start < 0 ? [] : lines.slice(start);
  if (body.length === 0) return <p className="muted pad">Keine Textänderungen (z. B. nur Dateimodus).</p>;
  return (
    <pre className="patch">
      {body.map((l, i) => {
        const cls = l.startsWith("@@") ? "hunk" : l.startsWith("+") ? "add" : l.startsWith("-") ? "del" : undefined;
        return (
          <span key={i} className={cls}>
            {l || " "}
            {"\n"}
          </span>
        );
      })}
    </pre>
  );
}
