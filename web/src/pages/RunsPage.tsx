import { Link } from "react-router";
import { runActive } from "../api";
import { RunTable } from "../components/runs";
import { useRuns } from "../runs";
import { Notice } from "../components/ui";
import { errorMessage } from "../format";

/** Alle Runs der Software-Fabriken. */
export function RunsPage() {
  const runs = useRuns({ limit: 100 });
  if (runs.isPending) return <p className="muted">Lade Runs …</p>;
  if (runs.isError) return <Notice tone="bad">{errorMessage(runs.error)}</Notice>;
  const active = runs.data.filter((r) => runActive(r.status));
  const past = runs.data.filter((r) => !runActive(r.status));
  return (
    <>
      <div className="pagehead">
        <h1>Fabrik-Runs</h1>
      </div>
      <p className="hint pagehint">
        Eine Fabrik verkettet Rollen-Agenten zu einer Pipeline (z. B. Planer → Freigabe → Entwickler → Reviewer → Freigabe → PR).
        Runs startest du in einem <Link to="/">Projekt</Link> oder mit <code>ade run feature --task "…"</code>.
      </p>
      <div className="sectionhead first">
        <h2>Laufend</h2>
      </div>
      {active.length === 0 ? (
        <div className="empty">
          <p>Kein Run läuft.</p>
        </div>
      ) : (
        <RunTable runs={active} showProject />
      )}
      {past.length > 0 && (
        <>
          <div className="sectionhead">
            <h2>Beendet</h2>
          </div>
          <RunTable runs={past} showProject />
        </>
      )}
    </>
  );
}
