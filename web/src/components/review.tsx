// Review und Übernahme (M4): Diff-Ansicht mit Monaco und die Aktionen Mergen, PR erstellen,
// Feedback geben und Verwerfen.
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { lazy, Suspense, useMemo, useState } from "react";
import { useNavigate } from "react-router";
import { api, mergeConflicts, type Diff, type FileChange, type FileSide, type Session, type Workspace } from "../api";
import { errorMessage, formatTime, shortCommit } from "../format";
import { agentBusy } from "../sessions";
import { FileStatusBadge, LineStats, Modal, Notice } from "./ui";

const DiffEditor = lazy(() => import("./DiffEditor"));

// --- Aktionen ----------------------------------------------------------------------------

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
              <span className="mono">{w.branch}</span> im Projekt-Repo @ <span className="mono">{shortCommit(w.head_commit)}</span>
              {w.collected_at && <span className="muted"> · {formatTime(w.collected_at)}</span>}
            </span>
          ) : (
            <span className="muted">Noch nicht ins Projekt-Repo übernommen – passiert automatisch, wenn ein Agent fertig ist.</span>
          )}
          {w.merge_commit && (
            <span>
              ✓ Gemergt in <span className="mono">{w.base_branch}</span> @ <span className="mono">{shortCommit(w.merge_commit)}</span>
              {w.merged_at && <span className="muted"> · {formatTime(w.merged_at)}</span>}
            </span>
          )}
          {w.pr_url && (
            <span>
              Pull Request:{" "}
              <a href={w.pr_url} target="_blank" rel="noreferrer">
                {w.pr_url.replace(/^https?:\/\/(www\.)?github\.com\//, "")}
              </a>
            </span>
          )}
        </div>
        <div className="actions">
          <button type="button" className="primary" disabled={!ready || !!busy || !hasChanges} onClick={() => setOpen("merge")}>
            Mergen in {w.base_branch}
          </button>
          <button type="button" disabled={!ready || !!busy || !hasChanges} onClick={() => setOpen("pr")}>
            {w.pr_url ? "PR aktualisieren" : "PR erstellen"}
          </button>
          <button type="button" disabled={!ready || !!busy} onClick={() => setOpen("feedback")}>
            Feedback geben
          </button>
          <button type="button" className="danger" onClick={() => setOpen("discard")}>
            Verwerfen
          </button>
        </div>
      </div>
      {busy && (
        <p className="hint">
          Der Agent arbeitet noch – Mergen, PR und Feedback sind möglich, sobald er fertig ist oder gestoppt wurde.
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
    <Modal title={`In ${w.base_branch} mergen`} onClose={onClose}>
      <p>
        ADE legt im Projekt <strong>{projectName}</strong>
        {repoPath && (
          <>
            {" "}
            (<code>{repoPath}</code>)
          </>
        )}{" "}
        einen Merge-Commit von <code>{w.branch}</code> in <code>{w.base_branch}</code> an. Offene Änderungen im Workspace
        werden vorher committet.
      </p>
      <p className="muted">
        Ist <code>{w.base_branch}</code> dort ausgecheckt, läuft <code>git merge</code> im Arbeitsbaum. Bei Konflikten
        ändert sich nichts.
      </p>
      <label className="check">
        <input type="checkbox" checked={discard} onChange={(e) => setDiscard(e.target.checked)} /> Workspace danach
        verwerfen
      </label>
      {merge.isError &&
        (conflicts.length ? (
          <Notice tone="warn" title="Konflikte – nichts wurde verändert">
            <ul className="conflicts">
              {conflicts.map((c) => (
                <li key={c} className="mono">
                  {c}
                </li>
              ))}
            </ul>
            <p>Per „Feedback geben“ den Agenten bitten, die Änderungen auf den aktuellen Stand anzupassen.</p>
          </Notice>
        ) : (
          <Notice tone="bad">{errorMessage(merge.error)}</Notice>
        ))}
      <div className="actions">
        <button type="button" onClick={onClose}>
          Abbrechen
        </button>
        <button type="button" className="primary" disabled={merge.isPending} onClick={() => merge.mutate()}>
          {merge.isPending ? "Merge läuft …" : "Mergen"}
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
    <Modal title={existing ? "Pull Request aktualisieren" : "Pull Request erstellen"} onClose={onClose}>
      {draft.isPending ? (
        <p className="muted">Lade Entwurf …</p>
      ) : draft.isError ? (
        <Notice tone="bad">{errorMessage(draft.error)}</Notice>
      ) : !d?.remote_url ? (
        <Notice tone="warn" title="Kein Remote">
          Das Projekt-Repo hat kein Remote <code>{d?.remote ?? "origin"}</code>. Für Pull Requests ein GitHub-Remote
          einrichten (<code>git remote add origin …</code>).
        </Notice>
      ) : (
        <>
          <p>
            Der Daemon pusht <code>{w.branch}</code> vom Host nach <code>{d.remote}</code> (<code>{d.remote_url}</code>)
            {existing ? (
              <>
                {" "}
                und aktualisiert damit{" "}
                <a href={existing} target="_blank" rel="noreferrer">
                  den bestehenden PR
                </a>
                .
              </>
            ) : (
              <>
                {" "}
                und erstellt mit <code>gh pr create</code> einen PR gegen <code>{d.base}</code>.
              </>
            )}{" "}
            Der Agent selbst hat keine Push-Rechte.
          </p>
          {!existing && (
            <form className="startform" onSubmit={(e) => e.preventDefault()}>
              <div className="field wide">
                <label htmlFor="pr-title">Titel</label>
                <input id="pr-title" value={title ?? d.title} maxLength={256} onChange={(e) => setTitle(e.target.value)} />
              </div>
              <div className="field wide">
                <label htmlFor="pr-body">Beschreibung</label>
                <textarea id="pr-body" rows={12} value={body ?? d.body} onChange={(e) => setBody(e.target.value)} />
              </div>
              <label className="check wide">
                <input type="checkbox" checked={isDraft} onChange={(e) => setIsDraft(e.target.checked)} /> Als Entwurf
                (Draft) erstellen
              </label>
            </form>
          )}
        </>
      )}
      {create.isError && <Notice tone="bad">{errorMessage(create.error)}</Notice>}
      <div className="actions">
        <button type="button" onClick={onClose}>
          Abbrechen
        </button>
        <button
          type="button"
          className="primary"
          disabled={create.isPending || !d?.remote_url || (!existing && !(title ?? d.title).trim())}
          onClick={() => create.mutate()}
        >
          {create.isPending ? "Push läuft …" : existing ? "Pushen" : "Pushen und PR erstellen"}
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
    <Modal title="Feedback geben" onClose={onClose}>
      <p>
        Eine neue Agent-Session im selben Workspace bekommt deinen Kommentar und den aktuellen Diff. Ihr Ergebnis landet als
        weiterer Commit in <code>{w.branch}</code>
        {w.pr_url ? "; mit „PR aktualisieren“ geht es danach in den Pull Request" : ""}.
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
          placeholder="Was soll der Agent ändern?"
          onChange={(e) => setComment(e.target.value)}
          onKeyDown={(e) => {
            if (e.key === "Enter" && (e.ctrlKey || e.metaKey) && comment.trim()) send.mutate();
          }}
        />
        {send.isError && <Notice tone="bad">{errorMessage(send.error)}</Notice>}
        <div className="actions">
          <button type="button" onClick={onClose}>
            Abbrechen
          </button>
          <button type="submit" className="primary" disabled={send.isPending || !comment.trim()}>
            {send.isPending ? "Startet …" : "Agent starten"}
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
    <Modal title="Workspace verwerfen" onClose={onClose}>
      <p>Entfernt rückstandsfrei:</p>
      <ul>
        <li>
          das Arbeitsverzeichnis <code>{w.path}</code> mit allen nicht gemergten Änderungen,
        </li>
        <li>
          den Branch <code>{w.branch}</code> im Projekt-Repo,
        </li>
        <li>Agent-Sessions, Verläufe, Terminal-Logs und IDE-Daten.</li>
      </ul>
      {live && <Notice tone="warn">Laufende Agenten, Terminals und die IDE werden dabei gestoppt.</Notice>}
      {w.pr_url && (
        <p className="muted">
          Der gepushte Branch und der{" "}
          <a href={w.pr_url} target="_blank" rel="noreferrer">
            Pull Request
          </a>{" "}
          bleiben auf GitHub.
        </p>
      )}
      {w.merge_commit && <p className="muted">Der Merge in {w.base_branch} bleibt erhalten.</p>}
      {del.isError && <Notice tone="bad">{errorMessage(del.error)}</Notice>}
      <div className="actions">
        <button type="button" onClick={onClose}>
          Abbrechen
        </button>
        <button type="button" className="danger" disabled={del.isPending} onClick={() => del.mutate()}>
          {del.isPending ? "Wird verworfen …" : "Endgültig verwerfen"}
        </button>
      </div>
    </Modal>
  );
}

// --- Diff-Ansicht -------------------------------------------------------------------------

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
      // Ohne Speicher gilt die Wahl nur für diese Seite.
    }
  };

  return (
    <div className="diffview">
      <aside className="difffiles">
        {files.length > 12 && (
          <input
            className="difffilter"
            placeholder="Dateien filtern …"
            value={filter}
            onChange={(e) => setFilter(e.target.value)}
            aria-label="Dateien filtern"
          />
        )}
        <ul>
          {shown.map((f) => (
            <li key={f.path}>
              <button
                type="button"
                className={f.path === current?.path ? "active" : undefined}
                onClick={() => setSelected(f.path)}
                title={f.old_path ? `${f.old_path} → ${f.path}` : f.path}
              >
                <FileStatusBadge status={f.status} />
                <span className="mono path">{basename(f.path)}</span>
                <span className="dir mono">{dirname(f.path)}</span>
                {f.binary ? <span className="muted">binär</span> : <LineStats additions={f.additions} deletions={f.deletions} />}
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
              <div className="segmented" role="group" aria-label="Darstellung">
                <button type="button" className={sideBySide ? "active" : undefined} onClick={() => setLayout(true)}>
                  Nebeneinander
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
  // Inhalte erst bei Auswahl laden; Zeilenzahlen im Schlüssel laden geänderte Dateien neu.
  const contents = useQuery({
    queryKey: ["file", workspaceId, file.path, file.old_path, head, file.additions, file.deletions],
    queryFn: () => api.file(workspaceId, file.path, file.old_path),
    staleTime: 10_000,
  });
  if (file.binary) return <p className="muted pad diffnote">Binärdatei – kein Text-Diff.</p>;
  if (contents.isPending) return <p className="muted pad diffnote">Lade Datei …</p>;
  if (contents.isError) return <Notice tone="bad">{errorMessage(contents.error)}</Notice>;
  const { original, modified } = contents.data;
  const problem = sideProblem(original) ?? sideProblem(modified);
  if (problem) return <p className="muted pad diffnote">{problem}</p>;
  return (
    <Suspense fallback={<p className="muted pad diffnote">Lade Editor …</p>}>
      <DiffEditor path={file.path} original={original.text ?? ""} modified={modified.text ?? ""} sideBySide={sideBySide} />
    </Suspense>
  );
}

function sideProblem(s: FileSide): string | undefined {
  if (s.too_large) return "Datei zu groß für den Diff-Editor (über 4 MiB).";
  if (s.binary) return "Binärdatei – kein Text-Diff.";
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
