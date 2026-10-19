import { useState } from "react";
import { useNavigate } from "react-router-dom";
import { adjustBalance, createKey } from "../api/keys";
import KeyForm, { keyWriteRequestFromForm } from "../components/KeyForm";
import PlainKeyModal from "../components/PlainKeyModal";
import { newRequestId } from "../components/BalanceDialog";
import { MobileFormHeader } from "../components/MobileChrome";
import { useT } from "../i18n";

export interface KeyUsageNotice {
  notice: string;
}

export default function KeyNew() {
  const navigate = useNavigate();
  const t = useT();
  const [created, setCreated] = useState<{ id: string; plain: string; balanceFailed: boolean } | null>(null);

  const title = t("new.title");
  return (
    <div className="form-page">
      <div className="fp-head mobile-hidden"><h1>{title}</h1></div>
      <MobileFormHeader title={title} backTo="/keys" />
      <KeyForm
        submitLabel={t("new.create")}
        onCancel={() => navigate("/keys")}
        onSubmit={async (values) => {
          const response = await createKey(keyWriteRequestFromForm(values));
          // The key exists from here on: a failed initial balance must not hide
          // the one-time plaintext key, so it is reported after the modal.
          let balanceFailed = false;
          if (values.quota_mode === "balance" && values.initial_balance_usd > 0) {
            try {
              await adjustBalance(response.key.id, values.initial_balance_usd, t("balance.initialNote"), newRequestId());
            } catch {
              balanceFailed = true;
            }
          }
          setCreated({ id: response.key.id, plain: response.plain_key, balanceFailed });
        }}
      />
      {created && (
        <PlainKeyModal
          plainKey={created.plain}
          title={t("plainModal.created")}
          onClose={() => {
            const { id, balanceFailed } = created;
            setCreated(null);
            if (balanceFailed) {
              const state: KeyUsageNotice = { notice: t("balance.initialFailed") };
              navigate(`/keys/${encodeURIComponent(id)}/usage`, { state });
            } else navigate("/keys");
          }}
        />
      )}
    </div>
  );
}
