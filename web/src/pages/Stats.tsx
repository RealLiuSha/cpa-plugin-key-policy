import { useEffect, useState, type ReactNode } from "react";
import { Link, useSearchParams } from "react-router-dom";
import { exportStatsCSV, fetchStats, isRangeTooLarge, type StatsExport } from "../api/stats";
import { fetchStatus } from "../api/status";
import { extractApiError } from "../api/error";
import { MobileTabBar } from "../components/MobileChrome";
import UnpricedBadge from "../components/UnpricedBadge";
import UsageChart from "../components/UsageChart";
import { formatCount, formatHitRate, formatUSD } from "../format";
import { useT } from "../i18n";
import { useSearchDraft, type SearchDraft } from "../search";
import type { LedgerHealth, StatsKey, StatsModel, StatsReport, StatsTotals } from "../types";

// One day is "today": the report ends on the current natural day, so a range of
// 1 covers only that day. The page opens on it.
const STATS_RANGES = [1, 7, 30, 90] as const;
type StatsRange = (typeof STATS_RANGES)[number];
const DEFAULT_STATS_RANGE: StatsRange = 1;

const HEALTH_COUNTERS = [
  "flush_failures", "skipped_failed", "ignored_unknown_key", "ignored_no_model", "ignored_empty_usage",
  "clamped_tokens", "invalid_usage", "unmetered_per_call", "unpriced_calls",
] as const satisfies readonly (keyof LedgerHealth)[];

function saveFile({ blob, filename }: StatsExport) {
  const url = URL.createObjectURL(blob);
  const link = document.createElement("a");
  link.href = url;
  link.download = filename;
  document.body.appendChild(link);
  link.click();
  link.remove();
  // Revoking right after click() can cancel the download in some browsers.
  window.setTimeout(() => URL.revokeObjectURL(url), 40_000);
}

function Kpis({ totals }: { totals: StatsTotals }) {
  const t = useT();
  const items: [string, string, boolean?][] = [
    ["stats.kpi.charged", formatUSD(totals.charged_usd)],
    ["stats.kpi.calls", formatCount(totals.calls)],
    ["stats.kpi.input", formatCount(totals.input_tokens)],
    ["stats.kpi.output", formatCount(totals.output_tokens)],
    ["stats.kpi.hitRate", formatHitRate(totals.cache_read_tokens, totals.input_tokens)],
    ["stats.kpi.unpriced", formatCount(totals.unpriced_calls), totals.unpriced_calls > 0],
  ];
  return (
    <div className="stats-kpis" data-testid="stats-kpis">
      {items.map(([key, value, warn]) => (
        <div className={"card stats-kpi" + (warn ? " warn" : "")} key={key}>
          <div className="stats-kpi-label">{t(key)}</div>
          <div className="stats-kpi-value">{value}</div>
        </div>
      ))}
    </div>
  );
}

// One table layout for the model and key breakdowns; only the first column differs.
function UsageTable<Row extends StatsTotals>({ title, nameLabel, rows, rowKey, name, testId, empty, children }: {
  title: string;
  nameLabel: string;
  rows: Row[];
  rowKey: (row: Row) => string;
  name: (row: Row) => ReactNode;
  testId: string;
  empty?: ReactNode;
  children?: ReactNode;
}) {
  const t = useT();
  return (
    <section className="stats-section" data-testid={testId}>
      <h2 className="usage-section-title">{title}</h2>
      {children}
      <div className="card table-wrap stats-table">
        <table>
          <thead>
            <tr>
              <th>{nameLabel}</th>
              <th className="num">{t("stats.colCharged")}</th>
              <th className="num">{t("stats.colCalls")}</th>
              <th className="num">{t("stats.colInput")}</th>
              <th className="num">{t("stats.colOutput")}</th>
              <th className="num">{t("stats.colCacheRead")}</th>
              <th className="num">{t("stats.colCacheWrite")}</th>
              <th className="num">{t("stats.colHitRate")}</th>
            </tr>
          </thead>
          <tbody>
            {rows.length === 0 && empty && <tr><td className="stats-no-match" colSpan={8}>{empty}</td></tr>}
            {rows.map((row) => (
              <tr key={rowKey(row)}>
                <td className="stats-name">{name(row)}</td>
                <td className="num strong">{formatUSD(row.charged_usd)}</td>
                <td className="num mono">{formatCount(row.calls)}</td>
                <td className="num mono">{formatCount(row.input_tokens)}</td>
                <td className="num mono">{formatCount(row.output_tokens)}</td>
                <td className="num mono">{formatCount(row.cache_read_tokens)}</td>
                <td className="num mono">{formatCount(row.cache_write_tokens)}</td>
                <td className="num mono">{formatHitRate(row.cache_read_tokens, row.input_tokens)}</td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
    </section>
  );
}

// priced is the backend's verdict on the calls in the range.
function ModelName({ model }: { model: StatsModel }) {
  return (
    <>
      <span className="mono">{model.name}</span>
      {!model.priced && <UnpricedBadge calls={model.unpriced_calls > 0 ? model.unpriced_calls : undefined} />}
    </>
  );
}

// A deleted key has no usage page any more, so it is not a link. The usage
// page returns to this page with the same range and search.
function KeyName({ entry, returnTo }: { entry: StatsKey; returnTo: string }) {
  const t = useT();
  const label = entry.name || entry.id;
  return (
    <>
      {entry.deleted ? <span>{label}</span> : <Link to={`/keys/${encodeURIComponent(entry.id)}/usage`} state={{ keyListReturnTo: returnTo }}>{label}</Link>}
      {entry.deleted && <span className="badge">{t("stats.deleted")}</span>}
      {entry.unpriced_calls > 0 && <UnpricedBadge calls={entry.unpriced_calls} />}
      {entry.name && <div className="mono muted">{entry.id}</div>}
    </>
  );
}

// The key search filters the rows already loaded, which are the top keys of
// the range when keys_truncated is set.
function matchesKey(entry: StatsKey, needle: string): boolean {
  return [entry.id, entry.name].some((value) => value.toLocaleLowerCase().includes(needle));
}

function StatsBody({ report, returnTo, query, search, onClearSearch }: {
  report: StatsReport;
  returnTo: string;
  query: string;
  search: SearchDraft;
  onClearSearch: () => void;
}) {
  const t = useT();
  const needle = query.trim().toLocaleLowerCase();
  const keys = needle ? report.keys.filter((entry) => matchesKey(entry, needle)) : report.keys;
  const qualified = report.unpriced_models.filter((model) => model.has_qualified_rows).map((model) => model.name);
  return (
    <>
      {report.unpriced_models.length > 0 && (
        <section className="notice stats-unpriced" role="status" data-testid="stats-unpriced">
          <span>{t("stats.unpricedNotice", { count: report.unpriced_models.length })}</span>
          <span className="stats-unpriced-names">
            {report.unpriced_models.map((model) => (
              <span key={model.name} className="mono" title={t("usage.unpricedCalls", { n: formatCount(model.calls) })}>{model.name}</span>
            ))}
          </span>
          <Link className="btn sm" to="/models">{t("stats.unpricedLink")}</Link>
          {qualified.length > 0 && (
            <div className="stats-unpriced-qualified" data-testid="stats-unpriced-qualified">
              <span className="mono">{qualified.join(", ")}</span>
              <span>{t("usage.unpricedQualified")}</span>
            </div>
          )}
        </section>
      )}
      <Kpis totals={report.totals} />
      {report.models.length === 0 ? (
        <div className="empty-state" data-testid="stats-empty">{t("stats.empty")}</div>
      ) : (
        <>
          {report.days.length > 1 ? (
            <section className="card stats-trend" data-testid="stats-trend">
              <div className="stats-trend-title">
                <strong>{t("stats.trendTitle", { days: report.days.length })}</strong>
                <span className="muted">{t("stats.period", { from: report.from, to: report.to, timezone: report.timezone })}</span>
              </div>
              <UsageChart
                points={report.days.map((day) => ({ date: day.date, usd: day.charged_usd, calls: day.calls }))}
                label={t("stats.trendTitle", { days: report.days.length })}
              />
            </section>
          ) : (
            // A one-day trend is a single bar: only the date is worth showing.
            <p className="muted" data-testid="stats-period">{t("stats.periodDay", { date: report.to, timezone: report.timezone })}</p>
          )}
          <UsageTable
            title={t("stats.modelsTitle")}
            nameLabel={t("stats.colModel")}
            rows={report.models}
            rowKey={(model) => model.name}
            name={(model) => <ModelName model={model} />}
            testId="stats-models"
          />
          <UsageTable
            title={t("stats.keysTitle")}
            nameLabel={t("stats.colKey")}
            rows={keys}
            rowKey={(entry) => entry.id}
            name={(entry) => <KeyName entry={entry} returnTo={returnTo} />}
            testId="stats-keys"
            empty={<><span>{t("keys.searchNoMatch")}</span><button type="button" className="btn sm" onClick={onClearSearch}>{t("quota.clearFilters")}</button></>}
          >
            <div className="stats-key-tools">
              <input
                className="input stats-key-search"
                type="search"
                value={search.value}
                onChange={search.onChange}
                onCompositionStart={search.onCompositionStart}
                onCompositionEnd={search.onCompositionEnd}
                placeholder={t("stats.keySearch")}
                aria-label={t("stats.keySearch")}
              />
              {report.keys_truncated && (
                <p className="notice" role="note">{t("stats.keysTruncated", { shown: formatCount(report.keys.length), total: formatCount(report.keys_total) })}</p>
              )}
            </div>
          </UsageTable>
        </>
      )}
    </>
  );
}

// Process-lifetime ledger counters from /status; a failure stays inside the panel.
function HealthPanel() {
  const t = useT();
  const [state, setState] = useState<{ health?: LedgerHealth; error?: string } | null>(null);

  useEffect(() => {
    let alive = true;
    fetchStatus()
      .then((status) => { if (alive) setState({ health: status.health }); })
      .catch((reason) => { if (alive) setState({ error: extractApiError(reason, t("stats.health.loadFailed")) }); });
    return () => { alive = false; };
  }, [t]);

  const health = state?.health;
  const flushError = health?.last_flush_error ?? "";
  let content: ReactNode;
  if (state === null) content = <div className="muted">{t("stats.loading")}</div>;
  else if (state.error) content = <div className="error">{state.error}</div>;
  else if (!health) content = <div className="muted">{t("stats.health.unavailable")}</div>;
  else content = (
    <>
      <p className="muted stats-health-hint">{t("stats.health.hint")}</p>
      <dl className="stats-health-list">
        <div>
          <dt>{t("stats.health.lastFlushAt")}</dt>
          <dd>{health.last_flush_at ? new Date(health.last_flush_at).toLocaleString() : t("stats.health.never")}</dd>
        </div>
        {flushError && (
          <div className="stats-health-error" data-testid="stats-flush-error">
            <dt>{t("stats.health.lastFlushError")}</dt>
            <dd className="mono">{flushError}</dd>
          </div>
        )}
        {HEALTH_COUNTERS.map((field) => (
          <div key={field} className={health[field] > 0 ? "" : "zero"} data-field={field}>
            <dt>{t(`stats.health.field.${field}`)}</dt>
            <dd className="mono">{formatCount(health[field])}</dd>
          </div>
        ))}
      </dl>
    </>
  );
  return (
    <details className={"card stats-health" + (flushError ? " has-error" : "")} data-testid="stats-health">
      <summary>
        <span>{t("stats.health.title")}</span>
        {flushError && <span className="badge danger">{t("stats.health.flushError")}</span>}
      </summary>
      {content}
    </details>
  );
}

// The range lives in the URL so a key's usage page can return to it.
function rangeFrom(value: string | null): StatsRange {
  return STATS_RANGES.find((option) => String(option) === value) ?? DEFAULT_STATS_RANGE;
}

export default function Stats() {
  const t = useT();
  const [params, setParams] = useSearchParams();
  const range = rangeFrom(params.get("days"));
  const query = params.get("q") ?? "";
  const search = useSearchDraft(query, setParams);
  const [loaded, setLoaded] = useState<{ range: StatsRange; report: StatsReport } | null>(null);
  const [error, setError] = useState("");
  const [attempt, setAttempt] = useState(0);
  const [exporting, setExporting] = useState(false);
  const [exportError, setExportError] = useState("");

  useEffect(() => {
    let alive = true;
    setError("");
    fetchStats(range)
      .then((report) => { if (alive) setLoaded({ range, report }); })
      .catch((reason) => { if (alive) setError(extractApiError(reason, t("stats.loadFailed"))); });
    return () => { alive = false; };
  }, [range, attempt, t]);

  const report = loaded?.report;
  const current = !error && loaded?.range === range ? report : undefined;
  const selectRange = (option: StatsRange) => {
    setParams((current) => {
      const next = new URLSearchParams(current);
      next.set("days", String(option));
      return next;
    }, { replace: true });
    setExportError("");
  };
  const clearSearch = () => {
    search.reset();
    setParams((current) => {
      const next = new URLSearchParams(current);
      next.delete("q");
      return next;
    }, { replace: true });
  };
  // A key's usage page returns to this range and search.
  const returnParams = new URLSearchParams({ days: String(range) });
  if (query) returnParams.set("q", query);
  // The export covers exactly the days on screen: the loaded report's from/to.
  const exportCSV = async () => {
    if (!current) return;
    setExporting(true);
    setExportError("");
    try {
      saveFile(await exportStatsCSV(current.from, current.to));
    } catch (reason) {
      setExportError(isRangeTooLarge(reason) ? t("stats.exportTooLarge") : extractApiError(reason, t("stats.exportFailed")));
    } finally {
      setExporting(false);
    }
  };

  return (
    <div className="page stats-page">
      <div className="page-head">
        <div><h1>{t("stats.title")}</h1><p className="muted">{t("stats.description")}</p></div>
        <div className="page-head-actions">
          <div className="seg stats-range" role="group" aria-label={t("stats.rangeLabel")}>
            {STATS_RANGES.map((option) => (
              <button key={option} type="button" className={"seg-btn" + (range === option ? " active" : "")} aria-pressed={range === option} onClick={() => selectRange(option)}>
                {option === 1 ? t("stats.rangeToday") : t("usage.rangeDays", { days: option })}
              </button>
            ))}
          </div>
          <button type="button" className="btn" disabled={exporting || !current || current.models.length === 0} onClick={() => void exportCSV()}>
            {t(exporting ? "stats.exporting" : "stats.export")}
          </button>
        </div>
      </div>
      {exportError && <div className="error" role="alert">{exportError}</div>}
      {error ? (
        <div className="error stats-error" role="alert">
          <span>{error}</span>
          <button type="button" className="btn sm" onClick={() => setAttempt((count) => count + 1)}>{t("stats.retry")}</button>
        </div>
      ) : !report ? (
        <div className="card muted" role="status">{t("stats.loading")}</div>
      ) : (
        <>
          {!current && <div className="muted" role="status">{t("stats.loading")}</div>}
          <StatsBody report={report} returnTo={`/stats?${returnParams}`} query={query} search={search} onClearSearch={clearSearch} />
        </>
      )}
      <HealthPanel />
      <MobileTabBar active="stats" />
    </div>
  );
}
