import { keyListReturnPath } from "../navigation";
import QuotaUsage from "../components/QuotaUsage";
import BalanceUsage from "../components/BalanceUsage";
import BalanceDialog from "../components/BalanceDialog";
import BalanceHistory from "../components/BalanceHistory";
import UnpricedBadge from "../components/UnpricedBadge";
import UsageChart from "../components/UsageChart";
import { useEffect, useState } from "react";
import { Link, useLocation, useParams } from "react-router-dom";
import { fetchKeyHistory, fetchKeyUsage } from "../api/keys";
import { extractApiError } from "../api/error";
import { formatCount, formatHitRate, formatUSD } from "../format";
import type { KeyHistoryResponse, KeyUsageResponse, ModelUsageEntry, UsageBucket, UsageHistoryDay, UsageWindow } from "../types";
import { useT } from "../i18n";

// Window switch for the per-model breakdown table: each model row has its own
// daily, 7-day and 30-day quota periods; the selected period applies to all rows.
type Window = "daily" | "weekly" | "monthly";

export const HISTORY_RANGES = [7, 30, 90] as const;
export type HistoryRange = (typeof HISTORY_RANGES)[number];
export const DEFAULT_HISTORY_RANGE: HistoryRange = 30;

const BUCKET_FIELDS = [
  "total_usd", "call_count", "unpriced_calls", "input_tokens", "output_tokens",
  "cache_read_tokens", "cache_cost_usd", "cache_write_tokens", "cache_write_usd",
] as const satisfies readonly (keyof UsageBucket)[];

export interface ModelRangeTotal {
  name: string;
  usage: Required<UsageBucket>;
}

// The range's per-model totals, summed from each day's by_model buckets;
// the most expensive model first.
export function sumHistoryByModel(days: UsageHistoryDay[]): ModelRangeTotal[] {
  const totals = new Map<string, Required<UsageBucket>>();
  for (const day of days) {
    for (const [name, bucket] of Object.entries(day.by_model ?? {})) {
      const sum = totals.get(name) ?? Object.fromEntries(BUCKET_FIELDS.map((field) => [field, 0])) as Required<UsageBucket>;
      for (const field of BUCKET_FIELDS) sum[field] += bucket[field] ?? 0;
      totals.set(name, sum);
    }
  }
  return [...totals].map(([name, usage]) => ({ name, usage }))
    .sort((a, b) => b.usage.total_usd - a.usage.total_usd || a.name.localeCompare(b.name));
}

// Billing-mode tag, reusing the existing .tag styling. Per-call rows use a
// distinct tint so they're scannable at a glance.
function BillingTag({ mode }: { mode?: string }) {
  const t = useT();
  const perCall = mode === "per_call";
  return (
    <span className={"tag " + (perCall ? "off" : "on")} style={perCall ? { color: "var(--accent)", borderColor: "var(--accent-ring)", background: "var(--accent-soft)" } : undefined}>
      {perCall ? t("keyUsage.billingPerCall") : t("keyUsage.billingTokens")}
    </span>
  );
}

function ModelRangeTable({ models, days }: { models: ModelRangeTotal[]; days: number }) {
  const t = useT();
  const maxUsd = Math.max(1, ...models.map((model) => model.usage.total_usd));
  return (
    <section className="usage-range-models" data-testid="usage-range-models">
      <h2 className="usage-section-title">{t("keyUsage.rangeModelsTitle", { days })}</h2>
      <div className="card table-wrap">
        <table>
          <thead>
            <tr>
              <th>{t("keyUsage.colModel")}</th>
              <th className="num">{t("keyUsage.colUsd")}</th>
              <th className="num">{t("keyUsage.colCalls")}</th>
              <th className="num">{t("keyUsage.colInput")}</th>
              <th className="num">{t("keyUsage.colOutput")}</th>
              <th className="num">{t("keyUsage.colCacheRead")}</th>
              <th className="num">{t("keyUsage.colCacheWrite")}</th>
              <th className="num">{t("keyUsage.colHitRate")}</th>
            </tr>
          </thead>
          <tbody>
            {models.length === 0 ? (
              <tr><td colSpan={8} className="muted keyusage-no-model">{t("keyUsage.rangeEmpty")}</td></tr>
            ) : models.map(({ name, usage }) => (
              <tr key={name}>
                <td>
                  <div className="mono">{name}</div>
                  {usage.unpriced_calls > 0 && <UnpricedBadge calls={usage.unpriced_calls} />}
                </td>
                <td className="num strong">{formatUSD(usage.total_usd)}</td>
                <td className="num mono">{formatCount(usage.call_count)}</td>
                <td className="num mono">{formatCount(usage.input_tokens)}</td>
                <td className="num mono">{formatCount(usage.output_tokens)}</td>
                <td className="num mono">{formatCount(usage.cache_read_tokens)} / {formatUSD(usage.cache_cost_usd)}</td>
                <td className="num mono">{formatCount(usage.cache_write_tokens)} / {formatUSD(usage.cache_write_usd)}</td>
                <td className="num mono">{formatHitRate(usage.cache_read_tokens, usage.input_tokens)}</td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
      <div className="mobile-only">
        <div className="bar-rank">
          {models.length === 0 ? <div className="muted">{t("keyUsage.rangeEmpty")}</div> : models.map(({ name, usage }) => (
            <div key={name} className="br-row">
              <div className="br-top">
                <span className="br-name">{name}</span>
                <span className="br-usd">{formatUSD(usage.total_usd)}</span>
              </div>
              <div className="br-bar"><span style={{ width: Math.max(2, (usage.total_usd / maxUsd) * 100) + "%" }} /></div>
              <div className="br-cap">
                {t("keys.mobile.callCount", { n: formatCount(usage.call_count) })} · {t("keyUsage.mobile.inputTok")} {formatCount(usage.input_tokens)} · {t("keyUsage.mobile.outputTok")} {formatCount(usage.output_tokens)}
                {usage.unpriced_calls > 0 && <> <UnpricedBadge calls={usage.unpriced_calls} /></>}
              </div>
            </div>
          ))}
        </div>
      </div>
    </section>
  );
}

// The 7/30/90-day history card and the range's per-model totals. Switching the
// range reloads only the history; a failure stays inside this section.
function UsageHistorySection({ keyId, dailyLimit }: { keyId: string; dailyLimit: number }) {
  const t = useT();
  const [range, setRange] = useState<HistoryRange>(DEFAULT_HISTORY_RANGE);
  const [loaded, setLoaded] = useState<{ range: HistoryRange; history: KeyHistoryResponse } | null>(null);
  const [error, setError] = useState("");

  useEffect(() => {
    let alive = true;
    setError("");
    fetchKeyHistory(keyId, range)
      .then((history) => { if (alive) setLoaded({ range, history }); })
      .catch((reason) => { if (alive) setError(extractApiError(reason, t("keyUsage.historyLoadFailed"))); });
    return () => { alive = false; };
  }, [keyId, range, t]);

  const days = loaded?.history.days ?? [];
  const shownRange = loaded?.range ?? range;
  const total = days.reduce((sum, day) => sum + (day.total_usd ?? 0), 0);
  return (
    <>
      <section className="card usage-history-card" data-testid="usage-history-chart">
        <div className="usage-history-title">
          <div>
            <strong>{t("keyUsage.historyTitle", { days: shownRange })}</strong>
            <span className="muted"> · {t("keyUsage.historyTotal", { amount: formatUSD(total) })} · {loaded?.history.timezone ?? "Asia/Shanghai"}</span>
          </div>
          <div className="seg usage-range-switch" role="group" aria-label={t("keyUsage.rangeLabel")}>
            {HISTORY_RANGES.map((option) => (
              <button key={option} type="button" className={"seg-btn" + (range === option ? " active" : "")} aria-pressed={range === option} onClick={() => setRange(option)}>
                {t("usage.rangeDays", { days: option })}
              </button>
            ))}
          </div>
        </div>
        {error ? <div className="error" role="alert">{error}</div> : loaded === null ? <div className="muted" role="status">{t("keyUsage.loading")}</div> : (
          <UsageChart
            points={days.map((day) => ({ date: day.date, usd: day.total_usd ?? 0, calls: day.call_count ?? 0 }))}
            label={t("keyUsage.historyTitle", { days: days.length })}
            limit={{ usd: dailyLimit, label: t("keyUsage.dailyLimitLine", { amount: formatUSD(dailyLimit) }) }}
          />
        )}
        {loaded !== null && loaded.range !== range && !error && <div className="muted" role="status">{t("keyUsage.loading")}</div>}
      </section>
      {loaded !== null && <ModelRangeTable models={sumHistoryByModel(days)} days={shownRange} />}
    </>
  );
}

export default function KeyUsage() {
  const { id } = useParams<{ id: string }>();
  const location = useLocation();
  const backTo = keyListReturnPath(location.state);
  // Set when a new balance key was created but its initial balance failed.
  const notice = (location.state as { notice?: unknown } | null)?.notice;
  const t = useT();
  const [data, setData] = useState<KeyUsageResponse | null>(null);
  const [error, setError] = useState("");
  const [loading, setLoading] = useState(true);
  const [win, setWin] = useState<Window>("daily");
  const [showBalance, setShowBalance] = useState(false);
  const [version, setVersion] = useState(0);
  const keyId = decodeURIComponent(id ?? "");

  useEffect(() => {
    let alive = true;
    (async () => {
      // A reload after a balance adjustment keeps the page on screen.
      if (version === 0) setLoading(true);
      setError("");
      try {
        if (!keyId) {
          setError(t("keyUsage.notFound"));
          return;
        }
        const usage = await fetchKeyUsage(keyId);
        if (alive) setData(usage);
      } catch (e) {
        if (alive) setError(extractApiError(e, t("keyUsage.loadFailed")));
      } finally {
        if (alive) setLoading(false);
      }
    })();
    return () => {
      alive = false;
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [keyId, version]);

  if (loading) return <div className="muted">{t("keyUsage.loading")}</div>;
  if (error || !data) return <div className="error">{error || t("keyUsage.notFound")}</div>;

  const models = data.models ?? [];
  const prepaid = data.usage.quota_mode === "balance";
  const windowOf = (model: ModelUsageEntry): UsageWindow => (
    win === "daily" ? model.daily : win === "weekly" ? model.weekly : model.monthly
  );

  // Mobile hero totals: sum across models for the active window.
  const heroUsd = models.reduce((sum, model) => sum + (windowOf(model).total_usd ?? 0), 0);
  const heroCalls = models.reduce((sum, model) => sum + (windowOf(model).call_count ?? 0), 0);
  const heroInput = models.reduce((sum, model) => sum + (windowOf(model).input_tokens ?? 0), 0);
  const heroOutput = models.reduce((sum, model) => sum + (windowOf(model).output_tokens ?? 0), 0);
  const heroLimit = win === "daily"
    ? data.daily_limit_usd
    : win === "weekly" ? data.weekly_limit_usd : data.monthly_limit_usd;
  const heroPct = heroLimit > 0 ? Math.min(100, (heroUsd / heroLimit) * 100) : 0;
  const maxModelUsd = Math.max(1, ...models.map((model) => windowOf(model).total_usd ?? 0));

  return (
    <div>
      {/* Header: back · key id (mono) · name · three-window toggle */}
      <div className="keyusage-header">
        <div className="keyusage-idline">
          <Link to={backTo}>
            <button className="btn sm">{t("keyUsage.back")}</button>
          </Link>
          <span className="mono keyusage-id">{data.key_id}</span>
          <span className="muted">{data.key_name}</span>
          <Link
            to={`/keys/${encodeURIComponent(data.key_id)}/edit`}
            className="mobile-only keyusage-edit-link"
          >
            <button type="button" className="btn sm">{t("keys.edit")}</button>
          </Link>
        </div>
        <div className="seg" role="tablist" aria-label={t("keyUsage.windowToggle")}>
          <button
            role="tab"
            aria-selected={win === "daily"}
            className={"seg-btn " + (win === "daily" ? "active" : "")}
            onClick={() => setWin("daily")}
          >
            {t("keyUsage.tabDaily")}
          </button>
          <button
            role="tab"
            aria-selected={win === "weekly"}
            className={"seg-btn " + (win === "weekly" ? "active" : "")}
            onClick={() => setWin("weekly")}
          >
            {t(prepaid ? "balance.tabWeekly" : "keyUsage.tabWeekly")}
          </button>
          <button
            role="tab"
            aria-selected={win === "monthly"}
            className={"seg-btn " + (win === "monthly" ? "active" : "")}
            onClick={() => setWin("monthly")}
          >
            {t(prepaid ? "balance.tabMonthly" : "keyUsage.tabMonthly")}
          </button>
        </div>
      </div>

      {typeof notice === "string" && notice && <div className="notice warn" role="alert">{notice}</div>}
      {prepaid ? (
        <section className="card quota-detail balance-detail">
          <div className="balance-detail-head"><h2>{t("balance.current")}</h2><button type="button" className="btn sm primary" onClick={() => setShowBalance(true)}>{t("balance.adjust")}</button></div>
          <BalanceUsage usage={data.usage} />
          <BalanceHistory keyId={data.key_id} version={version} />
        </section>
      ) : <section className="card quota-detail"><h2>{t("quota.currentPeriods")}</h2><QuotaUsage usage={data.usage} all /></section>}
      {showBalance && <BalanceDialog keyId={data.key_id} onClose={() => setShowBalance(false)} onComplete={() => setVersion((value) => value + 1)} />}
      <UsageHistorySection keyId={data.key_id} dailyLimit={data.daily_limit_usd} />

      <h2 className="usage-section-title mobile-hidden">{t(prepaid ? "balance.periodModelsTitle" : "keyUsage.periodModelsTitle")}</h2>
      <div className="card table-wrap">
        <table>
          <thead>
            <tr>
              <th>{t("keyUsage.colModel")}</th>
              <th>{t("keyUsage.colBillingMode")}</th>
              <th className="num">{t("keyUsage.colUsd")}</th>
              <th className="num">{t("keyUsage.colCalls")}</th>
              <th className="num">{t("keyUsage.colInput")}</th>
              <th className="num">{t("keyUsage.colOutput")}</th>
              <th className="num">{t("keyUsage.colCacheRead")}</th>
              <th className="num">{t("keyUsage.colCacheWrite")}</th>
              <th className="num">{t("keyUsage.colHitRate")}</th>
            </tr>
          </thead>
          <tbody>
            {models.length === 0 ? (
              <tr>
                <td colSpan={9} className="muted keyusage-no-model">
                  {t("keyUsage.noModel")}
                </td>
              </tr>
            ) : (
              models.map((model) => {
                const w = windowOf(model);
                return (
                  <tr key={model.name}>
                    <td>
                      <div className="mono">{model.name}</div>
                      {!model.priced && <UnpricedBadge />}
                    </td>
                    <td>
                      <BillingTag mode={model.billing_mode} />
                    </td>
                    <td className="num strong">{formatUSD(w.total_usd ?? 0)}</td>
                    <td className="num mono">{formatCount(w.call_count ?? 0)}</td>
                    <td className="num mono">{formatCount(w.input_tokens ?? 0)}</td>
                    <td className="num mono">{formatCount(w.output_tokens ?? 0)}</td>
                    <td className="num mono">{formatCount(w.cache_read_tokens ?? 0)} / {formatUSD(w.cache_cost_usd ?? 0)}</td>
                    <td className="num mono">{formatCount(w.cache_write_tokens ?? 0)} / {formatUSD(w.cache_write_usd ?? 0)}</td>
                    <td className="num mono">{formatHitRate(w.cache_read_tokens ?? 0, w.input_tokens ?? 0)}</td>
                  </tr>
                );
              })
            )}
          </tbody>
          {models.length > 1 && (
            <tfoot>
              <tr className="keyusage-total">
                <td>{t("keyUsage.total")}</td>
                <td />
                <td className="num strong">{formatUSD(heroUsd)}</td>
                <td className="num mono">{formatCount(heroCalls)}</td>
                <td className="num mono">{formatCount(heroInput)}</td>
                <td className="num mono">{formatCount(heroOutput)}</td>
                <td colSpan={3} />
              </tr>
            </tfoot>
          )}
        </table>
      </div>

      {/* Mobile: hero card + horizontal bar ranking */}
      <div className="mobile-only">
        <div className="usage-hero">
          <div className="uh-label">
            {win === "daily" ? t("keyUsage.mobile.today") : win === "weekly" ? t("keyUsage.mobile.last7Days") : t("keyUsage.mobile.last30Days")}
          </div>
          <div className="uh-amount">{formatUSD(heroUsd)}<span className="uh-unit">USD</span></div>
          <div className="uh-row">
            <div className="uh-ring">
              <svg width="64" height="64" viewBox="0 0 64 64">
                <circle cx="32" cy="32" r="26" fill="none" stroke="var(--muted-bg)" strokeWidth="6" />
                <circle cx="32" cy="32" r="26" fill="none" stroke="var(--accent)" strokeWidth="6"
                  strokeLinecap="round"
                  strokeDasharray={`${(heroPct / 100) * 163.36} 163.36`} />
              </svg>
              <span className="uh-pct">{Math.round(heroPct)}%</span>
            </div>
            <div className="uh-limits">
              <div><span className="uh-lk">{t("keyUsage.mobile.limit")}</span> <span className="uh-lv">{heroLimit > 0 ? formatUSD(heroLimit) : t("keyUsage.mobile.noLimit")}</span></div>
              <div><span className="uh-lk">{t("keyUsage.mobile.remaining")}</span> <span className="uh-lv">{heroLimit > 0 ? formatUSD(Math.max(0, heroLimit - heroUsd)) : "—"}</span></div>
            </div>
          </div>
          <div className="uh-stats">
            <span>{t("keyUsage.mobile.calls")} {formatCount(heroCalls)}</span>
            <span>{t("keyUsage.mobile.inputTok")} {formatCount(heroInput)}</span>
            <span>{t("keyUsage.mobile.outputTok")} {formatCount(heroOutput)}</span>
          </div>
        </div>

        <div className="section-label">{t("keyUsage.periodModelsTitle")}</div>
        <div className="bar-rank">
          {models.length === 0 ? (
            <div className="muted">{t("keyUsage.noModel")}</div>
          ) : models.map((model) => {
            const w = windowOf(model);
            const usd = w.total_usd ?? 0;
            const w2 = Math.max(2, (usd / maxModelUsd) * 100);
            const perCall = model.billing_mode === "per_call";
            return (
              <div key={model.name} className="br-row">
                <div className="br-top">
                  <span className="br-name">{model.name}</span>
                  <span className="br-usd">{formatUSD(usd)}</span>
                </div>
                <div className="br-bar"><span style={{ width: w2 + "%" }} /></div>
                <div className="br-cap">
                  {perCall ? t("keyUsage.billingPerCall") : t("keyUsage.billingTokens")} · {t("keys.mobile.callCount", { n: formatCount(w.call_count ?? 0) })}
                  {!model.priced && <> <UnpricedBadge /></>}
                </div>
              </div>
            );
          })}
        </div>
      </div>
    </div>
  );
}
