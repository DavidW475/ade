import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useState } from "react";
import { api, type EgressEvent, type Workspace } from "../api";
import { errorMessage, formatBytes, formatDuration, formatTime } from "../format";
import { Badge, Notice } from "./ui";

const reasonLabels: Record<string, string> = {
  allowlist: "Allowlist",
  approval: "im Dashboard freigegeben",
  provider: "API mit Key vom Proxy",
  not_allowed: "nicht freigegeben",
  denylist: "gesperrt (deny)",
  internal_address: "interne Adresse",
  no_policy: "keine Policy geladen",
  upstream_error: "Ziel nicht erreichbar",
  bad_request: "ungültige Anfrage",
};

function reasonLabel(r?: string): string {
  return (r && reasonLabels[r]) || r || "";
}

const kindLabels: Record<EgressEvent["kind"], string> = { connect: "HTTPS", http: "HTTP", api: "API" };

function List({ items }: { items: string[] | null | undefined }) {
  if (!items?.length) return <span className="muted">–</span>;
  return (
    <span className="netlist">
      {items.map((x) => (
        <code key={x}>{x}</code>
      ))}
    </span>
  );
}

/** Knöpfe, um einen geblockten Host dauerhaft zu erlauben (Ausnahme statt Eintrag in sandbox.yaml). */
function AllowButtons({ workspaceId, host, onError }: { workspaceId: string; host: string; onError: (msg: string) => void }) {
  const qc = useQueryClient();
  const grant = useMutation({
    mutationFn: (scope: "workspace" | "project") => api.addEgressGrant(workspaceId, host, scope),
    onSuccess: () => void qc.invalidateQueries({ queryKey: ["egress", workspaceId] }),
    onError: (e) => onError(errorMessage(e)),
  });
  return (
    <span className="rowactions">
      <button type="button" className="small primary" disabled={grant.isPending} onClick={() => grant.mutate("project")} title="Für alle Workspaces des Projekts">
        Erlauben (Projekt)
      </button>
      <button type="button" className="small" disabled={grant.isPending} onClick={() => grant.mutate("workspace")} title="Nur für diesen Workspace">
        nur hier
      </button>
    </span>
  );
}

/** Netzwerk-Ansicht eines Sandbox-Workspaces: Policy, Ziele und letzte Anfragen. */
export function NetworkPanel({ workspace: w }: { workspace: Workspace }) {
  const qc = useQueryClient();
  const [blockedOnly, setBlockedOnly] = useState(false);
  const [grantError, setGrantError] = useState("");
  const removeGrant = useMutation({
    mutationFn: (id: number) => api.deleteEgressGrant(id),
    onSuccess: () => void qc.invalidateQueries({ queryKey: ["egress", w.id] }),
    onError: (e) => setGrantError(errorMessage(e)),
  });
  const q = useQuery({
    queryKey: ["egress", w.id, blockedOnly],
    queryFn: () => api.workspaceEgress(w.id, blockedOnly),
    enabled: w.sandbox,
    refetchInterval: 3000,
  });
  if (!w.sandbox) return null;
  const data = q.data;
  const pol = data?.policy;
  const hosts = (data?.hosts ?? []).filter((h) => !blockedOnly || h.blocked > 0);
  const blockedCount = (data?.hosts ?? []).reduce((n, h) => n + h.blocked, 0);
  const grants = data?.grants ?? [];
  const granted = new Set(grants.map((g) => g.rule));
  // Ein Host gilt als erlaubt, wenn ihn eine Ausnahme oder die Allowlist ohne Platzhalter nennt.
  const allowed = new Set([...(pol?.effective_allow ?? []), ...granted]);

  return (
    <>
      <div className="sectionhead">
        <h2>
          Netzwerk{" "}
          {blockedCount > 0 && (
            <Badge tone="warn">
              {blockedCount} geblockt
            </Badge>
          )}
        </h2>
        <label className="checkbox">
          <input type="checkbox" checked={blockedOnly} onChange={(e) => setBlockedOnly(e.target.checked)} /> nur geblockte
        </label>
      </div>
      {q.isError && <Notice tone="bad">{errorMessage(q.error)}</Notice>}
      {pol && (
        <div className="panel netpolicy">
          {pol.error && (
            <Notice tone="warn" title=".ade/sandbox.yaml ungültig">
              {pol.error} – laufende Container behalten die zuletzt gültige Policy, neue starten erst nach der Korrektur.
            </Notice>
          )}
          <dl className="facts">
            <dt>Presets</dt>
            <dd>
              <List items={pol.presets} />
              {pol.default && <span className="muted"> (Voreinstellung – kein egress-Abschnitt in .ade/sandbox.yaml)</span>}
            </dd>
            <dt>Eigene Einträge</dt>
            <dd>
              <List items={pol.allow} />
            </dd>
            <dt>Gesperrt</dt>
            <dd>
              <List items={pol.deny} />
            </dd>
            <dt>Geblockte Ziele</dt>
            <dd>
              {pol.on_blocked === "ask" ? (
                <>
                  warten auf <a href="/approvals">Freigabe im Dashboard</a> (<code>on_blocked: ask</code>)
                </>
              ) : (
                <span>
                  sofort abgelehnt (<code>on_blocked: deny</code>)
                </span>
              )}
            </dd>
            <dt>Ausnahmen</dt>
            <dd>
              {grants.length ? (
                <span className="netlist">
                  {grants.map((g) => (
                    <span key={g.id} className="grant" title={g.workspace_id ? "nur dieser Workspace" : "alle Workspaces des Projekts"}>
                      <code>{g.rule}</code>
                      <span className="muted">{g.workspace_id ? "Workspace" : "Projekt"}</span>
                      <button
                        type="button"
                        className="small danger"
                        aria-label={`Ausnahme ${g.rule} entfernen`}
                        disabled={removeGrant.isPending}
                        onClick={() => removeGrant.mutate(g.id)}
                      >
                        ×
                      </button>
                    </span>
                  ))}
                </span>
              ) : (
                <span className="muted">keine – geblockte Ziele lassen sich unten mit einem Klick erlauben</span>
              )}
            </dd>
            <dt>API-Keys vom Proxy</dt>
            <dd>
              {pol.providers.length ? (
                <List items={pol.providers} />
              ) : (
                <span className="muted">keine – CLIs nutzen ihre eigene Anmeldung (über die Allowlist)</span>
              )}
            </dd>
          </dl>
          <details>
            <summary>Alle erlaubten Ziele ({pol.effective_allow?.length ?? 0})</summary>
            <p className="hint">
              Einträge ohne Port gelten für 80 und 443, <code>*.example.com</code> für alle Subdomains. Interne Adressen
              (Host, andere Container, private Netze) sind immer gesperrt. Änderungen an <code>.ade/sandbox.yaml</code> im
              Projekt-Repo wirken nach wenigen Sekunden.
            </p>
            <List items={pol.effective_allow} />
          </details>
        </div>
      )}

      {grantError && (
        <Notice tone="bad">
          {grantError}{" "}
          <button type="button" className="small" onClick={() => setGrantError("")}>
            OK
          </button>
        </Notice>
      )}
      {q.isPending ? (
        <p className="muted">Lade Netzwerk-Ereignisse …</p>
      ) : !hosts.length ? (
        <p className="muted">{blockedOnly ? "Keine geblockten Anfragen." : "Noch keine Anfragen aus dem Container."}</p>
      ) : (
        <div className="tablewrap">
          <table>
            <thead>
              <tr>
                <th>Ziel</th>
                <th>Erlaubt</th>
                <th>Geblockt</th>
                <th>Daten ↑ / ↓</th>
                <th>Zuletzt</th>
                <th />
              </tr>
            </thead>
            <tbody>
              {hosts.map((h) => (
                <tr key={h.host}>
                  <td className="mono">{h.host}</td>
                  <td>{h.allowed ? <Badge tone="ok">{h.allowed}</Badge> : <span className="muted">0</span>}</td>
                  <td>
                    {h.blocked ? (
                      <span title={reasonLabel(h.reason)}>
                        <Badge tone="bad">{h.blocked}</Badge> <span className="muted">{reasonLabel(h.reason)}</span>
                      </span>
                    ) : (
                      <span className="muted">0</span>
                    )}
                  </td>
                  <td className="muted">
                    {formatBytes(h.bytes_out)} / {formatBytes(h.bytes_in)}
                  </td>
                  <td className="muted">{formatTime(h.last)}</td>
                  <td>
                    {h.blocked > 0 && h.reason === "not_allowed" && !allowed.has(h.host) ? (
                      <AllowButtons workspaceId={w.id} host={h.host} onError={setGrantError} />
                    ) : granted.has(h.host) ? (
                      <Badge tone="ok">Ausnahme</Badge>
                    ) : null}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}

      {!!data?.events.length && (
        <details className="panel netevents">
          <summary>
            Letzte Anfragen ({data.events.length}
            {data.events.length >= 200 ? "+" : ""})
          </summary>
          <div className="tablewrap">
            <table>
              <thead>
                <tr>
                  <th>Zeit</th>
                  <th>Art</th>
                  <th>Ziel</th>
                  <th>Ergebnis</th>
                  <th>Daten ↑ / ↓</th>
                  <th>Dauer</th>
                </tr>
              </thead>
              <tbody>
                {data.events.map((e) => (
                  <tr key={e.id}>
                    <td className="muted">{formatTime(e.ts, true)}</td>
                    <td className="muted">{e.kind === "api" ? `API ${e.provider ?? ""}` : kindLabels[e.kind]}</td>
                    <td className="mono wrap">
                      {e.method && e.kind !== "connect" ? `${e.method} ` : ""}
                      {e.host}
                      {e.port && e.port !== 443 && e.port !== 80 ? `:${e.port}` : ""}
                      {e.path ?? ""}
                    </td>
                    <td title={e.error}>
                      <Badge tone={e.allowed ? "ok" : "bad"}>{e.allowed ? (e.status ? String(e.status) : "ok") : "geblockt"}</Badge>{" "}
                      <span className="muted">{reasonLabel(e.reason)}</span>
                    </td>
                    <td className="muted">
                      {formatBytes(e.bytes_out)} / {formatBytes(e.bytes_in)}
                    </td>
                    <td className="muted">{formatDuration(e.duration_ms)}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        </details>
      )}
    </>
  );
}
