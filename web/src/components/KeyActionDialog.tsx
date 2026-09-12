import { useState } from "react";
import { deleteKey, rotateKey } from "../api/keys";
import { extractApiError } from "../api/error";
import type { RotateKeyResponse } from "../types";
import { useT } from "../i18n";
import Modal from "./Modal";

export type KeyAction = "rotate" | "delete";

export default function KeyActionDialog({ keyId, action, onClose, onComplete }: {
  keyId: string;
  action: KeyAction;
  onClose: () => void;
  onComplete: (result: RotateKeyResponse | null) => void;
}) {
  const t = useT();
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const title = t(action === "rotate" ? "keys.resetKey" : "keys.delete");

  const submit = async () => {
    if (busy) return;
    setBusy(true);
    setError("");
    let result: RotateKeyResponse | null = null;
    try {
      if (action === "rotate") result = await rotateKey(keyId);
      else await deleteKey(keyId);
    } catch (reason) {
      setError(extractApiError(reason, t(action === "rotate" ? "keys.rotateFailed" : "keys.deleteFailed")));
      setBusy(false);
      return;
    }
    onClose();
    onComplete(result);
  };

  return (
    <Modal title={title} closeLabel={t("keyForm.cancel")} onClose={onClose} dismissible={!busy}>
      <p>{t(action === "rotate" ? "keys.rotateConfirm" : "keys.deleteConfirm", { id: keyId })}</p>
      {error && <div className="error" role="alert">{error}</div>}
      <div className="form-actions modal-actions">
        <button type="button" className="btn" disabled={busy} onClick={onClose}>{t("keyForm.cancel")}</button>
        <button type="button" className="btn danger" disabled={busy} onClick={() => void submit()}>{busy ? t("keyForm.submitting") : title}</button>
      </div>
    </Modal>
  );
}
