// Hilfen rund um Fabrik-Runs (ohne Komponenten, für Fast Refresh getrennt).
import { useQuery } from "@tanstack/react-query";
import { api, runActive, type Run } from "./api";

/** Erste Zeile der Aufgabe eines Runs. */
export function runTitle(r: Pick<Run, "task" | "factory">): string {
  const line = (r.task ?? "").split("\n")[0] ?? "";
  if (!line) return r.factory;
  return line.length > 90 ? `${line.slice(0, 90)} …` : line;
}

/** Fragt Runs ab – öfter, solange einer läuft. */
export function useRuns(filter: Parameters<typeof api.runs>[0]) {
  return useQuery({
    queryKey: ["runs", filter],
    queryFn: () => api.runs(filter),
    refetchInterval: (q) => (q.state.data?.some((r) => runActive(r.status)) ? 3000 : 15_000),
  });
}
