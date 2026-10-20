import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { Link, useSearchParams } from "react-router-dom";
import { fetchChannelData, type ChannelData } from "../api/channels";
import { extractApiError } from "../api/error";
import { groupChannels, mergeChannels, type ChannelRow } from "../channelMerge";
import { ALL, canonicalModel, channelTypes, filterChannels, modelCounts, useChannelModels, type ChannelModels } from "../channelModels";
import { formatMetric, formatMetricValue, isLow, MIN_COMPARED_ATTEMPTS, modelSummary, peerMedians, QUALITY_METRICS, type QualityMetric } from "../modelQuality";
import ChannelDetail, { DETAIL_TABS, type DetailTab } from "../components/ChannelDetail";
import ChannelFilter from "../components/ChannelFilter";
import { ChannelStateBadge, FailurePill } from "../components/ChannelState";
import { ConfirmDialog } from "../components/Modal";
import { formatCount, formatMillis, formatPercent } from "../format";
import { useT } from "../i18n";
import { useVisiblePolling } from "../polling";
import { setUnsaved } from "../store/unsaved";
import type { ChannelModelSummary } from "../types";

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

// The first models a channel serves; "…" while they are looked up.
function modelsText(row: ChannelRow, models: ChannelModels | null): string {
  if (!row.authIndex) return "—";
  const served = models?.get(row.authIndex);
  if (!served) return models ? "—" : "…";
  if (served.length === 0) return "—";
  return served.length > 2 ? `${served.slice(0, 2).join(", ")} +${served.length - 2}` : served.join(", ");
}

// Latencies of a window without attempts are not measurements.
function latency(row: ChannelRow, field: "latency_p50_ms" | "ttft_p50_ms"): string {
  const day = row.view?.last_24h;
  return day && day.attempts > 0 ? formatMillis(day[field]) : "—";
}

// One table column: the default view, or a model's view of its channels.
interface Column {
  key: string;
  header: string;
  num?: boolean;
  className?: string;
  title?: (row: ChannelRow) => string | undefined;
  cell: (row: ChannelRow) => React.ReactNode;
}

// A model's metric of a channel, marked when it is well below the channels
// serving the same model.
function MetricCell({ summary, metric, medians }: { summary?: ChannelModelSummary; metric: QualityMetric; medians: Partial<Record<QualityMetric, number>> }) {
  const t = useT();
  const low = isLow(summary, metric, medians);
  const peer = medians[metric];
  return (
    <span className={low ? "metric-low" : undefined} title={peer !== undefined ? t("channels.quality.peer", { value: formatMetricValue(metric, peer) }) : undefined}>
      {formatMetric(summary, metric)}{low && <span className="badge warn">{t("channels.quality.low")}</span>}
    </span>
  );
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
  const typeFilter = params.get("type") ?? ALL;
  const modelChoice = params.get("model") ?? ALL;
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
  const models = useChannelModels(rows, data?.ids ?? {});
  // A link may spell the model as the statistics do; the filter shows CPA's name.
  const modelFilter = canonicalModel(models, modelChoice);
  const shown = useMemo(() => filterChannels(rows, typeFilter, modelFilter, models), [rows, typeFilter, modelFilter, models]);
  const groups = useMemo(() => groupChannels(shown), [shown]);
  // With a model chosen the list compares the channels serving it.
  const comparing = modelFilter !== ALL;
  const summaryOf = (row: ChannelRow) => (comparing ? modelSummary(row.view?.models_24h, modelFilter) : undefined);
  const medians = useMemo(() => (comparing ? peerMedians(shown.map((row) => modelSummary(row.view?.models_24h, modelFilter))) : {}), [comparing, shown, modelFilter]);
  const selectedRow = selected ? rows.find((row) => row.authIndex === selected) ?? (data ? placeholderRow(selected) : undefined) : undefined;

  // The selection, tab and filters live in the URL: the back button closes
  // the detail on phones and other pages can link to a channel.
  const filters = () => {
    const next = new URLSearchParams();
    if (typeFilter !== ALL) next.set("type", typeFilter);
    if (modelFilter !== ALL) next.set("model", modelFilter);
    return next;
  };
  const search = (authIndex: string, nextTab: DetailTab = tab) => {
    const next = filters();
    next.set("auth_index", authIndex);
    if (nextTab !== "overview") next.set("tab", nextTab);
    return `?${next}`;
  };
  const leave = (target: string) => {
    if (dirty) setLeaving(target);
    else setParams(new URLSearchParams(target));
  };
  const setFilter = (name: "type" | "model", value: string) => {
    const next = new URLSearchParams(params);
    if (value === ALL) next.delete(name);
    else next.set(name, value);
    setParams(next, { replace: true });
  };
  const open = (event: React.MouseEvent, authIndex: string) => {
    if (!dirty || authIndex === selected) return;
    event.preventDefault();
    setLeaving(search(authIndex));
  };

  const columns: Column[] = [
    { key: "name", header: t("channels.col.name"), className: "channel-name-cell", cell: (row) => <ChannelName row={row} search={search} onOpen={open} /> },
    { key: "type", header: t("channels.col.type"), cell: (row) => <><span className="mono">{row.provider || "—"}</span> <OriginBadge row={row} /></> },
    ...(comparing ? [
      { key: "attempts", header: t("channels.col.modelAttempts"), num: true, className: "mono", cell: (row: ChannelRow) => formatCount(summaryOf(row)?.attempts ?? 0) },
      { key: "status", header: t("channels.col.status"), cell: (row: ChannelRow) => <div className="channel-badges"><ChannelStateBadge row={row} /><FailurePill summary={summaryOf(row)} /></div> },
      { key: "success", header: t("channels.col.success"), num: true, className: "mono", cell: (row: ChannelRow) => { const summary = summaryOf(row); return summary && summary.attempts > 0 ? formatPercent(summary.success_rate) : "—"; } },
      ...QUALITY_METRICS.map((metric) => ({ key: metric, header: t(`channels.col.${metric}`), num: true, className: "mono", cell: (row: ChannelRow) => <MetricCell summary={summaryOf(row)} metric={metric} medians={medians} /> })),
    ] : [
      { key: "models", header: t("channels.col.models"), className: "mono channel-models", title: (row: ChannelRow) => models?.get(row.authIndex)?.join("\n"), cell: (row: ChannelRow) => modelsText(row, models) },
      { key: "status", header: t("channels.col.status"), cell: (row: ChannelRow) => <div className="channel-badges"><ChannelStateBadge row={row} /><FailurePill summary={row.view?.last_24h} /></div> },
      { key: "success", header: t("channels.col.success"), num: true, className: "mono", cell: successRate },
      { key: "latency", header: t("channels.col.latency"), num: true, className: "mono", cell: (row: ChannelRow) => latency(row, "latency_p50_ms") },
      { key: "ttft", header: t("channels.col.ttft"), num: true, className: "mono", cell: (row: ChannelRow) => latency(row, "ttft_p50_ms") },
      { key: "rules", header: t("channels.col.rules"), num: true, className: "mono", cell: (row: ChannelRow) => row.view?.rules || "—" },
    ]),
  ];
  // The name and status lead each card; the rest are its facts.
  const facts = columns.filter((column) => column.key !== "name" && column.key !== "type" && column.key !== "status");

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
          {data && rows.length > 0 && (
            <ChannelFilter
              types={channelTypes(rows)}
              type={typeFilter}
              models={models && modelCounts(filterChannels(rows, typeFilter, ALL, models).map((row) => row.authIndex), models)}
              model={modelFilter}
              onType={(value) => setFilter("type", value)}
              onModel={(value) => setFilter("model", value)}
            />
          )}
          {comparing && data && <p className="field-hint channel-compare-hint">{t("channels.quality.hint", { model: modelFilter, min: MIN_COMPARED_ATTEMPTS })}</p>}
          {data && (
            <div className="key-list-context">
              <span>{shown.length === rows.length ? t("channels.count", { total: rows.length, groups: groups.length }) : t("channels.countFiltered", { shown: shown.length, total: rows.length })}</span>
              <span>{lastUpdated ? t("keys.lastUpdated", { time: lastUpdated.toLocaleTimeString() }) : ""}</span>
            </div>
          )}
          {!data ? (loading && <div className="card muted" role="status">{t("keys.loading")}</div>) : rows.length === 0 ? (
            <div className="empty-state">{t("channels.empty")}</div>
          ) : shown.length === 0 ? (
            <div className="empty-state">{t("channels.filteredEmpty")}</div>
          ) : groups.map((group) => (
            <section key={group.vendor || "-"} className="channel-group" aria-label={group.vendor || t("channels.noVendor")}>
              <h2 className="usage-section-title">{group.vendor || t("channels.noVendor")} <span className="muted">· {group.rows.length}</span></h2>
              <div className={"card table-wrap channel-table" + (comparing ? " comparing" : "")}>
                <table>
                  <thead>
                    <tr>{columns.map((column) => <th key={column.key} className={column.num ? "num" : undefined}>{column.header}</th>)}</tr>
                  </thead>
                  <tbody>
                    {group.rows.map((row) => (
                      <tr key={row.key} className={row.authIndex && row.authIndex === selected ? "selected" : ""} aria-current={row.authIndex && row.authIndex === selected ? "true" : undefined}>
                        {columns.map((column) => (
                          <td key={column.key} className={[column.num ? "num" : "", column.className ?? ""].join(" ").trim() || undefined} title={column.title?.(row)}>{column.cell(row)}</td>
                        ))}
                      </tr>
                    ))}
                  </tbody>
                </table>
              </div>
              <div className="channel-cards mobile-only">
                {group.rows.map((row) => (
                  <article key={row.key} className="card channel-card">
                    <div className="channel-card-head"><ChannelName row={row} search={search} onOpen={open} /><OriginBadge row={row} /></div>
                    <div className="channel-badges"><ChannelStateBadge row={row} /><FailurePill summary={comparing ? summaryOf(row) : row.view?.last_24h} /></div>
                    <dl className="channel-card-facts">
                      {facts.filter((column) => column.key !== "rules" || (row.view?.rules ?? 0) > 0).map((column) => <div key={column.key}><dt>{column.header}</dt><dd className="mono">{column.cell(row)}</dd></div>)}
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
            onClose={() => leave(`?${filters()}`)}
            onChanged={() => void load()}
            onDeleted={() => { setDirty(false); setParams(filters(), { replace: true }); void load(); }}
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
