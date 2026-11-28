import { SessionTable } from "../components/agents";
import { useSessions } from "../sessions";
import { Notice } from "../components/ui";
import { errorMessage } from "../format";
import { isActive, type Session } from "../api";

export function AgentsPage() {
  const sessions = useSessions({ limit: 100 });
  if (sessions.isPending) return <p className="muted">Loading agents …</p>;
  if (sessions.isError) return <Notice tone="bad">{errorMessage(sessions.error)}</Notice>;
  // Paused agents wait for a budget approval and belong to the running ones.
  const current = (st: Session["status"]) => isActive(st) || st === "paused";
  const active = sessions.data.filter((s) => current(s.status));
  const past = sessions.data.filter((s) => !current(s.status));
  return (
    <>
      <div className="pagehead">
        <h1>Agents</h1>
      </div>
      <div className="sectionhead first">
        <h2>Running</h2>
      </div>
      {active.length === 0 ? (
        <div className="empty">
          <p>No agent is running.</p>
          <p className="muted">You start agents in a project.</p>
        </div>
      ) : (
        <SessionTable sessions={active} />
      )}
      {past.length > 0 && (
        <>
          <div className="sectionhead">
            <h2>Ended</h2>
          </div>
          <SessionTable sessions={past} />
        </>
      )}
    </>
  );
}
