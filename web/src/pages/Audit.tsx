import { useCallback, useEffect, useState } from "react";
import { fetchAuditEvents } from "../api/audit";
import { extractApiError } from "../api/error";
import type { AuditEvent } from "../types";
import { useT } from "../i18n";

function formatChanges(event: AuditEvent): string {
  if (!event.changes) return "—";
  return Object.entries(event.changes)
    .map(([field, change]) => `${field}: ${JSON.stringify(change.from)} → ${JSON.stringify(change.to)}`)
    .join("; ");
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
      </div>
      <form className="key-list-toolbar" onSubmit={(event) => { event.preventDefault(); void load(); }}>
        <input
          className="input"
          value={keyId}
          onChange={(event) => setKeyId(event.target.value)}
          placeholder={t("audit.keyFilter")}
          aria-label={t("audit.keyFilter")}
        />
        <button className="btn sm" type="submit">{t("audit.filter")}</button>
      </form>
      {error && <div className="error">{error}</div>}
      {loading ? <div className="muted">{t("audit.loading")}</div> : (
        <div className="card table-wrap">
          <table>
            <thead><tr>
              <th>{t("audit.colTime")}</th>
              <th>{t("audit.colAction")}</th>
              <th>{t("audit.colKey")}</th>
              <th>{t("audit.colActor")}</th>
              <th>{t("audit.colChanges")}</th>
            </tr></thead>
            <tbody>
              {events.length === 0 ? (
                <tr><td colSpan={5} className="muted">{t("audit.empty")}</td></tr>
              ) : events.map((event, index) => (
                <tr key={`${event.ts}-${event.action}-${index}`}>
                  <td>{new Date(event.ts).toLocaleString()}</td>
                  <td className="mono">{event.action}</td>
                  <td className="mono">{event.key_id || "—"}</td>
                  <td title={t("audit.actorHint")}>{event.actor}</td>
                  <td className="audit-changes">{formatChanges(event)}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </div>
  );
}
