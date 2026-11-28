import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useState } from "react";
import { api, type EgressEvent, type Workspace } from "../api";
import { errorMessage, formatBytes, formatDuration, formatTime } from "../format";
import { Badge, Notice } from "./ui";

const reasonLabels: Record<string, string> = {
  allowlist: "allowlist",
  approval: "approved in the dashboard",
  provider: "API with key from the proxy",
  not_allowed: "not allowed",
  denylist: "blocked (deny)",
  internal_address: "internal address",
  no_policy: "no policy loaded",
  upstream_error: "target not reachable",
  bad_request: "invalid request",
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

/** Buttons to allow a blocked host permanently (exception instead of an entry in sandbox.yaml). */
function AllowButtons({ workspaceId, host, onError }: { workspaceId: string; host: string; onError: (msg: string) => void }) {
  const qc = useQueryClient();
  const grant = useMutation({
    mutationFn: (scope: "workspace" | "project") => api.addEgressGrant(workspaceId, host, scope),
    onSuccess: () => void qc.invalidateQueries({ queryKey: ["egress", workspaceId] }),
    onError: (e) => onError(errorMessage(e)),
  });
  return (
    <span className="rowactions">
      <button type="button" className="small primary" disabled={grant.isPending} onClick={() => grant.mutate("project")} title="For all workspaces of the project">
        Allow (project)
      </button>
      <button type="button" className="small" disabled={grant.isPending} onClick={() => grant.mutate("workspace")} title="Only for this workspace">
        only here
      </button>
    </span>
  );
}

/** Network view of a sandbox workspace: policy, targets and latest requests. */
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
  // A host counts as allowed if an exception or the allowlist names it without a wildcard.
  const allowed = new Set([...(pol?.effective_allow ?? []), ...granted]);

  return (
    <>
      <div className="sectionhead">
        <h2>
          Network{" "}
          {blockedCount > 0 && (
            <Badge tone="warn">
              {blockedCount} blocked
            </Badge>
          )}
        </h2>
        <label className="checkbox">
          <input type="checkbox" checked={blockedOnly} onChange={(e) => setBlockedOnly(e.target.checked)} /> blocked only
        </label>
      </div>
      {q.isError && <Notice tone="bad">{errorMessage(q.error)}</Notice>}
      {pol && (
        <div className="panel netpolicy">
          {pol.error && (
            <Notice tone="warn" title=".ade/sandbox.yaml invalid">
              {pol.error} – running containers keep the last valid policy, new ones only start after it is fixed.
            </Notice>
          )}
          <dl className="facts">
            <dt>Presets</dt>
            <dd>
              <List items={pol.presets} />
              {pol.default && <span className="muted"> (default – no egress section in .ade/sandbox.yaml)</span>}
            </dd>
            <dt>Own entries</dt>
            <dd>
              <List items={pol.allow} />
            </dd>
            <dt>Blocked</dt>
            <dd>
              <List items={pol.deny} />
            </dd>
            <dt>Blocked targets</dt>
            <dd>
              {pol.on_blocked === "ask" ? (
                <>
                  wait for an <a href="/approvals">approval in the dashboard</a> (<code>on_blocked: ask</code>)
                </>
              ) : (
                <span>
                  rejected right away (<code>on_blocked: deny</code>)
                </span>
              )}
            </dd>
            <dt>Exceptions</dt>
            <dd>
              {grants.length ? (
                <span className="netlist">
                  {grants.map((g) => (
                    <span key={g.id} className="grant" title={g.workspace_id ? "only this workspace" : "all workspaces of the project"}>
                      <code>{g.rule}</code>
                      <span className="muted">{g.workspace_id ? "workspace" : "project"}</span>
                      <button
                        type="button"
                        className="small danger"
                        aria-label={`Remove exception ${g.rule}`}
                        disabled={removeGrant.isPending}
                        onClick={() => removeGrant.mutate(g.id)}
                      >
                        ×
                      </button>
                    </span>
                  ))}
                </span>
              ) : (
                <span className="muted">none – blocked targets can be allowed below with one click</span>
              )}
            </dd>
            <dt>Credentials from the proxy</dt>
            <dd>
              {pol.providers.length ? (
                <List items={pol.providers} />
              ) : (
                <span className="muted">none – CLIs use their own login (through the allowlist)</span>
              )}
            </dd>
          </dl>
          <details>
            <summary>All allowed targets ({pol.effective_allow?.length ?? 0})</summary>
            <p className="hint">
              Entries without a port apply to 80 and 443, <code>*.example.com</code> to all subdomains. Internal addresses
              (host, other containers, private networks) are always blocked. Changes to <code>.ade/sandbox.yaml</code> in the
              project repository take effect within seconds.
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
        <p className="muted">Loading network events …</p>
      ) : !hosts.length ? (
        <p className="muted">{blockedOnly ? "No blocked requests." : "No requests from the container yet."}</p>
      ) : (
        <div className="tablewrap">
          <table>
            <thead>
              <tr>
                <th>Target</th>
                <th>Allowed</th>
                <th>Blocked</th>
                <th>Data ↑ / ↓</th>
                <th>Last</th>
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
                      <Badge tone="ok">exception</Badge>
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
            Latest requests ({data.events.length}
            {data.events.length >= 200 ? "+" : ""})
          </summary>
          <div className="tablewrap">
            <table>
              <thead>
                <tr>
                  <th>Time</th>
                  <th>Kind</th>
                  <th>Target</th>
                  <th>Result</th>
                  <th>Data ↑ / ↓</th>
                  <th>Duration</th>
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
                      <Badge tone={e.allowed ? "ok" : "bad"}>{e.allowed ? (e.status ? String(e.status) : "ok") : "blocked"}</Badge>{" "}
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
