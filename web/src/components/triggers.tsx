import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Link } from "react-router";
import { api, type TriggerView } from "../api";
import { errorMessage, formatTime } from "../format";
import { Badge, Notice } from "./ui";

/** Auslöser eines Projekts (triggers in .ade/project.yaml) mit Zustand. */
export function TriggerList({ projectId }: { projectId: string }) {
  const q = useQuery({
    queryKey: ["triggers", projectId],
    queryFn: () => api.triggers(projectId),
    refetchInterval: 15_000,
  });
  if (q.isPending) return <p className="muted">Lade Auslöser …</p>;
  if (q.isError) return <Notice tone="bad">{errorMessage(q.error)}</Notice>;
  if (q.data.length === 0)
    return (
      <div className="empty">
        <p>Keine Auslöser.</p>
        <p className="muted">
          Unter <code>triggers</code> in <code>.ade/project.yaml</code> starten Runs automatisch – je GitHub-Issue mit Label oder nach Zeitplan.
        </p>
      </div>
    );
  return (
    <div className="tablewrap">
      <table>
        <thead>
          <tr>
            <th>Auslöser</th>
            <th>Fabrik</th>
            <th>Status</th>
            <th>Letzter Lauf</th>
            <th>Nächster</th>
            <th />
          </tr>
        </thead>
        <tbody>
          {q.data.map((t) => (
            <TriggerRow key={t.name} projectId={projectId} t={t} />
          ))}
        </tbody>
      </table>
    </div>
  );
}

/** Abfrage-Intervall kompakt: 5 min, 1 h, 1 h 30 min. */
function formatInterval(ns: number): string {
  const min = Math.round(ns / 60e9);
  if (min < 60) return `${min} min`;
  return min % 60 ? `${Math.floor(min / 60)} h ${min % 60} min` : `${min / 60} h`;
}

function TriggerSource({ t }: { t: TriggerView }) {
  if (t.github_issues) {
    const g = t.github_issues;
    return (
      <>
        GitHub-Issues mit Label <code>{g.label}</code>
        {g.authors?.length ? <> von {g.authors.join(", ")}</> : null}
        <span className="muted"> · alle {formatInterval(g.interval_ns ?? 5 * 60e9)}</span>
      </>
    );
  }
  return (
    <>
      Zeitplan <code>{t.schedule?.cron}</code>
      {t.schedule?.catch_up === false && <span className="muted"> · ohne Nachholen</span>}
    </>
  );
}

function TriggerRow({ projectId, t }: { projectId: string; t: TriggerView }) {
  const qc = useQueryClient();
  const toggle = useMutation({
    mutationFn: () => api.pauseTrigger(projectId, t.name, !t.state.paused),
    onSettled: () => void qc.invalidateQueries({ queryKey: ["triggers", projectId] }),
  });
  const last = t.runs[0];
  return (
    <tr>
      <td className="wrap">
        <strong>{t.name}</strong>
        <div className="hint">
          <TriggerSource t={t} />
          {t.sandbox === false && <span className="muted"> · ohne Sandbox</span>}
        </div>
        {t.state.last_error && <p className="triggererror">Letzter Fehler: {t.state.last_error}</p>}
      </td>
      <td className="mono">{t.factory}</td>
      <td>{t.state.paused ? <Badge tone="warn">pausiert</Badge> : t.state.last_error ? <Badge tone="bad">Fehler</Badge> : <Badge tone="ok">aktiv</Badge>}</td>
      <td className="muted">
        {last ? (
          <>
            <Link to={`/runs/${last.run_id}`}>{formatTime(last.created_at)}</Link>
            {t.kind === "github_issues" && <> · Issue #{last.key}</>}
            {t.runs.length > 1 && <> · {t.runs.length} Runs</>}
          </>
        ) : t.state.last_check ? (
          `kein Run · geprüft ${formatTime(t.state.last_check)}`
        ) : (
          "–"
        )}
      </td>
      <td className="muted">{t.state.paused ? "–" : t.next ? formatTime(t.next) : "–"}</td>
      <td>
        <button type="button" disabled={toggle.isPending} onClick={() => toggle.mutate()}>
          {t.state.paused ? "Fortsetzen" : "Pausieren"}
        </button>
        {toggle.isError && <Notice tone="bad">{errorMessage(toggle.error)}</Notice>}
      </td>
    </tr>
  );
}
