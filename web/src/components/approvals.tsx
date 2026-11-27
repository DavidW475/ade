import { useMutation, useQueryClient } from "@tanstack/react-query";
import { useEffect, useRef, useState } from "react";
import { Link } from "react-router";
import {
  api,
  type Approval,
  type ApprovalDecision,
  type ApprovalStatus,
  type EgressApprovalDetail,
  type ToolApprovalDetail,
} from "../api";
import { errorMessage, formatTime } from "../format";
import { usePendingApprovals } from "../hooks/usePendingApprovals";
import { Badge, Notice } from "./ui";

const statusLabels: Record<ApprovalStatus, string> = {
  pending: "offen",
  approved: "erlaubt",
  denied: "abgelehnt",
  expired: "abgelaufen",
  canceled: "abgebrochen",
};

const decisionLabels: Record<ApprovalDecision, string> = {
  allow: "einmal erlaubt",
  allow_workspace: "für den Workspace erlaubt",
  allow_project: "für das Projekt erlaubt",
  deny: "abgelehnt",
};

export function ApprovalStatusBadge({ a }: { a: Approval }) {
  const tone = a.status === "approved" ? "ok" : a.status === "pending" ? "warn" : a.status === "denied" ? "bad" : "neutral";
  const label =
    a.status === "approved" && a.decision && a.kind === "egress" ? decisionLabels[a.decision] : statusLabels[a.status];
  return <Badge tone={tone}>{label}</Badge>;
}

/** Restzeit bis zum Ablauf, sekündlich aktualisiert. */
function Remaining({ until }: { until: string }) {
  const [now, setNow] = useState(() => Date.now());
  useEffect(() => {
    const t = setInterval(() => setNow(Date.now()), 1000);
    return () => clearInterval(t);
  }, []);
  const s = Math.max(0, Math.round((new Date(until).getTime() - now) / 1000));
  return (
    <span className="muted" title={`läuft ab um ${formatTime(until, true)}, dann gilt sie als abgelehnt`}>
      noch {Math.floor(s / 60)}:{String(s % 60).padStart(2, "0")} min
    </span>
  );
}

function str(v: unknown): string {
  return typeof v === "string" ? v : "";
}

/** Inhalt eines Werkzeug-Aufrufs: Befehl, Datei, Änderung oder die rohe Eingabe. */
function ToolInput({ d }: { d: ToolApprovalDetail }) {
  const i = d.input ?? {};
  if (d.tool === "Bash") {
    return (
      <>
        <pre className="approvalcode">{str(i.command)}</pre>
        {str(i.description) && <p className="hint">{str(i.description)}</p>}
      </>
    );
  }
  const path = str(i.file_path) || str(i.notebook_path);
  if (d.tool === "Edit" && path) {
    return (
      <>
        <p className="mono wrap">{path}</p>
        <pre className="approvalcode del">{str(i.old_string)}</pre>
        <pre className="approvalcode add">{str(i.new_string)}</pre>
      </>
    );
  }
  if (d.tool === "Write" && path) {
    return (
      <>
        <p className="mono wrap">{path}</p>
        <pre className="approvalcode">{str(i.content).slice(0, 4000)}</pre>
      </>
    );
  }
  return <pre className="approvalcode">{JSON.stringify(i, null, 2)}</pre>;
}

/** Eine Freigabe mit Inhalt und – solange offen – den Aktionen. */
export function ApprovalCard({ a, showContext = true }: { a: Approval; showContext?: boolean }) {
  const qc = useQueryClient();
  const [reason, setReason] = useState("");
  const decide = useMutation({
    mutationFn: (d: ApprovalDecision) => api.decideApproval(a.id, d, reason || undefined),
    onSettled: () => void qc.invalidateQueries({ queryKey: ["approvals"] }),
  });
  const pending = a.status === "pending";
  const tool = a.kind === "tool" ? (a.detail as ToolApprovalDetail) : undefined;
  const egress = a.kind === "egress" ? (a.detail as EgressApprovalDetail) : undefined;

  return (
    <li className={`card approval ${pending ? "pending" : ""}`}>
      <div className="cardtitle">
        <Badge tone={a.kind === "egress" ? "busy" : "neutral"}>{a.kind === "egress" ? "Netzwerk" : (tool?.tool ?? "Werkzeug")}</Badge>
        <strong className="approvaltitle">
          {egress ? (
            <>
              Agent will <code>{a.title}</code> erreichen
            </>
          ) : (
            a.title
          )}
        </strong>
        <ApprovalStatusBadge a={a} />
        {pending ? <Remaining until={a.expires_at} /> : <span className="muted">{formatTime(a.decided_at ?? a.created_at)}</span>}
      </div>
      {showContext && (
        <div className="meta">
          {a.sandbox ? "Sandbox" : "lokal"} · Workspace{" "}
          <Link to={`/workspaces/${a.workspace_id}`} className="mono">
            {a.workspace_id}
          </Link>
          {a.session_id && (
            <>
              {" "}
              · Session{" "}
              <Link to={`/sessions/${a.session_id}`} className="mono">
                {a.session_id}
              </Link>
            </>
          )}{" "}
          · {formatTime(a.created_at)}
        </div>
      )}
      {tool && pending && <ToolInput d={tool} />}
      {tool && !pending && (
        <details>
          <summary className="muted">Eingabe</summary>
          <ToolInput d={tool} />
        </details>
      )}
      {!pending && a.reason && <p className="hint">Begründung: {a.reason}</p>}
      {pending && (
        <>
          <div className="inline-form approvalactions">
            <input
              placeholder={tool ? "Begründung / Hinweis an den Agenten (optional)" : "Begründung (optional)"}
              value={reason}
              onChange={(e) => setReason(e.target.value)}
              aria-label="Begründung"
            />
            {tool ? (
              <button type="button" className="primary" disabled={decide.isPending} onClick={() => decide.mutate("allow")}>
                Erlauben
              </button>
            ) : (
              <>
                <button type="button" disabled={decide.isPending} onClick={() => decide.mutate("allow")} title="Nur die wartenden Anfragen">
                  Einmal
                </button>
                <button
                  type="button"
                  disabled={decide.isPending}
                  onClick={() => decide.mutate("allow_workspace")}
                  title="Ziel für diesen Workspace erlauben"
                >
                  Für diesen Workspace
                </button>
                <button
                  type="button"
                  className="primary"
                  disabled={decide.isPending}
                  onClick={() => decide.mutate("allow_project")}
                  title="Ziel für alle Workspaces des Projekts erlauben"
                >
                  Immer (Projekt)
                </button>
              </>
            )}
            <button type="button" className="danger" disabled={decide.isPending} onClick={() => decide.mutate("deny")}>
              Ablehnen
            </button>
          </div>
          {decide.isError && <Notice tone="bad">{errorMessage(decide.error)}</Notice>}
        </>
      )}
    </li>
  );
}

/** Offene Freigaben einer Session (Session-Ansicht). */
export function SessionApprovals({ sessionId }: { sessionId: string }) {
  const q = usePendingApprovals();
  const mine = (q.data ?? []).filter((a) => a.session_id === sessionId);
  if (!mine.length) return null;
  return (
    <div className="sessionapprovals">
      <Notice tone="warn" title="Der Agent wartet auf eine Freigabe">
        Erlauben oder ablehnen – der Agent macht danach weiter.
      </Notice>
      <ul className="approvals">
        {mine.map((a) => (
          <ApprovalCard key={a.id} a={a} showContext={false} />
        ))}
      </ul>
    </div>
  );
}

function notificationsSupported(): boolean {
  return typeof window !== "undefined" && "Notification" in window;
}

/**
 * Zahl offener Freigaben für die Navigation; setzt den Tab-Titel und meldet neue Freigaben als
 * Browser-Benachrichtigung (wenn der Nutzer zugestimmt hat und der Tab nicht im Vordergrund ist).
 */
export function ApprovalsNavBadge() {
  const q = usePendingApprovals();
  const seen = useRef<Set<string> | null>(null);
  const n = q.data?.length ?? 0;

  useEffect(() => {
    document.title = n > 0 ? `(${n}) ADE` : "ADE";
  }, [n]);

  useEffect(() => {
    if (!q.data) return;
    const ids = new Set(q.data.map((a) => a.id));
    const before = seen.current;
    seen.current = ids;
    if (!before || !notificationsSupported() || Notification.permission !== "granted" || !document.hidden) return;
    for (const a of q.data) {
      if (before.has(a.id)) continue;
      const n = new Notification("ADE: Freigabe nötig", {
        body: a.kind === "egress" ? `Agent will ${a.title} erreichen` : a.title,
        tag: `ade-approval-${a.id}`,
      });
      n.onclick = () => {
        window.focus();
        window.location.assign("/approvals");
      };
    }
  }, [q.data]);

  if (!n) return null;
  return (
    <span className="navbadge" aria-label={`${n} offene Freigaben`}>
      {n}
    </span>
  );
}

/** Schalter für Browser-Benachrichtigungen. */
export function NotificationToggle() {
  const [perm, setPerm] = useState(() => (notificationsSupported() ? Notification.permission : "denied"));
  if (!notificationsSupported()) return null;
  if (perm === "granted") return <span className="muted">Benachrichtigungen aktiv</span>;
  if (perm === "denied") return <span className="muted">Benachrichtigungen im Browser blockiert</span>;
  return (
    <button type="button" onClick={() => void Notification.requestPermission().then(setPerm)}>
      Benachrichtigungen aktivieren
    </button>
  );
}
