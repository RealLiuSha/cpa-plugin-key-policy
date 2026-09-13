import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { Link, useSearchParams } from "react-router-dom";
import { listKeys } from "../api/keys";
import { extractApiError } from "../api/error";
import type { KeyPublic, KeyStatus } from "../types";
import { useT } from "../i18n";
import { MobileTabBar } from "../components/MobileChrome";
import PlainKeyModal from "../components/PlainKeyModal";
import KeyMoreMenu from "../components/KeyMoreMenu";
import QuotaUsage from "../components/QuotaUsage";
import QuotaResetDialog from "../components/QuotaResetDialog";

export const KEY_LIST_PAGE_SIZE = 20;
export type KeySort = "attention" | "ratio" | "reset" | "name";
type StatusFilter = "all" | "limited" | "warning" | "disabled";
const filters: StatusFilter[] = ["all", "limited", "warning", "disabled"];
const sorts: KeySort[] = ["attention", "ratio", "reset", "name"];

export function filterKeys(keys: KeyPublic[], query: string, status: StatusFilter = "all"): KeyPublic[] {
  const needle = query.trim().toLocaleLowerCase();
  return keys.filter((key) => {
    const state = key.usage.status;
    const matchesState = status === "all" || (status === "limited" ? state === "limited" || state === "partial" : state === status);
    return matchesState && (!needle || [key.id, key.name, key.key_preview, ...key.models.map((model) => model.name)].some((value) => value.toLocaleLowerCase().includes(needle)));
  });
}

export function paginateKeys<T>(items: T[], page: number, pageSize = KEY_LIST_PAGE_SIZE): T[] {
  const start = Math.max(0, page) * pageSize;
  return items.slice(start, start + pageSize);
}

function highestRatio(key: KeyPublic): number {
  return Math.max(0, ...(key.usage.cycles ?? []).filter((cycle) => cycle.limit_usd > 0).map((cycle) => cycle.used_usd / cycle.limit_usd));
}
function nextReset(key: KeyPublic): number {
  return Math.min(Infinity, ...(key.usage.cycles ?? []).filter((cycle) => cycle.limit_usd > 0).map((cycle) => Date.parse(cycle.resets_at)));
}
export function sortKeys(keys: KeyPublic[], mode: KeySort): KeyPublic[] {
  const rank: Record<KeyStatus, number> = { limited: 0, partial: 1, warning: 2, normal: 3, disabled: 4 };
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
  const sort = sorts.find((value) => value === params.get("sort")) ?? "attention";
  const page = Math.max(0, Number.parseInt(params.get("page") ?? "0", 10) || 0);
  const [keys, setKeys] = useState<KeyPublic[]>([]);
  const [error, setError] = useState("");
  const [loading, setLoading] = useState(true);
  const [refreshing, setRefreshing] = useState(false);
  const [lastUpdated, setLastUpdated] = useState<Date | null>(null);
  const [resetKey, setResetKey] = useState<string | null>(null);
  const [plain, setPlain] = useState<string | null>(null);
  const requestID = useRef(0);

  const updateFilter = (field: string, value: string) => {
    setParams((current) => {
      const next = new URLSearchParams(current);
      if (value) next.set(field, value); else next.delete(field);
      if (field !== "page") next.delete("page");
      return next;
    }, { replace: true });
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

  const filtered = useMemo(() => sortKeys(filterKeys(keys, query, status), sort), [keys, query, status, sort]);
  const pageCount = Math.max(1, Math.ceil(filtered.length / KEY_LIST_PAGE_SIZE));
  const safePage = Math.min(page, pageCount - 1);
  const items = paginateKeys(filtered, safePage);
  const timezone = keys[0]?.usage.timezone ?? "Asia/Shanghai";
  const keyListReturnTo = `/keys${params.size ? `?${params}` : ""}`;

  return (
    <div className="key-list v5-key-list">
      <div className="key-page-heading"><div><h1>{t("header.keyList")}</h1><p className="muted">{t("quota.listHint")}</p></div><Link className="btn primary" to="/keys/new">+ {t("keys.newKey")}</Link></div>
      <div className="key-filter-tabs" role="group" aria-label={t("quota.filterLabel")}>
        {filters.map((value) => <button key={value} className={status === value ? "selected" : ""} aria-pressed={status === value} onClick={() => updateFilter("status", value)}>{t(`quota.filter.${value}`)}<span>{filterKeys(keys, "", value).length}</span></button>)}
      </div>
      <div className="key-list-toolbar">
        <input className="input key-list-search" type="search" value={query} onChange={(event) => updateFilter("q", event.target.value)} placeholder={t("keys.searchPlaceholder")} aria-label={t("keys.searchPlaceholder")} />
        <select className="input key-list-sort" value={sort} onChange={(event) => updateFilter("sort", event.target.value)} aria-label={t("keys.sortLabel")}>{sorts.map((value) => <option key={value} value={value}>{t(`quota.sort.${value}`)}</option>)}</select>
        <button className="btn" disabled={refreshing} onClick={() => void load()}>{t(refreshing ? "quota.refreshing" : "keys.refresh")}</button>
      </div>
      <div className="key-list-context"><span>{t("keys.pageSummary", { total: filtered.length })}</span><span>{timezone}{lastUpdated ? ` · ${t("keys.lastUpdated", { time: lastUpdated.toLocaleTimeString() })}` : ""}</span></div>
      {error && <div className="error" role="alert">{error}</div>}
      {loading ? <div className="card muted" role="status">{t("keys.loading")}</div> : items.length ? <div className="key-rows">
        {items.map((key) => {
          const state = key.usage.status;
          return <article className={`key-row state-${state ?? "unknown"}`} key={key.id} aria-label={key.name || key.id}>
            <div className="key-person">
              <Link className="key-person-name" to={`/keys/${encodeURIComponent(key.id)}/usage`} state={{ keyListReturnTo }}>{key.name || key.id}</Link>
              <span className={`key-status state-${state ?? "unknown"}`}>{t(`quota.status.${state ?? "unknown"}`)}</span>
              <div className="key-person-id">{key.id}</div><div className="key-person-meta"><code>{key.key_preview}</code><span>{t("quota.modelCount", { count: key.models.length })}</span></div>
              {key.usage.blocked_reason && <span className="key-reason">{t(`quota.reason.${key.usage.blocked_reason}`)}</span>}
              {!!key.usage.limited_models?.length && <span className="key-reason">{t("quota.limitedModels", { names: key.usage.limited_models.join("、") })}</span>}
            </div>
            <QuotaUsage usage={key.usage} />
            <div className="key-row-actions"><Link className="btn sm" to={`/keys/${encodeURIComponent(key.id)}/usage`} state={{ keyListReturnTo }}>{t("keys.detail")}</Link><button className="btn sm" onClick={() => setResetKey(key.id)}>{t("quota.resetTitle")}</button><KeyMoreMenu keyId={key.id} keyListReturnTo={keyListReturnTo} items={["edit", "rpm", "rotate", "delete"]} onResetComplete={load} onDeleted={load} onRotated={setPlain} /></div>
          </article>;
        })}
      </div> : <div className="card key-empty"><strong>{t(keys.length ? "keys.searchNoMatch" : "keys.empty")}</strong>{keys.length > 0 && <button className="btn" onClick={() => setParams({}, { replace: true })}>{t("quota.clearFilters")}</button>}</div>}
      {pageCount > 1 && <nav className="key-list-pager" aria-label={t("quota.pagination")}><button className="btn sm" disabled={safePage === 0} onClick={() => updateFilter("page", String(safePage - 1))}>{t("keys.prevPage")}</button><span>{t("keys.pageInfo", { cur: safePage + 1, total: pageCount })}</span><button className="btn sm" disabled={safePage >= pageCount - 1} onClick={() => updateFilter("page", String(safePage + 1))}>{t("keys.nextPage")}</button></nav>}
      <MobileTabBar active="keys" />
      {resetKey && <QuotaResetDialog keyId={resetKey} onClose={() => setResetKey(null)} onComplete={load} />}
      {plain && <PlainKeyModal plainKey={plain} title={t("keys.rotated")} onClose={() => setPlain(null)} />}
    </div>
  );
}
