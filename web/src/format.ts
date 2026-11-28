// Formatting helpers for the display.

/** Locale for numbers; dates and times follow the browser's locale. */
const NUMBER_LOCALE = "en-US";

export function shortCommit(sha: string): string {
  return sha.slice(0, 8);
}

/** Date and time; with timeOnly only the time (with seconds). */
export function formatTime(iso: string, timeOnly = false): string {
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return iso;
  if (timeOnly) return d.toLocaleTimeString(undefined, { hour: "2-digit", minute: "2-digit", second: "2-digit" });
  return d.toLocaleString(undefined, { dateStyle: "short", timeStyle: "short" });
}

export function formatUSD(usd: number): string {
  return `$${usd < 0.01 && usd > 0 ? usd.toFixed(4) : usd.toFixed(2)}`;
}

/** Compact tokens: 950, 12.3k, 1.2M. */
export function formatTokens(n: number): string {
  if (n < 1000) return String(n);
  if (n < 1_000_000) return `${(n / 1000).toLocaleString(NUMBER_LOCALE, { maximumFractionDigits: 1 })}k`;
  return `${(n / 1_000_000).toLocaleString(NUMBER_LOCALE, { maximumFractionDigits: 2 })}M`;
}

/** Formats a number with thousands separators. */
export function formatNumber(n: number): string {
  return n.toLocaleString(NUMBER_LOCALE);
}

export function formatDuration(ms: number): string {
  if (ms < 1000) return `${ms} ms`;
  const s = ms / 1000;
  if (s < 60) return `${s.toLocaleString(NUMBER_LOCALE, { maximumFractionDigits: 1 })} s`;
  return `${Math.floor(s / 60)} min ${Math.round(s % 60)} s`;
}

export function errorMessage(err: unknown): string {
  return err instanceof Error ? err.message : String(err);
}

export function formatBytes(n: number): string {
  if (n < 1024) return `${n} B`;
  const units = ["KB", "MB", "GB", "TB"];
  let v = n / 1024;
  let i = 0;
  while (v >= 1024 && i < units.length - 1) {
    v /= 1024;
    i++;
  }
  return `${v.toLocaleString(NUMBER_LOCALE, { maximumFractionDigits: v < 10 ? 1 : 0 })} ${units[i]}`;
}
