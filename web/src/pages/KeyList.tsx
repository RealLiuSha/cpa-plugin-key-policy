import { useCallback, useEffect, useMemo, useState } from "react";
import { Link } from "react-router-dom";
import { listKeys } from "../api/keys";
import { extractApiError } from "../api/error";
import type { KeyModelRef, KeyPublic } from "../types";
import KeyMoreMenu from "../components/KeyMoreMenu";
import { MobileTabBar } from "../components/MobileChrome";
import PlainKeyModal from "../components/PlainKeyModal";
import { useT } from "../i18n";

/** Default page size for the key list (client-side pagination). */
export const KEY_LIST_PAGE_SIZE = 10;
export type KeySort = "daily" | "ratio" | "active";

/** Deduplicate public model references case-insensitively for chip display. */
export function uniqueModelNames(models: KeyModelRef[] | undefined): string[] {
  const out: string[] = [];
  const seen = new Set<string>();
  for (const m of models ?? []) {
    const key = (m.name ?? "").toLowerCase();
    if (key && !seen.has(key)) {
      seen.add(key);
      out.push(m.name);
    }
  }
  return out;
}

/**
 * Client-side filter for the key list. Matches id, name, key preview, and
 * public model names (case-insensitive substring). Empty query returns all keys.
 */
export function filterKeys(keys: KeyPublic[], query: string): KeyPublic[] {
  const q = query.trim().toLowerCase();
  if (!q) return keys;
  return keys.filter((k) => {
    if (k.id.toLowerCase().includes(q)) return true;
    if ((k.name ?? "").toLowerCase().includes(q)) return true;
    if ((k.key_preview ?? "").toLowerCase().includes(q)) return true;
    for (const modelName of uniqueModelNames(k.models)) {
      if (modelName.toLowerCase().includes(q)) return true;
    }
    return false;
  });
}

/** Slice one page from a filtered list (0-based page index). */
export function paginateKeys<T>(
  items: T[],
  page: number,
  pageSize: number = KEY_LIST_PAGE_SIZE,
): T[] {
  const safePage = Math.max(0, page);
  const start = safePage * pageSize;
  return items.slice(start, start + pageSize);
}

function fmtUsd(n: number): string {
  return "$" + (Number.isFinite(n) ? n.toFixed(2) : "0.00");
}

/** One usage window: "$used / $limit" or "$used / 不限" when limit is 0. */
function formatUsageWindow(
  used: number,
  limit: number,
  unlimitedLabel: string,
): string {
  return fmtUsd(used) + " / " + (limit > 0 ? fmtUsd(limit) : unlimitedLabel);
}

/** True when a window has a positive limit and used has reached/exceeded it. */
export function isLimitHit(used: number, limit: number): boolean {
  return limit > 0 && used >= limit;
}

/** Any hard usage limit hit means the key is quota-blocked. */
export function isAnyLimitHit(usage: {
  daily_usd: number;
  weekly_usd: number;
  monthly_usd?: number;
  daily_limit_usd: number;
  weekly_limit_usd: number;
  monthly_limit_usd?: number;
}): boolean {
  return isLimitHit(usage.daily_usd, usage.daily_limit_usd)
    || isLimitHit(usage.weekly_usd, usage.weekly_limit_usd)
    || isLimitHit(usage.monthly_usd ?? 0, usage.monthly_limit_usd ?? 0);
}

function maxLimitRatio(k: KeyPublic): number {
  const pairs = [
    [k.usage.daily_usd, k.usage.daily_limit_usd],
    [k.usage.weekly_usd, k.usage.weekly_limit_usd],
    [k.usage.monthly_usd ?? 0, k.usage.monthly_limit_usd ?? 0],
  ];
  return Math.max(0, ...pairs.map(([used, limit]) => limit > 0 ? used / limit : 0));
}

export function sortKeys(keys: KeyPublic[], mode: KeySort): KeyPublic[] {
  return [...keys].sort((a, b) => {
    let delta = 0;
    if (mode === "daily") delta = b.usage.daily_usd - a.usage.daily_usd;
    if (mode === "ratio") delta = maxLimitRatio(b) - maxLimitRatio(a);
    if (mode === "active") delta = Date.parse(b.updated_at ?? "") - Date.parse(a.updated_at ?? "");
    if (!Number.isFinite(delta) || delta === 0) return a.id.localeCompare(b.id);
    return delta;
  });
}

export function formatResetCountdown(resetAt: string | undefined, nowMs: number): string {
  const target = Date.parse(resetAt ?? "");
  if (!Number.isFinite(target)) return "—";
  const seconds = Math.max(0, Math.floor((target - nowMs) / 1000));
  const hours = Math.floor(seconds / 3600);
  const minutes = Math.floor((seconds % 3600) / 60);
  const rest = seconds % 60;
  return [hours, minutes, rest].map((part) => String(part).padStart(2, "0")).join(":");
}

export type AccountingBoundaryKind = "daily" | "weekly" | "monthly" | "unlimited";

/** Select the shortest configured quota window whose accounting consequence is next. */
export function accountingBoundaryKind(key: KeyPublic): AccountingBoundaryKind {
  const hasModelDailyLimit = key.models.some((model) => (model.daily_limit_usd ?? 0) > 0);
  if (key.usage.daily_limit_usd > 0 || hasModelDailyLimit) return "daily";
  if (key.usage.weekly_limit_usd > 0) return "weekly";
  if ((key.usage.monthly_limit_usd ?? 0) > 0) return "monthly";
  return "unlimited";
}

export default function KeyList() {
  const t = useT();
  const [keys, setKeys] = useState<KeyPublic[]>([]);
  const [error, setError] = useState("");
  const [loading, setLoading] = useState(true);
  const [plain, setPlain] = useState<string | null>(null);
  const [plainTitle, setPlainTitle] = useState("");
  const [query, setQuery] = useState("");
  const [page, setPage] = useState(0);
  const [sort, setSort] = useState<KeySort>("daily");
  const [lastUpdated, setLastUpdated] = useState<Date | null>(null);
  const [nowMs, setNowMs] = useState(() => Date.now());

  // silent: refresh after reset/rotate/delete without swapping the whole list for "loading…".
  const load = useCallback(async (mode: "full" | "silent" = "full") => {
    if (mode === "full") setLoading(true);
    setError("");
    try {
      setKeys(await listKeys());
      setLastUpdated(new Date());
    } catch (e) {
      setError(extractApiError(e, t("keys.loadFailed")));
    } finally {
      if (mode === "full") setLoading(false);
    }
  }, [t]);

  const refreshSilent = useCallback(() => load("silent"), [load]);

  useEffect(() => {
    void load("full");
  }, [load]);

  useEffect(() => {
    const timer = window.setInterval(() => setNowMs(Date.now()), 1000);
    return () => window.clearInterval(timer);
  }, []);

  const filtered = useMemo(() => filterKeys(keys, query), [keys, query]);
  const sorted = useMemo(() => sortKeys(filtered, sort), [filtered, sort]);
  const pageCount = Math.max(1, Math.ceil(sorted.length / KEY_LIST_PAGE_SIZE));
  const safePage = Math.min(page, pageCount - 1);
  const pageItems = useMemo(
    () => paginateKeys(sorted, safePage, KEY_LIST_PAGE_SIZE),
    [sorted, safePage],
  );

  // Keep page index in range when filter shrinks; reset to first page on new query.
  useEffect(() => {
    setPage(0);
  }, [query]);

  useEffect(() => {
    setPage((p) => Math.min(p, Math.max(0, pageCount - 1)));
  }, [pageCount]);

  const onRotated = (plainKey: string) => {
    setPlain(plainKey);
    setPlainTitle(t("keys.rotated"));
  };

  return (
    <div className="key-list">
      <div className="fp-head mobile-hidden" style={{ margin: "0 0 16px" }}>
        <h1>{t("header.keyList")}</h1>
        <div className="fp-actions">
          <button className="btn sm" onClick={() => void load("full")}>{t("keys.refresh")}</button>
        </div>
      </div>
      <div className="mobile-only key-list-mobile-refresh">
        <button className="btn sm" onClick={() => void load("silent")}>{t("keys.refresh")}</button>
        <span className="muted" data-testid="last-updated-mobile">
          {t("keys.lastUpdated", { time: lastUpdated ? lastUpdated.toLocaleTimeString() : "—" })}
        </span>
      </div>
      {error && <div className="error">{error}</div>}
      {loading ? (
        <div className="muted">{t("keys.loading")}</div>
      ) : keys.length === 0 ? (
        <div className="card muted">{t("keys.empty")}</div>
      ) : (
        <>
          <div className="key-list-toolbar">
            <input
              className="input key-list-search"
              type="search"
              value={query}
              onChange={(e) => setQuery(e.target.value)}
              placeholder={t("keys.searchPlaceholder")}
              aria-label={t("keys.searchPlaceholder")}
            />
            <select
              className="input key-list-sort"
              value={sort}
              onChange={(e) => setSort(e.target.value as KeySort)}
              aria-label={t("keys.sortLabel")}
            >
              <option value="daily">{t("keys.sortDaily")}</option>
              <option value="ratio">{t("keys.sortRatio")}</option>
              <option value="active">{t("keys.sortActive")}</option>
            </select>
            <span className="muted key-list-summary">
              {t("keys.pageSummary", { total: filtered.length })}
              <span className="mobile-hidden" data-testid="last-updated-desktop">
                {" · "}{t("keys.lastUpdated", { time: lastUpdated ? lastUpdated.toLocaleTimeString() : "—" })}
              </span>
            </span>
          </div>
          {filtered.length === 0 ? (
            <div className="card muted">{t("keys.searchNoMatch")}</div>
          ) : (
            <>
              <div className="card table-wrap key-list-table mobile-hidden">
                <table>
                  <thead>
                    <tr>
                      <th>{t("keys.colIdName")}</th>
                      <th>{t("keys.colStatus")}</th>
                      <th>{t("keys.colPreview")}</th>
                      <th>{t("keys.colRpm")}</th>
                      <th title={t("usage.windowHelp", { timezone: keys[0]?.usage.timezone || "Asia/Shanghai" })}>
                        {t("keys.colUsage")} · {keys[0]?.usage.timezone || "Asia/Shanghai"}
                      </th>
                      <th>{t("keys.colAvailableModels")}</th>
                      <th>{t("keys.colActions")}</th>
                    </tr>
                  </thead>
                  <tbody>
                    {pageItems.map((k) => (
                      <KeyTableRow
                        key={k.id}
                        k={k}
                        onResetComplete={refreshSilent}
                        onRotated={onRotated}
                        onDeleted={refreshSilent}
                        nowMs={nowMs}
                      />
                    ))}
                  </tbody>
                </table>
              </div>
              <div className="key-list-cards mobile-only">
                {pageItems.map((k) => (
                  <KeyMobileCard
                    key={k.id}
                    k={k}
                    onResetComplete={refreshSilent}
                    onRotated={onRotated}
                    onDeleted={refreshSilent}
                    nowMs={nowMs}
                  />
                ))}
              </div>
              {pageCount > 1 && (
                <div className="key-list-pager" role="navigation" aria-label="pagination">
                  <button
                    type="button"
                    className="btn sm"
                    disabled={safePage <= 0}
                    onClick={() => setPage(safePage - 1)}
                  >
                    {t("keys.prevPage")}
                  </button>
                  <span className="page-info">
                    {t("keys.pageInfo", { cur: safePage + 1, total: pageCount })}
                  </span>
                  <button
                    type="button"
                    className="btn sm"
                    disabled={safePage >= pageCount - 1}
                    onClick={() => setPage(safePage + 1)}
                  >
                    {t("keys.nextPage")}
                  </button>
                </div>
              )}
            </>
          )}
        </>
      )}

      <Link to="/keys/new" className="fab" aria-label={t("keys.newKey")}>+</Link>
      <MobileTabBar active="keys" />

      {plain && (
        <PlainKeyModal
          plainKey={plain}
          title={plainTitle}
          onClose={() => setPlain(null)}
        />
      )}
    </div>
  );
}

function KeyActions({
  k,
  onResetComplete,
  onRotated,
  onDeleted,
}: {
  k: KeyPublic;
  onResetComplete: () => void | Promise<void>;
  onRotated: (plainKey: string) => void;
  onDeleted: () => void | Promise<void>;
}) {
  const t = useT();
  const id = encodeURIComponent(k.id);
  return (
    <div className="key-actions">
      <Link to={`/keys/${id}/edit`} className="btn sm">{t("keys.edit")}</Link>
      <Link to={`/keys/${id}/usage`} className="btn sm">{t("keys.detail")}</Link>
      <KeyMoreMenu
        keyId={k.id}
        onResetComplete={onResetComplete}
        onRotated={onRotated}
        onDeleted={onDeleted}
      />
    </div>
  );
}

function KeyTableRow({
  k,
  onResetComplete,
  onRotated,
  onDeleted,
  nowMs,
}: {
  k: KeyPublic;
  onResetComplete: () => void | Promise<void>;
  onRotated: (plainKey: string) => void;
  onDeleted: () => void | Promise<void>;
  nowMs: number;
}) {
  const t = useT();
  const modelNames = uniqueModelNames(k.models);
  const shown = modelNames.slice(0, 3);
  const more = Math.max(0, modelNames.length - 3);
  const unlimited = t("usage.unlimited");
  const daily = formatUsageWindow(k.usage.daily_usd, k.usage.daily_limit_usd, unlimited);
  const weekly = formatUsageWindow(k.usage.weekly_usd, k.usage.weekly_limit_usd, unlimited);
  const dailyHit = isLimitHit(k.usage.daily_usd, k.usage.daily_limit_usd);
  const weeklyHit = isLimitHit(k.usage.weekly_usd, k.usage.weekly_limit_usd);
  const monthlyHit = isLimitHit(k.usage.monthly_usd ?? 0, k.usage.monthly_limit_usd ?? 0);
  const anyHit = dailyHit || weeklyHit || monthlyHit;
  const state = !k.enabled ? "disabled" : anyHit ? "limited" : k.usage.soft_limit_hit ? "warning" : "normal";
  const boundaryKind = accountingBoundaryKind(k);

  return (
    <tr
      className={[!k.enabled && "row-disabled", anyHit && "usage-limit-hit", state === "warning" && "usage-soft-warning"].filter(Boolean).join(" ") || undefined}
      data-testid={`key-state-${state}-${k.id}`}
    >
      <td>
        <div className="key-id">{k.id}</div>
        {k.name ? <div className="muted key-name">{k.name}</div> : null}
      </td>
      <td>
        <span className={"tag " + (state === "normal" ? "on" : "off") + ` state-${state}`}>
          {state === "disabled" ? t("keys.disabled") : state === "limited" ? t("keys.quotaBlocked") : state === "warning" ? t("keys.softWarning") : t("keys.enabled")}
        </span>
      </td>
      <td className="mono">{k.key_preview}</td>
      <td>{k.rpm > 0 ? k.rpm : t("usage.unlimited")}</td>
      <td className={"key-usage-cell" + (anyHit ? " usage-over" : "")} data-testid={`usage-cell-${k.id}`}>
        <div className={dailyHit ? "usage-line over" : undefined} data-testid={`usage-daily-${k.id}`}>
          {t("usage.today")} {daily}
        </div>
        <div className={"muted" + (weeklyHit ? " usage-line over" : "")} data-testid={`usage-weekly-${k.id}`}>
          {t("usage.last7Days")} {weekly}
        </div>
        <div className="muted usage-reset-countdown" data-testid={`usage-reset-${k.id}`}>
          {boundaryKind === "unlimited"
            ? t("usage.boundaryUnlimited")
            : `${t(`usage.boundary${boundaryKind[0].toUpperCase()}${boundaryKind.slice(1)}`)} ${formatResetCountdown(k.usage.next_accounting_boundary_at, nowMs)}`}
        </div>
      </td>
      <td>
        {shown.length > 0 ? (
          <div className="key-model-chips">
            <span className="muted">{modelNames.length}</span>
            {shown.map((modelName) => (
              <span key={modelName} className="chip">{modelName}</span>
            ))}
            {more > 0 && <span className="chip more">+{more}</span>}
          </div>
        ) : (
          <span className="muted">—</span>
        )}
      </td>
      <td className="key-actions-cell">
        <KeyActions
          k={k}
          onResetComplete={onResetComplete}
          onRotated={onRotated}
          onDeleted={onDeleted}
        />
      </td>
    </tr>
  );
}

/**
 * Mobile card: same ops as the desktop row (edit / detail / more). No whole-card
 * navigation and no swipe-to-revoke — those conflicted with the desktop IA.
 */
function KeyMobileCard({
  k,
  onResetComplete,
  onRotated,
  onDeleted,
  nowMs,
}: {
  k: KeyPublic;
  onResetComplete: () => void | Promise<void>;
  onRotated: (plainKey: string) => void;
  onDeleted: () => void | Promise<void>;
  nowMs: number;
}) {
  const t = useT();
  const modelNames = uniqueModelNames(k.models);
  const shownChips = modelNames.slice(0, 2);
  const moreCount = Math.max(0, modelNames.length - 2);
  const dailyLimit = k.usage.daily_limit_usd > 0 ? k.usage.daily_limit_usd : 0;
  const weeklyLimit = k.usage.weekly_limit_usd > 0 ? k.usage.weekly_limit_usd : 0;
  const monthlyLimit = (k.usage.monthly_limit_usd ?? 0) > 0 ? k.usage.monthly_limit_usd ?? 0 : 0;
  const dailyHit = isLimitHit(k.usage.daily_usd, k.usage.daily_limit_usd);
  const weeklyHit = isLimitHit(k.usage.weekly_usd, k.usage.weekly_limit_usd);
  const monthlyHit = isLimitHit(k.usage.monthly_usd ?? 0, k.usage.monthly_limit_usd ?? 0);
  const over = dailyHit || weeklyHit || monthlyHit;
  const state = !k.enabled ? "disabled" : over ? "limited" : k.usage.soft_limit_hit ? "warning" : "normal";
  // Prefer the shortest configured window; a monthly-only key still gets a
  // meaningful quota progress bar rather than looking unlimited.
  const barLimit = dailyLimit > 0 ? dailyLimit : weeklyLimit > 0 ? weeklyLimit : monthlyLimit;
  const barUsed = dailyLimit > 0 ? k.usage.daily_usd : weeklyLimit > 0 ? k.usage.weekly_usd : k.usage.monthly_usd ?? 0;
  const pct = barLimit > 0 ? Math.min(100, (barUsed / barLimit) * 100) : 0;
  const unlimited = t("usage.unlimited");
  const boundaryKind = accountingBoundaryKind(k);

  return (
    <div
      className={"keycard" + (k.enabled ? "" : " disabled") + (over ? " over" : "") + (state === "warning" ? " warning" : "")}
      data-testid={`keycard-${k.id}`}
      data-state={state}
    >
      <span className="state-indicator" data-testid={`key-state-${state}-${k.id}`} aria-hidden="true" />
      <div className="kc-head">
        <span className="kc-dot" />
        <span className="kc-name">{k.name || k.id}</span>
      </div>
      <div className="kc-preview">{k.key_preview}</div>
      {barLimit > 0 && <div className="kc-bar"><span style={{ width: pct + "%" }} /></div>}
      <div className="kc-meta">
        <span className={dailyHit ? "usage-line over" : undefined} data-testid={`usage-daily-${k.id}`}>
          {t("usage.today")} {formatUsageWindow(k.usage.daily_usd, k.usage.daily_limit_usd, unlimited)}
        </span>
        <span className={weeklyHit ? "usage-line over" : undefined} data-testid={`usage-weekly-${k.id}`}>
          {t("usage.last7Days")} {formatUsageWindow(k.usage.weekly_usd, k.usage.weekly_limit_usd, unlimited)}
        </span>
      </div>
      <div className="kc-meta">
        <span>{boundaryKind === "unlimited"
          ? t("usage.boundaryUnlimited")
          : `${t(`usage.boundary${boundaryKind[0].toUpperCase()}${boundaryKind.slice(1)}`)} ${formatResetCountdown(k.usage.next_accounting_boundary_at, nowMs)}`}</span>
      </div>
      <div className="kc-models">
        <span className="muted">{t("keys.availableModelsCount", { count: modelNames.length })}</span>
        {shownChips.length > 0 && <div className="kc-chips">
          {shownChips.map((modelName) => <span key={modelName} className="chip">{modelName}</span>)}
          {moreCount > 0 && <span className="chip more">+{moreCount}</span>}
        </div>}
      </div>
      <div className="kc-actions">
        <KeyActions
          k={k}
          onResetComplete={onResetComplete}
          onRotated={onRotated}
          onDeleted={onDeleted}
        />
      </div>
    </div>
  );
}
