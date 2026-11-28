// Helpers around factory runs (without components, separate for Fast Refresh).
import { useQuery } from "@tanstack/react-query";
import { api, runActive, type Run } from "./api";

/** First line of a run's task. */
export function runTitle(r: Pick<Run, "task" | "factory">): string {
  const line = (r.task ?? "").split("\n")[0] ?? "";
  if (!line) return r.factory;
  return line.length > 90 ? `${line.slice(0, 90)} …` : line;
}

/** Queries runs – more often while one is running. */
export function useRuns(filter: Parameters<typeof api.runs>[0]) {
  return useQuery({
    queryKey: ["runs", filter],
    queryFn: () => api.runs(filter),
    refetchInterval: (q) => (q.state.data?.some((r) => runActive(r.status)) ? 3000 : 15_000),
  });
}
