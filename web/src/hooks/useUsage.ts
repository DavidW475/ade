import { useQuery } from "@tanstack/react-query";
import { api } from "../api";

/** Costs and tokens (all projects or one), refreshed every 15 s. "-": project not known yet. */
export function useUsage(projectId?: string) {
  return useQuery({
    queryKey: ["usage", projectId ?? ""],
    queryFn: () => api.usage(projectId),
    refetchInterval: 15_000,
    enabled: projectId !== "-",
  });
}
