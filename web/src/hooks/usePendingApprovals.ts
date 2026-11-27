import { useQuery } from "@tanstack/react-query";
import { api } from "../api";

/** Offene Freigaben, alle 2 s aktualisiert (gemeinsamer Cache für Badge, Seite und Sessions). */
export function usePendingApprovals() {
  return useQuery({
    queryKey: ["approvals", "pending"],
    queryFn: () => api.approvals({ status: "pending" }),
    refetchInterval: 2000,
  });
}
