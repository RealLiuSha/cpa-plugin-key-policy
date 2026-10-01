import { useCallback, useEffect, useState } from "react";
import { fetchAuditEvents } from "../api/audit";
import { extractApiError } from "../api/error";
import type { AuditEvent } from "../types";
import { useT } from "../i18n";

type Translate = (key: string, variables?: Record<string, string | number>) => string;

function labelOr(t: Translate, key: string, fallback: string): string {
  const label = t(key);
  return label === key ? fallback : label;
}

function formatValue(value: unknown): string {
  if (value === null || value === undefined || value === "") return "—";
  if (Array.isArray(value)) return value.map(formatValue).join("; ");
  if (typeof value === "object") {
    return Object.entries(value as Record<string, unknown>).map(([name, item]) => `${name} ${formatValue(item)}`).join(", ") || "—";
  }
  return String(value);
}

// The subject is the key, or for model events the model the change names.
export function auditSubject(event: AuditEvent): string {
  if (event.key_id) return event.key_id;
  const model = event.changes?.model;
  return formatValue(model?.to || model?.from);
}

// Enum values and per-model limits read better in the operator's language.
function fieldValue(field: string, value: unknown, t: Translate): string {
  if (field === "billing_mode" && (value === "tokens" || value === "per_call")) return t(value === "tokens" ? "models.tokens" : "models.perCall");
  if (field === "window" && typeof value === "string") return labelOr(t, `quota.${value}`, value);
  if (field === "model_daily_limits" && value && typeof value === "object") {
    return Object.entries(value as Record<string, unknown>).map(([name, limit]) => `${name} ${limit ? `$${limit}` : t("keyForm.unlimited")}`).join(", ") || "—";
  }
  return formatValue(value);
}

// Changes worth reading: the model identity is already the subject, a field
// whose value did not change says nothing, and a creation only has new values.
export function auditChanges(event: AuditEvent, t: Translate): string[] {
  const created = /^(create|import_create)_/.test(event.action);
  return Object.entries(event.changes ?? {})
    .filter(([field, change]) => field !== "model" && JSON.stringify(change.from) !== JSON.stringify(change.to))
    .map(([field, change]) => {
      const label = labelOr(t, `audit.field.${field}`, field);
      if (field === "removed") return `${label}：${formatValue(change.from)}`;
      if (created) return `${label}：${fieldValue(field, change.to, t)}`;
      return `${label}：${fieldValue(field, change.from, t)} → ${fieldValue(field, change.to, t)}`;
    });
}

export default function Audit() {
  const t = useT();
  const [keyId, setKeyId] = useState("");
  const [events, setEvents] = useState<AuditEvent[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState("");

  const load = useCallback(async () => {
    setLoading(true);
    setError("");
    try {
      setEvents(await fetchAuditEvents(keyId.trim(), 100));
    } catch (cause) {
      setError(extractApiError(cause, t("audit.loadFailed")));
    } finally {
      setLoading(false);
    }
  }, [keyId, t]);

  useEffect(() => {
    void load();
    // The filter applies on explicit submit to avoid one request per keystroke.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  return (
    <div className="audit-page">
      <div className="fp-head">
        <h1>{t("audit.title")}</h1>
        <p className="muted">{t("audit.description")}</p>
      </div>
      <form className="key-list-toolbar" onSubmit={(event) => { event.preventDefault(); void load(); }}>
        <input className="input" value={keyId} onChange={(event) => setKeyId(event.target.value)} placeholder={t("audit.keyFilter")} aria-label={t("audit.keyFilter")} />
        <button className="btn sm" type="submit">{t("audit.filter")}</button>
      </form>
      {error && <div className="error" role="alert">{error}</div>}
      {loading ? <div className="muted">{t("audit.loading")}</div> : events.length === 0 ? <div className="card muted">{t("audit.empty")}</div> : (
        <div className="audit-list">
          {events.map((event, index) => {
            const changes = auditChanges(event, t);
            return (
              <article className="card audit-item" key={`${event.ts}-${event.action}-${index}`}>
                <div className="audit-head">
                  <strong>{labelOr(t, `audit.action.${event.action}`, event.action)}</strong>
                  <span className="mono">{auditSubject(event)}</span>
                  <time className="muted" dateTime={event.ts}>{new Date(event.ts).toLocaleString()}</time>
                </div>
                {changes.length > 0 && <ul className="audit-changes">{changes.map((change) => <li key={change}>{change}</li>)}</ul>}
              </article>
            );
          })}
        </div>
      )}
    </div>
  );
}
