import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useState } from "react";
import { Link, useParams } from "react-router";
import { api, runActive, type Run, type RunStep } from "../api";
import { ApprovalCard } from "../components/approvals";
import { ArtifactView, Pipeline, RunStatusBadge, StepStatusBadge } from "../components/runs";
import { runTitle } from "../runs";
import { SandboxBadge } from "../components/sandbox";
import { Notice } from "../components/ui";
import { errorMessage, formatDuration, formatTime, formatUSD } from "../format";

/** Zeitlimit kompakt: 2 h, 1 h 30 min, 45 min. */
function formatHours(ms: number): string {
  const min = Math.round(ms / 60_000);
  if (min < 60) return formatDuration(ms);
  return min % 60 ? `${Math.floor(min / 60)} h ${min % 60} min` : `${min / 60} h`;
}
import { usePendingApprovals } from "../hooks/usePendingApprovals";

export function RunPage() {
  const { id = "" } = useParams();
  const run = useQuery({
    queryKey: ["run", id],
    queryFn: () => api.run(id),
    refetchInterval: (q) => (q.state.data && runActive(q.state.data.status) ? 2000 : false),
  });
  const project = useQuery({
    queryKey: ["project", run.data?.project_id],
    queryFn: () => api.project(run.data?.project_id ?? ""),
    enabled: !!run.data,
  });
  const [selected, setSelected] = useState<number | undefined>(undefined);

  if (run.isPending) return <p className="muted">Lade Run …</p>;
  if (run.isError) return <Notice tone="bad">{errorMessage(run.error)}</Notice>;
  const r = run.data;
  const idx = selected ?? Math.min(r.current, r.steps.length - 1);
  const step = r.steps[idx];

  return (
    <>
      <div className="crumbs">
        <Link to="/runs">Runs</Link> / {project.data ? <Link to={`/projects/${r.project_id}`}>{project.data.name}</Link> : r.project_id} /{" "}
        <span className="mono">{r.id}</span>
      </div>
      <div className="pagehead">
        <h1 className="sessiontitle">{runTitle(r)}</h1>
        <RunStatusBadge status={r.status} />
        {runActive(r.status) && <CancelRun run={r} />}
      </div>

      {r.error && (
        <Notice tone={r.status === "canceled" ? "info" : "bad"} title={r.status === "canceled" ? "Abgebrochen" : "Fehlgeschlagen"}>
          {r.error}
        </Notice>
      )}
      {r.status === "done" && r.pr_url && (
        <Notice tone="info" title="Fertig">
          Pull Request:{" "}
          <a href={r.pr_url} target="_blank" rel="noreferrer">
            {r.pr_url}
          </a>
        </Notice>
      )}

      <dl className="facts panel">
        <dt>Fabrik</dt>
        <dd>
          <code>{r.factory}</code>{" "}
          <span className="muted">{r.source === "builtin" ? "(mitgelieferte Vorlage)" : "(.ade/factories im Projekt)"}</span>
        </dd>
        <dt>Workspace</dt>
        <dd>
          {r.workspace_gone ? (
            <span className="muted">verworfen</span>
          ) : r.workspace_id ? (
            <>
              <Link to={`/workspaces/${r.workspace_id}`} className="mono">
                {r.workspace_id}
              </Link>{" "}
              <SandboxBadge workspace={r} />
            </>
          ) : (
            "–"
          )}
        </dd>
        <dt>Kosten</dt>
        <dd>
          {formatUSD(r.cost_usd)}
          {r.budget_usd ? <span className="muted"> von {formatUSD(r.budget_usd)} (budget.max_cost_usd)</span> : null}
          {r.max_duration_ns ? <span className="muted"> · Arbeitszeit höchstens {formatHours(r.max_duration_ns / 1e6)}</span> : null}
        </dd>
        <dt>Gestartet</dt>
        <dd>
          {formatTime(r.created_at)}
          {r.ended_at && <span className="muted"> · beendet {formatTime(r.ended_at)}</span>}
        </dd>
        {r.pr_url && (
          <>
            <dt>Pull Request</dt>
            <dd>
              <a href={r.pr_url} target="_blank" rel="noreferrer">
                {r.pr_url}
              </a>
            </dd>
          </>
        )}
      </dl>

      <div className="sectionhead">
        <h2>Ablauf</h2>
      </div>
      <Pipeline run={r} selected={idx} onSelect={setSelected} />
      {step && <StepDetail run={r} step={step} />}

      {r.task && (
        <details className="panel taskdetails">
          <summary>Aufgabe</summary>
          <pre className="artifact">{r.task}</pre>
        </details>
      )}

      <div className="sectionhead">
        <h2>Artefakte</h2>
      </div>
      {r.artifacts.length === 0 ? (
        <div className="empty">
          <p className="muted">Noch keine. Die letzte Antwort einer Rolle wird ihr Artefakt (z. B. plan.md) und geht an die folgenden Rollen.</p>
        </div>
      ) : (
        r.artifacts.map((a) => (
          <details key={a.name} className="panel artifactbox" open={r.artifacts.length === 1}>
            <summary>
              <code>{a.name}</code> <span className="muted">von {a.role} (Schritt {a.step})</span>
            </summary>
            <ArtifactView runId={r.id} name={a.name} />
          </details>
        ))
      )}
    </>
  );
}

function StepDetail({ run, step }: { run: Run; step: RunStep }) {
  const pending = usePendingApprovals();
  const gate = step.approval_id ? pending.data?.find((a) => a.id === step.approval_id) : undefined;
  return (
    <div className="panel stepdetail">
      <div className="cardtitle">
        <strong>{step.name}</strong>
        <StepStatusBadge status={step.status} />
        {step.started_at && (
          <span className="muted">
            {formatTime(step.started_at)}
            {step.ended_at && ` – ${formatTime(step.ended_at, true)}`}
          </span>
        )}
      </div>
      {step.kind === "role" && (
        <p className="hint">
          Rolle <code>{step.role}</code>
          {step.read_only ? " mit Lesezugriff (kann den Workspace nicht ändern)" : " mit Schreibzugriff"}
          {step.output && (
            <>
              {" "}
              · Ausgabe <code>{step.output}</code>
            </>
          )}
          {step.session_id && (
            <>
              {" "}
              · <Link to={`/sessions/${step.session_id}`}>Session ansehen</Link>
            </>
          )}
        </p>
      )}
      {step.kind === "gate" && (
        <>
          {gate ? (
            <ul className="approvals">
              <ApprovalCard a={gate} showContext={false} />
            </ul>
          ) : step.status === "pending" ? (
            <p className="hint">Hier wartet der Run auf deine Freigabe im Dashboard.</p>
          ) : step.status === "done" ? (
            <p className="hint">Freigegeben.</p>
          ) : null}
          {gate && (
            <p className="hint">
              Freigeben setzt den Run fort; ein Hinweis im Begründungsfeld geht an die nächste Rolle. Ablehnen bricht den Run ab.
            </p>
          )}
        </>
      )}
      {step.kind === "action" && (
        <p className="hint">
          Pusht den Branch und erstellt einen Pull Request (wie „PR erstellen“ im Workspace).{" "}
          {run.pr_url && (
            <a href={run.pr_url} target="_blank" rel="noreferrer">
              {run.pr_url}
            </a>
          )}
        </p>
      )}
      {step.kind === "decision" && <p className="hint">Entscheidung: springt zurück, wenn die Bedingung wahr ist.</p>}
      {step.iteration > 1 && <p className="hint">Durchlauf {step.iteration}</p>}
      {step.error && (
        <Notice tone={step.status === "canceled" || step.status === "skipped" || step.status === "looped" ? "info" : step.status === "running" ? "warn" : "bad"}>
          {step.error}
        </Notice>
      )}
    </div>
  );
}

function CancelRun({ run }: { run: Run }) {
  const qc = useQueryClient();
  const cancel = useMutation({
    mutationFn: () => api.cancelRun(run.id),
    onSettled: () => {
      void qc.invalidateQueries({ queryKey: ["run", run.id] });
      void qc.invalidateQueries({ queryKey: ["runs"] });
      void qc.invalidateQueries({ queryKey: ["approvals"] });
    },
  });
  return (
    <div>
      <button
        type="button"
        className="danger"
        disabled={cancel.isPending}
        onClick={() => window.confirm("Run abbrechen? Die laufende Session wird gestoppt; der Workspace bleibt erhalten.") && cancel.mutate()}
      >
        {cancel.isPending ? "Breche ab …" : "Abbrechen"}
      </button>
      {cancel.isError && <Notice tone="bad">{errorMessage(cancel.error)}</Notice>}
    </div>
  );
}
