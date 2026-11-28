import { useQuery, useQueryClient } from "@tanstack/react-query";
import { useEffect, useState } from "react";
import { Link, useParams } from "react-router";
import { api } from "../api";
import { IdeStateBadge } from "../components/ide";
import { Notice } from "../components/ui";
import { errorMessage } from "../format";

/** Start page of an IDE tab: start the IDE, wait for it, then redirect to the subdomain. */
export function IdePage() {
  const { id = "" } = useParams();
  const [attempt, setAttempt] = useState(0);
  const [redirectError, setRedirectError] = useState<unknown>(null);
  const qc = useQueryClient();

  const start = useQuery({
    queryKey: ["ide-start", id, attempt],
    queryFn: async () => {
      const st = await api.startIde(id);
      qc.setQueryData(["ide", id], st); // replace the old error state
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

  const error = start.error ?? redirectError ?? (state === "error" ? new Error(status.data?.error ?? "unknown error") : null);

  return (
    <div className="idelaunch">
      <div className="crumbs">
        <Link to={`/workspaces/${id}`} className="mono">
          {workspace.data?.branch ?? id}
        </Link>
      </div>
      <h1>Browser IDE</h1>
      {error ? (
        <>
          <Notice tone="bad" title="IDE could not be started">
            {errorMessage(error)}
          </Notice>
          <button
            type="button"
            onClick={() => {
              setRedirectError(null);
              setAttempt((a) => a + 1);
            }}
          >
            Try again
          </button>
        </>
      ) : (
        <p>
          {state && <IdeStateBadge state={state} />}{" "}
          {state === "installing"
            ? "openvscode-server is downloaded on the first start (about 75 MB) …"
            : state === "running"
              ? "Redirecting to the IDE …"
              : "Starting the IDE …"}
        </p>
      )}
    </div>
  );
}
