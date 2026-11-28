import { useMutation, useQueryClient } from "@tanstack/react-query";
import { useEffect, useRef, useState } from "react";
import { Link } from "react-router";
import {
  api,
  type Approval,
  type ApprovalDecision,
  type ApprovalStatus,
  type BudgetApprovalDetail,
  type EgressApprovalDetail,
  type GateApprovalDetail,
  type ToolApprovalDetail,
} from "../api";
import { errorMessage, formatTime, formatUSD } from "../format";
import { usePendingApprovals } from "../hooks/usePendingApprovals";
import { SensitiveNotice } from "./review";
import { Badge, Notice } from "./ui";

const statusLabels: Record<ApprovalStatus, string> = {
  pending: "pending",
  approved: "allowed",
  denied: "denied",
  expired: "expired",
  canceled: "canceled",
};

const decisionLabels: Record<ApprovalDecision, string> = {
  allow: "allowed once",
  allow_workspace: "allowed for the workspace",
  allow_project: "allowed for the project",
  deny: "denied",
};

export function ApprovalStatusBadge({ a }: { a: Approval }) {
  const tone = a.status === "approved" ? "ok" : a.status === "pending" ? "warn" : a.status === "denied" ? "bad" : "neutral";
  const label =
    a.status === "approved" && a.decision && a.kind === "egress" ? decisionLabels[a.decision] : statusLabels[a.status];
  return <Badge tone={tone}>{label}</Badge>;
}

/** Remaining time until expiry, updated every second. */
function Remaining({ until }: { until: string }) {
  const [now, setNow] = useState(() => Date.now());
  useEffect(() => {
    const t = setInterval(() => setNow(Date.now()), 1000);
    return () => clearInterval(t);
  }, []);
  const s = Math.max(0, Math.round((new Date(until).getTime() - now) / 1000));
  const text =
    s >= 3600 ? `${Math.floor(s / 3600)} h ${Math.floor((s % 3600) / 60)} min left` : `${Math.floor(s / 60)}:${String(s % 60).padStart(2, "0")} min left`;
  return (
    <span className="muted" title={`expires at ${formatTime(until)}, then it counts as denied`}>
      {text}
    </span>
  );
}

function str(v: unknown): string {
  return typeof v === "string" ? v : "";
}

/** Content of a tool call: command, file, change or the raw input. */
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

/** An approval with its content and – while pending – the actions. */
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
  const budget = a.kind === "budget" ? (a.detail as BudgetApprovalDetail) : undefined;
  const gate = a.kind === "gate" ? (a.detail as GateApprovalDetail) : undefined;
  // Gate with on_deny: denying sends the run back with the reason.
  const back = gate?.on_deny;
  const kindLabel = egress ? "Network" : budget ? "Budget" : gate ? "Factory" : (tool?.tool ?? "Tool");

  return (
    <li className={`card approval ${pending ? "pending" : ""}`}>
      <div className="cardtitle">
        <Badge tone={egress ? "busy" : budget || gate ? "warn" : "neutral"}>{kindLabel}</Badge>
        <strong className="approvaltitle">
          {egress ? (
            <>
              Agent wants to reach <code>{a.title}</code>
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
          {a.sandbox ? "sandbox" : "local"} · workspace{" "}
          <Link to={`/workspaces/${a.workspace_id}`} className="mono">
            {a.workspace_id}
          </Link>
          {a.session_id && (
            <>
              {" "}
              · session{" "}
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
          <summary className="muted">Input</summary>
          <ToolInput d={tool} />
        </details>
      )}
      {budget && (
        <p className="hint">
          {budget.scope === "monthly" ? "Monthly costs of the project" : "Costs of the session"}: {formatUSD(budget.spent_usd)} of{" "}
          {formatUSD(budget.limit_usd)}. The agent is paused.
          {pending && ` “Raise budget” allows another ${formatUSD(budget.step_usd)} and resumes it.`}
        </p>
      )}
      {gate?.sensitive?.length ? <SensitiveNotice paths={gate.sensitive} /> : null}
      {gate?.unattended && (
        <p className="hint">
          Run <Link to={`/runs/${gate.run_id}`} className="mono">{gate.run_id}</Link> wants to open a pull request without a gate and changed the
          files named above. Check the diff in the{" "}
          <Link to={`/workspaces/${a.workspace_id}`}>workspace</Link> first.
          {pending && " Approving pushes and opens the PR, denying cancels the run."}
        </p>
      )}
      {gate && !gate.unattended && (
        <p className="hint">
          Run <Link to={`/runs/${gate.run_id}`} className="mono">{gate.run_id}</Link> waits before step <code>{gate.step}</code>.
          {gate.artifacts?.length ? <> To review: {gate.artifacts.map((n) => <code key={n}>{n} </code>)}</> : null}
          {gate.iteration && gate.max_iterations ? ` · pass ${gate.iteration} of at most ${gate.max_iterations}.` : ""}
          {pending &&
            (back ? (
              <>
                {" "}
                Approving continues the run, a note goes to the next role. “Back to {back}” jumps back with your reason as feedback –
                the reason is required for that.
              </>
            ) : (
              " A note in the reason field goes to the next role; denying cancels the run."
            ))}
        </p>
      )}
      {!pending && a.reason && <p className="hint">Reason: {a.reason}</p>}
      {pending && (
        <>
          <div className="inline-form approvalactions">
            <input
              placeholder={
                back ? `Note to the next role – or feedback for ${back} (required when sending back)` : tool || gate ? "Reason / note to the agent (optional)" : "Reason (optional)"
              }
              value={reason}
              onChange={(e) => setReason(e.target.value)}
              aria-label="Reason"
            />
            {tool || budget || gate ? (
              <button type="button" className="primary" disabled={decide.isPending} onClick={() => decide.mutate("allow")}>
                {budget ? "Raise budget" : gate ? "Approve" : "Allow"}
              </button>
            ) : (
              <>
                <button type="button" disabled={decide.isPending} onClick={() => decide.mutate("allow")} title="Only the waiting requests">
                  Once
                </button>
                <button
                  type="button"
                  disabled={decide.isPending}
                  onClick={() => decide.mutate("allow_workspace")}
                  title="Allow the target for this workspace"
                >
                  For this workspace
                </button>
                <button
                  type="button"
                  className="primary"
                  disabled={decide.isPending}
                  onClick={() => decide.mutate("allow_project")}
                  title="Allow the target for all workspaces of the project"
                >
                  Always (project)
                </button>
              </>
            )}
            <button
              type="button"
              className="danger"
              disabled={decide.isPending || (!!back && !reason.trim())}
              title={back && !reason.trim() ? "Enter a reason first – it goes back as feedback" : undefined}
              onClick={() => decide.mutate("deny")}
            >
              {back ? `Back to ${back}` : "Deny"}
            </button>
          </div>
          {decide.isError && <Notice tone="bad">{errorMessage(decide.error)}</Notice>}
        </>
      )}
    </li>
  );
}

/** Pending approvals of a session (session view). */
export function SessionApprovals({ sessionId }: { sessionId: string }) {
  const q = usePendingApprovals();
  const mine = (q.data ?? []).filter((a) => a.session_id === sessionId);
  if (!mine.length) return null;
  return (
    <div className="sessionapprovals">
      <Notice tone="warn" title="The agent is waiting for an approval">
        {mine.some((a) => a.kind === "budget")
          ? "Raise the budget so the agent continues, or deny to end it."
          : "Allow or deny – the agent continues afterwards."}
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
 * Number of pending approvals for the navigation; sets the tab title and reports new approvals as
 * browser notifications (if the user agreed and the tab is not in the foreground).
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
      const n = new Notification("ADE: approval needed", {
        body: a.kind === "egress" ? `Agent wants to reach ${a.title}` : a.kind === "budget" ? `Agent paused: ${a.title}` : a.title,
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
    <span className="navbadge" aria-label={`${n} pending approvals`}>
      {n}
    </span>
  );
}

/** Switch for browser notifications. */
export function NotificationToggle() {
  const [perm, setPerm] = useState(() => (notificationsSupported() ? Notification.permission : "denied"));
  if (!notificationsSupported()) return null;
  if (perm === "granted") return <span className="muted">Notifications active</span>;
  if (perm === "denied") return <span className="muted">Notifications blocked in the browser</span>;
  return (
    <button type="button" onClick={() => void Notification.requestPermission().then(setPerm)}>
      Enable notifications
    </button>
  );
}
