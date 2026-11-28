// Helpers around agent sessions (without components, separate for Fast Refresh).
import { useQuery } from "@tanstack/react-query";
import { api, isActive, type Session } from "./api";

/** Short title of a session: first line of the task or the command. */
export function sessionTitle(s: Pick<Session, "task" | "command" | "adapter" | "role">): string {
  if (s.task) {
    const line = s.task.split("\n")[0] ?? "";
    const title = line.length > 90 ? `${line.slice(0, 90)} …` : line;
    // Put the role of a factory (not feedback) before the title: all roles of a run have the same task.
    return s.role && s.role !== "feedback" ? `${s.role}: ${title}` : title;
  }
  if (s.command?.length) return s.command[0] === "sh" && s.command[1] === "-c" ? s.command.slice(2).join(" ") : s.command.join(" ");
  return s.adapter === "pty" ? "Terminal" : s.adapter;
}

/** Queries sessions – more often while one of them is active. */
export function useSessions(filter: Parameters<typeof api.sessions>[0]) {
  return useQuery({
    queryKey: ["sessions", filter],
    queryFn: () => api.sessions(filter),
    refetchInterval: (q) => (q.state.data?.some((s) => isActive(s.status)) ? 3000 : 15_000),
  });
}


/** Is an agent working in the workspace right now (not only waiting)? */
export function agentBusy(sessions: Session[] | undefined): Session | undefined {
  return sessions?.find((x) => x.live && !x.capabilities.terminal && x.status !== "idle");
}
