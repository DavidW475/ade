import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useState, type FormEvent } from "react";
import { Link } from "react-router";
import { api, type Project, type UsageTotal } from "../api";
import { UsageAmount } from "../components/usage";
import { useUsage } from "../hooks/useUsage";
import { SessionTable } from "../components/agents";
import { useSessions } from "../sessions";
import { Badge, Notice } from "../components/ui";
import { errorMessage } from "../format";

export function ProjectsPage() {
  const projects = useQuery({ queryKey: ["projects"], queryFn: api.projects });
  const usage = useUsage();

  return (
    <>
      <div className="pagehead">
        <h1>Projekte</h1>
        {usage.data && usage.data.month.count > 0 && (
          <span className="muted">
            Kosten diesen Monat: <UsageAmount total={usage.data.month} />
          </span>
        )}
      </div>

      <RunningAgents />

      <AddProject />

      {projects.isPending ? (
        <p className="muted">Lade Projekte …</p>
      ) : projects.isError ? (
        <Notice tone="bad">{errorMessage(projects.error)}</Notice>
      ) : projects.data.length === 0 ? (
        <div className="empty">
          <p>Noch keine Projekte registriert.</p>
          <p className="muted">
            Pfad zu einem Git-Repository oben eintragen oder im Terminal <code>ade project add &lt;pfad&gt;</code>{" "}
            ausführen.
          </p>
        </div>
      ) : (
        <ul className="cards">
          {projects.data.map((p) => (
            <ProjectCard key={p.id} project={p} month={usage.data?.projects?.[p.id]} />
          ))}
        </ul>
      )}
    </>
  );
}

function ProjectCard({ project: p, month }: { project: Project; month?: UsageTotal }) {
  return (
    <li className="card">
      <Link to={`/projects/${p.id}`} className="cardlink">
        <div className="cardtitle">
          <strong>{p.name}</strong>
          {p.config_error && <Badge tone="bad">Konfiguration ungültig</Badge>}
          {p.warnings?.length ? <Badge tone="warn">/mnt</Badge> : null}
        </div>
        <div className="mono muted">{p.repo_path}</div>
        <div className="meta">
          Basis-Branch <code>{p.base_branch}</code>
          {month && month.count > 0 && (
            <>
              {" "}
              · Kosten diesen Monat <UsageAmount total={month} />
            </>
          )}
        </div>
      </Link>
    </li>
  );
}

function AddProject() {
  const qc = useQueryClient();
  const [path, setPath] = useState("");
  const add = useMutation({
    mutationFn: (p: string) => api.addProject(p),
    onSuccess: () => {
      setPath("");
      void qc.invalidateQueries({ queryKey: ["projects"] });
    },
  });

  const submit = (e: FormEvent) => {
    e.preventDefault();
    if (path.trim()) add.mutate(path.trim());
  };

  return (
    <section className="panel">
      <form className="inline-form" onSubmit={submit}>
        <label htmlFor="repo-path">Projekt hinzufügen</label>
        <input
          id="repo-path"
          className="mono"
          placeholder="/home/ich/git/mein-repo"
          value={path}
          onChange={(e) => setPath(e.target.value)}
          autoComplete="off"
          spellCheck={false}
        />
        <button type="submit" className="primary" disabled={add.isPending || !path.trim()}>
          {add.isPending ? "Prüfe …" : "Hinzufügen"}
        </button>
      </form>
      <p className="hint">Absoluter Pfad in der WSL. Repositories unter /mnt/c sind langsam.</p>
      {add.isError && <Notice tone="bad">{errorMessage(add.error)}</Notice>}
      {add.data && (
        <Notice tone={add.data.warnings?.length || add.data.config_error ? "warn" : "info"} title={`„${add.data.name}“ registriert`}>
          {add.data.warnings?.map((w) => <p key={w}>{w}</p>)}
          {add.data.config_error && <p>Konfiguration: {add.data.config_error}</p>}
          <Link to={`/projects/${add.data.id}`}>Zum Projekt</Link>
        </Notice>
      )}
    </section>
  );
}

function RunningAgents() {
  const sessions = useSessions({ active: true });
  if (!sessions.data?.length) return null;
  return (
    <section className="running">
      <div className="sectionhead first">
        <h2>Laufende Agenten</h2>
        <Link to="/agents">Alle Agenten</Link>
      </div>
      <SessionTable sessions={sessions.data} />
    </section>
  );
}
