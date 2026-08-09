import { Routes, Route, Navigate, useNavigate, Link, useLocation } from "react-router-dom";
import { useEffect, useState } from "react";
import { isAuthed, subscribe, clearSession, getSession, bootstrapFromPanel } from "./store/session";
import { useT } from "./i18n";
import Login from "./pages/Login";
import KeyList from "./pages/KeyList";
import KeyNew from "./pages/KeyNew";
import KeyEdit from "./pages/KeyEdit";
import KeyUsage from "./pages/KeyUsage";
import ModelPick from "./pages/ModelPick";
import Models from "./pages/Models";
import ModelForm from "./pages/ModelForm";
import CredentialGroups from "./pages/CredentialGroups";
import CredentialGroupForm from "./pages/CredentialGroupForm";
import Audit from "./pages/Audit";

function useAuthTick() {
  const [, setTick] = useState(0);
  useEffect(() => subscribe(() => setTick((t) => t + 1)), []);
  return isAuthed();
}

// Desktop top horizontal nav. Mirrors the Stitch "Quiet Paper" design: left =
// app title + base url, right = nav links + logout. Mobile keeps the legacy
// .header (hidden on desktop via CSS) and bottom tab bar instead.
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
  const onCredentialGroups = loc.pathname === "/credential-groups" || loc.pathname.startsWith("/credential-groups/");
  const onAudit = loc.pathname === "/audit";
  return (
    <div className="topnav">
      <div className="topnav-inner">
        <div className="topnav-brand">
          <span className="tn-title">{t("header.title")}</span>
          <span className="tn-sub">{s.baseUrl}</span>
        </div>
        <div className="topnav-actions">
          <Link to="/keys" className={"tn-link" + (onKeys && !onNew ? " active" : "")}>{t("header.keyList")}</Link>
          <Link to="/keys/new" className={"tn-link" + (onNew ? " active" : "")}>{t("header.newKey")}</Link>
          <Link to="/models" className={"tn-link" + (onModels ? " active" : "")}>{t("header.models")}</Link>
          <Link to="/credential-groups" className={"tn-link" + (onCredentialGroups ? " active" : "")}>{t("header.credentialGroups")}</Link>
          <Link to="/audit" className={"tn-link" + (onAudit ? " active" : "")}>{t("header.audit")}</Link>
          <button
            className="btn sm"
            onClick={() => { clearSession(); nav("/login"); }}
          >
            {t("header.logout")}
          </button>
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
        <Route path="/models/pick-target" element={<ModelPick />} />
        <Route path="/models/:name/edit" element={<ModelForm />} />
        <Route path="/credential-groups" element={<CredentialGroups />} />
        <Route path="/credential-groups/new" element={<CredentialGroupForm />} />
        <Route path="/credential-groups/:name/edit" element={<CredentialGroupForm />} />
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
