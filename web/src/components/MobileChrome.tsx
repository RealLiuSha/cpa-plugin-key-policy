import { Link, useNavigate } from "react-router-dom";
import { useT } from "../i18n";

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
 * Mobile bottom tab bar. Active tab gets a 2px primary underline. The "usage"
 * tab is optional — only shown once a specific key's usage page is open.
 */
export function MobileTabBar({
  active,
  showUsage = false,
  usagePath = "/keys",
}: {
  active: "keys" | "usage" | "new";
  showUsage?: boolean;
  usagePath?: string;
}) {
  const t = useT();
  const nav = useNavigate();
  const tab = (id: "keys" | "usage" | "new", label: string, icon: string, target: string) => (
    <button
      className={"tab" + (active === id ? " active" : "")}
      onClick={() => nav(target)}
    >
      <span className="tab-icon">{icon}</span>
      <span>{label}</span>
    </button>
  );
  return (
    <nav className={"tabbar" + (showUsage ? "" : " tabbar--no-usage")}>
      {tab("keys", t("keys.mobile.tabKeys"), "#", "/keys")}
      {showUsage && tab("usage", t("keys.mobile.tabUsage"), "#", usagePath)}
      {tab("new", t("keys.mobile.tabNew"), "+", "/keys/new")}
    </nav>
  );
}
