import { useQuery } from "@tanstack/react-query";
import { api } from "../api";

/** Kosten und Tokens (alle Projekte oder eines), alle 15 s aktualisiert. "-": Projekt noch unbekannt. */
export function useUsage(projectId?: string) {
  return useQuery({
    queryKey: ["usage", projectId ?? ""],
    queryFn: () => api.usage(projectId),
    refetchInterval: 15_000,
    enabled: projectId !== "-",
  });
}
