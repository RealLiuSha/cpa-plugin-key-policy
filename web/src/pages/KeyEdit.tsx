import { useEffect, useMemo, useState } from "react";
import { useLocation, useNavigate, useParams } from "react-router-dom";
import { deleteKey, listKeys, patchKey, rotateKey } from "../api/keys";
import KeyForm, { keyWriteRequestFromForm, type KeyFormValues } from "../components/KeyForm";
import KeyMoreMenu, { type KeyMoreMenuItem } from "../components/KeyMoreMenu";
import { MobileFormHeader, MobileTabBar } from "../components/MobileChrome";
import PlainKeyModal from "../components/PlainKeyModal";
import { useT } from "../i18n";
import type { KeyPublic } from "../types";

const EDIT_RESET_ITEMS: KeyMoreMenuItem[] = ["daily", "weekly", "monthly", "rpm"];

interface ReturnedModelState {
  createdModel?: string;
  draftKey?: KeyFormValues;
}

export default function KeyEdit() {
  const { id } = useParams<{ id: string }>();
  const navigate = useNavigate();
  const location = useLocation();
  const t = useT();
  const [key, setKey] = useState<KeyPublic | null>(null);
  const [error, setError] = useState("");
  const [loading, setLoading] = useState(true);
  const [plain, setPlain] = useState<string | null>(null);
  const [plainTitle, setPlainTitle] = useState("");

  useEffect(() => {
    void (async () => {
      setLoading(true);
      try {
        const all = await listKeys();
        const found = all.find((candidate) => candidate.id === decodeURIComponent(id ?? ""));
        if (found) setKey(found); else setError(t("keys.notFound"));
      } catch (reason) {
        setError((reason as Error).message ?? t("keys.loadFailed"));
      } finally {
        setLoading(false);
      }
    })();
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

  const onRotate = async () => {
    if (!confirm(t("keys.rotateConfirm", { id: key.id }))) return;
    try {
      const response = await rotateKey(key.id);
      setPlain(response.plain_key);
      setPlainTitle(t("keys.rotated"));
    } catch (reason) {
      alert((reason as Error).message ?? t("keys.rotateFailed"));
    }
  };
  const onDelete = async () => {
    if (!confirm(t("keys.deleteConfirm", { id: key.id }))) return;
    try {
      await deleteKey(key.id);
      navigate("/keys");
    } catch (reason) {
      alert((reason as Error).message ?? t("keys.deleteFailed"));
    }
  };
  const resetMenuProps = { keyId: key.id, items: EDIT_RESET_ITEMS, summaryLabel: t("keys.reset") };
  const title = t("edit.title", { id: key.id });

  return (
    <div className="form-page">
      <div className="fp-head mobile-hidden">
        <h1>{t("edit.hTitle")}</h1>
        <div className="fp-actions">
          <KeyMoreMenu {...resetMenuProps} />
          <button className="btn sm" onClick={() => void onRotate()}>{t("keys.resetKey")}</button>
          <button className="btn sm" onClick={() => navigate("/keys")}>{t("keyForm.cancel")}</button>
        </div>
      </div>
      <div className="fp-idline mobile-hidden">{key.id}<span className="fp-name">{key.name}</span></div>
      <MobileFormHeader title={title} backTo="/keys" />
      <div className="mobile-only mobile-key-reset"><KeyMoreMenu {...resetMenuProps} /></div>
      <KeyForm
        initial={initial}
        idReadOnly
        showCurrentUsage
        returnPath={`/keys/${encodeURIComponent(key.id)}/edit`}
        submitLabel={t("edit.save")}
        onCancel={() => navigate("/keys")}
        dangerLabel={t("keys.delete")}
        onDanger={() => void onDelete()}
        onSubmit={async (values) => {
          await patchKey(keyWriteRequestFromForm(values));
          navigate("/keys");
        }}
      />
      {plain && <PlainKeyModal plainKey={plain} title={plainTitle} onClose={() => setPlain(null)} />}
      <MobileTabBar active="keys" />
    </div>
  );
}
