import { useEffect, useState } from "react";
import { Link } from "react-router-dom";
import { fetchAuditEvents } from "../api/audit";
import { extractApiError } from "../api/error";
import type { AuditEvent } from "../types";
import { formatUSD } from "../format";
import { useT } from "../i18n";

export const BALANCE_HISTORY_SHOWN = 20;

export interface BalanceChange {
  ts: string;
  amount: number;
  after: number | null;
  note: string;
}

// The adjust_balance audit events are the record of balance adjustments;
// usage deductions are not events and show up in the usage history instead.
export function balanceChanges(events: AuditEvent[]): BalanceChange[] {
  return events.map((event) => {
    const amount = event.changes?.amount_usd?.to;
    const after = event.changes?.balance_usd?.to;
    const note = event.changes?.note?.to;
    return {
      ts: event.ts,
      amount: typeof amount === "number" ? amount : 0,
      after: typeof after === "number" ? after : null,
      note: typeof note === "string" ? note : "",
    };
  });
}

export function formatSignedUSD(value: number): string {
  return (value > 0 ? "+" : value < 0 ? "−" : "") + formatUSD(Math.abs(value));
}

// version changes after an adjustment so the list reloads.
export default function BalanceHistory({ keyId, version }: { keyId: string; version: number }) {
  const t = useT();
  const [changes, setChanges] = useState<BalanceChange[] | null>(null);
  const [error, setError] = useState("");

  useEffect(() => {
    let active = true;
    setError("");
    void fetchAuditEvents({ keyId, action: "adjust_balance", limit: BALANCE_HISTORY_SHOWN + 1 })
      .then((events) => { if (active) setChanges(balanceChanges(events)); })
      .catch((reason: unknown) => { if (active) setError(extractApiError(reason, t("balance.historyFailed"))); });
    return () => { active = false; };
  }, [keyId, version, t]);

  return (
    <div className="balance-history" data-testid="balance-history">
      <h3>{t("balance.historyTitle")}</h3>
      {error ? <p className="error">{error}</p> : changes === null ? <p className="muted">{t("keys.loading")}</p> : changes.length === 0 ? <p className="muted">{t("balance.historyEmpty")}</p> : <>
        <ul>
          {changes.slice(0, BALANCE_HISTORY_SHOWN).map((change, index) => (
            <li key={`${change.ts}-${index}`}>
              <time className="muted" dateTime={change.ts}>{new Date(change.ts).toLocaleString()}</time>
              <strong className={change.amount < 0 ? "balance-debit" : "balance-credit"}>{formatSignedUSD(change.amount)}</strong>
              <span className="muted">{change.after === null ? "" : t("balance.historyAfter", { amount: formatUSD(change.after) })}</span>
              {change.note && <span className="balance-note">{change.note}</span>}
            </li>
          ))}
        </ul>
        {changes.length > BALANCE_HISTORY_SHOWN && <Link className="btn sm" to={`/audit?${new URLSearchParams({ key_id: keyId, action: "adjust_balance" })}`}>{t("balance.historyMore")}</Link>}
      </>}
    </div>
  );
}
