import { useState } from "react";
import { Link, useLocation, useNavigate } from "react-router-dom";
import { useT } from "../i18n";
import { NAV_ITEMS, navSection } from "../navigation";
import { useLeaveGuard } from "./LeaveGuard";
import Modal from "./Modal";

/** Compact top bar for mobile create/edit/detail screens (desktop keeps h1/h2). */
export function MobileFormHeader({ title, backTo }: { title: string; backTo: string }) {
  const t = useT();
  return (
    <div className="mobile-form-header mobile-only">
      <Link to={backTo} className="mfb-back">
        {t("keyUsage.back")}
      </Link>
      <h2 className="mfb-title">{title}</h2>
    </div>
  );
}

/**
 * Mobile bottom tab bar: the primary sections plus a "more" tab whose sheet
 * lists the rest. The tab of the current section gets a 2px primary
 * underline; while a section from the sheet is open, the "more" tab shows
 * its name.
 */
export function MobileTabBar() {
  const t = useT();
  const nav = useNavigate();
  const active = navSection(useLocation().pathname);
  const [sheetOpen, setSheetOpen] = useState(false);
  const guard = useLeaveGuard();
  const extra = NAV_ITEMS.filter((item) => !item.primary);
  const extraActive = extra.find((item) => item.id === active);
  const go = (path: string) => {
    setSheetOpen(false);
    if (!guard.asks(path)) nav(path);
  };
  return (
    <>
      <nav className="tabbar" aria-label={t("header.navLabel")}>
        {NAV_ITEMS.filter((item) => item.primary).map((item) => (
          <button key={item.id} type="button" className={"tab" + (active === item.id ? " active" : "")} aria-current={active === item.id ? "page" : undefined} onClick={() => go(item.path)}>
            <span className="tab-icon" aria-hidden="true">{item.icon}</span>
            <span>{t(`header.${item.id}`)}</span>
          </button>
        ))}
        <button type="button" className={"tab" + (extraActive ? " active" : "")} aria-haspopup="dialog" aria-expanded={sheetOpen} onClick={() => setSheetOpen(true)}>
          <span className="tab-icon" aria-hidden="true">⋯</span>
          <span>{extraActive ? t(`header.${extraActive.id}`) : t("header.more")}</span>
        </button>
      </nav>
      {sheetOpen && (
        <Modal title={t("header.more")} closeLabel={t("models.close")} onClose={() => setSheetOpen(false)}>
          <div className="more-sheet">
            {extra.map((item) => (
              <button key={item.id} type="button" className={"more-sheet-item" + (active === item.id ? " active" : "")} aria-current={active === item.id ? "page" : undefined} onClick={() => go(item.path)}>
                <span className="tab-icon" aria-hidden="true">{item.icon}</span>
                <span>{t(`header.${item.id}`)}</span>
              </button>
            ))}
          </div>
        </Modal>
      )}
      {guard.dialog}
    </>
  );
}
