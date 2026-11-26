export interface Health {
  service: string;
  status: "ok" | "degraded";
  version: string;
  schema: number;
}

export async function fetchHealth(signal?: AbortSignal): Promise<Health> {
  const res = await fetch("/api/health", { signal });
  if (!res.ok && res.status !== 503) throw new Error(`HTTP ${res.status}`);
  return (await res.json()) as Health;
}

/** Meldet, ob der Browser eine gültige ADE-Session hat. */
export async function fetchAuthenticated(signal?: AbortSignal): Promise<boolean> {
  const res = await fetch("/api/session", { signal });
  if (res.status === 401) return false;
  if (!res.ok) throw new Error(`HTTP ${res.status}`);
  return true;
}
