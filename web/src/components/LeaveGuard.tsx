import { useState, type ReactNode } from "react";
import { useLocation, useNavigate } from "react-router-dom";
import { useT } from "../i18n";
import { unsavedMessage } from "../store/unsaved";
import { ConfirmDialog } from "./Modal";

// The section links ask before they leave a page with unsaved edits. asks(path)
// is true when it opened the question instead of letting the link go; dialog
// is the question, to be rendered by the caller.
export function useLeaveGuard(): { asks: (path: string) => boolean; dialog: ReactNode } {
  const t = useT();
  const nav = useNavigate();
  const location = useLocation();
  const [pending, setPending] = useState<{ path: string; message: string } | null>(null);
  const asks = (path: string) => {
    const message = unsavedMessage();
    if (!message || path === location.pathname + location.search) return false;
    setPending({ path, message });
    return true;
  };
  const dialog = pending && (
    <ConfirmDialog
      title={t("channels.leaveTitle")}
      message={t(pending.message)}
      cancelLabel={t("channels.leaveStay")}
      confirmLabel={t("channels.leaveDiscard")}
      onCancel={() => setPending(null)}
      onConfirm={() => { setPending(null); nav(pending.path); }}
    />
  );
  return { asks, dialog };
}
