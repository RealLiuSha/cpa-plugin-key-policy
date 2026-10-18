import KeyActionDialog, { type KeyAction } from "../components/KeyActionDialog";
import QuotaResetDialog from "../components/QuotaResetDialog";
import BalanceDialog from "../components/BalanceDialog";
import { balanceOf, isBalanceKey } from "../components/BalanceUsage";
import { extractApiError } from "../api/error";
import { keyListReturnPath } from "../navigation";
import { useEffect, useState } from "react";
import { useLocation, useNavigate, useParams } from "react-router-dom";
import { listKeys, patchKey } from "../api/keys";
import KeyForm, { keyWriteRequestFromForm } from "../components/KeyForm";
import KeyMoreMenu, { type KeyMoreMenuItem } from "../components/KeyMoreMenu";
import { MobileFormHeader, MobileTabBar } from "../components/MobileChrome";
import PlainKeyModal from "../components/PlainKeyModal";
import { useT } from "../i18n";
import type { KeyPublic } from "../types";

const EDIT_RESET_ITEMS: KeyMoreMenuItem[] = ["rpm"];

export default function KeyEdit() {
  const { id } = useParams<{ id: string }>();
  const navigate = useNavigate();
  const location = useLocation();
  const backTo = keyListReturnPath(location.state);
  const t = useT();
  const [key, setKey] = useState<KeyPublic | null>(null);
  const [error, setError] = useState("");
  const [loading, setLoading] = useState(true);
  const [plain, setPlain] = useState<string | null>(null);
  const [plainTitle, setPlainTitle] = useState("");
  const [pendingAction, setPendingAction] = useState<KeyAction | null>(null);
  const [showReset, setShowReset] = useState(false);
  const [showBalance, setShowBalance] = useState(false);

  useEffect(() => {
    let alive = true;
    void (async () => {
      setLoading(true);
      setError("");
      try {
        const all = await listKeys();
        if (!alive) return;
        const found = all.find((candidate) => candidate.id === decodeURIComponent(id ?? ""));
        if (found) setKey(found); else setError(t("keys.notFound"));
      } catch (reason) {
        if (alive) setError(extractApiError(reason, t("keys.loadFailed")));
      } finally {
        if (alive) setLoading(false);
      }
    })();
    return () => { alive = false; };
  }, [id, t]);

  if (loading) return <div className="muted">{t("keys.loading")}</div>;
  if (error || !key) return <div className="error">{error || t("edit.notFound")}</div>;

  const refreshUsage = async () => {
    const refreshed = (await listKeys()).find((candidate) => candidate.id === key.id);
    if (refreshed) setKey(refreshed);
  };
  const title = t("edit.title", { id: key.id });

  return (
    <div className="form-page">
      <div className="fp-head mobile-hidden">
        <h1>{t("edit.hTitle")}</h1>
        <div className="fp-actions">
          <button className="btn sm" onClick={() => navigate(backTo)}>{t("keyForm.cancel")}</button>
        </div>
      </div>
      <div className="fp-idline mobile-hidden">{key.id}<span className="fp-name">{key.name}</span></div>
      <MobileFormHeader title={title} backTo={backTo} />
      <div className="key-edit-operations">
        {isBalanceKey(key)
          ? <button className="btn sm" onClick={() => setShowBalance(true)}>{t("balance.adjust")}</button>
          : <button className="btn sm" onClick={() => setShowReset(true)}>{t("quota.resetTitle")}</button>}
        <button className="btn sm" onClick={() => setPendingAction("rotate")}>{t("keys.resetKey")}</button>
        <KeyMoreMenu keyId={key.id} items={EDIT_RESET_ITEMS} onResetComplete={refreshUsage} />
      </div>
      <KeyForm
        initial={key}
        idReadOnly
        showCurrentUsage
        submitLabel={t("edit.save")}
        onCancel={() => navigate(backTo)}
        dangerLabel={t("keys.delete")}
        onDanger={() => setPendingAction("delete")}
        onSubmit={async (values) => {
          await patchKey(keyWriteRequestFromForm(values));
          navigate(backTo);
        }}
      />
      {showReset && <QuotaResetDialog keyId={key.id} onClose={() => setShowReset(false)} onComplete={refreshUsage} />}
      {showBalance && <BalanceDialog keyId={key.id} onClose={() => setShowBalance(false)} onComplete={refreshUsage} />}
      {pendingAction && <KeyActionDialog keyId={key.id} balanceUSD={key.usage.balance_usd === undefined ? undefined : balanceOf(key.usage).balance} action={pendingAction} onClose={() => setPendingAction(null)} onComplete={(result) => {
        if (result) {
          setKey(result.key);
          setPlain(result.plain_key);
          setPlainTitle(t("keys.rotated"));
        } else navigate(backTo);
      }} />}
      {plain && <PlainKeyModal plainKey={plain} title={plainTitle} onClose={() => setPlain(null)} />}
      <MobileTabBar active="keys" />
    </div>
  );
}
