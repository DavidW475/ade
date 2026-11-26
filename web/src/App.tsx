import { useEffect, useState } from "react";
import { fetchAuthenticated, fetchHealth, type Health } from "./api";

type State =
  | { kind: "loading" }
  | { kind: "error"; message: string }
  | { kind: "ready"; health: Health; authenticated: boolean };

export function App() {
  const [state, setState] = useState<State>({ kind: "loading" });

  useEffect(() => {
    const ctrl = new AbortController();
    Promise.all([fetchHealth(ctrl.signal), fetchAuthenticated(ctrl.signal)])
      .then(([health, authenticated]) => setState({ kind: "ready", health, authenticated }))
      .catch((err: unknown) => {
        if (ctrl.signal.aborted) return;
        setState({ kind: "error", message: err instanceof Error ? err.message : String(err) });
      });
    return () => ctrl.abort();
  }, []);

  return (
    <main className="shell">
      <header className="brand">
        <img src="/favicon.svg" alt="" width={32} height={32} />
        <span>ADE</span>
      </header>
      <section className="card">{renderBody(state)}</section>
    </main>
  );
}

function renderBody(state: State) {
  switch (state.kind) {
    case "loading":
      return <p className="muted">Verbinde mit dem Daemon …</p>;
    case "error":
      return (
        <>
          <h1>Daemon nicht erreichbar</h1>
          <p className="muted">{state.message}</p>
        </>
      );
    case "ready": {
      const { health, authenticated } = state;
      return (
        <>
          <h1>
            <span className={`dot ${health.status}`} aria-hidden /> ADE läuft
          </h1>
          <dl className="facts">
            <dt>Version</dt>
            <dd>{health.version}</dd>
            <dt>Status</dt>
            <dd>{health.status === "ok" ? "bereit" : "eingeschränkt"}</dd>
            <dt>Schema</dt>
            <dd>{health.schema}</dd>
            <dt>Anmeldung</dt>
            <dd>{authenticated ? "angemeldet" : "nicht angemeldet"}</dd>
          </dl>
          {!authenticated && (
            <p className="hint">
              Zum Anmelden im Terminal <code>ade start</code> ausführen.
            </p>
          )}
        </>
      );
    }
  }
}
