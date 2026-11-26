import { useEffect, useRef, type ReactNode } from "react";
import type { FileStatus, SessionStatus, WorkspaceStatus } from "../api";

const workspaceLabels: Record<WorkspaceStatus, string> = {
  creating: "wird angelegt",
  setup: "Setup läuft",
  ready: "bereit",
  setup_failed: "Setup fehlgeschlagen",
  error: "Fehler",
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
  added: "neu",
  modified: "geändert",
  deleted: "gelöscht",
  renamed: "umbenannt",
  copied: "kopiert",
  type_changed: "Typ geändert",
  unmerged: "Konflikt",
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
  starting: "startet",
  running: "arbeitet",
  idle: "wartet",
  done: "fertig",
  failed: "fehlgeschlagen",
  stopped: "gestoppt",
  interrupted: "unterbrochen",
};

const sessionTone: Record<SessionStatus, Tone> = {
  starting: "busy",
  running: "busy",
  idle: "ok",
  done: "neutral",
  failed: "bad",
  stopped: "neutral",
  interrupted: "warn",
};

export function SessionStatusBadge({ status, terminal = false }: { status: SessionStatus; terminal?: boolean }) {
  const label = terminal && status === "running" ? "läuft" : sessionLabels[status];
  return <Badge tone={sessionTone[status]}>{label}</Badge>;
}

/** Modaler Dialog (natives <dialog>): Escape und Klick auf den Hintergrund schließen ihn. */
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
