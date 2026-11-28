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
        <h1>Projects</h1>
        {usage.data && usage.data.month.count > 0 && (
          <span className="muted">
            Costs this month: <UsageAmount total={usage.data.month} />
          </span>
        )}
      </div>

      <RunningAgents />

      <AddProject />

      {projects.isPending ? (
        <p className="muted">Loading projects …</p>
      ) : projects.isError ? (
        <Notice tone="bad">{errorMessage(projects.error)}</Notice>
      ) : projects.data.length === 0 ? (
        <div className="empty">
          <p>No projects registered yet.</p>
          <p className="muted">
            Enter the path to a Git repository above or run <code>ade project add &lt;path&gt;</code>{" "}
            in the terminal.
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
          {p.config_error && <Badge tone="bad">configuration invalid</Badge>}
          {p.warnings?.length ? <Badge tone="warn">/mnt</Badge> : null}
        </div>
        <div className="mono muted">{p.repo_path}</div>
        <div className="meta">
          Base branch <code>{p.base_branch}</code>
          {month && month.count > 0 && (
            <>
              {" "}
              · costs this month <UsageAmount total={month} />
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
        <label htmlFor="repo-path">Add project</label>
        <input
          id="repo-path"
          className="mono"
          placeholder="/home/me/git/my-repo"
          value={path}
          onChange={(e) => setPath(e.target.value)}
          autoComplete="off"
          spellCheck={false}
        />
        <button type="submit" className="primary" disabled={add.isPending || !path.trim()}>
          {add.isPending ? "Checking …" : "Add"}
        </button>
      </form>
      <p className="hint">Absolute path in WSL. Repositories under /mnt/c are slow.</p>
      {add.isError && <Notice tone="bad">{errorMessage(add.error)}</Notice>}
      {add.data && (
        <Notice tone={add.data.warnings?.length || add.data.config_error ? "warn" : "info"} title={`“${add.data.name}” registered`}>
          {add.data.warnings?.map((w) => <p key={w}>{w}</p>)}
          {add.data.config_error && <p>Configuration: {add.data.config_error}</p>}
          <Link to={`/projects/${add.data.id}`}>To the project</Link>
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
        <h2>Running agents</h2>
        <Link to="/agents">All agents</Link>
      </div>
      <SessionTable sessions={sessions.data} />
    </section>
  );
}
