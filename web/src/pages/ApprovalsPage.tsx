import { useQuery } from "@tanstack/react-query";
import { api } from "../api";
import { ApprovalCard, NotificationToggle } from "../components/approvals";
import { usePendingApprovals } from "../hooks/usePendingApprovals";
import { Notice } from "../components/ui";
import { errorMessage } from "../format";

/** Warteschlange aller offenen Freigaben und die letzten Entscheidungen. */
export function ApprovalsPage() {
  const pending = usePendingApprovals();
  const recent = useQuery({ queryKey: ["approvals", "recent"], queryFn: () => api.approvals({ limit: 50 }), refetchInterval: 5000 });
  const done = (recent.data ?? []).filter((a) => a.status !== "pending");

  return (
    <>
      <div className="pagehead">
        <h1>Freigaben</h1>
        <div className="actions">
          <NotificationToggle />
        </div>
      </div>
      <p className="hint pagehint">
        Agenten fragen hier vor Werkzeug-Aufrufen, die keine Regel in <code>approvals</code> (<code>.ade/project.yaml</code>) erlaubt,
        und vor Netzwerk-Zielen außerhalb der Allowlist (<code>egress.on_blocked: ask</code>). Ohne Entscheidung gilt eine Anfrage
        nach Ablauf der Zeit als abgelehnt.
      </p>
      {pending.isError && <Notice tone="bad">{errorMessage(pending.error)}</Notice>}
      {pending.isPending ? (
        <p className="muted">Lade …</p>
      ) : !pending.data?.length ? (
        <div className="empty">
          <p>Keine offenen Freigaben.</p>
        </div>
      ) : (
        <ul className="approvals">
          {pending.data.map((a) => (
            <ApprovalCard key={a.id} a={a} />
          ))}
        </ul>
      )}

      {done.length > 0 && (
        <>
          <div className="sectionhead">
            <h2>Zuletzt entschieden</h2>
          </div>
          <ul className="approvals">
            {done.map((a) => (
              <ApprovalCard key={a.id} a={a} />
            ))}
          </ul>
        </>
      )}
    </>
  );
}
