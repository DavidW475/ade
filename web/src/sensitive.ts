// Sensitive paths (#103): a human must see changes to them before a push.
import type { FileChange } from "./api";

/** Paths whose change a human must see before a push (like workspace.SensitivePaths). */
export const sensitivePaths = [".github/workflows/", ".github/actions/", ".ade/"];

export function isSensitive(path: string): boolean {
  return sensitivePaths.some((p) => (path + "/").startsWith(p));
}

/** Changed files under the sensitive paths (also old paths of renamed files). */
export function sensitiveFiles(files: FileChange[] | undefined): string[] {
  const out = new Set<string>();
  for (const f of files ?? []) {
    for (const p of [f.path, f.old_path]) if (p && isSensitive(p)) out.add(p);
  }
  return [...out].sort();
}
