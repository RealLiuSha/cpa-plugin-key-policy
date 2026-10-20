import { useEffect, useState } from "react";
import { useSearchParams } from "react-router-dom";
import { fetchAuditEvents } from "../api/audit";
import { extractApiError } from "../api/error";
import type { AuditEvent } from "../types";
import { formatUSD } from "../format";
import { formatSignedUSD } from "../components/BalanceHistory";
import { useT } from "../i18n";
import { labelOr } from "../labels";

type Translate = (key: string, variables?: Record<string, string | number>) => string;

function formatValue(value: unknown): string {
  if (value === null || value === undefined || value === "") return "";
  if (Array.isArray(value)) return value.map(formatValue).join("; ");
  if (typeof value === "object") {
    return Object.entries(value as Record<string, unknown>).map(([name, item]) => `${name} ${formatValue(item)}`).join(", ");
  }
  return String(value);
}

// The subject is the key, the target of a channel, adapter or search change,
// or for model events the price row: the model and, when the event names it
// without changing it, the provider qualifier.
export function auditSubject(event: AuditEvent): string {
  if (event.key_id) return event.key_id;
  if (event.target) return event.target;
  const model = event.changes?.model;
  const name = formatValue(model?.to || model?.from) || "—";
  const provider = event.changes?.provider;
  return provider && provider.from === provider.to && provider.from ? `${name} (${formatValue(provider.from)})` : name;
}

const LIMIT_FIELDS = new Set(["daily_limit_usd", "weekly_limit_usd", "monthly_limit_usd"]);
const USED_FIELDS = new Set(["daily_usd", "weekly_usd", "monthly_usd", "balance_usd"]);
const PRICE_FIELDS = new Set(["input_price_per_million", "output_price_per_million", "cache_read_price_per_million", "cache_write_price_per_million", "per_call_usd"]);

// A value in the operator's language; "" means the side has no value.
// Configured limits and prices are shown verbatim, used amounts as money.
function fieldValue(field: string, value: unknown, t: Translate): string {
  if (field === "cache_write_price_per_million" && value === null) return t("models.sameAsInput");
  if (field === "provider" && value === "") return t("models.anyProvider");
  if (field === "enabled" && typeof value === "boolean") return t(value ? "audit.value.on" : "audit.value.off");
  if (field === "billing_mode" && (value === "tokens" || value === "per_call")) return t(value === "tokens" ? "models.tokens" : "models.perCall");
  if (field === "window" && typeof value === "string" && value) return labelOr(t, `quota.${value}`, value);
  if (field === "quota_mode" && (value === "cycle" || value === "balance")) return t(`balance.mode.${value}`);
  if (typeof value === "number") {
    if (field === "amount_usd") return formatSignedUSD(value);
    if ((LIMIT_FIELDS.has(field) || field === "rpm") && value === 0) return t("usage.unlimited");
    if (LIMIT_FIELDS.has(field) || PRICE_FIELDS.has(field)) return `$${value}`;
    if (USED_FIELDS.has(field)) return formatUSD(value);
    if (field === "billing_multiplier") return `×${value}`;
  }
  if (field === "next_reset_at" && typeof value === "string" && value) {
    const date = new Date(value);
    return Number.isNaN(date.getTime()) ? value : date.toLocaleString();
  }
  if (field === "model_daily_limits" && value && typeof value === "object") {
    return Object.entries(value as Record<string, unknown>).map(([name, limit]) => `${name} ${limit ? `$${limit}` : t("usage.unlimited")}`).join(", ");
  }
  return formatValue(value);
}

// Changes worth reading: the model is already the subject and an unchanged
// field says nothing. A creation shows its new values, a deletion what was
// removed, and a change from nothing only the new value.
export function auditChanges(event: AuditEvent, t: Translate): string[] {
  const created = /^(create|import_create)_/.test(event.action);
  const deleted = /^delete_/.test(event.action);
  return Object.entries(event.changes ?? {})
    .filter(([field, change]) => field !== "model" && JSON.stringify(change.from) !== JSON.stringify(change.to))
    .flatMap(([field, change]) => {
      const label = labelOr(t, `audit.field.${field}`, field);
      const from = created ? "" : fieldValue(field, change.from, t);
      const to = deleted ? "" : fieldValue(field, change.to, t);
      if (from && to) return [`${label}：${from} → ${to}`];
      if (from && !deleted) return [`${label}：${from} → —`];
      return from || to ? [`${label}：${from || to}`] : [];
    });
}

// Every action the plugin records or once recorded, in the filter's order.
export const AUDIT_ACTIONS = [
  "create_key", "update_key", "rotate_key", "delete_key", "reset_usage", "adjust_balance",
  "create_model", "update_model", "delete_model", "import_create_model", "import_update_prices", "migrate_state",
  "update_channel", "delete_channel", "upsert_adapter", "delete_adapter", "try_adapter", "update_search", "import_search",
  "run_quality", "update_inspection", "start_inspection", "clear_quality",
] as const;

// One event as a card: what was done, to what, when, and the changed fields.
export function AuditEventCard({ event }: { event: AuditEvent }) {
  const t = useT();
  const changes = auditChanges(event, t);
  return (
    <article className="card audit-item">
      <div className="audit-head">
        <strong>{labelOr(t, `audit.action.${event.action}`, event.action)}</strong>
        <span className="mono">{auditSubject(event)}</span>
        <time className="muted" dateTime={event.ts}>{new Date(event.ts).toLocaleString()}</time>
      </div>
      {changes.length > 0 && <ul className="audit-changes">{changes.map((change) => <li key={change}>{change}</li>)}</ul>}
    </article>
  );
}

// "Load more" asks again for the newest events with a larger limit; the
// server allows at most 1000.
export const AUDIT_LIMITS = [100, 500, 1000] as const;

export default function Audit() {
  const t = useT();
  // The filters live in the URL, so a link from a key's page opens the log
  // filtered to that key and its balance adjustments.
  const [params, setParams] = useSearchParams();
  const keyId = (params.get("key_id") ?? "").trim();
  const action = AUDIT_ACTIONS.find((value) => value === params.get("action")) ?? "";
  const limit = AUDIT_LIMITS.find((value) => String(value) === params.get("limit")) ?? AUDIT_LIMITS[0];
  const filter = `${keyId}\n${action}`;
  const [draftKey, setDraftKey] = useState(keyId);
  const [result, setResult] = useState<{ filter: string; limit: number; events: AuditEvent[] } | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState("");
  const [refresh, setRefresh] = useState(0);

  useEffect(() => { setDraftKey(keyId); }, [keyId]);
  useEffect(() => {
    let alive = true;
    setLoading(true);
    setError("");
    fetchAuditEvents({ keyId, action, limit })
      .then((events) => { if (alive) setResult({ filter, limit, events }); })
      .catch((cause) => { if (alive) setError(extractApiError(cause, t("audit.loadFailed"))); })
      .finally(() => { if (alive) setLoading(false); });
    return () => { alive = false; };
  }, [filter, keyId, action, limit, refresh, t]);

  // A new filter starts again from the newest 100 events.
  const applyFilters = (next: { key_id: string; action: string }) => {
    setParams((current) => {
      const updated = new URLSearchParams(current);
      for (const [field, value] of Object.entries(next)) {
        if (value) updated.set(field, value); else updated.delete(field);
      }
      updated.delete("limit");
      return updated;
    }, { replace: true });
    setRefresh((count) => count + 1);
  };
  // The step after the events on screen; after a failed attempt the URL
  // already asks for it, so the click retries.
  const loadMore = (shown: number) => {
    const next = AUDIT_LIMITS.find((value) => value > shown);
    if (!next) return;
    if (next === limit) {
      setRefresh((count) => count + 1);
      return;
    }
    setParams((current) => {
      const updated = new URLSearchParams(current);
      updated.set("limit", String(next));
      return updated;
    }, { replace: true });
  };
  const clearFilters = () => {
    setDraftKey("");
    applyFilters({ key_id: "", action: "" });
  };

  // The list on screen stays while more of it loads; a new filter replaces it.
  const events = result?.filter === filter ? result.events : null;
  const filtered = Boolean(keyId || action);
  const mayHaveMore = Boolean(result && events && events.length >= result.limit);

  return (
    <div className="audit-page">
      <div className="fp-head">
        <h1>{t("audit.title")}</h1>
        <p className="muted">{t("audit.description")}</p>
      </div>
      <form className="key-list-toolbar" onSubmit={(event) => { event.preventDefault(); applyFilters({ key_id: draftKey.trim(), action }); }}>
        <input className="input" value={draftKey} onChange={(event) => setDraftKey(event.target.value)} placeholder={t("audit.keyFilter")} aria-label={t("audit.keyFilter")} />
        <select className="input audit-action" value={action} onChange={(event) => applyFilters({ key_id: draftKey.trim(), action: event.target.value })} aria-label={t("audit.actionFilter")}>
          <option value="">{t("audit.actionAll")}</option>
          {AUDIT_ACTIONS.map((value) => <option key={value} value={value}>{labelOr(t, `audit.action.${value}`, value)}</option>)}
        </select>
        <button className="btn sm" type="submit">{t("audit.filter")}</button>
      </form>
      {error && <div className="error" role="alert">{error}</div>}
      {events === null ? (loading && <div className="muted" role="status">{t("audit.loading")}</div>) : events.length === 0 ? (
        <div className="card muted audit-empty">
          <span>{t(filtered ? "audit.emptyFiltered" : "audit.empty")}</span>
          {filtered && <button type="button" className="btn sm" onClick={clearFilters}>{t("quota.clearFilters")}</button>}
        </div>
      ) : (
        <>
          <div className="audit-summary muted">{t("audit.shown", { count: events.length })}</div>
          <div className="audit-list">
            {events.map((event, index) => <AuditEventCard key={`${event.ts}-${event.action}-${index}`} event={event} />)}
          </div>
          {mayHaveMore && (result!.limit < AUDIT_LIMITS[AUDIT_LIMITS.length - 1] ? (
            <button type="button" className="btn audit-more" disabled={loading} onClick={() => loadMore(result!.limit)}>{t(loading ? "audit.loading" : "audit.loadMore")}</button>
          ) : (
            <p className="muted audit-capped" role="note">{t("audit.capped", { count: result!.limit })}</p>
          ))}
        </>
      )}
    </div>
  );
}
