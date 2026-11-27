import { useQuery } from "@tanstack/react-query";
import { lazy, Suspense } from "react";
import { Link, Route, Routes } from "react-router";
import { fetchAuthenticated, fetchHealth } from "./api";
import { Notice } from "./components/ui";
import { errorMessage } from "./format";
import { ApprovalsNavBadge } from "./components/approvals";
import { AgentsPage } from "./pages/AgentsPage";
import { ApprovalsPage } from "./pages/ApprovalsPage";
import { IdePage } from "./pages/IdePage";
import { ProjectPage } from "./pages/ProjectPage";
import { ProjectsPage } from "./pages/ProjectsPage";
import { RunPage } from "./pages/RunPage";
import { RunsPage } from "./pages/RunsPage";
import { WorkspacePage } from "./pages/WorkspacePage";

// Die Session-Seite bringt xterm.js mit – erst laden, wenn sie gebraucht wird.
const SessionPage = lazy(() => import("./pages/SessionPage").then((m) => ({ default: m.SessionPage })));

export function App() {
  const health = useQuery({ queryKey: ["health"], queryFn: fetchHealth, refetchInterval: 15_000 });
  const auth = useQuery({ queryKey: ["session"], queryFn: fetchAuthenticated });

  return (
    <div className="app">
      <header className="topbar">
        <Link to="/" className="brand">
          <img src="/favicon.svg" alt="" width={24} height={24} />
          <span>ADE</span>
        </Link>
        <nav>
          <Link to="/">Projekte</Link>
          <Link to="/agents">Agenten</Link>
          <Link to="/runs">Runs</Link>
          {auth.data && (
            <Link to="/approvals" className="navitem">
              Freigaben <ApprovalsNavBadge />
            </Link>
          )}
        </nav>
        <div className="daemon" title={health.data ? `Schema ${health.data.schema}` : undefined}>
          {health.isError ? (
            <>
              <span className="dot bad" aria-hidden /> Daemon nicht erreichbar
            </>
          ) : health.data ? (
            <>
              <span className={`dot ${health.data.status === "ok" ? "ok" : "warn"}`} aria-hidden /> ADE läuft ·{" "}
              {health.data.version}
            </>
          ) : (
            "…"
          )}
        </div>
      </header>

      <main className="content">
        {auth.isPending ? (
          <p className="muted">Verbinde mit dem Daemon …</p>
        ) : auth.isError ? (
          <Notice tone="bad" title="Daemon nicht erreichbar">
            {errorMessage(auth.error)}
          </Notice>
        ) : !auth.data ? (
          <Notice tone="info" title="Nicht angemeldet">
            Zum Anmelden im Terminal <code>ade start</code> ausführen.
          </Notice>
        ) : (
          <Routes>
            <Route path="/" element={<ProjectsPage />} />
            <Route path="/projects/:id" element={<ProjectPage />} />
            <Route path="/workspaces/:id" element={<WorkspacePage />} />
            <Route path="/agents" element={<AgentsPage />} />
            <Route path="/runs" element={<RunsPage />} />
            <Route path="/runs/:id" element={<RunPage />} />
            <Route path="/approvals" element={<ApprovalsPage />} />
            <Route path="/ide/:id" element={<IdePage />} />
            <Route
              path="/sessions/:id"
              element={
                <Suspense fallback={<p className="muted">Lade …</p>}>
                  <SessionPage />
                </Suspense>
              }
            />
            <Route
              path="*"
              element={
                <Notice tone="warn" title="Seite nicht gefunden">
                  <Link to="/">Zur Projektübersicht</Link>
                </Notice>
              }
            />
          </Routes>
        )}
      </main>
    </div>
  );
}
