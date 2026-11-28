import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useState } from "react";
import { Link, useNavigate } from "react-router";
import { api, type Run, type RunStatus, type RunStep, type StepPlan, type StepStatus } from "../api";
import { errorMessage, formatTime, formatUSD } from "../format";
import { runTitle } from "../runs";
import { sandboxDefault, useSandboxInfo } from "../sandbox";
import { SandboxToggle } from "./sandbox";
import { Badge, Notice } from "./ui";

type Tone = "ok" | "warn" | "bad" | "busy" | "neutral";

const runLabels: Record<RunStatus, [string, Tone]> = {
  running: ["running", "busy"],
  waiting: ["waiting for approval", "warn"],
  paused: ["paused (budget)", "warn"],
  done: ["done", "ok"],
  failed: ["failed", "bad"],
  canceled: ["canceled", "neutral"],
};

export function RunStatusBadge({ status }: { status: RunStatus }) {
  const [label, tone] = runLabels[status];
  return <Badge tone={tone}>{label}</Badge>;
}

const stepLabels: Record<StepStatus, [string, Tone]> = {
  pending: ["pending", "neutral"],
  running: ["running", "busy"],
  waiting: ["waiting", "warn"],
  done: ["done", "ok"],
  failed: ["failed", "bad"],
  canceled: ["canceled", "neutral"],
  skipped: ["skipped", "neutral"],
  looped: ["jumped back", "warn"],
};

export function StepStatusBadge({ status }: { status: StepStatus }) {
  const [label, tone] = stepLabels[status];
  return <Badge tone={tone}>{label}</Badge>;
}

function stepKindLabel(st: Pick<RunStep, "kind" | "role" | "read_only">): string {
  if (st.kind === "gate") return "approval";
  if (st.kind === "action") return "pull request";
  if (st.kind === "decision") return "decision";
  return `${st.role ?? "role"}${st.read_only ? " · read-only" : ""}`;
}

/** Current step of a running run, short. */
function currentStep(r: Run): string {
  const st = r.steps[r.current];
  return st ? `${st.name} (${stepKindLabel(st)})` : "";
}

/** Positions of the flow in units: one step or a parallel group. */
function units(steps: RunStep[]): RunStep[][] {
  const out: RunStep[][] = [];
  for (const st of steps) {
    const last = out[out.length - 1];
    if (st.group !== undefined && last && last[0]?.group === st.group) last.push(st);
    else out.push([st]);
  }
  return out;
}

function kindIcon(st: RunStep): string {
  return st.kind === "gate" ? "◆" : st.kind === "action" ? "⇪" : st.kind === "decision" ? "↺" : st.read_only ? "◎" : "●";
}

/** Condition and jump back of a step, short (for pipeline and start dialog). */
export function PlanNote({ step, plan }: { step: Pick<RunStep, "kind">; plan?: StepPlan }) {
  if (!plan) return null;
  return (
    <>
      {plan.when && step.kind !== "decision" && (
        <span className="pcond" title="Condition: otherwise skipped">
          if <code>{plan.when}</code>
        </span>
      )}
      {plan.goto && step.kind === "decision" && (
        <span className="pcond loop" title={`at most ${plan.max_iterations ?? "?"} passes`}>
          ↺ back to <strong>{plan.goto}</strong>
          {plan.when && (
            <>
              , if <code>{plan.when}</code>
            </>
          )}
        </span>
      )}
      {plan.goto && step.kind === "gate" && (
        <span className="pcond loop" title={`at most ${plan.max_iterations ?? "?"} passes`}>
          Deny ↺ back to <strong>{plan.goto}</strong>
        </span>
      )}
    </>
  );
}

function PipelineStep({ run, st, selected, onSelect }: { run: Run; st: RunStep; selected?: number; onSelect?: (i: number) => void }) {
  const current = st.index === run.current && (st.status === "running" || st.status === "waiting");
  const running = st.group !== undefined && st.status === "running";
  const plan = run.plan?.[st.index];
  return (
    <div className={`pstep ${st.status} ${current || running ? "current" : ""} ${selected === st.index ? "selected" : ""}`}>
      <button type="button" onClick={() => onSelect?.(st.index)} aria-current={current ? "step" : undefined} aria-pressed={selected === st.index}>
        <span className="pkind">
          {kindIcon(st)} {st.index + 1}
        </span>
        <span className="pname">
          {st.name}
          {st.iteration > 1 && (
            <span className="piter" title={`pass ${st.iteration}`}>
              {" "}
              ×{st.iteration}
            </span>
          )}
        </span>
        <span className="prole">{stepKindLabel(st)}</span>
        <PlanNote step={st} plan={plan} />
        <StepStatusBadge status={st.status} />
      </button>
    </div>
  );
}

/** Pipeline view: steps in order, parallel groups side by side, conditions and jumps back. */
export function Pipeline({ run, selected, onSelect }: { run: Run; selected?: number; onSelect?: (i: number) => void }) {
  return (
    <ol className="pipeline" aria-label="Steps">
      {units(run.steps).map((u) => {
        const [first] = u;
        if (!first) return null;
        return first.group === undefined ? (
          <li key={first.index} className="punit">
            <PipelineStep run={run} st={first} selected={selected} onSelect={onSelect} />
          </li>
        ) : (
          <li key={first.index} className="punit pgroup" aria-label="parallel group">
            <span className="pgrouplabel">parallel</span>
            <div className="pgroupmembers">
              {u.map((st) => (
                <PipelineStep key={st.index} run={run} st={st} selected={selected} onSelect={onSelect} />
              ))}
            </div>
          </li>
        );
      })}
    </ol>
  );
}

/** Trigger of a run: name and reference (issue link or date). */
export function RunTrigger({ run }: { run: Pick<Run, "trigger" | "trigger_ref" | "closes"> }) {
  if (!run.trigger) return null;
  const ref = run.trigger_ref ?? "";
  return (
    <span className="runtrigger">
      ⚡ <code>{run.trigger}</code>
      {ref.startsWith("https://") ? (
        <>
          {" "}
          <a href={ref} target="_blank" rel="noreferrer">
            {run.closes ? `Issue #${run.closes}` : ref}
          </a>
        </>
      ) : ref ? (
        <span className="muted"> {ref}</span>
      ) : null}
    </span>
  );
}

export function RunTable({ runs, showProject = false }: { runs: Run[]; showProject?: boolean }) {
  return (
    <div className="tablewrap">
      <table>
        <thead>
          <tr>
            <th>Run</th>
            <th>Factory</th>
            <th>Status</th>
            <th>Step</th>
            {showProject && <th>Project</th>}
            <th>Costs</th>
            <th>Started</th>
          </tr>
        </thead>
        <tbody>
          {runs.map((r) => (
            <tr key={r.id}>
              <td className="wrap">
                <Link to={`/runs/${r.id}`}>{runTitle(r)}</Link>
                {r.trigger && (
                  <div className="hint">
                    <RunTrigger run={r} />
                  </div>
                )}
              </td>
              <td className="mono">{r.factory}</td>
              <td>
                <RunStatusBadge status={r.status} />
              </td>
              <td className="muted">
                {r.status === "done" ? `${r.steps.length}/${r.steps.length}` : `${Math.min(r.current + 1, r.steps.length)}/${r.steps.length} ${currentStep(r)}`}
              </td>
              {showProject && (
                <td>
                  <Link to={`/projects/${r.project_id}`} className="mono">
                    {r.project_id}
                  </Link>
                </td>
              )}
              <td className="muted">
                {formatUSD(r.cost_usd)}
                {r.budget_usd ? ` / ${formatUSD(r.budget_usd)}` : ""}
              </td>
              <td className="muted">{formatTime(r.created_at)}</td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}

export function StartRunForm({ projectId, onCancel }: { projectId: string; onCancel?: () => void }) {
  const qc = useQueryClient();
  const navigate = useNavigate();
  const factories = useQuery({ queryKey: ["factories", projectId], queryFn: () => api.factories(projectId) });
  const [name, setName] = useState("");
  const [task, setTask] = useState("");
  const [inputs, setInputs] = useState<Record<string, string>>({});
  const sandboxInfo = useSandboxInfo();
  const [sandboxChoice, setSandbox] = useState<boolean | null>(null);
  const sandbox = sandboxChoice ?? sandboxDefault(sandboxInfo.data);

  const usable = (factories.data ?? []).filter((f) => !f.error);
  const broken = (factories.data ?? []).filter((f) => f.error);
  const chosen = usable.find((f) => f.name === name) ?? usable.find((f) => f.name === "feature") ?? usable[0];
  // Further inputs of the factory besides task (e.g. branch for review-only).
  const extra = Object.entries(chosen?.inputs ?? {})
    .filter(([k]) => k !== "task")
    .sort(([a], [b]) => a.localeCompare(b));
  const missing = extra.some(([k, def]) => def.required && !inputs[k]?.trim());

  const start = useMutation({
    mutationFn: () =>
      api.startRun({
        project_id: projectId,
        factory: chosen?.name ?? "",
        task,
        inputs: Object.fromEntries(extra.map(([k]): [string, string] => [k, inputs[k] ?? ""]).filter(([, v]) => v.trim() !== "")),
        sandbox: sandbox && !!sandboxInfo.data?.available,
      }),
    onSuccess: (r) => {
      void qc.invalidateQueries({ queryKey: ["runs"] });
      void qc.invalidateQueries({ queryKey: ["workspaces", projectId] });
      void navigate(`/runs/${r.id}`);
    },
  });
  const submit = (e: { preventDefault(): void }) => {
    e.preventDefault();
    if (chosen && task.trim() && !missing && !start.isPending) start.mutate();
  };

  return (
    <form className="panel startform" onSubmit={submit}>
      <div className="field">
        <label htmlFor="sr-factory">Factory</label>
        <select id="sr-factory" value={chosen?.name ?? ""} onChange={(e) => setName(e.target.value)}>
          {usable.map((f) => (
            <option key={f.name} value={f.name}>
              {f.name}
              {f.source === "builtin" ? " (template)" : ""}
            </option>
          ))}
        </select>
      </div>
      <div className="field wide">
        {chosen?.description && <p className="hint">{chosen.description}</p>}
        {chosen?.steps && (
          <p className="hint">
            {chosen.steps.map((s) => (s.kind === "gate" ? `◆ ${s.name}` : s.kind === "action" ? "⇪ PR" : s.kind === "decision" ? `↺ ${s.name}` : `${s.role}${s.read_only ? " (read-only)" : ""}`)).join(" → ")}
          </p>
        )}
        {broken.map((f) => (
          <Notice key={f.name} tone="warn" title={`Factory ${f.name} is invalid`}>
            <code>{f.error}</code>
          </Notice>
        ))}
      </div>
      <div className="field wide">
        <label htmlFor="sr-task">Task</label>
        <textarea
          id="sr-task"
          rows={5}
          required
          placeholder="What should the factory build?"
          value={task}
          onChange={(e) => setTask(e.target.value)}
          onKeyDown={(e) => {
            if (e.key === "Enter" && (e.ctrlKey || e.metaKey)) submit(e);
          }}
        />
      </div>
      {extra.map(([k, def]) => (
        <div className="field wide" key={`${chosen?.name}-${k}`}>
          <label htmlFor={`sr-in-${k}`}>
            {k}
            {def.type === "branch" ? " (branch)" : ""}
            {def.required ? "" : " – optional"}
          </label>
          <input
            id={`sr-in-${k}`}
            className={def.type === "branch" ? "mono" : undefined}
            required={def.required}
            placeholder={def.description ?? (def.type === "branch" ? "default: base branch of the project" : "")}
            value={inputs[k] ?? ""}
            onChange={(e) => setInputs({ ...inputs, [k]: e.target.value })}
          />
        </div>
      ))}
      <div className="field wide">
        <SandboxToggle id="sr-sandbox" checked={sandbox} onChange={setSandbox} info={sandboxInfo.data} />
        <p className="hint">
          All roles work in the same new workspace (one after another or, for parallel steps, at the same time). Read-only roles (planner, reviewer) only see it read-only: in the sandbox
          in their own container with <code>/workspace</code> mounted read-only, without a sandbox in a copy whose changes
          are discarded.
        </p>
      </div>
      {factories.isError && (
        <div className="wide">
          <Notice tone="bad">{errorMessage(factories.error)}</Notice>
        </div>
      )}
      {start.isError && (
        <div className="wide">
          <Notice tone="bad">{errorMessage(start.error)}</Notice>
        </div>
      )}
      <div className="actions wide">
        {onCancel && (
          <button type="button" onClick={onCancel}>
            Cancel
          </button>
        )}
        <button type="submit" className="primary" disabled={!chosen || !task.trim() || missing || start.isPending}>
          {start.isPending ? "Starting …" : "Start run"}
        </button>
      </div>
    </form>
  );
}

export function StartRun({ projectId, disabled }: { projectId: string; disabled?: boolean }) {
  const [open, setOpen] = useState(false);
  if (!open)
    return (
      <button type="button" className="primary" disabled={disabled} onClick={() => setOpen(true)}>
        Start run
      </button>
    );
  return (
    <div className="startwrap">
      <StartRunForm projectId={projectId} onCancel={() => setOpen(false)} />
    </div>
  );
}

/** Formats JSON indented; null if the text is not JSON. */
function prettyJSON(text: string): string | null {
  const t = text.trim();
  if (!t.startsWith("{") && !t.startsWith("[")) return null;
  try {
    return JSON.stringify(JSON.parse(t), null, 2);
  } catch {
    return null;
  }
}

/** Content of an artifact as text (Markdown is not rendered: the content comes from the agent); JSON indented. */
export function ArtifactView({ runId, name, iteration }: { runId: string; name: string; iteration?: number }) {
  const q = useQuery({ queryKey: ["artifact", runId, name, iteration ?? 0], queryFn: () => api.artifact(runId, name, iteration) });
  if (q.isPending) return <p className="muted">Loading {name} …</p>;
  if (q.isError) return <Notice tone="bad">{errorMessage(q.error)}</Notice>;
  const json = prettyJSON(q.data);
  if (json === null && name.endsWith(".json")) {
    return (
      <>
        <Notice tone="warn">Not valid JSON – shown raw.</Notice>
        <pre className="artifact">{q.data}</pre>
      </>
    );
  }
  return <pre className={`artifact ${json !== null ? "json" : ""}`}>{json ?? q.data}</pre>;
}

/** Artifact with a choice of the pass (latest version preselected). */
export function ArtifactVersions({ runId, artifact }: { runId: string; artifact: Run["artifacts"][number] }) {
  const its = artifact.iterations ?? [];
  const [it, setIt] = useState<number | undefined>(undefined);
  return (
    <>
      {its.length > 1 && (
        <div className="itertabs" role="group" aria-label="Pass">
          {its.map((n) => (
            <button key={n} type="button" className={(it ?? its[its.length - 1]) === n ? "active" : ""} onClick={() => setIt(n)}>
              Pass {n}
            </button>
          ))}
        </div>
      )}
      <ArtifactView runId={runId} name={artifact.name} iteration={it} />
    </>
  );
}
