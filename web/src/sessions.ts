// Hilfen rund um Agent-Sessions (ohne Komponenten, für Fast Refresh getrennt).
import { useQuery } from "@tanstack/react-query";
import { api, isActive, type Session } from "./api";

/** Kurzer Titel einer Session: erste Zeile der Aufgabe bzw. der Befehl. */
export function sessionTitle(s: Pick<Session, "task" | "command" | "adapter" | "role">): string {
  if (s.task) {
    const line = s.task.split("\n")[0] ?? "";
    const title = line.length > 90 ? `${line.slice(0, 90)} …` : line;
    // Rollen einer Fabrik (nicht Feedback) vor den Titel: Alle Rollen eines Runs haben dieselbe Aufgabe.
    return s.role && s.role !== "feedback" ? `${s.role}: ${title}` : title;
  }
  if (s.command?.length) return s.command[0] === "sh" && s.command[1] === "-c" ? s.command.slice(2).join(" ") : s.command.join(" ");
  return s.adapter === "pty" ? "Terminal" : s.adapter;
}

/** Fragt Sessions ab – öfter, solange eine davon aktiv ist. */
export function useSessions(filter: Parameters<typeof api.sessions>[0]) {
  return useQuery({
    queryKey: ["sessions", filter],
    queryFn: () => api.sessions(filter),
    refetchInterval: (q) => (q.state.data?.some((s) => isActive(s.status)) ? 3000 : 15_000),
  });
}


/** Arbeitet im Workspace gerade ein Agent (nicht nur wartend)? */
export function agentBusy(sessions: Session[] | undefined): Session | undefined {
  return sessions?.find((x) => x.live && !x.capabilities.terminal && x.status !== "idle");
}
