import { useQuery } from "@tanstack/react-query";
import { Link, Route, Routes } from "react-router";
import { fetchAuthenticated, fetchHealth } from "./api";
import { Notice } from "./components/ui";
import { errorMessage } from "./format";
import { ProjectPage } from "./pages/ProjectPage";
import { ProjectsPage } from "./pages/ProjectsPage";
import { WorkspacePage } from "./pages/WorkspacePage";

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
