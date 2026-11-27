import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useState } from "react";
import { Link, useNavigate } from "react-router";
import { api, type Run, type RunStatus, type RunStep, type StepStatus } from "../api";
import { errorMessage, formatTime, formatUSD } from "../format";
import { runTitle } from "../runs";
import { sandboxDefault, useSandboxInfo } from "../sandbox";
import { SandboxToggle } from "./sandbox";
import { Badge, Notice } from "./ui";

type Tone = "ok" | "warn" | "bad" | "busy" | "neutral";

const runLabels: Record<RunStatus, [string, Tone]> = {
  running: ["läuft", "busy"],
  waiting: ["wartet auf Freigabe", "warn"],
  paused: ["pausiert (Budget)", "warn"],
  done: ["fertig", "ok"],
  failed: ["fehlgeschlagen", "bad"],
  canceled: ["abgebrochen", "neutral"],
};

export function RunStatusBadge({ status }: { status: RunStatus }) {
  const [label, tone] = runLabels[status];
  return <Badge tone={tone}>{label}</Badge>;
}

const stepLabels: Record<StepStatus, [string, Tone]> = {
  pending: ["offen", "neutral"],
  running: ["läuft", "busy"],
  waiting: ["wartet", "warn"],
  done: ["erledigt", "ok"],
  failed: ["fehlgeschlagen", "bad"],
  canceled: ["abgebrochen", "neutral"],
  skipped: ["übersprungen", "neutral"],
  looped: ["zurückgesprungen", "warn"],
};

export function StepStatusBadge({ status }: { status: StepStatus }) {
  const [label, tone] = stepLabels[status];
  return <Badge tone={tone}>{label}</Badge>;
}

function stepKindLabel(st: Pick<RunStep, "kind" | "role" | "read_only">): string {
  if (st.kind === "gate") return "Freigabe";
  if (st.kind === "action") return "Pull Request";
  if (st.kind === "decision") return "Entscheidung";
  return `${st.role ?? "Rolle"}${st.read_only ? " · lesend" : ""}`;
}

/** Aktueller Schritt eines laufenden Runs, kurz. */
function currentStep(r: Run): string {
  const st = r.steps[r.current];
  return st ? `${st.name} (${stepKindLabel(st)})` : "";
}

/** Pipeline-Ansicht: alle Schritte in Reihenfolge mit Status. */
export function Pipeline({ run, selected, onSelect }: { run: Run; selected?: number; onSelect?: (i: number) => void }) {
  return (
    <ol className="pipeline" aria-label="Schritte">
      {run.steps.map((st) => {
        const current = st.index === run.current && (st.status === "running" || st.status === "waiting");
        return (
          <li key={st.index} className={`pstep ${st.status} ${current ? "current" : ""} ${selected === st.index ? "selected" : ""}`}>
            <button type="button" onClick={() => onSelect?.(st.index)} aria-current={current ? "step" : undefined}>
              <span className="pkind">{st.kind === "gate" ? "◆" : st.kind === "action" ? "⇪" : st.kind === "decision" ? "↺" : st.read_only ? "◎" : "●"}</span>
              <span className="pname">
                {st.name}
                {st.iteration > 1 && <span className="muted"> ×{st.iteration}</span>}
              </span>
              <span className="prole">{stepKindLabel(st)}</span>
              <StepStatusBadge status={st.status} />
            </button>
          </li>
        );
      })}
    </ol>
  );
}

export function RunTable({ runs, showProject = false }: { runs: Run[]; showProject?: boolean }) {
  return (
    <div className="tablewrap">
      <table>
        <thead>
          <tr>
            <th>Run</th>
            <th>Fabrik</th>
            <th>Status</th>
            <th>Schritt</th>
            {showProject && <th>Projekt</th>}
            <th>Kosten</th>
            <th>Gestartet</th>
          </tr>
        </thead>
        <tbody>
          {runs.map((r) => (
            <tr key={r.id}>
              <td className="wrap">
                <Link to={`/runs/${r.id}`}>{runTitle(r)}</Link>
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
  // Weitere Eingaben der Fabrik neben task (z. B. branch bei review-only).
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
        <label htmlFor="sr-factory">Fabrik</label>
        <select id="sr-factory" value={chosen?.name ?? ""} onChange={(e) => setName(e.target.value)}>
          {usable.map((f) => (
            <option key={f.name} value={f.name}>
              {f.name}
              {f.source === "builtin" ? " (Vorlage)" : ""}
            </option>
          ))}
        </select>
      </div>
      <div className="field wide">
        {chosen?.description && <p className="hint">{chosen.description}</p>}
        {chosen?.steps && (
          <p className="hint">
            {chosen.steps.map((s) => (s.kind === "gate" ? `◆ ${s.name}` : s.kind === "action" ? "⇪ PR" : s.kind === "decision" ? `↺ ${s.name}` : `${s.role}${s.read_only ? " (lesend)" : ""}`)).join(" → ")}
          </p>
        )}
        {broken.map((f) => (
          <Notice key={f.name} tone="warn" title={`Fabrik ${f.name} ist ungültig`}>
            <code>{f.error}</code>
          </Notice>
        ))}
      </div>
      <div className="field wide">
        <label htmlFor="sr-task">Aufgabe</label>
        <textarea
          id="sr-task"
          rows={5}
          required
          placeholder="Was soll die Fabrik bauen?"
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
            {def.type === "branch" ? " (Branch)" : ""}
            {def.required ? "" : " – optional"}
          </label>
          <input
            id={`sr-in-${k}`}
            className={def.type === "branch" ? "mono" : undefined}
            required={def.required}
            placeholder={def.description ?? (def.type === "branch" ? "Standard: Basis-Branch des Projekts" : "")}
            value={inputs[k] ?? ""}
            onChange={(e) => setInputs({ ...inputs, [k]: e.target.value })}
          />
        </div>
      ))}
      <div className="field wide">
        <SandboxToggle id="sr-sandbox" checked={sandbox} onChange={setSandbox} info={sandboxInfo.data} />
        <p className="hint">
          Alle Rollen arbeiten im selben neuen Workspace (nacheinander oder, bei parallelen Schritten, gleichzeitig). Lese-Rollen (Planer, Reviewer) sehen ihn nur lesend: in der Sandbox
          in einem eigenen Container mit nur lesend gemountetem <code>/workspace</code>, ohne Sandbox in einer Kopie, deren Änderungen
          verworfen werden.
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
            Abbrechen
          </button>
        )}
        <button type="submit" className="primary" disabled={!chosen || !task.trim() || missing || start.isPending}>
          {start.isPending ? "Starte …" : "Run starten"}
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
        Run starten
      </button>
    );
  return (
    <div className="startwrap">
      <StartRunForm projectId={projectId} onCancel={() => setOpen(false)} />
    </div>
  );
}

/** Inhalt eines Artefakts als Text (Markdown wird nicht gerendert: Inhalt stammt vom Agenten). */
export function ArtifactView({ runId, name }: { runId: string; name: string }) {
  const q = useQuery({ queryKey: ["artifact", runId, name], queryFn: () => api.artifact(runId, name) });
  if (q.isPending) return <p className="muted">Lade {name} …</p>;
  if (q.isError) return <Notice tone="bad">{errorMessage(q.error)}</Notice>;
  let text = q.data;
  if (name.endsWith(".json")) {
    try {
      text = JSON.stringify(JSON.parse(q.data), null, 2);
    } catch {
      // kein gültiges JSON: roh zeigen
    }
  }
  return <pre className="artifact">{text}</pre>;
}
