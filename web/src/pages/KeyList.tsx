import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { Link, useSearchParams } from "react-router-dom";
import { listKeys } from "../api/keys";
import { extractApiError } from "../api/error";
import type { KeyPublic, KeyStatus, QuotaMode } from "../types";
import { useT } from "../i18n";
import { MobileTabBar } from "../components/MobileChrome";
import PlainKeyModal from "../components/PlainKeyModal";
import KeyMoreMenu from "../components/KeyMoreMenu";
import QuotaUsage from "../components/QuotaUsage";
import QuotaResetDialog from "../components/QuotaResetDialog";
import BalanceUsage, { balanceOf, balanceSpentRatio, isBalanceKey } from "../components/BalanceUsage";
import BalanceDialog from "../components/BalanceDialog";
import { useSearchDraft } from "../search";

export const KEY_LIST_PAGE_SIZE = 20;
export type KeySort = "attention" | "ratio" | "reset" | "name";
type StatusFilter = "all" | "limited" | "warning" | "disabled";
type ModeFilter = "all" | QuotaMode;
const filters: StatusFilter[] = ["all", "limited", "warning", "disabled"];
const modes: ModeFilter[] = ["all", "cycle", "balance"];
const sorts: KeySort[] = ["attention", "ratio", "reset", "name"];

export function filterKeys(keys: KeyPublic[], query: string, status: StatusFilter = "all", mode: ModeFilter = "all"): KeyPublic[] {
  const needle = query.trim().toLocaleLowerCase();
  return keys.filter((key) => {
    const matchesState = status === "all" || key.usage.status === status;
    const matchesMode = mode === "all" || (mode === "balance") === isBalanceKey(key);
    return matchesState && matchesMode && (!needle || [key.id, key.name, key.key_preview].some((value) => value.toLocaleLowerCase().includes(needle)));
  });
}

export function paginateKeys<T>(items: T[], page: number, pageSize = KEY_LIST_PAGE_SIZE): T[] {
  const start = Math.max(0, page) * pageSize;
  return items.slice(start, start + pageSize);
}

// How close the key is to being refused: the most used period quota, or the
// spent share of a balance key's credit.
function highestRatio(key: KeyPublic): number {
  if (isBalanceKey(key)) return balanceSpentRatio(key.usage);
  return Math.max(0, ...(key.usage.cycles ?? []).filter((cycle) => cycle.limit_usd > 0).map((cycle) => cycle.used_usd / cycle.limit_usd));
}
function nextReset(key: KeyPublic): number {
  return Math.min(Infinity, ...(key.usage.cycles ?? []).filter((cycle) => cycle.limit_usd > 0).map((cycle) => Date.parse(cycle.resets_at)));
}
export function sortKeys(keys: KeyPublic[], mode: KeySort): KeyPublic[] {
  const rank: Record<KeyStatus, number> = { limited: 0, warning: 1, normal: 2, disabled: 3 };
  return [...keys].sort((a, b) => {
    let delta = 0;
    if (mode === "attention") delta = (rank[a.usage.status ?? "normal"] - rank[b.usage.status ?? "normal"]) || highestRatio(b) - highestRatio(a);
    if (mode === "ratio") delta = highestRatio(b) - highestRatio(a);
    if (mode === "reset") delta = nextReset(a) - nextReset(b);
    if (!Number.isNaN(delta) && delta !== 0) return delta;
    return (a.name || a.id).localeCompare(b.name || b.id) || a.id.localeCompare(b.id);
  });
}

export default function KeyList() {
  const t = useT();
  const [params, setParams] = useSearchParams();
  const query = params.get("q") ?? "";
  const status = filters.find((value) => value === params.get("status")) ?? "all";
  const mode = modes.find((value) => value === params.get("mode")) ?? "all";
  const sort = sorts.find((value) => value === params.get("sort")) ?? "attention";
  const page = Math.max(0, Number.parseInt(params.get("page") ?? "0", 10) || 0);
  const [keys, setKeys] = useState<KeyPublic[]>([]);
  const [error, setError] = useState("");
  const [loading, setLoading] = useState(true);
  const [refreshing, setRefreshing] = useState(false);
  const [lastUpdated, setLastUpdated] = useState<Date | null>(null);
  const [resetKey, setResetKey] = useState<string | null>(null);
  const [balanceKey, setBalanceKey] = useState<string | null>(null);
  const [plain, setPlain] = useState<string | null>(null);
  const requestID = useRef(0);
  const search = useSearchDraft(query, setParams);

  const updateFilter = (field: string, value: string) => {
    setParams((current) => {
      const next = new URLSearchParams(current);
      if (value) next.set(field, value); else next.delete(field);
      if (field !== "page") next.delete("page");
      return next;
    }, { replace: true });
  };
  const clearFilters = () => {
    search.reset();
    setParams({}, { replace: true });
  };
  const load = useCallback(async () => {
    const id = ++requestID.current;
    setRefreshing(true);
    try {
      const data = await listKeys();
      if (id !== requestID.current) return;
      setKeys(data);
      setLastUpdated(new Date());
      setError("");
    } catch (reason) {
      if (id === requestID.current) setError(extractApiError(reason, t("keys.loadFailed")));
    } finally {
      if (id === requestID.current) { setLoading(false); setRefreshing(false); }
    }
  }, [t]);
  useEffect(() => {
    void load();
    const refreshVisible = () => { if (document.visibilityState === "visible") void load(); };
    const timer = window.setInterval(refreshVisible, 30_000);
    window.addEventListener("focus", refreshVisible);
    return () => { requestID.current++; window.clearInterval(timer); window.removeEventListener("focus", refreshVisible); };
  }, [load]);

  const filtered = useMemo(() => sortKeys(filterKeys(keys, query, status, mode), sort), [keys, query, status, mode, sort]);
  const pageCount = Math.max(1, Math.ceil(filtered.length / KEY_LIST_PAGE_SIZE));
  const safePage = Math.min(page, pageCount - 1);
  const items = paginateKeys(filtered, safePage);
  const timezone = keys[0]?.usage.timezone ?? "Asia/Shanghai";
  const keyListReturnTo = `/keys${params.size ? `?${params}` : ""}`;

  return (
    <div className="key-list v5-key-list">
      <div className="key-page-heading"><div><h1>{t("header.keyList")}</h1><p className="muted">{t("quota.listHint")}</p></div><Link className="btn primary" to="/keys/new">+ {t("keys.newKey")}</Link></div>
      <div className="key-filter-tabs" role="group" aria-label={t("quota.filterLabel")}>
        {filters.map((value) => <button key={value} className={status === value ? "selected" : ""} aria-pressed={status === value} onClick={() => updateFilter("status", value)}>{t(`quota.filter.${value}`)}<span>{filterKeys(keys, "", value, mode).length}</span></button>)}
      </div>
      <div className="key-list-toolbar">
        <input className="input key-list-search" type="search" value={search.value} onChange={search.onChange} onCompositionStart={search.onCompositionStart} onCompositionEnd={search.onCompositionEnd} placeholder={t("keys.searchPlaceholder")} aria-label={t("keys.searchPlaceholder")} />
        <select className="input key-list-mode" value={mode} onChange={(event) => updateFilter("mode", event.target.value === "all" ? "" : event.target.value)} aria-label={t("balance.modeFilterLabel")}>{modes.map((value) => <option key={value} value={value}>{t(`balance.modeFilter.${value}`)}</option>)}</select>
        <select className="input key-list-sort" value={sort} onChange={(event) => updateFilter("sort", event.target.value)} aria-label={t("keys.sortLabel")}>{sorts.map((value) => <option key={value} value={value}>{t(`quota.sort.${value}`)}</option>)}</select>
        <button className="btn" disabled={refreshing} onClick={() => void load()}>{t(refreshing ? "quota.refreshing" : "keys.refresh")}</button>
      </div>
      <div className="key-list-context"><span>{t("keys.pageSummary", { total: filtered.length })}</span><span>{timezone}{lastUpdated ? ` · ${t("keys.lastUpdated", { time: lastUpdated.toLocaleTimeString() })}` : ""}</span></div>
      {error && <div className="error" role="alert">{error}</div>}
      {loading ? <div className="card muted" role="status">{t("keys.loading")}</div> : items.length ? <div className="key-rows">
        {items.map((key) => {
          const state = key.usage.status;
          const prepaid = isBalanceKey(key);
          return <article className={`key-row state-${state ?? "unknown"}`} key={key.id} aria-label={key.name || key.id}>
            <div className="key-person">
              <Link className="key-person-name" to={`/keys/${encodeURIComponent(key.id)}/usage`} state={{ keyListReturnTo }}>{key.name || key.id}</Link>
              <span className={`key-status state-${state ?? "unknown"}`}>{t(`quota.status.${state ?? "unknown"}`)}</span>
              {prepaid && <span className="key-mode">{t("balance.badge")}</span>}
              <div className="key-person-id">{key.id}</div><div className="key-person-meta"><code>{key.key_preview}</code></div>
              {key.usage.blocked_reason && <span className="key-reason">{t(`quota.reason.${key.usage.blocked_reason}`)}</span>}
            </div>
            {prepaid ? <BalanceUsage usage={key.usage} /> : <QuotaUsage usage={key.usage} />}
            <div className="key-row-actions"><Link className="btn sm" to={`/keys/${encodeURIComponent(key.id)}/usage`} state={{ keyListReturnTo }}>{t("keys.detail")}</Link><Link className="btn sm" to={`/keys/${encodeURIComponent(key.id)}/edit`} state={{ keyListReturnTo }}>{t("keys.edit")}</Link>{prepaid ? <button className="btn sm" onClick={() => setBalanceKey(key.id)}>{t("balance.adjust")}</button> : <button className="btn sm" onClick={() => setResetKey(key.id)}>{t("quota.resetTitle")}</button>}<KeyMoreMenu keyId={key.id} keyListReturnTo={keyListReturnTo} items={["rpm", "rotate", "delete"]} balanceUSD={key.usage.balance_usd === undefined ? undefined : balanceOf(key.usage).balance} onResetComplete={load} onDeleted={load} onRotated={setPlain} /></div>
          </article>;
        })}
      </div> : <div className="card key-empty"><strong>{t(keys.length ? "keys.searchNoMatch" : "keys.empty")}</strong>{keys.length > 0 && <button className="btn" onClick={clearFilters}>{t("quota.clearFilters")}</button>}</div>}
      {pageCount > 1 && <nav className="key-list-pager" aria-label={t("quota.pagination")}><button className="btn sm" disabled={safePage === 0} onClick={() => updateFilter("page", String(safePage - 1))}>{t("keys.prevPage")}</button><span>{t("keys.pageInfo", { cur: safePage + 1, total: pageCount })}</span><button className="btn sm" disabled={safePage >= pageCount - 1} onClick={() => updateFilter("page", String(safePage + 1))}>{t("keys.nextPage")}</button></nav>}
      <MobileTabBar active="keys" />
      {resetKey && <QuotaResetDialog keyId={resetKey} onClose={() => setResetKey(null)} onComplete={load} />}
      {balanceKey && <BalanceDialog keyId={balanceKey} onClose={() => setBalanceKey(null)} onComplete={load} />}
      {plain && <PlainKeyModal plainKey={plain} title={t("keys.rotated")} onClose={() => setPlain(null)} />}
    </div>
  );
}
