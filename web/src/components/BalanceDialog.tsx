import { useEffect, useState } from "react";
import { Link } from "react-router-dom";
import axios from "axios";
import { adjustBalance, fetchKeyUsage } from "../api/keys";
import { extractApiError } from "../api/error";
import type { BalanceAdjustment, KeyUsageResponse } from "../types";
import { formatUSD } from "../format";
import { useT } from "../i18n";
import Modal from "./Modal";

export const BALANCE_PRESETS = [10, 50, 100, 500];
export const MAX_BALANCE_ADJUSTMENT_USD = 1_000_000;
export const MAX_BALANCE_NOTE = 200;

type Direction = "credit" | "debit";

// One id per dialog: a retry after a lost response reuses it, so the server
// answers with the first result instead of adjusting twice.
export function newRequestId(): string {
  if (typeof crypto.randomUUID === "function") return crypto.randomUUID();
  const bytes = crypto.getRandomValues(new Uint8Array(16));
  return [...bytes].map((value) => value.toString(16).padStart(2, "0")).join("");
}

function errorCode(reason: unknown): string {
  if (!axios.isAxiosError(reason)) return "";
  const code = (reason.response?.data as { error?: { code?: unknown } } | undefined)?.error?.code;
  return typeof code === "string" ? code : "";
}

// A rejection the server answered (4xx) did not change the balance; anything
// else (timeout, network, 5xx) may have, and must be retried with the same id.
function uncertainFailure(reason: unknown): boolean {
  if (!axios.isAxiosError(reason)) return false;
  const status = reason.response?.status;
  return status === undefined || status >= 500;
}

export default function BalanceDialog({ keyId, onClose, onComplete }: {
  keyId: string;
  onClose: () => void;
  onComplete: () => void | Promise<void>;
}) {
  const t = useT();
  const [requestId] = useState(newRequestId);
  const [data, setData] = useState<KeyUsageResponse | null>(null);
  const [loading, setLoading] = useState(true);
  const [direction, setDirection] = useState<Direction>("credit");
  const [amount, setAmount] = useState("");
  const [note, setNote] = useState("");
  const [busy, setBusy] = useState(false);
  const [uncertain, setUncertain] = useState(false);
  const [error, setError] = useState("");
  const [result, setResult] = useState<BalanceAdjustment | null>(null);
  const [notice, setNotice] = useState("");

  useEffect(() => {
    let active = true;
    void fetchKeyUsage(keyId).then((value) => { if (active) setData(value); })
      .catch((reason: unknown) => { if (active) setError(extractApiError(reason, t("keys.loadFailed"))); })
      .finally(() => { if (active) setLoading(false); });
    return () => { active = false; };
  }, [keyId, t]);

  const balanceMode = data?.usage?.quota_mode === "balance";
  const current = data?.usage?.balance_usd ?? 0;
  const value = Number.parseFloat(amount);
  const valid = Number.isFinite(value) && value > 0 && value <= MAX_BALANCE_ADJUSTMENT_USD;
  const signed = direction === "credit" ? value : -value;
  const after = valid ? current + signed : current;
  const locked = busy || uncertain;

  const submit = async () => {
    if (!valid || busy) return;
    setBusy(true);
    setError("");
    try {
      const adjusted = await adjustBalance(keyId, signed, note, requestId);
      setResult(adjusted);
      setUncertain(false);
    } catch (reason) {
      const code = errorCode(reason);
      if (uncertainFailure(reason)) {
        setUncertain(true);
        setError(t("balance.uncertain"));
      } else if (code === "not_balance_mode") {
        setError(t("balance.notBalanceMode"));
      } else if (code === "request_id_conflict") {
        setError(t("balance.requestConflict"));
      } else {
        setError(extractApiError(reason, t("balance.failed")));
      }
      setBusy(false);
      return;
    }
    try {
      await onComplete();
    } catch {
      setNotice(t("balance.refreshFailed"));
    } finally {
      setBusy(false);
    }
  };

  const title = t("balance.adjustTitle");
  return (
    <Modal title={title} closeLabel={t("keyForm.cancel")} onClose={onClose} dismissible={!busy}>
      <p className="quota-reset-person"><strong>{data?.key_name || keyId}</strong><span className="muted">{keyId}</span></p>
      {result ? (
        <div className="quota-reset-success" role="status">
          <strong>{result.duplicate ? t("balance.duplicate") : t(result.amount_usd >= 0 ? "balance.credited" : "balance.debited", { amount: formatUSD(Math.abs(result.amount_usd)) })}</strong>
          {/* A repeated request id answers with its first result, so the balance is the one right after it. */}
          <p>{t(result.duplicate ? "balance.afterThen" : "balance.nowBalance", { amount: formatUSD(result.balance_usd) })}</p>
        </div>
      ) : loading ? <p className="muted">{t("keys.loading")}</p> : data && !balanceMode ? (
        <div className="balance-mode-hint" role="alert">
          <p>{t("balance.notBalanceMode")}</p>
          <Link className="btn sm" to={`/keys/${encodeURIComponent(keyId)}/edit`} onClick={onClose}>{t("keys.edit")}</Link>
        </div>
      ) : data ? (
        <form className="balance-form" onSubmit={(event) => { event.preventDefault(); void submit(); }}>
          <div className="seg balance-direction" role="radiogroup" aria-label={t("balance.direction")}>
            {(["credit", "debit"] as Direction[]).map((item) => (
              <button key={item} type="button" role="radio" aria-checked={direction === item} className={"seg-btn " + (direction === item ? "active" : "")} disabled={locked} onClick={() => setDirection(item)}>{t(`balance.${item}`)}</button>
            ))}
          </div>
          <label className="balance-amount">
            <span className="field-label">{t("balance.amountLabel")}</span>
            <input className="input" type="number" inputMode="decimal" min="0" step="0.01" autoFocus value={amount} disabled={locked} placeholder="0.00" onChange={(event) => setAmount(event.target.value)} />
          </label>
          {direction === "credit" && (
            <div className="balance-presets" role="group" aria-label={t("balance.presets")}>
              {BALANCE_PRESETS.map((preset) => <button key={preset} type="button" className="btn sm" disabled={locked} onClick={() => setAmount(String(preset))}>+{formatUSD(preset)}</button>)}
            </div>
          )}
          <label>
            <span className="field-label">{t("balance.noteLabel")}</span>
            <input className="input" value={note} maxLength={MAX_BALANCE_NOTE} disabled={locked} placeholder={t("balance.notePlaceholder")} onChange={(event) => setNote(event.target.value)} />
          </label>
          <dl className={"quota-reset-preview" + (valid && after <= 0 ? " balance-preview-negative" : "")} aria-live="polite">
            <div><dt>{t("balance.current")}</dt><dd>{formatUSD(current)}</dd></div>
            <div><dt>{t("balance.after")}</dt><dd>{valid ? formatUSD(after) : "—"}</dd></div>
          </dl>
          {amount !== "" && !valid && <p className="error">{t("balance.amountInvalid", { max: formatUSD(MAX_BALANCE_ADJUSTMENT_USD) })}</p>}
          {valid && after <= 0 && <p className="balance-warning">{t("balance.afterNotPositive")}</p>}
          <p className="muted quota-reset-help">{t("balance.help")}</p>
        </form>
      ) : null}
      {error && <div className="error" role="alert">{error}</div>}
      {notice && <p className="quota-reset-help" role="status">{notice}</p>}
      <div className="form-actions modal-actions">
        {result ? <button className="btn primary" onClick={onClose}>{t("quota.done")}</button> : <>
          <button className="btn" disabled={busy} onClick={onClose}>{t("keyForm.cancel")}</button>
          {balanceMode && <button className="btn primary" disabled={busy || !valid} onClick={() => void submit()}>
            {busy ? t("balance.submitting") : uncertain ? t("balance.retry") : valid ? t(direction === "credit" ? "balance.confirmCredit" : "balance.confirmDebit", { amount: formatUSD(value) }) : t(direction === "credit" ? "balance.credit" : "balance.debit")}
          </button>}
        </>}
      </div>
    </Modal>
  );
}
