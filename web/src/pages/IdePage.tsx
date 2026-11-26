import { useQuery, useQueryClient } from "@tanstack/react-query";
import { useEffect, useState } from "react";
import { Link, useParams } from "react-router";
import { api } from "../api";
import { IdeStateBadge } from "../components/ide";
import { Notice } from "../components/ui";
import { errorMessage } from "../format";

/** Startseite eines IDE-Tabs: IDE starten, auf sie warten, dann zur Subdomain weiterleiten. */
export function IdePage() {
  const { id = "" } = useParams();
  const [attempt, setAttempt] = useState(0);
  const [redirectError, setRedirectError] = useState<unknown>(null);
  const qc = useQueryClient();

  const start = useQuery({
    queryKey: ["ide-start", id, attempt],
    queryFn: async () => {
      const st = await api.startIde(id);
      qc.setQueryData(["ide", id], st); // alten Fehlerzustand ersetzen
      return st;
    },
    retry: false,
    staleTime: Infinity,
  });
  const status = useQuery({
    queryKey: ["ide", id],
    queryFn: () => api.ideStatus(id),
    enabled: start.isSuccess,
    refetchInterval: (q) => (q.state.data?.state === "running" || q.state.data?.state === "error" ? false : 500),
  });
  const workspace = useQuery({ queryKey: ["workspace", id], queryFn: () => api.workspace(id) });
  const state = status.data?.state ?? start.data?.state;

  useEffect(() => {
    if (state !== "running") return;
    let cancelled = false;
    api
      .ideTicket(id)
      .then(({ url }) => {
        if (!cancelled) window.location.replace(url);
      })
      .catch((err: unknown) => {
        if (!cancelled) setRedirectError(err);
      });
    return () => {
      cancelled = true;
    };
  }, [state, id]);

  useEffect(() => {
    document.title = `IDE ${workspace.data?.branch ?? id} – ADE`;
  }, [workspace.data, id]);

  const error = start.error ?? redirectError ?? (state === "error" ? new Error(status.data?.error ?? "unbekannter Fehler") : null);

  return (
    <div className="idelaunch">
      <div className="crumbs">
        <Link to={`/workspaces/${id}`} className="mono">
          {workspace.data?.branch ?? id}
        </Link>
      </div>
      <h1>Browser-IDE</h1>
      {error ? (
        <>
          <Notice tone="bad" title="IDE konnte nicht gestartet werden">
            {errorMessage(error)}
          </Notice>
          <button
            type="button"
            onClick={() => {
              setRedirectError(null);
              setAttempt((a) => a + 1);
            }}
          >
            Erneut versuchen
          </button>
        </>
      ) : (
        <p>
          {state && <IdeStateBadge state={state} />}{" "}
          {state === "installing"
            ? "openvscode-server wird beim ersten Start heruntergeladen (etwa 75 MB) …"
            : state === "running"
              ? "Weiterleitung zur IDE …"
              : "IDE wird gestartet …"}
        </p>
      )}
    </div>
  );
}
