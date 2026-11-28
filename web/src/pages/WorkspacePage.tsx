import { useQuery } from "@tanstack/react-query";
import { Link, useParams } from "react-router";
import { api, isBusy } from "../api";
import { SessionTable, StartAgent } from "../components/agents";
import { IdeButton } from "../components/ide";
import { NetworkPanel } from "../components/network";
import { SandboxFacts, SandboxNotice } from "../components/sandbox";
import { DiffView, ReviewPanel, SensitiveNotice } from "../components/review";
import { sensitiveFiles } from "../sensitive";
import { useSessions } from "../sessions";
import { LineStats, Notice, WorkspaceStatusBadge } from "../components/ui";
import { errorMessage, formatTime, shortCommit } from "../format";
import { UsageAmount } from "../components/usage";
import { useUsage } from "../hooks/useUsage";

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
  const usage = useUsage(ws.data?.project_id ?? "-");
  const hasTree = ws.data && ws.data.status !== "creating" && ws.data.status !== "error";
  const diff = useQuery({
    queryKey: ["diff", id],
    // Only the file list; the diff editor loads contents per file.
    queryFn: () => api.diff(id, false),
    enabled: !!hasTree,
    refetchInterval: 5000,
  });
  const sessions = useSessions({ workspace_id: id });

  if (ws.isPending) return <p className="muted">Loading workspace …</p>;
  if (ws.isError) return <Notice tone="bad">{errorMessage(ws.error)}</Notice>;
  const w = ws.data;

  return (
    <>
      <div className="crumbs">
        <Link to="/">Projects</Link> / <Link to={`/projects/${w.project_id}`}>{project.data?.name ?? w.project_id}</Link> /{" "}
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
        <dt>Path</dt>
        <dd className="mono">{w.path}</dd>
        <dt>Base</dt>
        <dd className="mono">
          {w.base_branch} @ {shortCommit(w.base_commit)}
        </dd>
        <SandboxFacts workspace={w} />
        <dt>Costs</dt>
        <dd>
          <UsageAmount total={usage.data?.workspaces?.[w.id]} empty="$0.00" />
        </dd>
        <dt>Created</dt>
        <dd>{formatTime(w.created_at)}</dd>
      </dl>
      <SandboxNotice workspace={w} />

      {w.error && (
        <Notice tone={w.status === "setup_failed" ? "warn" : "bad"} title="Error">
          {w.error}
        </Notice>
      )}
      {w.setup_log && (
        <details className="panel" open={w.status === "setup_failed"}>
          <summary>Setup output</summary>
          <pre className="log">{w.setup_log}</pre>
        </details>
      )}

      <div className="sectionhead">
        <h2>Agents in this workspace</h2>
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
        <p className="muted">No agents in this workspace yet.</p>
      )}

      <div className="sectionhead">
        <h2>Changes compared to {w.base_branch}</h2>
        {diff.data && diff.data.files?.length ? (
          <span>
            {diff.data.files.length} {diff.data.files.length === 1 ? "file" : "files"}{" "}
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
        <p className="muted">No working directory.</p>
      ) : diff.isPending ? (
        <p className="muted">Computing diff …</p>
      ) : diff.isError ? (
        <Notice tone="bad">{errorMessage(diff.error)}</Notice>
      ) : !diff.data.files?.length ? (
        <div className="empty">
          <p>No changes.</p>
          <p className="muted">
            Changes in the workspace (<code>{w.path}</code>) appear here automatically.
          </p>
        </div>
      ) : (
        <>
          <SensitiveNotice paths={sensitiveFiles(diff.data.files)} />
          <DiffView workspaceId={w.id} diff={diff.data} />
        </>
      )}

      <NetworkPanel workspace={w} />
    </>
  );
}
