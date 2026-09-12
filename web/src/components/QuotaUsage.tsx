import "./quota.css";
import type { QuotaCycle, UsageSummary } from "../types";
import { useT } from "../i18n";

export function formatUSD(value: number): string {
  return new Intl.NumberFormat(undefined, { style: "currency", currency: "USD", currencyDisplay: "narrowSymbol", maximumFractionDigits: 2 }).format(value);
}

export function formatQuotaDate(value: string, timezone = "Asia/Shanghai"): string {
  const date = new Date(value);
  if (!Number.isFinite(date.getTime())) return "—";
  return new Intl.DateTimeFormat(undefined, {
    timeZone: timezone, month: "2-digit", day: "2-digit", hour: "2-digit", minute: "2-digit", hourCycle: "h23",
  }).format(date);
}

export function QuotaPeriod({ cycle, timezone }: { cycle: QuotaCycle; timezone?: string }) {
  const t = useT();
  const limited = cycle.limit_usd > 0;
  const remaining = Math.max(0, cycle.limit_usd - cycle.used_usd);
  const ratio = limited ? Math.min(100, cycle.used_usd / cycle.limit_usd * 100) : 0;
  return (
    <div className={`quota-period${limited && remaining === 0 ? " quota-exhausted" : ratio >= 80 ? " quota-warning" : ""}`} data-testid={`quota-${cycle.window}`}>
      <div className="quota-title"><span>{t(`quota.${cycle.window}`)}</span><span className="muted">{limited ? t("quota.remaining") : t("usage.unlimited")}</span></div>
      <div className="quota-amount"><strong>{formatUSD(limited ? remaining : cycle.used_usd)}</strong><span>{limited ? `/ ${formatUSD(cycle.limit_usd)}` : t("quota.used")}</span></div>
      {limited && <div className="quota-track" role="meter" aria-label={t(`quota.${cycle.window}`)} aria-valuemin={0} aria-valuemax={cycle.limit_usd} aria-valuenow={Math.min(cycle.used_usd, cycle.limit_usd)}><span style={{ width: `${ratio}%` }} /></div>}
      <div className="quota-date">{t("quota.resetsOn", { date: formatQuotaDate(cycle.resets_at, timezone) })}</div>
    </div>
  );
}

export default function QuotaUsage({ usage, all = false }: { usage: UsageSummary; all?: boolean }) {
  const t = useT();
  const cycles = usage.cycles ?? [];
  const configured = cycles.filter((cycle) => cycle.limit_usd > 0);
  const shown = all ? cycles : configured.length ? configured : cycles.slice(0, 1);
  return <div className="quota-periods">{shown.length ? shown.map((cycle) => <QuotaPeriod key={cycle.window} cycle={cycle} timezone={usage.timezone} />) : <span className="muted">{t("quota.unavailable")}</span>}</div>;
}
