import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { Link, useSearchParams } from "react-router-dom";
import { fetchChannelData, type ChannelData } from "../api/channels";
import { extractApiError } from "../api/error";
import { groupChannels, mergeChannels, type ChannelRow } from "../channelMerge";
import ChannelDetail, { DETAIL_TABS, type DetailTab } from "../components/ChannelDetail";
import { BalanceValue, ChannelStateBadge, FailurePill } from "../components/ChannelState";
import { ConfirmDialog } from "../components/Modal";
import { formatMillis, formatPercent, formatUSD } from "../format";
import { useT } from "../i18n";
import { useVisiblePolling } from "../polling";
import { setUnsaved } from "../store/unsaved";

export const CHANNEL_REFRESH_MS = 30_000;

// A channel the URL names but the list does not have (yet): the detail still
// loads by its auth_index.
function placeholderRow(authIndex: string): ChannelRow {
  return { key: authIndex, authIndex, origin: "plugin", name: authIndex, provider: "", baseUrl: "", vendor: "", state: "unknown" };
}

function successRate(row: ChannelRow): string {
  const day = row.view?.last_24h;
  return day && day.attempts > 0 ? formatPercent(day.success_rate) : "—";
}

// Latencies of a window without attempts are not measurements.
function latency(row: ChannelRow, field: "latency_p50_ms" | "ttft_p50_ms"): string {
  const day = row.view?.last_24h;
  return day && day.attempts > 0 ? formatMillis(day[field]) : "—";
}

function upstreamToday(row: ChannelRow): string {
  return (row.view?.meta.cost_ratio ?? 0) > 0 ? formatUSD(row.view!.today.upstream_cost_usd) : "—";
}

function ChannelName({ row, search, onOpen }: { row: ChannelRow; search: (authIndex: string) => string; onOpen: (event: React.MouseEvent, authIndex: string) => void }) {
  const t = useT();
  if (!row.authIndex) {
    const hint = row.sharedWith === undefined ? t("channels.noAuthIndex") : t("channels.sharedAuthIndex", { name: row.sharedWith });
    return <div className="channel-name-block"><span className="channel-name">{row.name}</span><div className="field-hint">{hint}</div></div>;
  }
  return <Link className="channel-name" to={{ search: search(row.authIndex) }} onClick={(event) => onOpen(event, row.authIndex)}>{row.name}</Link>;
}

function OriginBadge({ row }: { row: ChannelRow }) {
  const t = useT();
  if (row.origin === "plugin") return null;
  return <span className="badge">{t(`channels.origin.${row.origin}`)}</span>;
}

export default function Channels() {
  const t = useT();
  const [params, setParams] = useSearchParams();
  const selected = params.get("auth_index") ?? "";
  const tab: DetailTab = DETAIL_TABS.find((value) => value === params.get("tab")) ?? "overview";
  const [data, setData] = useState<ChannelData | null>(null);
  const [error, setError] = useState("");
  const [loading, setLoading] = useState(true);
  const [lastUpdated, setLastUpdated] = useState<Date | null>(null);
  // The open channel has changes not saved yet; leaving it asks first.
  // leaving is where the confirmed leave goes: "" closes the detail.
  const [dirty, setDirty] = useState(false);
  const [leaving, setLeaving] = useState<string | null>(null);
  const requestID = useRef(0);
  // The section links ask too.
  useEffect(() => setUnsaved("channels", dirty ? "channels.leaveMessage" : null), [dirty]);
  useEffect(() => () => setUnsaved("channels", null), []);

  const load = useCallback(async () => {
    const id = ++requestID.current;
    setLoading(true);
    try {
      const next = await fetchChannelData();
      if (id !== requestID.current) return;
      setData(next);
      setError("");
      setLastUpdated(new Date());
    } catch (reason) {
      if (id === requestID.current) setError(extractApiError(reason, t("channels.loadFailed")));
    } finally {
      if (id === requestID.current) setLoading(false);
    }
  }, [t]);
  useEffect(() => {
    void load();
    return () => { requestID.current++; };
  }, [load]);
  useVisiblePolling(() => void load(), CHANNEL_REFRESH_MS);

  const rows = useMemo(() => data ? mergeChannels({
    config: data.cpa?.config ?? [],
    authFiles: data.cpa?.authFiles ?? [],
    channels: data.channels,
    cpaListed: data.cpa !== null,
  }) : [], [data]);
  const groups = useMemo(() => groupChannels(rows), [rows]);
  const selectedRow = selected ? rows.find((row) => row.authIndex === selected) ?? (data ? placeholderRow(selected) : undefined) : undefined;

  // The selection and tab live in the URL: the back button closes the
  // detail on phones and other pages can link to a channel.
  const search = (authIndex: string, nextTab: DetailTab = tab) => {
    const next = new URLSearchParams({ auth_index: authIndex });
    if (nextTab !== "overview") next.set("tab", nextTab);
    return `?${next}`;
  };
  const leave = (target: string) => {
    if (dirty) setLeaving(target);
    else setParams(new URLSearchParams(target));
  };
  const open = (event: React.MouseEvent, authIndex: string) => {
    if (!dirty || authIndex === selected) return;
    event.preventDefault();
    setLeaving(search(authIndex));
  };

  return (
    <div className={"page channels-page" + (selectedRow ? " has-selection" : "")}>
      <div className="page-head">
        <div><h1>{t("channels.title")}</h1><p className="muted">{t("channels.description")}</p></div>
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
      {data && data.cpa === null && <p className="notice" role="status">{t("channels.cpaFailed", { error: extractApiError(data.cpaError, t("channels.cpaFailedUnknown")) })}</p>}
      <div className={"channels-layout" + (selectedRow ? " has-detail" : "")}>
        <div className="channel-list">
          {data && <div className="key-list-context"><span>{t("channels.count", { total: rows.length, groups: groups.length })}</span><span>{lastUpdated ? t("keys.lastUpdated", { time: lastUpdated.toLocaleTimeString() }) : ""}</span></div>}
          {!data ? (loading && <div className="card muted" role="status">{t("keys.loading")}</div>) : rows.length === 0 ? (
            <div className="empty-state">{t("channels.empty")}</div>
          ) : groups.map((group) => (
            <section key={group.vendor || "-"} className="channel-group" aria-label={group.vendor || t("channels.noVendor")}>
              <h2 className="usage-section-title">{group.vendor || t("channels.noVendor")} <span className="muted">· {group.rows.length}</span></h2>
              <div className="card table-wrap channel-table">
                <table>
                  <thead>
                    <tr>
                      <th>{t("channels.col.name")}</th>
                      <th>{t("channels.col.type")}</th>
                      <th>{t("channels.col.status")}</th>
                      <th className="num">{t("channels.col.success")}</th>
                      <th className="num">{t("channels.col.latency")}</th>
                      <th className="num">{t("channels.col.ttft")}</th>
                      <th className="num">{t("channels.col.upstreamToday")}</th>
                      <th className="num">{t("channels.col.rules")}</th>
                      <th className="num">{t("channels.col.balance")}</th>
                    </tr>
                  </thead>
                  <tbody>
                    {group.rows.map((row) => (
                      <tr key={row.key} className={row.authIndex && row.authIndex === selected ? "selected" : ""} aria-current={row.authIndex && row.authIndex === selected ? "true" : undefined}>
                        <td className="channel-name-cell"><ChannelName row={row} search={search} onOpen={open} /></td>
                        <td><span className="mono">{row.provider || "—"}</span> <OriginBadge row={row} /></td>
                        <td><div className="channel-badges"><ChannelStateBadge row={row} /><FailurePill summary={row.view?.last_24h} /></div></td>
                        <td className="num mono">{successRate(row)}</td>
                        <td className="num mono">{latency(row, "latency_p50_ms")}</td>
                        <td className="num mono">{latency(row, "ttft_p50_ms")}</td>
                        <td className="num mono">{upstreamToday(row)}</td>
                        <td className="num mono">{row.view?.rules || "—"}</td>
                        <td className="num"><BalanceValue balance={row.view?.balance} /></td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              </div>
              <div className="channel-cards mobile-only">
                {group.rows.map((row) => (
                  <article key={row.key} className="card channel-card">
                    <div className="channel-card-head"><ChannelName row={row} search={search} onOpen={open} /><OriginBadge row={row} /></div>
                    <div className="channel-badges"><ChannelStateBadge row={row} /><FailurePill summary={row.view?.last_24h} /></div>
                    <dl className="channel-card-facts">
                      <div><dt>{t("channels.col.success")}</dt><dd className="mono">{successRate(row)}</dd></div>
                      <div><dt>{t("channels.col.latency")}</dt><dd className="mono">{latency(row, "latency_p50_ms")}</dd></div>
                      <div><dt>{t("channels.col.ttft")}</dt><dd className="mono">{latency(row, "ttft_p50_ms")}</dd></div>
                      {(row.view?.meta.cost_ratio ?? 0) > 0 && <div><dt>{t("channels.col.upstreamToday")}</dt><dd className="mono">{upstreamToday(row)}</dd></div>}
                      {(row.view?.rules ?? 0) > 0 && <div><dt>{t("channels.col.rules")}</dt><dd className="mono">{row.view!.rules}</dd></div>}
                      {row.view?.balance && <div><dt>{t("channels.col.balance")}</dt><dd><BalanceValue balance={row.view.balance} /></dd></div>}
                    </dl>
                  </article>
                ))}
              </div>
            </section>
          ))}
        </div>
        {selectedRow && (
          <ChannelDetail
            key={selectedRow.authIndex}
            row={selectedRow}
            tab={tab}
            onTab={(next) => setParams(new URLSearchParams(search(selectedRow.authIndex, next).slice(1)), { replace: true })}
            onClose={() => leave("")}
            onChanged={() => void load()}
            onDirty={setDirty}
          />
        )}
      </div>
      {leaving !== null && (
        <ConfirmDialog
          title={t("channels.leaveTitle")}
          message={t("channels.leaveMessage")}
          cancelLabel={t("channels.leaveStay")}
          confirmLabel={t("channels.leaveDiscard")}
          onCancel={() => setLeaving(null)}
          onConfirm={() => { setLeaving(null); setParams(new URLSearchParams(leaving)); }}
        />
      )}
    </div>
  );
}
