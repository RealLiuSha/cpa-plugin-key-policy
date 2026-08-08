import { useMemo, useState } from "react";
import { Link, useNavigate, useLocation } from "react-router-dom";
import { createKey } from "../api/keys";
import KeyForm, { keyWriteRequestFromForm } from "../components/KeyForm";
import PlainKeyModal from "../components/PlainKeyModal";
import { MobileFormHeader, MobileTabBar } from "../components/MobileChrome";
import { useT } from "../i18n";
import type { KeyPublic, ModelRule } from "../types";

export default function KeyNew() {
  const nav = useNavigate();
  const loc = useLocation();
  const t = useT();
  const [plain, setPlain] = useState<string | null>(null);
  const [unpricedAfterSave, setUnpricedAfterSave] = useState(0);

  const title = t("new.title");

  // When the standalone model-picker page returns here with a selection,
  // merge it into the form's initial models.
  const picked = (loc.state as { pickedModels?: ModelRule[] } | null)?.pickedModels;
  const initial = useMemo<KeyPublic | undefined>(
    () => (picked ? ({ id: "", name: "", enabled: true, rpm: 0, models: picked, daily_limit_usd: 0, weekly_limit_usd: 0 } as KeyPublic) : undefined),
    [picked],
  );

  return (
    <div className="form-page">
      <div className="fp-head mobile-hidden">
        <h1>{title}</h1>
      </div>
      <MobileFormHeader title={title} backTo="/keys" />
      {unpricedAfterSave > 0 && (
        <div className="kf-unpriced-after-save" data-testid="unpriced-after-save">
          {t("keyForm.unpricedAfterSave", { n: unpricedAfterSave })}{" "}
          <Link to="/mapping">{t("keyForm.unpricedGoMapping")}</Link>
        </div>
      )}
      <KeyForm
        initial={initial}
        pickPath="/keys/new/models"
        submitLabel={t("new.create")}
        onCancel={() => nav("/keys")}
        onSubmit={async (v, meta) => {
          const r = await createKey(keyWriteRequestFromForm(v));
          if (meta.newUnpricedCount > 0) {
            setUnpricedAfterSave(meta.newUnpricedCount);
          }
          setPlain(r.plain_key);
        }}
      />
      <p className="fp-note mobile-hidden">{t("login.memoryNote")}</p>
      {plain && (
        <PlainKeyModal
          plainKey={plain}
          title={t("plainModal.created")}
          onClose={() => {
            setPlain(null);
            nav("/keys");
          }}
        />
      )}
      <MobileTabBar active="new" />
    </div>
  );
}
