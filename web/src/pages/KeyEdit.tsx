import KeyActionDialog, { type KeyAction } from "../components/KeyActionDialog";
import QuotaResetDialog from "../components/QuotaResetDialog";
import { extractApiError } from "../api/error";
import { keyListReturnPath } from "../navigation";
import { useEffect, useMemo, useState } from "react";
import { useLocation, useNavigate, useParams } from "react-router-dom";
import { listKeys, patchKey } from "../api/keys";
import KeyForm, { keyWriteRequestFromForm, type KeyFormValues } from "../components/KeyForm";
import KeyMoreMenu, { type KeyMoreMenuItem } from "../components/KeyMoreMenu";
import { MobileFormHeader, MobileTabBar } from "../components/MobileChrome";
import PlainKeyModal from "../components/PlainKeyModal";
import { useT } from "../i18n";
import type { KeyPublic } from "../types";

const EDIT_RESET_ITEMS: KeyMoreMenuItem[] = ["rpm"];

interface ReturnedModelState {
  createdModel?: string;
  draftKey?: KeyFormValues;
}

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

  const returned = location.state as ReturnedModelState | null;
  const initial = useMemo<KeyPublic | null>(() => {
    if (!key) return null;
    if (!returned?.draftKey) return key;
    const models = [...returned.draftKey.models];
    if (returned.createdModel && !models.some((model) => model.name.toLowerCase() === returned.createdModel?.toLowerCase())) {
      models.push({ name: returned.createdModel, daily_limit_usd: 0 });
    }
    return { ...key, ...returned.draftKey, id: key.id, models };
  }, [key, returned]);

  if (loading) return <div className="muted">{t("keys.loading")}</div>;
  if (error || !key || !initial) return <div className="error">{error || t("edit.notFound")}</div>;

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
        <button className="btn sm" onClick={() => setShowReset(true)}>{t("quota.resetTitle")}</button>
        <button className="btn sm" onClick={() => setPendingAction("rotate")}>{t("keys.resetKey")}</button>
        <KeyMoreMenu keyId={key.id} items={EDIT_RESET_ITEMS} onResetComplete={refreshUsage} />
      </div>
      <KeyForm
        initial={initial}
        idReadOnly
        showCurrentUsage
        returnPath={`/keys/${encodeURIComponent(key.id)}/edit`}
        keyListReturnTo={backTo}
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
      {pendingAction && <KeyActionDialog keyId={key.id} action={pendingAction} onClose={() => setPendingAction(null)} onComplete={(result) => {
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
