import "./quota.css";
import type { KeyPublic, UsageSummary } from "../types";
import { useT } from "../i18n";
import { formatUSD } from "../format";

export function isBalanceKey(key: Pick<KeyPublic, "quota_mode" | "usage">): boolean {
  return (key.quota_mode ?? key.usage.quota_mode) === "balance";
}

// A balance key's money: what is left, the signed sum of all adjustments, and
// what was spent out of it. The balance can be below zero after calls that
// were in flight when it ran out.
export function balanceOf(usage: UsageSummary): { balance: number; credited: number; spent: number } {
  const balance = usage.balance_usd ?? 0;
  const credited = usage.net_credited_usd ?? 0;
  return { balance, credited, spent: Math.max(0, credited - balance) };
}

// Share of the credit already spent, for sorting next to period quotas: a key
// that was never credited counts as used up.
export function balanceSpentRatio(usage: UsageSummary): number {
  const { balance, credited, spent } = balanceOf(usage);
  if (credited <= 0) return balance > 0 ? 0 : 1;
  return Math.min(1, spent / credited);
}

export default function BalanceUsage({ usage }: { usage: UsageSummary }) {
  const t = useT();
  const { balance, credited, spent } = balanceOf(usage);
  const exhausted = balance <= 0;
  const ratio = credited > 0 ? Math.min(100, (spent / credited) * 100) : 0;
  return (
    <div className="quota-periods">
      <div className={`quota-period balance-period${exhausted ? " quota-exhausted" : usage.soft_limit_hit ? " quota-warning" : ""}`} data-testid="balance-usage">
        <div className="quota-title"><span>{t("balance.title")}</span><span className="muted">{exhausted ? t("balance.exhausted") : t("quota.remaining")}</span></div>
        <div className="quota-amount"><strong>{formatUSD(balance)}</strong>{credited > 0 && <span>/ {formatUSD(credited)}</span>}</div>
        {credited > 0 && <div className="quota-track" role="meter" aria-label={t("balance.spentMeter")} aria-valuemin={0} aria-valuemax={credited} aria-valuenow={Math.min(spent, credited)}><span style={{ width: `${ratio}%` }} /></div>}
        <div className="quota-date">{credited > 0 ? t("balance.summary", { credited: formatUSD(credited), spent: formatUSD(spent) }) : t("balance.noCredit")}</div>
      </div>
    </div>
  );
}
