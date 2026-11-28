import { useEffect, useRef, type ReactNode } from "react";
import type { FileStatus, SessionStatus, WorkspaceStatus } from "../api";

const workspaceLabels: Record<WorkspaceStatus, string> = {
  creating: "creating",
  setup: "setup running",
  ready: "ready",
  setup_failed: "setup failed",
  error: "error",
};

const workspaceTone: Record<WorkspaceStatus, Tone> = {
  creating: "busy",
  setup: "busy",
  ready: "ok",
  setup_failed: "warn",
  error: "bad",
};

type Tone = "ok" | "warn" | "bad" | "busy" | "neutral";

export function Badge({ tone = "neutral", children }: { tone?: Tone; children: ReactNode }) {
  return <span className={`badge ${tone}`}>{children}</span>;
}

export function WorkspaceStatusBadge({ status }: { status: WorkspaceStatus }) {
  return <Badge tone={workspaceTone[status]}>{workspaceLabels[status]}</Badge>;
}

const fileLabels: Record<FileStatus, string> = {
  added: "new",
  modified: "modified",
  deleted: "deleted",
  renamed: "renamed",
  copied: "copied",
  type_changed: "type changed",
  unmerged: "conflict",
};

export function FileStatusBadge({ status }: { status: FileStatus }) {
  const tone: Tone = status === "added" ? "ok" : status === "deleted" ? "bad" : status === "unmerged" ? "warn" : "neutral";
  return <Badge tone={tone}>{fileLabels[status]}</Badge>;
}

export function Notice({ tone, title, children }: { tone: "warn" | "bad" | "info"; title?: string; children: ReactNode }) {
  return (
    <div className={`notice ${tone}`} role={tone === "bad" ? "alert" : "status"}>
      {title && <strong>{title}</strong>}
      <div>{children}</div>
    </div>
  );
}

export function LineStats({ additions, deletions }: { additions: number; deletions: number }) {
  return (
    <span className="linestats">
      <span className="add">+{additions}</span> <span className="del">−{deletions}</span>
    </span>
  );
}

const sessionLabels: Record<SessionStatus, string> = {
  starting: "starting",
  running: "working",
  idle: "waiting",
  done: "done",
  failed: "failed",
  stopped: "stopped",
  interrupted: "interrupted",
  paused: "paused (budget)",
};

const sessionTone: Record<SessionStatus, Tone> = {
  starting: "busy",
  running: "busy",
  idle: "ok",
  done: "neutral",
  failed: "bad",
  stopped: "neutral",
  interrupted: "warn",
  paused: "warn",
};

export function SessionStatusBadge({ status, terminal = false }: { status: SessionStatus; terminal?: boolean }) {
  const label = terminal && status === "running" ? "running" : sessionLabels[status];
  return <Badge tone={sessionTone[status]}>{label}</Badge>;
}

/** Modal dialog (native <dialog>): Escape and a click on the backdrop close it. */
export function Modal({ title, onClose, children }: { title: string; onClose: () => void; children: ReactNode }) {
  const ref = useRef<HTMLDialogElement>(null);
  useEffect(() => {
    const d = ref.current;
    if (d && !d.open) d.showModal();
    return () => d?.close();
  }, []);
  return (
    <dialog
      ref={ref}
      className="modal"
      aria-label={title}
      onCancel={(e) => {
        e.preventDefault();
        onClose();
      }}
      onClick={(e) => e.target === ref.current && onClose()}
    >
      <div className="modalbody">
        <h2>{title}</h2>
        {children}
      </div>
    </dialog>
  );
}
