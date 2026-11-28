import { useQuery } from "@tanstack/react-query";
import { api, type SandboxInfo, type Workspace } from "../api";
import { networkHint } from "../sandbox";
import { Badge, Notice } from "./ui";

/** “Sandbox” switch for new workspaces. */
export function SandboxToggle({ checked, onChange, info, id }: { checked: boolean; onChange: (v: boolean) => void; info?: SandboxInfo; id: string }) {
  const unavailable = !info?.available;
  return (
    <div className="sandboxtoggle">
      <label htmlFor={id} title={unavailable ? info?.error ?? "Docker not reachable" : undefined}>
        <input id={id} type="checkbox" checked={checked && !unavailable} disabled={unavailable} onChange={(e) => onChange(e.target.checked)} />
        Sandbox (Docker)
      </label>
      <span className="hint">
        {unavailable
          ? `Not available: ${info?.error ?? "Docker not reachable"}`
          : checked
            ? `Agent, setup and IDE run isolated in the container (${[info.image, info.runtime].filter(Boolean).join(", ")}). ${networkHint}`
            : "Without a sandbox the agent runs with your permissions directly in the workspace directory."}
      </span>
    </div>
  );
}

/** Marks whether a workspace runs isolated. */
export function SandboxBadge({ workspace }: { workspace: Pick<Workspace, "sandbox"> }) {
  return workspace.sandbox ? (
    <Badge tone="ok">Sandbox</Badge>
  ) : (
    <Badge tone="warn">local</Badge>
  );
}

/** Container details of a workspace (workspace page). */
export function SandboxFacts({ workspace: w }: { workspace: Workspace }) {
  const sb = useQuery({
    queryKey: ["workspace-sandbox", w.id],
    queryFn: () => api.workspaceSandbox(w.id),
    enabled: w.sandbox,
    refetchInterval: 10_000,
  });
  if (!w.sandbox) {
    return (
      <>
        <dt>Execution</dt>
        <dd>
          <SandboxBadge workspace={w} /> <span className="muted">without a container, with your permissions</span>
        </dd>
      </>
    );
  }
  const c = sb.data?.container;
  return (
    <>
      <dt>Execution</dt>
      <dd>
        <SandboxBadge workspace={w} />{" "}
        {c ? <Badge tone={c.running ? "ok" : "neutral"}>{c.running ? "container running" : `container ${c.state}`}</Badge> : <Badge>no container</Badge>}{" "}
        {sb.data?.network === "open" ? (
          <span title="Container from an older ADE version; it is recreated with an isolated network on the next start">
            <Badge tone="warn">network open until the restart</Badge>
          </span>
        ) : (
          <Badge tone="ok">network: allowlist only</Badge>
        )}
      </dd>
      <dt>Image</dt>
      <dd className="mono">{w.sandbox_image || sb.data?.image || "–"}</dd>
    </>
  );
}

/** Notice on the workspace page. */
export function SandboxNotice({ workspace: w }: { workspace: Workspace }) {
  if (!w.sandbox) return null;
  return (
    <Notice tone="info" title="Sandbox">
      The container only sees this workspace directory (as <code>/workspace</code>), runs without root permissions and with resource limits. {networkHint}
    </Notice>
  );
}
