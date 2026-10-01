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
  const s = getSession();
  if (!s) return null;
  // Active state: highlight the nav item matching the current path prefix.
  const onKeys = loc.pathname === "/keys" || loc.pathname.startsWith("/keys/");
  const onNew = loc.pathname === "/keys/new" || loc.pathname.startsWith("/keys/new/");
  const onModels = loc.pathname === "/models" || loc.pathname.startsWith("/models/");
  const onAudit = loc.pathname === "/audit";
  const embedded = isEmbedded();
  return (
    <div className={"topnav" + (embedded ? " embedded" : "")}>
      <div className="topnav-inner">
        {!embedded && (
          <div className="topnav-brand">
            <span className="tn-title">{t("header.title")}</span>
            <span className="tn-sub">{s.baseUrl}</span>
          </div>
        )}
        <div className="topnav-actions">
          <Link to="/keys" className={"tn-link" + (onKeys && !onNew ? " active" : "")}>{t("header.keyList")}</Link>
          <Link to="/keys/new" className={"tn-link" + (onNew ? " active" : "")}>{t("header.newKey")}</Link>
          <Link to="/models" className={"tn-link" + (onModels ? " active" : "")}>{t("header.models")}</Link>
          <Link to="/audit" className={"tn-link" + (onAudit ? " active" : "")}>{t("header.audit")}</Link>
          {!embedded && (
            <button className="btn sm" onClick={() => { clearSession(); nav("/login"); }}>
              {t("header.logout")}
            </button>
          )}
        </div>
      </div>
    </div>
  );
}

function Shell() {
  const authed = useAuthTick();
  const [bootstrapped, setBootstrapped] = useState(false);
  const t = useT();

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
        <Route path="/keys" element={<KeyList />} />
        <Route path="/keys/new" element={<KeyNew />} />
        <Route path="/keys/:id/edit" element={<KeyEdit />} />
        <Route path="/keys/:id/usage" element={<KeyUsage />} />
        <Route path="/models" element={<Models />} />
        <Route path="/models/new" element={<ModelForm />} />
        <Route path="/models/:name/edit" element={<ModelForm />} />
        <Route path="/audit" element={<Audit />} />
        <Route path="*" element={<Navigate to="/keys" replace />} />
      </Routes>
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
