import { useQuery } from "@tanstack/react-query";
import { api } from "../api";
import { ApprovalCard, NotificationToggle } from "../components/approvals";
import { usePendingApprovals } from "../hooks/usePendingApprovals";
import { Notice } from "../components/ui";
import { errorMessage } from "../format";

/** Queue of all pending approvals and the latest decisions. */
export function ApprovalsPage() {
  const pending = usePendingApprovals();
  const recent = useQuery({ queryKey: ["approvals", "recent"], queryFn: () => api.approvals({ limit: 50 }), refetchInterval: 5000 });
  const done = (recent.data ?? []).filter((a) => a.status !== "pending");

  return (
    <>
      <div className="pagehead">
        <h1>Approvals</h1>
        <div className="actions">
          <NotificationToggle />
        </div>
      </div>
      <p className="hint pagehint">
        Agents ask here before tool calls that no rule in <code>approvals</code> (<code>.ade/project.yaml</code>) allows,
        and before network targets outside the allowlist (<code>egress.on_blocked: ask</code>). Without a decision a request
        counts as denied once the time has expired.
      </p>
      {pending.isError && <Notice tone="bad">{errorMessage(pending.error)}</Notice>}
      {pending.isPending ? (
        <p className="muted">Loading …</p>
      ) : !pending.data?.length ? (
        <div className="empty">
          <p>No pending approvals.</p>
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
            <h2>Recently decided</h2>
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
