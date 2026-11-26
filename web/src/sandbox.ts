import { useQuery } from "@tanstack/react-query";
import { api, type SandboxInfo } from "./api";

/** Sandbox-Status des Daemons (Docker erreichbar? Voreinstellung?). */
export function useSandboxInfo() {
  return useQuery({ queryKey: ["sandbox"], queryFn: api.sandbox, staleTime: 30_000, retry: false });
}

/** Wert des Schalters, solange der Nutzer nichts gewählt hat. */
export function sandboxDefault(info?: SandboxInfo): boolean {
  return !!info && info.available && info.default;
}

/** Hinweis, solange die Sandbox noch keine Netzwerk-Einschränkung hat (M6). */
export const networkHint = "Der Container hat noch normales Netz – Egress-Regeln kommen mit M6.";
