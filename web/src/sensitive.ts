// Sensible Pfade (#103): Änderungen daran muss vor einem Push ein Mensch sehen.
import type { FileChange } from "./api";

/** Pfade, deren Änderung vor einem Push ein Mensch sehen muss (wie workspace.SensitivePaths). */
export const sensitivePaths = [".github/workflows/", ".github/actions/", ".ade/"];

export function isSensitive(path: string): boolean {
  return sensitivePaths.some((p) => (path + "/").startsWith(p));
}

/** Geänderte Dateien unter den sensiblen Pfaden (auch alte Pfade umbenannter Dateien). */
export function sensitiveFiles(files: FileChange[] | undefined): string[] {
  const out = new Set<string>();
  for (const f of files ?? []) {
    for (const p of [f.path, f.old_path]) if (p && isSensitive(p)) out.add(p);
  }
  return [...out].sort();
}
