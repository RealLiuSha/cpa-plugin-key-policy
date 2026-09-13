import { keyListReturnPath } from "../navigation";
import QuotaUsage from "../components/QuotaUsage";
import { useEffect, useState } from "react";
import { Link, useLocation, useParams } from "react-router-dom";
import { fetchKeyHistory, fetchKeyUsage } from "../api/keys";
import { extractApiError } from "../api/error";
import type { KeyHistoryResponse, KeyUsageResponse, ModelUsageEntry, UsageWindow } from "../types";
import { useT } from "../i18n";
import { MobileTabBar } from "../components/MobileChrome";

// Window switch for the per-model breakdown table: each model row has its own
// daily, 7-day and 30-day quota periods; the selected period applies to all rows.
type Window = "daily" | "weekly" | "monthly";

function fmtUsd(n: number): string {
  return "$" + (Number.isFinite(n) ? n.toFixed(2) : "0.00");
}

// Compact integer formatting with thousands separators. 0 shows as "0".
function fmtInt(n: number): string {
  if (!n || n <= 0) return "0";
  return Math.round(n).toLocaleString("en-US");
}

// Hit-rate = cacheRead / (cacheRead + input), expressed as a percentage.
// Returns "—" when there's no input activity for the window (avoid 0/0).
function hitRate(w: UsageWindow): string {
  const cr = w.cache_read_tokens ?? 0;
  const inp = w.input_tokens ?? 0;
  const denom = cr + inp;
  if (denom <= 0) return "—";
  return Math.round((cr / denom) * 100) + "%";
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

export function UsageHistoryChart({ history, dailyLimit }: { history: KeyHistoryResponse | null; dailyLimit: number }) {
  const t = useT();
  const days = history?.days ?? [];
  const width = 640;
  const height = 180;
  const plotTop = 16;
  const plotBottom = 150;
  const plotHeight = plotBottom - plotTop;
  const maxValue = Math.max(1, dailyLimit, ...days.map((day) => day.total_usd ?? 0));
  const slot = days.length > 0 ? width / days.length : width;
  const limitY = plotBottom - (dailyLimit / maxValue) * plotHeight;
  return (
    <div className="card usage-history-card" data-testid="usage-history-chart">
      <div className="usage-history-title">
        <strong>{t("keyUsage.historyTitle")}</strong>
        <span className="muted">{history?.timezone ?? "Asia/Shanghai"}</span>
      </div>
      <svg viewBox={`0 0 ${width} ${height}`} role="img" aria-label={t("keyUsage.historyTitle")}>
        <line x1="0" x2={width} y1={plotBottom} y2={plotBottom} className="usage-chart-axis" />
        {dailyLimit > 0 && (
          <line x1="0" x2={width} y1={limitY} y2={limitY} className="usage-chart-limit" data-testid="usage-history-limit-line" />
        )}
        {days.map((day, index) => {
          const value = day.total_usd ?? 0;
          const barHeight = (value / maxValue) * plotHeight;
          return (
            <rect
              key={day.date}
              x={index * slot + slot * 0.16}
              y={plotBottom - barHeight}
              width={Math.max(1, slot * 0.68)}
              height={barHeight}
              className="usage-chart-bar"
            >
              <title>{day.date}: {fmtUsd(value)}</title>
            </rect>
          );
        })}
      </svg>
    </div>
  );
}

export default function KeyUsage() {
  const { id } = useParams<{ id: string }>();
  const backTo = keyListReturnPath(useLocation().state);
  const t = useT();
  const [data, setData] = useState<KeyUsageResponse | null>(null);
  const [error, setError] = useState("");
  const [loading, setLoading] = useState(true);
  const [win, setWin] = useState<Window>("daily");
  const [history, setHistory] = useState<KeyHistoryResponse | null>(null);

  useEffect(() => {
    let alive = true;
    (async () => {
      setLoading(true);
      setError("");
      try {
        const keyId = decodeURIComponent(id ?? "");
        if (!keyId) {
          setError(t("keyUsage.notFound"));
          return;
        }
        const [usage, usageHistory] = await Promise.all([
          fetchKeyUsage(keyId),
          fetchKeyHistory(keyId, 30),
        ]);
        if (alive) {
          setData(usage);
          setHistory(usageHistory);
        }
      } catch (e) {
        setError(extractApiError(e, t("keyUsage.loadFailed")));
      } finally {
        if (alive) setLoading(false);
      }
    })();
    return () => {
      alive = false;
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [id]);

  if (loading) return <div className="muted">{t("keyUsage.loading")}</div>;
  if (error || !data) return <div className="error">{error || t("keyUsage.notFound")}</div>;

  const models = data.models ?? [];
  const hasUsage = models.some((model) =>
    (model.daily.call_count ?? 0) > 0
    || (model.weekly.call_count ?? 0) > 0
    || (model.monthly?.call_count ?? 0) > 0
    || (model.daily.total_usd ?? 0) > 0
    || (model.weekly.total_usd ?? 0) > 0
    || (model.monthly?.total_usd ?? 0) > 0,
  );

  const windowOf = (model: ModelUsageEntry): UsageWindow => (
    win === "daily" ? model.daily : win === "weekly" ? model.weekly : (model.monthly ?? { total_usd: 0 })
  );

  // Mobile hero totals: sum across public models for the active window.
  const heroUsd = models.reduce((sum, model) => sum + (windowOf(model).total_usd ?? 0), 0);
  const heroCalls = models.reduce((sum, model) => sum + (windowOf(model).call_count ?? 0), 0);
  const heroInput = models.reduce((sum, model) => sum + (windowOf(model).input_tokens ?? 0), 0);
  const heroOutput = models.reduce((sum, model) => sum + (windowOf(model).output_tokens ?? 0), 0);
  const heroLimit = win === "daily"
    ? data.daily_limit_usd
    : win === "weekly" ? data.weekly_limit_usd : (data.monthly_limit_usd ?? 0);
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
            {t("keyUsage.tabWeekly")}
          </button>
          <button
            role="tab"
            aria-selected={win === "monthly"}
            className={"seg-btn " + (win === "monthly" ? "active" : "")}
            onClick={() => setWin("monthly")}
          >
            {t("keyUsage.tabMonthly")}
          </button>
        </div>
      </div>

      {data.usage && <section className="card quota-detail"><h2>{t("quota.currentPeriods")}</h2><QuotaUsage usage={data.usage} all /></section>}
      <UsageHistoryChart history={history} dailyLimit={data.daily_limit_usd} />

      {/* Desktop: hero summary + per-model table. */}
      <div className="usage-hero-d">
        <div className="uhd-tiles">
          <div className="uhd-tile">
            <span className="uhd-tk">
              {win === "daily" ? t("keyUsage.mobile.todaySpend") : win === "weekly" ? t("keyUsage.mobile.weekSpend") : t("keyUsage.mobile.monthSpend")}
            </span>
            <span className={"uhd-tv" + (heroLimit > 0 && heroUsd >= heroLimit ? " accent" : "")}>{fmtUsd(heroUsd)}</span>
          </div>
          <div className="uhd-tile">
            <span className="uhd-tk">{t("keyUsage.colCalls")}</span>
            <span className="uhd-tv">{fmtInt(heroCalls)}</span>
          </div>
          <div className="uhd-tile">
            <span className="uhd-tk">{t("keyUsage.mobile.limit")}</span>
            <span className="uhd-tv">{heroLimit > 0 ? fmtUsd(heroLimit) : t("keyUsage.mobile.noLimit")}</span>
          </div>
        </div>
        {heroLimit > 0 && (
          <>
            <div className={"uhd-bar" + (heroUsd >= heroLimit ? " over" : "")}>
              <span style={{ width: Math.min(100, heroPct) + "%" }} />
            </div>
            <div className="uhd-barcap">
              <span>{fmtUsd(heroUsd)} / {fmtUsd(heroLimit)}</span>
              <span className={heroUsd >= heroLimit ? "over" : ""}>{Math.round(heroPct)}%</span>
            </div>
          </>
        )}
      </div>

      <div className="card table-wrap">
        {!hasUsage && <div className="muted keyusage-empty">{t("keyUsage.empty")}</div>}
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
                  <tr key={model.name} className={model.in_config ? "" : "keyusage-residual"}>
                    <td>
                      <div className="mono">{model.name}</div>
                      {!model.in_config && <span className="keyusage-badge">{t("keyUsage.notInConfig")}</span>}
                    </td>
                    <td>
                      <BillingTag mode={model.billing_mode} />
                    </td>
                    <td className="num strong">{fmtUsd(w.total_usd ?? 0)}</td>
                    <td className="num mono">{fmtInt(w.call_count ?? 0)}</td>
                    <td className="num mono">{fmtInt(w.input_tokens ?? 0)}</td>
                    <td className="num mono">{fmtInt(w.output_tokens ?? 0)}</td>
                    <td className="num mono">{fmtInt(w.cache_read_tokens ?? 0)} / {fmtUsd(w.cache_cost_usd ?? 0)}</td>
                    <td className="num mono">{fmtInt(w.cache_write_tokens ?? 0)} / {fmtUsd(w.cache_write_usd ?? 0)}</td>
                    <td className="num mono">{hitRate(w)}</td>
                  </tr>
                );
              })
            )}
          </tbody>
        </table>
      </div>

      {/* Mobile: hero card + horizontal bar ranking */}
      <div className="mobile-only">
        <div className="usage-hero">
          <div className="uh-label">
            {win === "daily" ? t("keyUsage.mobile.today") : win === "weekly" ? t("keyUsage.mobile.last7Days") : t("keyUsage.mobile.last30Days")}
          </div>
          <div className="uh-amount">{fmtUsd(heroUsd)}<span className="uh-unit">USD</span></div>
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
              <div><span className="uh-lk">{t("keyUsage.mobile.limit")}</span> <span className="uh-lv">{heroLimit > 0 ? fmtUsd(heroLimit) : t("keyUsage.mobile.noLimit")}</span></div>
              <div><span className="uh-lk">{t("keyUsage.mobile.remaining")}</span> <span className="uh-lv">{heroLimit > 0 ? fmtUsd(Math.max(0, heroLimit - heroUsd)) : "—"}</span></div>
            </div>
          </div>
          <div className="uh-stats">
            <span>{t("keyUsage.mobile.calls")} {fmtInt(heroCalls)}</span>
            <span>{t("keyUsage.mobile.inputTok")} {fmtInt(heroInput)}</span>
            <span>{t("keyUsage.mobile.outputTok")} {fmtInt(heroOutput)}</span>
          </div>
        </div>

        <div className="section-label">{t("keyUsage.mobile.byModel")}</div>
        <div className="bar-rank">
          {models.length === 0 ? (
            <div className="muted">{t("keyUsage.noModel")}</div>
          ) : models.map((model) => {
            const w = windowOf(model);
            const usd = w.total_usd ?? 0;
            const w2 = Math.max(2, (usd / maxModelUsd) * 100);
            const perCall = model.billing_mode === "per_call";
            return (
              <div key={model.name} className={"br-row" + (model.in_config ? "" : " br-residual")}>
                <div className="br-top">
                  <span className="br-name">{model.name}{!model.in_config && <span className="br-badge">!</span>}</span>
                  <span className="br-usd">{fmtUsd(usd)}</span>
                </div>
                <div className="br-bar"><span style={{ width: w2 + "%" }} /></div>
                <div className="br-cap">
                  {perCall ? t("keyUsage.billingPerCall") : t("keyUsage.billingTokens")} · {t("keys.mobile.callCount", { n: fmtInt(w.call_count ?? 0) })}
                </div>
              </div>
            );
          })}
        </div>
      </div>

      <MobileTabBar
        active="usage"
        showUsage
        usagePath={`/keys/${encodeURIComponent(data.key_id)}/usage`}
      />
    </div>
  );
}
