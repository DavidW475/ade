import { useQuery } from "@tanstack/react-query";
import { useState } from "react";
import { api, type EgressEvent, type Workspace } from "../api";
import { errorMessage, formatBytes, formatDuration, formatTime } from "../format";
import { Badge, Notice } from "./ui";

const reasonLabels: Record<string, string> = {
  allowlist: "Allowlist",
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

/** Netzwerk-Ansicht eines Sandbox-Workspaces: Policy, Ziele und letzte Anfragen. */
export function NetworkPanel({ workspace: w }: { workspace: Workspace }) {
  const [blockedOnly, setBlockedOnly] = useState(false);
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
