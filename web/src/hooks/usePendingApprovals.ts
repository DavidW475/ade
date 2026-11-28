import { useQuery } from "@tanstack/react-query";
import { api } from "../api";

/** Pending approvals, refreshed every 2 s (shared cache for badge, page and sessions). */
export function usePendingApprovals() {
  return useQuery({
    queryKey: ["approvals", "pending"],
    queryFn: () => api.approvals({ status: "pending" }),
    refetchInterval: 2000,
  });
}
