// Formatierungshilfen für die Anzeige.

export function shortCommit(sha: string): string {
  return sha.slice(0, 8);
}

export function formatTime(iso: string): string {
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return iso;
  return d.toLocaleString("de-DE", { dateStyle: "short", timeStyle: "short" });
}

export function errorMessage(err: unknown): string {
  return err instanceof Error ? err.message : String(err);
}
