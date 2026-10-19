import { Routes, Route, Navigate, useNavigate, Link, useLocation } from "react-router-dom";
import { useEffect, useState } from "react";
import { isAuthed, subscribe, clearSession, getSession, bootstrapFromPanel } from "./store/session";
import { useT } from "./i18n";
import Login from "./pages/Login";
import KeyList from "./pages/KeyList";
import KeyNew from "./pages/KeyNew";
import KeyEdit from "./pages/KeyEdit";
import KeyUsage from "./pages/KeyUsage";
import Models from "./pages/Models";
import ModelForm from "./pages/ModelForm";
import Audit from "./pages/Audit";
import Stats from "./pages/Stats";
import Overview from "./pages/Overview";
import Channels from "./pages/Channels";
import Search from "./pages/Search";
import Quality from "./pages/Quality";
import Settings from "./pages/Settings";
import { useLeaveGuard } from "./components/LeaveGuard";
import { MobileTabBar } from "./components/MobileChrome";
import { NAV_ITEMS, navSection } from "./navigation";

function useAuthTick() {
  const [, setTick] = useState(0);
  useEffect(() => subscribe(() => setTick((t) => t + 1)), []);
  return isAuthed();
}

// Inside the CPA management panel the host already shows the product chrome,
// the endpoint and its own logout, so the plugin keeps only its section tabs.
function isEmbedded(): boolean {
  try {
    return window.self !== window.top;
  } catch {
    return true;
  }
}

// Desktop top horizontal nav: app title + base url and logout when opened on
// its own, section links always. Mobile uses the bottom tab bar instead.
function TopNav() {
  const t = useT();
  const nav = useNavigate();
  const loc = useLocation();
  const guard = useLeaveGuard();
  const s = getSession();
  if (!s) return null;
  const active = navSection(loc.pathname);
  const embedded = isEmbedded();
  return (
    <>
      <div className={"topnav" + (embedded ? " embedded" : "")}>
        <div className="topnav-inner">
          {!embedded && (
            <div className="topnav-brand">
              <span className="tn-title">{t("header.title")}</span>
              <span className="tn-sub">{s.baseUrl}</span>
            </div>
          )}
          <nav className="topnav-actions" aria-label={t("header.navLabel")}>
            {NAV_ITEMS.map((item) => (
              <Link key={item.id} to={item.path} className={"tn-link" + (active === item.id ? " active" : "")} aria-current={active === item.id ? "page" : undefined}
                onClick={(event) => {
                  // A modified click opens the section elsewhere and keeps this page.
                  if (event.button !== 0 || event.metaKey || event.ctrlKey || event.shiftKey || event.altKey) return;
                  if (guard.asks(item.path)) event.preventDefault();
                }}>
                {t(`header.${item.id}`)}
              </Link>
            ))}
            {!embedded && (
              <button className="btn sm" onClick={() => { clearSession(); nav("/login"); }}>
                {t("header.logout")}
              </button>
            )}
          </nav>
        </div>
      </div>
      {guard.dialog}
    </>
  );
}

function Shell() {
  const authed = useAuthTick();
  const [bootstrapped, setBootstrapped] = useState(false);
  const t = useT();
  // The tab title follows the product name in the current language.
  useEffect(() => { document.title = t("header.title"); });

  // When not yet authenticated, try once to reuse the panel's saved
  // management key (same-origin iframe embed). Only runs when not authed and
  // not already attempted, so a manual login or a successful bootstrap won't
  // re-trigger it.
  useEffect(() => {
    if (authed || bootstrapped) return;
    let alive = true;
    void bootstrapFromPanel().finally(() => {
      if (alive) setBootstrapped(true);
    });
    return () => {
      alive = false;
    };
  }, [authed, bootstrapped]);

  if (!authed) {
    if (!bootstrapped) {
      return <div className="app muted" style={{ padding: "40px 20px" }}>{t("session.restoring")}</div>;
    }
    return (
      <Routes>
        <Route path="/login" element={<Login />} />
        <Route path="*" element={<Navigate to="/login" replace />} />
      </Routes>
    );
  }
  return (
    <div className="app">
      <TopNav />
      <Routes>
        <Route path="/overview" element={<Overview />} />
        <Route path="/keys" element={<KeyList />} />
        <Route path="/keys/new" element={<KeyNew />} />
        <Route path="/keys/:id/edit" element={<KeyEdit />} />
        <Route path="/keys/:id/usage" element={<KeyUsage />} />
        <Route path="/models" element={<Models />} />
        <Route path="/models/new" element={<ModelForm key="new" mode="new" />} />
        <Route path="/models/edit" element={<ModelForm key="edit" mode="edit" />} />
        <Route path="/channels" element={<Channels />} />
        <Route path="/stats" element={<Stats />} />
        <Route path="/search" element={<Search />} />
        <Route path="/quality" element={<Quality />} />
        <Route path="/audit" element={<Audit />} />
        <Route path="/settings" element={<Settings />} />
        <Route path="*" element={<Navigate to="/overview" replace />} />
      </Routes>
      <MobileTabBar />
    </div>
  );
}

export default function App() {
  return (
    <Routes>
      <Route path="/*" element={<Shell />} />
    </Routes>
  );
}
