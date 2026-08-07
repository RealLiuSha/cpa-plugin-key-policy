import { useCallback, useEffect, useLayoutEffect, useRef, useState } from "react";
import type { CSSProperties } from "react";
import { deleteKey, resetRPM, resetUsage, rotateKey } from "../api/keys";
import { useT } from "../i18n";

/** Menu actions the more-menu can expose; order is caller-controlled via `items`. */
export type KeyMoreMenuItem = "daily" | "weekly" | "rpm" | "rotate" | "delete";

const DEFAULT_LIST_ITEMS: KeyMoreMenuItem[] = [
  "daily",
  "weekly",
  "rpm",
  "rotate",
  "delete",
];

const LABEL_KEY: Record<KeyMoreMenuItem, string> = {
  daily: "keys.resetDaily",
  weekly: "keys.resetWeekly",
  rpm: "keys.resetRpm",
  rotate: "keys.resetKey",
  delete: "keys.delete",
};

// Tab bar is 56px + small margin; keep panel clear of it on mobile.
const BOTTOM_SAFE_PX = 64;
const VIEW_PAD = 8;
const GAP = 6;

/**
 * Configurable "more" dropdown for key ops. List pages use the full five-item
 * set; edit pages pass a shorter `items` array for reset-only actions. API
 * calls live here so callers only wire completion callbacks.
 *
 * The panel is position:fixed while open so it is not trapped by card stacking
 * contexts, table overflow, or the fixed tab bar.
 */
export default function KeyMoreMenu({
  keyId,
  items = DEFAULT_LIST_ITEMS,
  summaryLabel,
  onResetComplete,
  onRotated,
  onDeleted,
  onOpenChange,
}: {
  keyId: string;
  items?: KeyMoreMenuItem[];
  /** Defaults to keys.more; edit page passes keys.reset. */
  summaryLabel?: string;
  onResetComplete?: () => void | Promise<void>;
  onRotated?: (plainKey: string) => void;
  onDeleted?: () => void | Promise<void>;
  onOpenChange?: (open: boolean) => void;
}) {
  const t = useT();
  const menuRef = useRef<HTMLDetailsElement>(null);
  const panelRef = useRef<HTMLDivElement>(null);
  const [open, setOpen] = useState(false);
  const [panelStyle, setPanelStyle] = useState<CSSProperties | undefined>();

  const updateOpen = useCallback(
    (next: boolean) => {
      setOpen(next);
      onOpenChange?.(next);
    },
    [onOpenChange],
  );

  const closeMenu = useCallback(() => {
    if (menuRef.current) {
      menuRef.current.open = false;
    }
    updateOpen(false);
  }, [updateOpen]);

  const placePanel = useCallback(() => {
    const summary = menuRef.current?.querySelector("summary");
    const panel = panelRef.current;
    if (!summary || !panel) return;

    const rect = summary.getBoundingClientRect();
    const panelHeight = panel.offsetHeight || items.length * 40 + 8;
    const panelWidth = Math.max(panel.offsetWidth || 128, 128);

    // Bottom reserve matches tab bar; openDown decision and final clamp share it.
    const maxBottom = window.innerHeight - BOTTOM_SAFE_PX;
    const spaceBelow = maxBottom - rect.bottom - GAP;
    const spaceAbove = rect.top - GAP - VIEW_PAD;

    let top: number;
    if (spaceBelow >= panelHeight) {
      top = rect.bottom + GAP;
    } else if (spaceAbove >= panelHeight) {
      top = rect.top - GAP - panelHeight;
    } else {
      // Neither side fits fully: pin just above the tab-bar reserve.
      top = maxBottom - panelHeight;
    }
    if (top < VIEW_PAD) top = VIEW_PAD;
    if (top + panelHeight > maxBottom) {
      top = Math.max(VIEW_PAD, maxBottom - panelHeight);
    }

    let left = rect.right - panelWidth;
    if (left < VIEW_PAD) left = VIEW_PAD;
    if (left + panelWidth > window.innerWidth - VIEW_PAD) {
      left = Math.max(VIEW_PAD, window.innerWidth - VIEW_PAD - panelWidth);
    }

    setPanelStyle({
      position: "fixed",
      top,
      left,
      right: "auto",
      bottom: "auto",
      zIndex: 200,
      minWidth: panelWidth,
    });
  }, [items.length]);

  useLayoutEffect(() => {
    if (!open) {
      setPanelStyle(undefined);
      return;
    }
    placePanel();
    // Second pass after the panel paints so offsetHeight is accurate.
    const id = requestAnimationFrame(() => placePanel());
    return () => cancelAnimationFrame(id);
  }, [open, placePanel, items]);

  useEffect(() => {
    if (!open) return;

    const onDocumentPointerDown = (event: PointerEvent) => {
      if (!menuRef.current?.contains(event.target as Node)) {
        closeMenu();
      }
    };
    const onDocumentKeyDown = (event: KeyboardEvent) => {
      if (event.key !== "Escape") return;
      closeMenu();
      menuRef.current?.querySelector<HTMLElement>("summary")?.focus();
    };
    const onReposition = () => placePanel();

    document.addEventListener("pointerdown", onDocumentPointerDown);
    document.addEventListener("keydown", onDocumentKeyDown);
    window.addEventListener("resize", onReposition);
    // Capture scroll from table-wrap / page so the fixed panel tracks the trigger.
    window.addEventListener("scroll", onReposition, true);
    return () => {
      document.removeEventListener("pointerdown", onDocumentPointerDown);
      document.removeEventListener("keydown", onDocumentKeyDown);
      window.removeEventListener("resize", onReposition);
      window.removeEventListener("scroll", onReposition, true);
    };
  }, [closeMenu, open, placePanel]);

  const runItem = async (kind: KeyMoreMenuItem) => {
    closeMenu();
    if (kind === "daily" && !confirm(t("keys.resetDailyConfirm", { id: keyId }))) return;
    if (kind === "weekly" && !confirm(t("keys.resetWeeklyConfirm", { id: keyId }))) return;
    if (kind === "rotate" && !confirm(t("keys.rotateConfirm", { id: keyId }))) return;
    if (kind === "delete" && !confirm(t("keys.deleteConfirm", { id: keyId }))) return;

    try {
      if (kind === "rpm") {
        await resetRPM(keyId);
        await onResetComplete?.();
        return;
      }
      if (kind === "daily" || kind === "weekly") {
        await resetUsage(keyId, kind);
        await onResetComplete?.();
        return;
      }
      if (kind === "rotate") {
        const r = await rotateKey(keyId);
        onRotated?.(r.plain_key);
        await onResetComplete?.();
        return;
      }
      if (kind === "delete") {
        await deleteKey(keyId);
        await onDeleted?.();
      }
    } catch (e) {
      const fallback =
        kind === "rotate"
          ? t("keys.rotateFailed")
          : kind === "delete"
            ? t("keys.deleteFailed")
            : t("keys.resetFailed");
      alert((e as Error).message ?? fallback);
    }
  };

  const summary = summaryLabel ?? t("keys.more");

  return (
    <details
      ref={menuRef}
      className={"more-menu" + (open ? " more-menu--open" : "")}
      onClick={(e) => e.stopPropagation()}
      onToggle={(e) => updateOpen(e.currentTarget.open)}
    >
      <summary className="btn sm" aria-label={summary}>
        {summary} <span aria-hidden="true">▾</span>
      </summary>
      <div
        ref={panelRef}
        className="more-menu-options"
        role="menu"
        style={panelStyle}
      >
        {items.map((kind) => (
          <button
            key={kind}
            type="button"
            role="menuitem"
            className={kind === "delete" ? "danger" : undefined}
            onClick={() => void runItem(kind)}
          >
            {t(LABEL_KEY[kind])}
          </button>
        ))}
      </div>
    </details>
  );
}
