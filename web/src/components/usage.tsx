import type { UsageTotal } from "../api";
import { formatTokens, formatUSD } from "../format";

/** Kosten einer Summe mit Tokens als Tooltip; „*“ markiert unvollständige Preise. */
export function UsageAmount({ total, empty = "–" }: { total?: UsageTotal; empty?: string }) {
  if (!total || total.count === 0) return <span className="muted">{empty}</span>;
  const input = total.input_tokens + total.cache_read_tokens + total.cache_write_tokens;
  return (
    <span title={`${formatTokens(input)} Tokens ein, ${formatTokens(total.output_tokens)} aus${total.unpriced ? " – für ein Modell ist kein Preis bekannt" : ""}`}>
      {formatUSD(total.cost_usd)}
      {total.unpriced ? " *" : ""}
    </span>
  );
}

/** Monatskosten eines Projekts mit Budget-Balken. */
export function MonthlyBudget({ spent, budget }: { spent: number; budget?: number }) {
  if (!budget) return null;
  const ratio = Math.min(spent / budget, 1);
  return (
    <>
      <span className="muted"> von {formatUSD(budget)} Monatsbudget</span>
      <div className="budgetbar" role="meter" aria-valuemin={0} aria-valuemax={budget} aria-valuenow={spent} aria-label="Monatsbudget">
        <span className={ratio >= 1 ? "over" : ratio >= 0.8 ? "near" : ""} style={{ width: `${ratio * 100}%` }} />
      </div>
    </>
  );
}
