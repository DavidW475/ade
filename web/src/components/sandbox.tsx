import { useQuery } from "@tanstack/react-query";
import { api, type SandboxInfo, type Workspace } from "../api";
import { networkHint } from "../sandbox";
import { Badge, Notice } from "./ui";

/** Schalter „Sandbox“ für neue Workspaces. */
export function SandboxToggle({ checked, onChange, info, id }: { checked: boolean; onChange: (v: boolean) => void; info?: SandboxInfo; id: string }) {
  const unavailable = !info?.available;
  return (
    <div className="sandboxtoggle">
      <label htmlFor={id} title={unavailable ? info?.error ?? "Docker nicht erreichbar" : undefined}>
        <input id={id} type="checkbox" checked={checked && !unavailable} disabled={unavailable} onChange={(e) => onChange(e.target.checked)} />
        Sandbox (Docker)
      </label>
      <span className="hint">
        {unavailable
          ? `Nicht verfügbar: ${info?.error ?? "Docker nicht erreichbar"}`
          : checked
            ? `Agent, Setup und IDE laufen isoliert im Container (${[info.image, info.runtime].filter(Boolean).join(", ")}). ${networkHint}`
            : "Ohne Sandbox läuft der Agent mit deinen Rechten direkt im Workspace-Verzeichnis."}
      </span>
    </div>
  );
}

/** Kennzeichnung, ob ein Workspace isoliert läuft. */
export function SandboxBadge({ workspace }: { workspace: Pick<Workspace, "sandbox"> }) {
  return workspace.sandbox ? (
    <Badge tone="ok">Sandbox</Badge>
  ) : (
    <Badge tone="warn">lokal</Badge>
  );
}

/** Container-Details eines Workspaces (Workspace-Seite). */
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
        <dt>Ausführung</dt>
        <dd>
          <SandboxBadge workspace={w} /> <span className="muted">ohne Container, mit deinen Rechten</span>
        </dd>
      </>
    );
  }
  const c = sb.data?.container;
  return (
    <>
      <dt>Ausführung</dt>
      <dd>
        <SandboxBadge workspace={w} />{" "}
        {c ? <Badge tone={c.running ? "ok" : "neutral"}>{c.running ? "Container läuft" : `Container ${c.state}`}</Badge> : <Badge>kein Container</Badge>}{" "}
        {sb.data?.network === "open" ? (
          <span title="Container aus einer älteren ADE-Version; er wird beim nächsten Start mit abgeschottetem Netz neu angelegt">
            <Badge tone="warn">Netz offen bis zum Neustart</Badge>
          </span>
        ) : (
          <Badge tone="ok">Netz: nur Allowlist</Badge>
        )}
      </dd>
      <dt>Image</dt>
      <dd className="mono">{w.sandbox_image || sb.data?.image || "–"}</dd>
    </>
  );
}

/** Hinweis auf der Workspace-Seite. */
export function SandboxNotice({ workspace: w }: { workspace: Workspace }) {
  if (!w.sandbox) return null;
  return (
    <Notice tone="info" title="Sandbox">
      Der Container sieht nur dieses Workspace-Verzeichnis (als <code>/workspace</code>), läuft ohne Root-Rechte und mit Ressourcen-Grenzen. {networkHint}
    </Notice>
  );
}
