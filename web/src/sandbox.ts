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

/** Hinweis zur Netzwerk-Isolation der Sandbox (M6). */
export const networkHint =
  "Ins Internet kommt er nur über den Egress-Proxy und nur zu den Zielen aus .ade/sandbox.yaml (egress); API-Keys fügt der Proxy ein, der Container sieht sie nicht.";
