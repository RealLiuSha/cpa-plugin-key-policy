import { useMemo, useState } from "react";
import { useLocation, useNavigate } from "react-router-dom";
import { createKey } from "../api/keys";
import KeyForm, { keyWriteRequestFromForm, type KeyFormValues } from "../components/KeyForm";
import PlainKeyModal from "../components/PlainKeyModal";
import { MobileFormHeader, MobileTabBar } from "../components/MobileChrome";
import { useT } from "../i18n";
import type { KeyPublic } from "../types";

interface ReturnedModelState {
  createdModel?: string;
  draftKey?: KeyFormValues;
}

export default function KeyNew() {
  const navigate = useNavigate();
  const location = useLocation();
  const t = useT();
  const [plain, setPlain] = useState<string | null>(null);

  const returned = location.state as ReturnedModelState | null;
  const initial = useMemo<KeyPublic | undefined>(() => {
    if (!returned?.draftKey) return undefined;
    const models = [...returned.draftKey.models];
    if (returned.createdModel && !models.some((model) => model.name.toLowerCase() === returned.createdModel?.toLowerCase())) {
      models.push({ name: returned.createdModel, daily_limit_usd: 0 });
    }
    return {
      ...returned.draftKey,
      models,
      key_preview: "",
      usage: {
        daily_usd: 0,
        weekly_usd: 0,
        monthly_usd: 0,
        daily_limit_usd: returned.draftKey.daily_limit_usd,
        weekly_limit_usd: returned.draftKey.weekly_limit_usd,
        monthly_limit_usd: returned.draftKey.monthly_limit_usd,
      },
    };
  }, [returned]);

  const title = t("new.title");
  return (
    <div className="form-page">
      <div className="fp-head mobile-hidden"><h1>{title}</h1></div>
      <MobileFormHeader title={title} backTo="/keys" />
      <KeyForm
        initial={initial}
        returnPath="/keys/new"
        submitLabel={t("new.create")}
        onCancel={() => navigate("/keys")}
        onSubmit={async (values) => {
          const response = await createKey(keyWriteRequestFromForm(values));
          setPlain(response.plain_key);
        }}
      />
      <p className="fp-note mobile-hidden">{t("login.memoryNote")}</p>
      {plain && (
        <PlainKeyModal
          plainKey={plain}
          title={t("plainModal.created")}
          onClose={() => {
            setPlain(null);
            navigate("/keys");
          }}
        />
      )}
      <MobileTabBar active="new" />
    </div>
  );
}
