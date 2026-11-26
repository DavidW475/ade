import { SessionTable } from "../components/agents";
import { useSessions } from "../sessions";
import { Notice } from "../components/ui";
import { errorMessage } from "../format";
import { isActive } from "../api";

export function AgentsPage() {
  const sessions = useSessions({ limit: 100 });
  if (sessions.isPending) return <p className="muted">Lade Agenten …</p>;
  if (sessions.isError) return <Notice tone="bad">{errorMessage(sessions.error)}</Notice>;
  const active = sessions.data.filter((s) => isActive(s.status));
  const past = sessions.data.filter((s) => !isActive(s.status));
  return (
    <>
      <div className="pagehead">
        <h1>Agenten</h1>
      </div>
      <div className="sectionhead first">
        <h2>Laufend</h2>
      </div>
      {active.length === 0 ? (
        <div className="empty">
          <p>Kein Agent läuft.</p>
          <p className="muted">Agenten startest du in einem Projekt.</p>
        </div>
      ) : (
        <SessionTable sessions={active} />
      )}
      {past.length > 0 && (
        <>
          <div className="sectionhead">
            <h2>Beendet</h2>
          </div>
          <SessionTable sessions={past} />
        </>
      )}
    </>
  );
}
