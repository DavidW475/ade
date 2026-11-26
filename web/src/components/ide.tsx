import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { api, ideBusy, type IdeState } from "../api";
import { Badge } from "./ui";

const ideLabels: Record<IdeState, string> = {
  stopped: "gestoppt",
  installing: "wird installiert",
  starting: "startet",
  running: "läuft",
  error: "Fehler",
};

export function IdeStateBadge({ state }: { state: IdeState }) {
  const tone = state === "running" ? "ok" : state === "error" ? "bad" : ideBusy(state) ? "busy" : "neutral";
  return <Badge tone={tone}>IDE {ideLabels[state]}</Badge>;
}

/**
 * „IDE öffnen“: öffnet /ide/<id> in einem neuen Tab. Die Seite startet die IDE, wartet auf sie
 * und leitet mit einem Einmal-Ticket auf ws-<id>.localhost weiter.
 */
export function IdeButton({ workspaceId, disabled }: { workspaceId: string; disabled?: boolean }) {
  const qc = useQueryClient();
  const status = useQuery({
    queryKey: ["ide", workspaceId],
    queryFn: () => api.ideStatus(workspaceId),
    enabled: !disabled,
    refetchInterval: (q) => (q.state.data && ideBusy(q.state.data.state) ? 1000 : 5000),
  });
  const stop = useMutation({
    mutationFn: () => api.stopIde(workspaceId),
    onSettled: () => void qc.invalidateQueries({ queryKey: ["ide", workspaceId] }),
  });
  const state = status.data?.state;
  return (
    <span className="idebutton">
      {state && state !== "stopped" && <IdeStateBadge state={state} />}
      {disabled ? (
        <button type="button" disabled title="Der Workspace hat noch kein Arbeitsverzeichnis">
          IDE öffnen
        </button>
      ) : (
        <a className="button" href={`/ide/${encodeURIComponent(workspaceId)}`} target="_blank" rel="noopener">
          IDE öffnen
        </a>
      )}
      {state === "running" && (
        <button
          type="button"
          onClick={() => stop.mutate()}
          disabled={stop.isPending}
          title={status.data?.connections ? `${status.data.connections} offene Verbindung(en)` : undefined}
        >
          IDE stoppen
        </button>
      )}
    </span>
  );
}
