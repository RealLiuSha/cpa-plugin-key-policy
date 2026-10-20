import { useCallback, useEffect, useState } from "react";
import { Link } from "react-router-dom";
import { fetchOverview } from "../api/overview";
import { extractApiError } from "../api/error";
import { AuditEventCard } from "./Audit";
import { channelPath } from "../navigation";
import { formatCount, formatPercent, formatUSD } from "../format";
import { useT } from "../i18n";
import type { OverviewChannelAttention, OverviewResponse } from "../types";

type Translate = ReturnType<typeof useT>;

function channelReason(item: OverviewChannelAttention, t: Translate): string {
  if (item.reason === "failure_rate") return t("overview.channelReason.failure_rate", { rate: formatPercent(item.value ?? 0) });
  if (item.reason === "degraded" || item.reason === "suspect") {
    return t(`overview.channelReason.${item.reason}`, { model: item.model ?? "", correct: item.correct ?? 0, answered: item.answered ?? 0 });
  }
  return item.reason;
}

// An inspection verdict leads to the inspection page, a failure rate to the
// channel.
const attentionPath = (item: OverviewChannelAttention) => (item.reason === "failure_rate" ? channelPath(item.auth_index) : "/inspection");

function Kpis({ overview }: { overview: OverviewResponse }) {
  const t = useT();
  const today = overview.today;
  const failureShare = today.attempts > 0 ? ` (${formatPercent(today.failures / today.attempts)})` : "";
  const items: [string, string, boolean?][] = [
    ["overview.kpi.requests", formatCount(today.requests)],
    ["overview.kpi.failures", formatCount(today.failures) + failureShare, today.failures > 0],
    ["overview.kpi.billed", formatUSD(today.billed_usd)],
  ];
  return (
    <section className="overview-section" aria-label={t("overview.todayTitle")}>
      <h2 className="usage-section-title">{t("overview.todayTitle")}</h2>
      <div className="stats-kpis overview-kpis" data-testid="overview-kpis">
        {items.map(([key, value, warn]) => (
          <div className={"card stats-kpi" + (warn ? " warn" : "")} key={key}>
            <div className="stats-kpi-label">{t(key)}</div>
            <div className="stats-kpi-value">{value}</div>
          </div>
        ))}
      </div>
    </section>
  );
}

function KeysCard({ keys }: { keys: OverviewResponse["keys"] }) {
  const t = useT();
  return (
    <section className="card overview-card" data-testid="overview-keys">
      <div className="overview-card-head">
        <h2>{t("overview.keysTitle")}</h2>
        <Link to="/keys">{t("overview.allKeys")}</Link>
      </div>
      <p className="muted">{t("overview.keysSummary", { total: keys.total, enabled: keys.enabled, limited: keys.limited, warning: keys.warning })}</p>
      {keys.attention.length === 0 ? <p className="muted">{t("overview.keysClear")}</p> : (
        <ul className="overview-list">
          {keys.attention.map((key) => (
            <li key={key.id}>
              <Link to={`/keys/${encodeURIComponent(key.id)}/usage`}>{key.name || key.id}</Link>
              <span className={`key-status state-${key.status}`}>{t(`quota.status.${key.status}`)}</span>
              {key.blocked_reason && <span className="muted">{t(`quota.reason.${key.blocked_reason}`)}</span>}
            </li>
          ))}
        </ul>
      )}
    </section>
  );
}

function ChannelsCard({ channels }: { channels: OverviewResponse["channels"] }) {
  const t = useT();
  return (
    <section className="card overview-card" data-testid="overview-channels">
      <div className="overview-card-head">
        <h2>{t("overview.channelsTitle")}</h2>
        <Link to="/channels">{t("overview.allChannels")}</Link>
      </div>
      <p className="muted">{t("overview.channelsSummary", { total: channels.total })}</p>
      {channels.attention.length === 0 ? <p className="muted">{t("overview.channelsClear")}</p> : (
        <ul className="overview-list">
          {channels.attention.map((item) => (
            <li key={`${item.auth_index}-${item.reason}-${item.model ?? ""}-${item.effort ?? ""}`}>
              <Link to={attentionPath(item)} className={item.display_name ? "" : "mono"}>{item.display_name || item.auth_index}</Link>
              <span className={`badge ${item.reason === "suspect" ? "warn" : "danger"}`}>{channelReason(item, t)}</span>
            </li>
          ))}
        </ul>
      )}
    </section>
  );
}

function ModulesCard({ overview }: { overview: OverviewResponse }) {
  const t = useT();
  const { search, adapters } = overview.modules;
  const searchState = search.enabled === search.declared ? (search.enabled ? "on" : "off") : "applying";
  return (
    <section className="card overview-card" data-testid="overview-modules">
      <div className="overview-card-head">
        <h2>{t("overview.modulesTitle")}</h2>
        <Link to="/settings">{t("overview.toSettings")}</Link>
      </div>
      <dl className="overview-facts">
        <div><dt><Link to="/search">{t("overview.moduleSearch")}</Link></dt><dd>{t(`overview.searchState.${searchState}`)}</dd></div>
        <div><dt><Link to="/channels">{t("overview.moduleAdapters")}</Link></dt>{adapters ? <dd>{t("overview.adapterCount", { rules: adapters.rules, enabled: adapters.enabled_rules })}</dd> : <dd className="overview-warn">{t("overview.moduleUnavailable")}</dd>}</div>
      </dl>
    </section>
  );
}

function WarningsCard({ warnings }: { warnings: string[] }) {
  const t = useT();
  return (
    <section className="card overview-card" data-testid="overview-warnings">
      <div className="overview-card-head"><h2>{t("overview.warningsTitle")}</h2></div>
      {warnings.length === 0 ? <p className="muted">{t("overview.warningsClear")}</p> : (
        <ul className="notice overview-warnings">{warnings.map((warning) => <li key={warning}>{warning}</li>)}</ul>
      )}
    </section>
  );
}

export default function Overview() {
  const t = useT();
  const [overview, setOverview] = useState<OverviewResponse | null>(null);
  const [error, setError] = useState("");
  const [loading, setLoading] = useState(true);

  const load = useCallback(async (alive: () => boolean = () => true) => {
    setLoading(true);
    setError("");
    try {
      const next = await fetchOverview();
      if (!alive()) return;
      setOverview(next);
    } catch (reason) {
      if (alive()) setError(extractApiError(reason, t("overview.loadFailed")));
    } finally {
      if (alive()) setLoading(false);
    }
  }, [t]);

  useEffect(() => {
    let alive = true;
    void load(() => alive);
    return () => { alive = false; };
  }, [load]);

  return (
    <div className="page overview-page">
      <div className="page-head">
        <div><h1>{t("overview.title")}</h1><p className="muted">{t("overview.description")}</p></div>
        <div className="page-head-actions">
          <button type="button" className="btn" disabled={loading} onClick={() => void load()}>{t(loading ? "quota.refreshing" : "keys.refresh")}</button>
        </div>
      </div>
      {error && (
        <div className="error stats-error" role="alert">
          <span>{error}</span>
          <button type="button" className="btn sm" onClick={() => void load()}>{t("stats.retry")}</button>
        </div>
      )}
      {!overview ? (loading && <div className="card muted" role="status">{t("keys.loading")}</div>) : (
        <>
          <Kpis overview={overview} />
          <div className="overview-grid">
            <KeysCard keys={overview.keys} />
            <ChannelsCard channels={overview.channels} />
            <ModulesCard overview={overview} />
            <WarningsCard warnings={overview.warnings} />
          </div>
          <section className="overview-section" data-testid="overview-changes">
            <div className="overview-card-head">
              <h2 className="usage-section-title">{t("overview.changesTitle")}</h2>
              <Link to="/audit">{t("overview.allChanges")}</Link>
            </div>
            {overview.recent_changes.length === 0 ? <div className="card muted">{t("overview.changesEmpty")}</div> : (
              <div className="audit-list">
                {overview.recent_changes.map((event, index) => <AuditEventCard key={`${event.ts}-${event.action}-${index}`} event={event} />)}
              </div>
            )}
          </section>
        </>
      )}
    </div>
  );
}
