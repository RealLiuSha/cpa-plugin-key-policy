import { useEffect, useId, useRef, type ReactNode } from "react";

interface ModalProps {
  title: string;
  closeLabel: string;
  children: ReactNode;
  onClose: () => void;
  wide?: boolean;
  dismissible?: boolean;
}

export default function Modal({ title, closeLabel, children, onClose, wide = false, dismissible = true }: ModalProps) {
  const titleID = useId();
  const dialogRef = useRef<HTMLElement>(null);
  const onCloseRef = useRef(onClose);
  const dismissibleRef = useRef(dismissible);
  onCloseRef.current = onClose;
  dismissibleRef.current = dismissible;

  useEffect(() => {
    const previouslyFocused = document.activeElement instanceof HTMLElement ? document.activeElement : null;
    dialogRef.current?.focus();
    const handleKeyDown = (event: KeyboardEvent) => {
      if (event.key === "Escape" && dismissibleRef.current) {
        onCloseRef.current();
        return;
      }
      if (event.key !== "Tab" || !dialogRef.current) return;
      const focusable = [...dialogRef.current.querySelectorAll<HTMLElement>('a[href], button:not(:disabled), input:not(:disabled), select:not(:disabled), textarea:not(:disabled), [tabindex]:not([tabindex="-1"])')];
      if (focusable.length === 0) {
        event.preventDefault();
        dialogRef.current.focus();
        return;
      }
      const first = focusable[0];
      const last = focusable[focusable.length - 1];
      if (event.shiftKey && document.activeElement === first) {
        event.preventDefault();
        last.focus();
      } else if (!event.shiftKey && document.activeElement === last) {
        event.preventDefault();
        first.focus();
      }
    };
    document.addEventListener("keydown", handleKeyDown);
    return () => {
      document.removeEventListener("keydown", handleKeyDown);
      previouslyFocused?.focus();
    };
  }, []);

  return (
    <div className="modal-overlay" onMouseDown={(event) => { if (dismissible && event.target === event.currentTarget) onClose(); }}>
      <section
        ref={dialogRef}
        className={`modal${wide ? " modal-wide" : ""}`}
        role="dialog"
        aria-modal="true"
        aria-labelledby={titleID}
        tabIndex={-1}
      >
        <div className="modal-head">
          <h2 id={titleID}>{title}</h2>
          <button type="button" className="modal-close" aria-label={closeLabel} disabled={!dismissible} onClick={onClose}>×</button>
        </div>
        {children}
      </section>
    </div>
  );
}

interface ConfirmDialogProps {
  title: string;
  message: string;
  cancelLabel: string;
  confirmLabel: string;
  onCancel: () => void;
  onConfirm: () => void;
  busy?: boolean;
}

export function ConfirmDialog({ title, message, cancelLabel, confirmLabel, onCancel, onConfirm, busy = false }: ConfirmDialogProps) {
  return (
    <Modal title={title} closeLabel={cancelLabel} onClose={onCancel} dismissible={!busy}>
      <p className="modal-message">{message}</p>
      <div className="form-actions modal-actions">
        <button type="button" className="btn" disabled={busy} onClick={onCancel}>{cancelLabel}</button>
        <button type="button" className="btn danger" disabled={busy} onClick={onConfirm}>{confirmLabel}</button>
      </div>
    </Modal>
  );
}
