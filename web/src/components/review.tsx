// Review and delivery (M4): diff view with Monaco and the actions merge, create PR, give
// feedback and discard.
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { lazy, Suspense, useMemo, useState } from "react";
import { useNavigate } from "react-router";
import { api, mergeConflicts, type Diff, type FileChange, type FileSide, type Session, type Workspace } from "../api";
import { errorMessage, formatTime, shortCommit } from "../format";
import { agentBusy } from "../sessions";
import { isSensitive } from "../sensitive";
import { FileStatusBadge, LineStats, Modal, Notice } from "./ui";

const DiffEditor = lazy(() => import("./DiffEditor"));

// --- Sensitive paths ----------------------------------------------------------------------

/** Warning when workflows, actions or .ade/ were changed. */
export function SensitiveNotice({ paths }: { paths: string[] }) {
  if (!paths.length) return null;
  return (
    <Notice tone="warn" title="Caution: changes to CI or ADE configuration">
      <p>
        Workflows and actions run with the repository's secrets after the push, <code>.ade/</code> controls setup, sandbox and approvals. Review
        these files carefully:
      </p>
      <ul className="sensitivelist">
        {paths.map((p) => (
          <li key={p} className="mono">
            {p}
          </li>
        ))}
      </ul>
    </Notice>
  );
}

// --- Actions ----------------------------------------------------------------------------

type Action = "merge" | "pr" | "feedback" | "discard";

export function ReviewPanel({
  workspace: w,
  projectName,
  repoPath,
  sessions,
  hasChanges,
}: {
  workspace: Workspace;
  projectName: string;
  repoPath?: string;
  sessions: Session[] | undefined;
  hasChanges: boolean;
}) {
  const [open, setOpen] = useState<Action | null>(null);
  const busy = agentBusy(sessions);
  const ready = w.status === "ready" || w.status === "setup_failed";
  const close = () => setOpen(null);

  return (
    <section className="panel review">
      <div className="reviewhead">
        <div className="reviewstate">
          {w.head_commit ? (
            <span>
              <span className="mono">{w.branch}</span> in the project repository @ <span className="mono">{shortCommit(w.head_commit)}</span>
              {w.collected_at && <span className="muted"> · {formatTime(w.collected_at)}</span>}
            </span>
          ) : (
            <span className="muted">Not taken into the project repository yet – this happens automatically when an agent is done.</span>
          )}
          {w.merge_commit && (
            <span>
              ✓ Merged into <span className="mono">{w.base_branch}</span> @ <span className="mono">{shortCommit(w.merge_commit)}</span>
              {w.merged_at && <span className="muted"> · {formatTime(w.merged_at)}</span>}
            </span>
          )}
          {w.pr_url && (
            <span>
              Pull request:{" "}
              <a href={w.pr_url} target="_blank" rel="noreferrer">
                {w.pr_url.replace(/^https?:\/\/(www\.)?github\.com\//, "")}
              </a>
            </span>
          )}
        </div>
        <div className="actions">
          <button type="button" className="primary" disabled={!ready || !!busy || !hasChanges} onClick={() => setOpen("merge")}>
            Merge into {w.base_branch}
          </button>
          <button type="button" disabled={!ready || !!busy || !hasChanges} onClick={() => setOpen("pr")}>
            {w.pr_url ? "Update PR" : "Create PR"}
          </button>
          <button type="button" disabled={!ready || !!busy} onClick={() => setOpen("feedback")}>
            Give feedback
          </button>
          <button type="button" className="danger" onClick={() => setOpen("discard")}>
            Discard
          </button>
        </div>
      </div>
      {busy && (
        <p className="hint">
          The agent is still working – merge, PR and feedback are possible as soon as it is done or was stopped.
        </p>
      )}

      {open === "merge" && <MergeDialog workspace={w} projectName={projectName} repoPath={repoPath} onClose={close} />}
      {open === "pr" && <PRDialog workspace={w} onClose={close} />}
      {open === "feedback" && <FeedbackDialog workspace={w} onClose={close} />}
      {open === "discard" && <DiscardDialog workspace={w} sessions={sessions} onClose={close} />}
    </section>
  );
}

function useWorkspaceUpdate(id: string) {
  const qc = useQueryClient();
  return (w: Workspace) => {
    qc.setQueryData(["workspace", id], (old: Workspace | undefined) => ({ ...old, ...w }));
    void qc.invalidateQueries({ queryKey: ["workspace", id] });
    void qc.invalidateQueries({ queryKey: ["diff", id] });
  };
}

function MergeDialog({
  workspace: w,
  projectName,
  repoPath,
  onClose,
}: {
  workspace: Workspace;
  projectName: string;
  repoPath?: string;
  onClose: () => void;
}) {
  const navigate = useNavigate();
  const update = useWorkspaceUpdate(w.id);
  const [discard, setDiscard] = useState(false);
  const merge = useMutation({
    mutationFn: async () => {
      const merged = await api.merge(w.id);
      if (discard) await api.deleteWorkspace(w.id);
      return merged;
    },
    onSuccess: (merged) => {
      if (discard) {
        void navigate(`/projects/${w.project_id}`);
        return;
      }
      update(merged);
      onClose();
    },
  });
  const conflicts = mergeConflicts(merge.error);

  return (
    <Modal title={`Merge into ${w.base_branch}`} onClose={onClose}>
      <p>
        In the project <strong>{projectName}</strong>
        {repoPath && (
          <>
            {" "}
            (<code>{repoPath}</code>)
          </>
        )}{" "}
        ADE creates a merge commit of <code>{w.branch}</code> into <code>{w.base_branch}</code>. Open changes in the workspace
        are committed first.
      </p>
      <p className="muted">
        If <code>{w.base_branch}</code> is checked out there, <code>git merge</code> runs in the working tree. With conflicts
        nothing changes.
      </p>
      <label className="check">
        <input type="checkbox" checked={discard} onChange={(e) => setDiscard(e.target.checked)} /> Discard the workspace
        afterwards
      </label>
      {merge.isError &&
        (conflicts.length ? (
          <Notice tone="warn" title="Conflicts – nothing was changed">
            <ul className="conflicts">
              {conflicts.map((c) => (
                <li key={c} className="mono">
                  {c}
                </li>
              ))}
            </ul>
            <p>Use “Give feedback” to ask the agent to adapt the changes to the current state.</p>
          </Notice>
        ) : (
          <Notice tone="bad">{errorMessage(merge.error)}</Notice>
        ))}
      <div className="actions">
        <button type="button" onClick={onClose}>
          Cancel
        </button>
        <button type="button" className="primary" disabled={merge.isPending} onClick={() => merge.mutate()}>
          {merge.isPending ? "Merging …" : "Merge"}
        </button>
      </div>
    </Modal>
  );
}

function PRDialog({ workspace: w, onClose }: { workspace: Workspace; onClose: () => void }) {
  const update = useWorkspaceUpdate(w.id);
  const draft = useQuery({ queryKey: ["pr-draft", w.id], queryFn: () => api.prDraft(w.id), staleTime: Infinity });
  const [title, setTitle] = useState<string | null>(null);
  const [body, setBody] = useState<string | null>(null);
  const [isDraft, setIsDraft] = useState(false);
  const create = useMutation({
    mutationFn: () =>
      api.createPR(w.id, { title: title ?? draft.data?.title, body: body ?? draft.data?.body, draft: isDraft }),
    onSuccess: (updated) => {
      update(updated);
      onClose();
    },
  });
  const existing = w.pr_url;
  const d = draft.data;

  return (
    <Modal title={existing ? "Update pull request" : "Create pull request"} onClose={onClose}>
      {draft.isPending ? (
        <p className="muted">Loading draft …</p>
      ) : draft.isError ? (
        <Notice tone="bad">{errorMessage(draft.error)}</Notice>
      ) : !d?.remote_url ? (
        <Notice tone="warn" title="No remote">
          The project repository has no remote <code>{d?.remote ?? "origin"}</code>. For pull requests, set up a GitHub remote
          (<code>git remote add origin …</code>).
        </Notice>
      ) : (
        <>
          <p>
            The daemon pushes <code>{w.branch}</code> from the host to <code>{d.remote}</code> (<code>{d.remote_url}</code>)
            {existing ? (
              <>
                {" "}
                and thereby updates{" "}
                <a href={existing} target="_blank" rel="noreferrer">
                  the existing PR
                </a>
                .
              </>
            ) : (
              <>
                {" "}
                and creates a PR against <code>{d.base}</code> with <code>gh pr create</code>.
              </>
            )}{" "}
            The agent itself has no push rights.
          </p>
          {!existing && (
            <form className="startform" onSubmit={(e) => e.preventDefault()}>
              <div className="field wide">
                <label htmlFor="pr-title">Title</label>
                <input id="pr-title" value={title ?? d.title} maxLength={256} onChange={(e) => setTitle(e.target.value)} />
              </div>
              <div className="field wide">
                <label htmlFor="pr-body">Description</label>
                <textarea id="pr-body" rows={12} value={body ?? d.body} onChange={(e) => setBody(e.target.value)} />
              </div>
              <label className="check wide">
                <input type="checkbox" checked={isDraft} onChange={(e) => setIsDraft(e.target.checked)} /> Create as
                draft
              </label>
            </form>
          )}
        </>
      )}
      {create.isError && <Notice tone="bad">{errorMessage(create.error)}</Notice>}
      <div className="actions">
        <button type="button" onClick={onClose}>
          Cancel
        </button>
        <button
          type="button"
          className="primary"
          disabled={create.isPending || !d?.remote_url || (!existing && !(title ?? d.title).trim())}
          onClick={() => create.mutate()}
        >
          {create.isPending ? "Pushing …" : existing ? "Push" : "Push and create PR"}
        </button>
      </div>
    </Modal>
  );
}

function FeedbackDialog({ workspace: w, onClose }: { workspace: Workspace; onClose: () => void }) {
  const navigate = useNavigate();
  const qc = useQueryClient();
  const [comment, setComment] = useState("");
  const send = useMutation({
    mutationFn: () => api.feedback(w.id, { comment }),
    onSuccess: (s) => {
      void qc.invalidateQueries({ queryKey: ["sessions"] });
      void navigate(`/sessions/${s.id}`);
    },
  });
  return (
    <Modal title="Give feedback" onClose={onClose}>
      <p>
        A new agent session in the same workspace gets your comment and the current diff. Its result ends up as
        another commit in <code>{w.branch}</code>
        {w.pr_url ? "; with “Update PR” it then goes into the pull request" : ""}.
      </p>
      <form
        onSubmit={(e) => {
          e.preventDefault();
          if (comment.trim()) send.mutate();
        }}
      >
        <textarea
          rows={8}
          value={comment}
          autoFocus
          placeholder="What should the agent change?"
          onChange={(e) => setComment(e.target.value)}
          onKeyDown={(e) => {
            if (e.key === "Enter" && (e.ctrlKey || e.metaKey) && comment.trim()) send.mutate();
          }}
        />
        {send.isError && <Notice tone="bad">{errorMessage(send.error)}</Notice>}
        <div className="actions">
          <button type="button" onClick={onClose}>
            Cancel
          </button>
          <button type="submit" className="primary" disabled={send.isPending || !comment.trim()}>
            {send.isPending ? "Starting …" : "Start agent"}
          </button>
        </div>
      </form>
    </Modal>
  );
}

function DiscardDialog({
  workspace: w,
  sessions,
  onClose,
}: {
  workspace: Workspace;
  sessions: Session[] | undefined;
  onClose: () => void;
}) {
  const navigate = useNavigate();
  const del = useMutation({
    mutationFn: () => api.deleteWorkspace(w.id),
    onSuccess: () => void navigate(`/projects/${w.project_id}`),
  });
  const live = sessions?.some((s) => s.live);
  return (
    <Modal title="Discard workspace" onClose={onClose}>
      <p>Removes without leftovers:</p>
      <ul>
        <li>
          the working directory <code>{w.path}</code> with all unmerged changes,
        </li>
        <li>
          the branch <code>{w.branch}</code> in the project repository,
        </li>
        <li>agent sessions, histories, terminal logs and IDE data.</li>
      </ul>
      {live && <Notice tone="warn">Running agents, terminals and the IDE are stopped in the process.</Notice>}
      {w.pr_url && (
        <p className="muted">
          The pushed branch and the{" "}
          <a href={w.pr_url} target="_blank" rel="noreferrer">
            pull request
          </a>{" "}
          stay on GitHub.
        </p>
      )}
      {w.merge_commit && <p className="muted">The merge into {w.base_branch} stays.</p>}
      {del.isError && <Notice tone="bad">{errorMessage(del.error)}</Notice>}
      <div className="actions">
        <button type="button" onClick={onClose}>
          Cancel
        </button>
        <button type="button" className="danger" disabled={del.isPending} onClick={() => del.mutate()}>
          {del.isPending ? "Discarding …" : "Discard for good"}
        </button>
      </div>
    </Modal>
  );
}

// --- Diff view --------------------------------------------------------------------------

const layoutKey = "ade.diff.sideBySide";

function loadSideBySide(): boolean {
  try {
    return localStorage.getItem(layoutKey) !== "false";
  } catch {
    return true;
  }
}

export function DiffView({ workspaceId, diff }: { workspaceId: string; diff: Diff }) {
  const files = useMemo(() => diff.files ?? [], [diff.files]);
  const [selected, setSelected] = useState<string | null>(null);
  const [filter, setFilter] = useState("");
  const [sideBySide, setSideBySide] = useState(loadSideBySide);
  const current = files.find((f) => f.path === selected) ?? files[0];
  const shown = filter ? files.filter((f) => f.path.toLowerCase().includes(filter.toLowerCase())) : files;

  const setLayout = (v: boolean) => {
    setSideBySide(v);
    try {
      localStorage.setItem(layoutKey, String(v));
    } catch {
      // Without storage the choice only applies to this page.
    }
  };

  return (
    <div className="diffview">
      <aside className="difffiles">
        {files.length > 12 && (
          <input
            className="difffilter"
            placeholder="Filter files …"
            value={filter}
            onChange={(e) => setFilter(e.target.value)}
            aria-label="Filter files"
          />
        )}
        <ul>
          {shown.map((f) => (
            <li key={f.path}>
              <button
                type="button"
                className={[f.path === current?.path && "active", (isSensitive(f.path) || (f.old_path && isSensitive(f.old_path))) && "sensitive"].filter(Boolean).join(" ") || undefined}
                onClick={() => setSelected(f.path)}
                title={(f.old_path ? `${f.old_path} → ${f.path}` : f.path) + (isSensitive(f.path) || (f.old_path && isSensitive(f.old_path)) ? " – sensitive path (CI/ADE)" : "")}
              >
                <FileStatusBadge status={f.status} />
                <span className="mono path">{basename(f.path)}</span>
                <span className="dir mono">{dirname(f.path)}</span>
                {f.binary ? <span className="muted">binary</span> : <LineStats additions={f.additions} deletions={f.deletions} />}
              </button>
            </li>
          ))}
        </ul>
      </aside>
      <div className="diffmain">
        {current && (
          <>
            <div className="diffbar">
              <span className="mono path">
                {current.old_path ? (
                  <>
                    {current.old_path} → {current.path}
                  </>
                ) : (
                  current.path
                )}
              </span>
              <div className="segmented" role="group" aria-label="Layout">
                <button type="button" className={sideBySide ? "active" : undefined} onClick={() => setLayout(true)}>
                  Side by side
                </button>
                <button type="button" className={!sideBySide ? "active" : undefined} onClick={() => setLayout(false)}>
                  Inline
                </button>
              </div>
            </div>
            <FilePane workspaceId={workspaceId} file={current} head={diff.head_commit} sideBySide={sideBySide} />
          </>
        )}
      </div>
    </div>
  );
}

function FilePane({
  workspaceId,
  file,
  head,
  sideBySide,
}: {
  workspaceId: string;
  file: FileChange;
  head: string;
  sideBySide: boolean;
}) {
  // Load contents only when selected; line counts in the key reload changed files.
  const contents = useQuery({
    queryKey: ["file", workspaceId, file.path, file.old_path, head, file.additions, file.deletions],
    queryFn: () => api.file(workspaceId, file.path, file.old_path),
    staleTime: 10_000,
  });
  if (file.binary) return <p className="muted pad diffnote">Binary file – no text diff.</p>;
  if (contents.isPending) return <p className="muted pad diffnote">Loading file …</p>;
  if (contents.isError) return <Notice tone="bad">{errorMessage(contents.error)}</Notice>;
  const { original, modified } = contents.data;
  const problem = sideProblem(original) ?? sideProblem(modified);
  if (problem) return <p className="muted pad diffnote">{problem}</p>;
  return (
    <Suspense fallback={<p className="muted pad diffnote">Loading editor …</p>}>
      <DiffEditor path={file.path} original={original.text ?? ""} modified={modified.text ?? ""} sideBySide={sideBySide} />
    </Suspense>
  );
}

function sideProblem(s: FileSide): string | undefined {
  if (s.too_large) return "File too large for the diff editor (over 4 MiB).";
  if (s.binary) return "Binary file – no text diff.";
  return undefined;
}

function basename(p: string): string {
  const i = p.lastIndexOf("/");
  return i < 0 ? p : p.slice(i + 1);
}

function dirname(p: string): string {
  const i = p.lastIndexOf("/");
  return i < 0 ? "" : p.slice(0, i);
}
