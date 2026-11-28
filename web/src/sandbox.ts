import { useQuery } from "@tanstack/react-query";
import { api, type SandboxInfo } from "./api";

/** Sandbox status of the daemon (Docker reachable? default?). */
export function useSandboxInfo() {
  return useQuery({ queryKey: ["sandbox"], queryFn: api.sandbox, staleTime: 30_000, retry: false });
}

/** Value of the switch as long as the user has not chosen anything. */
export function sandboxDefault(info?: SandboxInfo): boolean {
  return !!info && info.available && info.default;
}

/** Notice about the network isolation of the sandbox (M6). */
export const networkHint =
  "It only reaches the internet through the egress proxy and only the targets from .ade/sandbox.yaml (egress); the proxy inserts API keys and tokens, the container does not see them.";
