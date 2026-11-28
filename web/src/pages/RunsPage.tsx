import { Link } from "react-router";
import { runActive } from "../api";
import { RunTable } from "../components/runs";
import { useRuns } from "../runs";
import { Notice } from "../components/ui";
import { errorMessage } from "../format";

/** All runs of the software factories. */
export function RunsPage() {
  const runs = useRuns({ limit: 100 });
  if (runs.isPending) return <p className="muted">Loading runs …</p>;
  if (runs.isError) return <Notice tone="bad">{errorMessage(runs.error)}</Notice>;
  const active = runs.data.filter((r) => runActive(r.status));
  const past = runs.data.filter((r) => !runActive(r.status));
  return (
    <>
      <div className="pagehead">
        <h1>Factory runs</h1>
      </div>
      <p className="hint pagehint">
        A factory chains role agents into a pipeline (e.g. planner → approval → implementer → reviewer → approval → PR).
        You start runs in a <Link to="/">project</Link> or with <code>ade run feature --task "…"</code>.
      </p>
      <div className="sectionhead first">
        <h2>Running</h2>
      </div>
      {active.length === 0 ? (
        <div className="empty">
          <p>No run is running.</p>
        </div>
      ) : (
        <RunTable runs={active} showProject />
      )}
      {past.length > 0 && (
        <>
          <div className="sectionhead">
            <h2>Ended</h2>
          </div>
          <RunTable runs={past} showProject />
        </>
      )}
    </>
  );
}
