import { useEffect, useState } from "react";
import axios from "axios";
import { fetchKeyUsage, resetUsage } from "../api/keys";
import type { KeyUsageResponse, QuotaWindow } from "../types";
import { extractApiError } from "../api/error";
import { useT } from "../i18n";
import Modal from "./Modal";
import { formatQuotaDate, formatUSD } from "./QuotaUsage";

export default function QuotaResetDialog({ keyId, initialWindow, onClose, onComplete }: {
  keyId: string;
  initialWindow?: QuotaWindow;
  onClose: () => void;
  onComplete: () => void | Promise<void>;
}) {
  const t = useT();
  const [data, setData] = useState<KeyUsageResponse | null>(null);
  const [selected, setSelected] = useState<QuotaWindow>(initialWindow ?? "weekly");
  const [error, setError] = useState("");
  const [notice, setNotice] = useState("");
  const [loading, setLoading] = useState(true);
  const [busy, setBusy] = useState(false);
  const [completed, setCompleted] = useState<string | null>(null);
  const [retry, setRetry] = useState(0);

  useEffect(() => {
    let active = true;
    setLoading(true);
    setError("");
    void fetchKeyUsage(keyId).then((value) => {
      if (!active) return;
      setData(value);
      if (!initialWindow && retry === 0) {
        const blocked = value.usage?.cycles?.find((cycle) => `${cycle.window}_exceeded` === value.usage?.blocked_reason);
        setSelected(blocked?.window ?? (value.usage?.limited_models?.length ? "daily" : value.usage?.cycles?.find((cycle) => cycle.limit_usd > 0)?.window) ?? "weekly");
      }
    }).catch((reason: unknown) => { if (active) setError(extractApiError(reason, t("keys.loadFailed"))); })
      .finally(() => { if (active) setLoading(false); });
    return () => { active = false; };
  }, [keyId, initialWindow, retry, t]);

  const cycle = data?.usage?.cycles?.find((item) => item.window === selected);
  const submit = async () => {
    if (busy || !cycle) return;
    setBusy(true);
    setError("");
    try {
      const result = await resetUsage(keyId, selected, {
        started_at: cycle.started_at,
        reset_after_manual_at: cycle.reset_after_manual_at,
      });
      setCompleted(result.next_accounting_boundary_at);
      setNotice("");
    } catch (reason) {
      if (axios.isAxiosError(reason) && reason.response?.status === 409) {
        setNotice(t("quota.previewChanged"));
        setLoading(true);
        setData(null);
        setRetry((value) => value + 1);
      } else {
        setError(extractApiError(reason, t("keys.resetFailed")));
      }
      setBusy(false);
      return;
    }
    try {
      await onComplete();
    } catch {
      setNotice(t("quota.resetRefreshFailed"));
    } finally { setBusy(false); }
  };

  return (
    <Modal title={t("quota.resetTitle")} closeLabel={t("keyForm.cancel")} onClose={onClose} dismissible={!busy}>
      <p className="quota-reset-person"><strong>{data?.key_name || keyId}</strong><span className="muted">{keyId}</span></p>
      {completed ? <div className="quota-reset-success" role="status"><strong>{t("quota.resetDone")}</strong><p>{t("quota.resetsOn", { date: formatQuotaDate(completed, data?.usage?.timezone) })}</p></div> : <>
        {loading ? <p className="muted">{t("keys.loading")}</p> : <>
          {data?.usage?.cycles?.length ? <>
            <fieldset className="quota-reset-options" disabled={busy}><legend>{t("quota.chooseWindow")}</legend>
              {data.usage.cycles.map((item) => <label key={item.window} className={selected === item.window ? "selected" : ""}><input type="radio" name="quota-window" value={item.window} checked={selected === item.window} onChange={() => setSelected(item.window)} />{t(`quota.${item.window}`)}</label>)}
            </fieldset>
            {cycle && <dl className="quota-reset-preview"><div><dt>{t("quota.currentUsed")}</dt><dd>{formatUSD(cycle.used_usd)} → {formatUSD(0)}</dd></div><div><dt>{t("quota.afterReset")}</dt><dd>{formatQuotaDate(cycle.reset_after_manual_at, data.usage.timezone)}</dd></div></dl>}
            <p className="muted quota-reset-help">{t("quota.resetHelp")}</p>
            {selected === "daily" && <p className="muted quota-reset-help">{t("quota.resetDailyHelp")}</p>}
          </> : <p className="error">{t("quota.unavailable")}</p>}
        </>}
      </>}
      {error && <div className="error" role="alert">{error}</div>}
      {notice && <p className="quota-reset-help" role="status">{notice}</p>}
      <div className="form-actions modal-actions">
        {completed ? <button className="btn primary" disabled={busy} onClick={onClose}>{t("quota.done")}</button> : <>
          <button className="btn" disabled={busy} onClick={onClose}>{t("keyForm.cancel")}</button>
          {!data && !loading ? <button className="btn" onClick={() => setRetry((value) => value + 1)}>{t("keys.refresh")}</button> : <button className="btn primary" disabled={loading || busy || !cycle} onClick={() => void submit()}>{t(busy ? "quota.resetting" : "quota.confirmReset")}</button>}
        </>}
      </div>
    </Modal>
  );
}
