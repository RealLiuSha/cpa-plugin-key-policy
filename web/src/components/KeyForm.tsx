import { useState } from "react";
import type { KeyPublic, KeyWriteRequest, QuotaMode } from "../types";
import { extractApiError } from "../api/error";
import { formatUSD } from "../format";
import { useT } from "../i18n";
import { balanceOf } from "./BalanceUsage";
import { MAX_BALANCE_ADJUSTMENT_USD } from "./BalanceDialog";

export interface KeyFormValues {
  id: string;
  name: string;
  enabled: boolean;
  rpm: number;
  daily_limit_usd: number;
  weekly_limit_usd: number;
  monthly_limit_usd: number;
  quota_mode: QuotaMode;
  // Balance written right after a new balance key is created; 0 writes none.
  initial_balance_usd: number;
}

// A balance key has no period limits, so they are left out: the server clears
// them when a key switches to balance mode.
export function keyWriteRequestFromForm(values: KeyFormValues): KeyWriteRequest {
  const request: KeyWriteRequest = {
    id: values.id,
    name: values.name.trim() || undefined,
    enabled: values.enabled,
    rpm: values.rpm,
    quota_mode: values.quota_mode,
  };
  if (values.quota_mode === "cycle") {
    request.daily_limit_usd = values.daily_limit_usd;
    request.weekly_limit_usd = values.weekly_limit_usd;
    request.monthly_limit_usd = values.monthly_limit_usd;
  }
  return request;
}

interface Props {
  initial?: KeyPublic;
  idReadOnly?: boolean;
  submitLabel: string;
  onSubmit: (values: KeyFormValues) => Promise<void>;
  onCancel: () => void;
  error?: string;
  showCurrentUsage?: boolean;
  dangerLabel?: string;
  onDanger?: () => void;
}

// A new key without an ID gets a short random one; IDs never change later.
export function generatedKeyID(): string {
  const bytes = new Uint8Array(4);
  crypto.getRandomValues(bytes);
  return "key-" + [...bytes].map((value) => value.toString(16).padStart(2, "0")).join("");
}

function parseNumber(value: string): number {
  const parsed = Number.parseFloat(value);
  return Number.isFinite(parsed) ? parsed : 0;
}

export default function KeyForm({
  initial,
  idReadOnly,
  submitLabel,
  onSubmit,
  onCancel,
  error,
  showCurrentUsage = false,
  dangerLabel,
  onDanger,
}: Props) {
  const t = useT();
  const [id, setID] = useState(initial?.id ?? "");
  const [name, setName] = useState(initial?.name ?? "");
  const [enabled, setEnabled] = useState(initial?.enabled ?? true);
  const [rpm, setRPM] = useState(initial?.rpm ?? 0);
  const [dailyLimit, setDailyLimit] = useState(initial?.daily_limit_usd ?? 0);
  const [weeklyLimit, setWeeklyLimit] = useState(initial?.weekly_limit_usd ?? 0);
  const [monthlyLimit, setMonthlyLimit] = useState(initial?.monthly_limit_usd ?? 0);
  const savedMode: QuotaMode = initial?.quota_mode ?? initial?.usage.quota_mode ?? "cycle";
  const [quotaMode, setQuotaMode] = useState<QuotaMode>(savedMode);
  const [initialBalance, setInitialBalance] = useState("");
  const [busy, setBusy] = useState(false);
  const [localError, setLocalError] = useState("");

  // What saving a changed quota type does to this key, in one notice.
  const keptBalance = initial ? balanceOf(initial.usage).balance : 0;
  const switchNotes: string[] = [];
  let switchWarns = false;
  if (initial && savedMode === "cycle" && quotaMode === "balance") {
    if (dailyLimit > 0 || weeklyLimit > 0 || monthlyLimit > 0) {
      switchNotes.push(t("balance.switchClearsLimits"));
      switchWarns = true;
    }
    if (keptBalance <= 0) {
      switchNotes.push(t("balance.switchEmptyBalance"));
      switchWarns = true;
    } else switchNotes.push(t("balance.switchStartsEmpty", { amount: formatUSD(keptBalance) }));
  }
  if (initial && savedMode === "balance" && quotaMode === "cycle") switchNotes.push(t("balance.switchKeepsBalance", { amount: formatUSD(keptBalance) }));

  const submit = async (event: React.FormEvent) => {
    event.preventDefault();
    setLocalError("");
    if ([rpm, dailyLimit, weeklyLimit, monthlyLimit].some((value) => value < 0)) {
      setLocalError(t("keyForm.limitInvalid"));
      return;
    }
    const startingBalance = initialBalance.trim() === "" ? 0 : Number.parseFloat(initialBalance);
    if (!Number.isFinite(startingBalance) || startingBalance < 0 || startingBalance > MAX_BALANCE_ADJUSTMENT_USD) {
      setLocalError(t("balance.amountInvalid", { max: formatUSD(MAX_BALANCE_ADJUSTMENT_USD) }));
      return;
    }
    setBusy(true);
    try {
      const values: KeyFormValues = {
        id: id.trim() || generatedKeyID(), name, enabled, rpm,
        daily_limit_usd: dailyLimit, weekly_limit_usd: weeklyLimit, monthly_limit_usd: monthlyLimit,
        quota_mode: quotaMode, initial_balance_usd: quotaMode === "balance" && !initial ? startingBalance : 0,
      };
      await onSubmit(values);
    } catch (reason) {
      setLocalError(extractApiError(reason, t("keyForm.submitFailed")));
    } finally {
      setBusy(false);
    }
  };

  return (
    <form className="card key-form" onSubmit={submit}>
      {(error || localError) && <div className="error">{error || localError}</div>}

      <section className="kf-section">
        <h2>{t("keyForm.mobile.sectionBasic")}</h2>
        <div className="form-grid">
          <label><span className="field-label">{t("keyForm.idLabel")}</span><input className="input" value={id} disabled={idReadOnly} placeholder={t("keyForm.idPlaceholder")} onChange={(event) => setID(event.target.value)} /></label>
          <label><span className="field-label">{t("keyForm.nameLabel")}</span><input className="input" value={name} placeholder={t("keyForm.namePlaceholder")} onChange={(event) => setName(event.target.value)} /></label>
          <label><span className="field-label">{t("keyForm.rpmLabel")}</span><input className="input" type="number" min="0" value={rpm} onChange={(event) => setRPM(parseNumber(event.target.value))} /></label>
          <fieldset className="check-field">
            <legend>{t("keyForm.statusLabel")}</legend>
            <label className="check-row"><input type="checkbox" checked={enabled} onChange={(event) => setEnabled(event.target.checked)} />{t("keyForm.enableKey")}</label>
          </fieldset>
        </div>
      </section>

      <section className="kf-section">
        <h2>{t("keyForm.mobile.sectionLimits")}</h2>
        <div className="kf-mode">
          <span className="field-label" id="quota-mode-label">{t("balance.modeLabel")}</span>
          <div className="seg kf-mode-seg" role="radiogroup" aria-labelledby="quota-mode-label">
            {(["cycle", "balance"] as QuotaMode[]).map((mode) => (
              <button key={mode} type="button" role="radio" aria-checked={quotaMode === mode} className={"seg-btn " + (quotaMode === mode ? "active" : "")} onClick={() => setQuotaMode(mode)}>{t(`balance.mode.${mode}`)}</button>
            ))}
          </div>
          <small className="field-hint">{t(`balance.modeHint.${quotaMode}`)}</small>
        </div>
        {switchNotes.length > 0 && <div className={`notice kf-mode-notice${switchWarns ? "" : " info"}`} role={switchWarns ? "alert" : "status"}>
          {switchNotes.length === 1 ? switchNotes[0] : <ul>{switchNotes.map((note) => <li key={note}>{note}</li>)}</ul>}
        </div>}
        {quotaMode === "cycle" ? (
          <div className="form-grid">
            <label>
              <span className="field-label">{t("keyForm.dailyLimitLabel")}{dailyLimit === 0 && <span className="badge">{t("keyForm.unlimited")}</span>}</span>
              <input className="input" type="number" min="0" step="0.01" value={dailyLimit} onChange={(event) => setDailyLimit(parseNumber(event.target.value))} />
              <small className="field-hint">{t("keyForm.dailyLimitHint")}</small>
              {showCurrentUsage && initial && <small className="kf-current-usage">{t("keyForm.currentUsage", { amount: formatUSD(initial.usage.daily_usd) })}</small>}
            </label>
            <label>
              <span className="field-label">{t("keyForm.weeklyLimitLabel")}{weeklyLimit === 0 && <span className="badge">{t("keyForm.unlimited")}</span>}</span>
              <input className="input" type="number" min="0" step="0.01" value={weeklyLimit} onChange={(event) => setWeeklyLimit(parseNumber(event.target.value))} />
              <small className="field-hint">{t("keyForm.weeklyLimitHint")}</small>
              {showCurrentUsage && initial && <small className="kf-current-usage">{t("keyForm.currentUsage", { amount: formatUSD(initial.usage.weekly_usd) })}</small>}
            </label>
            <label>
              <span className="field-label">{t("keyForm.monthlyLimitLabel")}{monthlyLimit === 0 && <span className="badge">{t("keyForm.unlimited")}</span>}</span>
              <input className="input" type="number" min="0" step="0.01" value={monthlyLimit} onChange={(event) => setMonthlyLimit(parseNumber(event.target.value))} />
              <small className="field-hint">{t("keyForm.monthlyLimitHint")}</small>
              {showCurrentUsage && initial && <small className="kf-current-usage">{t("keyForm.currentUsage", { amount: formatUSD(initial.usage.monthly_usd) })}</small>}
            </label>
          </div>
        ) : initial ? (
          savedMode === "balance" && <p className="kf-balance-current">{t("balance.currentOnEdit", { amount: formatUSD(keptBalance) })}</p>
        ) : (
          <div className="form-grid">
            <label>
              <span className="field-label">{t("balance.initialLabel")}</span>
              <input className="input" type="number" min="0" step="0.01" inputMode="decimal" value={initialBalance} placeholder="0.00" onChange={(event) => setInitialBalance(event.target.value)} />
              <small className="field-hint">{t("balance.initialHint")}</small>
            </label>
          </div>
        )}
      </section>

      <div className="form-actions">
        {dangerLabel && onDanger && <button type="button" className="btn danger" onClick={onDanger}>{dangerLabel}</button>}
        <span className="form-actions-spacer" />
        <button type="button" className="btn" onClick={onCancel}>{t("keyForm.cancel")}</button>
        <button className="btn primary" disabled={busy}>{busy ? t("keyForm.submitting") : submitLabel}</button>
      </div>
    </form>
  );
}
