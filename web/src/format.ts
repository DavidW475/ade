// Formatierungshilfen für die Anzeige.

export function shortCommit(sha: string): string {
  return sha.slice(0, 8);
}

/** Datum und Uhrzeit; mit timeOnly nur die Uhrzeit (mit Sekunden). */
export function formatTime(iso: string, timeOnly = false): string {
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return iso;
  if (timeOnly) return d.toLocaleTimeString("de-DE", { hour: "2-digit", minute: "2-digit", second: "2-digit" });
  return d.toLocaleString("de-DE", { dateStyle: "short", timeStyle: "short" });
}

export function formatUSD(usd: number): string {
  return `${usd < 0.01 && usd > 0 ? usd.toFixed(4) : usd.toFixed(2)} $`;
}

export function formatDuration(ms: number): string {
  if (ms < 1000) return `${ms} ms`;
  const s = ms / 1000;
  if (s < 60) return `${s.toLocaleString("de-DE", { maximumFractionDigits: 1 })} s`;
  return `${Math.floor(s / 60)} min ${Math.round(s % 60)} s`;
}

export function errorMessage(err: unknown): string {
  return err instanceof Error ? err.message : String(err);
}
