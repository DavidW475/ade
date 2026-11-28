import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Link, useParams, useSearchParams } from "react-router";
import { api, runActive, type Run, type RunStep } from "../api";
import { ApprovalCard } from "../components/approvals";
import { ArtifactVersions, ArtifactView, PlanNote, Pipeline, RunStatusBadge, RunTrigger, StepStatusBadge } from "../components/runs";
import { runTitle } from "../runs";
import { SandboxBadge } from "../components/sandbox";
import { Notice } from "../components/ui";
import { errorMessage, formatDuration, formatTime, formatUSD } from "../format";

/** Compact time limit: 2 h, 1 h 30 min, 45 min. */
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
  // Selected step in the URL (?step=<name>), so it can be linked.
  const [params, setParams] = useSearchParams();

  if (run.isPending) return <p className="muted">Loading run …</p>;
  if (run.isError) return <Notice tone="bad">{errorMessage(run.error)}</Notice>;
  const r = run.data;
  const named = r.steps.find((s) => s.name === params.get("step"));
  const idx = named?.index ?? Math.min(r.current, r.steps.length - 1);
  const setSelected = (i: number) => {
    const name = r.steps[i]?.name;
    if (name) setParams({ step: name }, { replace: true });
  };
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
        <Notice tone={r.status === "canceled" ? "info" : "bad"} title={r.status === "canceled" ? "Canceled" : "Failed"}>
          {r.error}
        </Notice>
      )}
      {r.status === "done" && r.pr_url && (
        <Notice tone="info" title="Done">
          Pull request:{" "}
          <a href={r.pr_url} target="_blank" rel="noreferrer">
            {r.pr_url}
          </a>
        </Notice>
      )}

      <dl className="facts panel">
        <dt>Factory</dt>
        <dd>
          <code>{r.factory}</code>{" "}
          <span className="muted">{r.source === "builtin" ? "(built-in template)" : "(.ade/factories in the project)"}</span>
        </dd>
        {r.trigger && (
          <>
            <dt>Triggered by</dt>
            <dd>
              <RunTrigger run={r} />
              {r.closes ? <span className="muted"> · the pull request closes the issue</span> : null}
            </dd>
          </>
        )}
        <dt>Workspace</dt>
        <dd>
          {r.workspace_gone ? (
            <span className="muted">discarded</span>
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
        <dt>Costs</dt>
        <dd>
          {formatUSD(r.cost_usd)}
          {r.budget_usd ? <span className="muted"> of {formatUSD(r.budget_usd)} (budget.max_cost_usd)</span> : null}
          {r.max_duration_ns ? <span className="muted"> · working time at most {formatHours(r.max_duration_ns / 1e6)}</span> : null}
        </dd>
        <dt>Started</dt>
        <dd>
          {formatTime(r.created_at)}
          {r.ended_at && <span className="muted"> · ended {formatTime(r.ended_at)}</span>}
        </dd>
        {r.pr_url && (
          <>
            <dt>Pull request</dt>
            <dd>
              <a href={r.pr_url} target="_blank" rel="noreferrer">
                {r.pr_url}
              </a>
            </dd>
          </>
        )}
      </dl>

      <div className="sectionhead">
        <h2>Flow</h2>
      </div>
      <Pipeline run={r} selected={idx} onSelect={setSelected} />
      {step && (
        <StepDetail
          run={r}
          step={step}
          iteration={Number(params.get("iteration")) || undefined}
          onIteration={(n) => setParams({ step: step.name, iteration: String(n) }, { replace: true })}
        />
      )}

      {r.task && (
        <details className="panel taskdetails">
          <summary>Task</summary>
          <pre className="artifact">{r.task}</pre>
        </details>
      )}

      <div className="sectionhead">
        <h2>Artifacts</h2>
      </div>
      {r.artifacts.length === 0 ? (
        <div className="empty">
          <p className="muted">None yet. The last answer of a role becomes its artifact (e.g. plan.md) and goes to the following roles.</p>
        </div>
      ) : (
        r.artifacts.map((a) => (
          <details key={a.name} className="panel artifactbox" open={r.artifacts.length === 1}>
            <summary>
              <code>{a.name}</code> <span className="muted">by {a.role} (step {a.step})</span>
              {(a.iterations?.length ?? 0) > 1 && <span className="muted"> · {a.iterations?.length} passes</span>}
            </summary>
            <ArtifactVersions runId={r.id} artifact={a} />
          </details>
        ))
      )}
    </>
  );
}

/** All executions of a position (earlier passes from history, then the latest). */
function executions(run: Run, index: number): RunStep[] {
  const all = [...(run.history ?? []).filter((h) => h.index === index), ...run.steps.filter((s) => s.index === index)];
  return all.sort((a, b) => a.iteration - b.iteration || a.row - b.row);
}

function StepDetail({ run, step, iteration, onIteration }: { run: Run; step: RunStep; iteration?: number; onIteration: (n: number) => void }) {
  const execs = executions(run, step.index);
  // Selected pass (?iteration=), otherwise the latest.
  const ex = execs.filter((e) => e.iteration === iteration).pop() ?? step;
  return (
    <div className="panel stepdetail">
      <div className="cardtitle">
        <strong>{step.name}</strong>
        {step.group !== undefined && <span className="badge neutral">parallel</span>}
        <PlanNote step={step} plan={run.plan?.[step.index]} />
      </div>
      {execs.length > 1 && (
        <div className="itertabs" role="group" aria-label="Pass">
          {execs.map((e) => (
            <button key={e.row} type="button" className={e.row === ex.row ? "active" : ""} onClick={() => onIteration(e.iteration)}>
              Pass {e.iteration} <StepStatusBadge status={e.status} />
            </button>
          ))}
        </div>
      )}
      <Execution run={run} step={ex} latest={ex.row === step.row} />
    </div>
  );
}

/** One execution of a step: status, session, artifact of this pass, error. */
function Execution({ run, step, latest }: { run: Run; step: RunStep; latest: boolean }) {
  const pending = usePendingApprovals();
  const gate = step.approval_id ? pending.data?.find((a) => a.id === step.approval_id) : undefined;
  const plan = run.plan?.[step.index];
  const artifact = step.output ? run.artifacts.find((a) => a.name === step.output) : undefined;
  const hasVersion = !!artifact && step.status === "done" && (artifact.iterations?.includes(step.iteration) ?? latest);
  const validating = step.kind === "role" && (step.fixes ?? 0) > 0;
  return (
    <>
      <p className="hint">
        <StepStatusBadge status={step.status} /> Pass {step.iteration}
        {plan?.goto && plan.max_iterations ? ` of at most ${plan.max_iterations}` : ""}
        {step.started_at && (
          <>
            {" "}
            · {formatTime(step.started_at)}
            {step.ended_at && ` – ${formatTime(step.ended_at, true)}`}
          </>
        )}
      </p>
      {step.kind === "role" && (
        <p className="hint">
          Role <code>{step.role}</code>
          {step.read_only ? " with read access (cannot change the workspace)" : " with write access"}
          {step.output && (
            <>
              {" "}
              · output <code>{step.output}</code>
            </>
          )}
          {step.session_id && (
            <>
              {" "}
              · <Link to={`/sessions/${step.session_id}`}>View session</Link>
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
            <p className="hint">Here the run waits for your approval in the dashboard.</p>
          ) : step.status === "done" ? (
            <p className="hint">Approved.</p>
          ) : null}
        </>
      )}
      {step.kind === "action" && (
        <p className="hint">
          Pushes the branch and creates a pull request (like “Create PR” in the workspace).{" "}
          {run.pr_url && (
            <a href={run.pr_url} target="_blank" rel="noreferrer">
              {run.pr_url}
            </a>
          )}
        </p>
      )}
      {step.kind === "decision" && (
        <p className="hint">
          Decision: jumps back to <code>{plan?.goto ?? "?"}</code> if the condition is true
          {plan?.max_iterations ? ` (at most ${plan.max_iterations} passes, then ${plan.on_exhausted === "continue" ? "continue" : "error"})` : ""}.
        </p>
      )}
      {validating && step.status !== "done" && step.error ? (
        <Notice tone={step.status === "running" ? "warn" : "bad"} title={`Check errors in artifact ${step.output ?? ""} – correction ${step.fixes ?? 0} of ${MAX_FIXES}`}>
          <code>{step.error}</code>
        </Notice>
      ) : (
        step.error && (
          <Notice tone={step.status === "canceled" || step.status === "skipped" || step.status === "looped" ? "info" : step.status === "running" ? "warn" : "bad"}>
            {step.error}
          </Notice>
        )
      )}
      {validating && step.status === "done" && (
        <p className="hint">
          The artifact only matched the schema after {step.fixes === 1 ? "one correction" : `${step.fixes} corrections`}.
        </p>
      )}
      {artifact && hasVersion && (
        <details className="artifactbox" open>
          <summary>
            <code>{artifact.name}</code> <span className="muted">from pass {step.iteration}</span>
          </summary>
          <ArtifactView runId={run.id} name={artifact.name} iteration={artifact.iterations?.includes(step.iteration) ? step.iteration : undefined} />
        </details>
      )}
    </>
  );
}

/** Correction rounds for an invalid structured artifact (factory.MaxFixes). */
const MAX_FIXES = 2;

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
        onClick={() => window.confirm("Cancel the run? The running session is stopped; the workspace is kept.") && cancel.mutate()}
      >
        {cancel.isPending ? "Canceling …" : "Cancel"}
      </button>
      {cancel.isError && <Notice tone="bad">{errorMessage(cancel.error)}</Notice>}
    </div>
  );
}
