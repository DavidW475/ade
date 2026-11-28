import type { UsageTotal } from "../api";
import { formatTokens, formatUSD } from "../format";

/** Costs of a sum with tokens as tooltip; “*” marks incomplete prices. */
export function UsageAmount({ total, empty = "–" }: { total?: UsageTotal; empty?: string }) {
  if (!total || total.count === 0) return <span className="muted">{empty}</span>;
  const input = total.input_tokens + total.cache_read_tokens + total.cache_write_tokens;
  return (
    <span title={`${formatTokens(input)} tokens in, ${formatTokens(total.output_tokens)} out${total.unpriced ? " – no price is known for a model" : ""}`}>
      {formatUSD(total.cost_usd)}
      {total.unpriced ? " *" : ""}
    </span>
  );
}

/** Monthly costs of a project with a budget bar. */
export function MonthlyBudget({ spent, budget }: { spent: number; budget?: number }) {
  if (!budget) return null;
  const ratio = Math.min(spent / budget, 1);
  return (
    <>
      <span className="muted"> of {formatUSD(budget)} monthly budget</span>
      <div className="budgetbar" role="meter" aria-valuemin={0} aria-valuemax={budget} aria-valuenow={spent} aria-label="Monthly budget">
        <span className={ratio >= 1 ? "over" : ratio >= 0.8 ? "near" : ""} style={{ width: `${ratio * 100}%` }} />
      </div>
    </>
  );
}
