import { useEffect, useState } from "react";
import { useNavigate, useParams } from "react-router-dom";
import { fetchClassifyRules, upsertClassifyRule } from "../api/credentialGroups";
import { extractApiError } from "../api/error";
import { useT } from "../i18n";
import type { ClassifyRule } from "../types";

export default function CredentialGroupForm() {
  const { name } = useParams<{ name?: string }>();
  const navigate = useNavigate();
  const t = useT();
  const [rule, setRule] = useState<ClassifyRule>({ name: name ? decodeURIComponent(name) : "", field: "filename", pattern: "", group: "", enabled: true });
  const [error, setError] = useState("");
  const [saving, setSaving] = useState(false);

  useEffect(() => {
    if (!name) return;
    void fetchClassifyRules().then((rules) => {
      const found = rules.find((candidate) => candidate.name.toLowerCase() === decodeURIComponent(name).toLowerCase());
      if (found) setRule(found); else setError(t("credentialGroups.notFound"));
    }).catch((reason) => setError(extractApiError(reason, t("credentialGroups.loadFailed"))));
  }, [name, t]);

  const save = async (event: React.FormEvent) => {
    event.preventDefault();
    setSaving(true);
    setError("");
    try {
      await upsertClassifyRule(rule);
      navigate("/credential-groups");
    } catch (reason) {
      setError(extractApiError(reason, t("credentialGroups.saveFailed")));
    } finally {
      setSaving(false);
    }
  };

  return (
    <form className="form-page" onSubmit={save}>
      <h1>{name ? t("credentialGroups.editTitle") : t("credentialGroups.newTitle")}</h1>
      {error && <div className="error">{error}</div>}
      <section className="card form-grid">
        <label>{t("credentialGroups.name")}<input className="input" value={rule.name} disabled={!!name} onChange={(event) => setRule((previous) => ({ ...previous, name: event.target.value }))} /></label>
        <label>{t("credentialGroups.field")}<input className="input" value={rule.field} onChange={(event) => setRule((previous) => ({ ...previous, field: event.target.value }))} /></label>
        <label>{t("credentialGroups.pattern")}<input className="input" value={rule.pattern} onChange={(event) => setRule((previous) => ({ ...previous, pattern: event.target.value }))} /></label>
        <label>{t("credentialGroups.group")}<input className="input" value={rule.group} onChange={(event) => setRule((previous) => ({ ...previous, group: event.target.value }))} /></label>
        <label className="check-row"><input type="checkbox" checked={rule.enabled} onChange={(event) => setRule((previous) => ({ ...previous, enabled: event.target.checked }))} />{t("credentialGroups.enabled")}</label>
      </section>
      <div className="form-actions"><button type="button" className="btn" onClick={() => navigate("/credential-groups")}>{t("keyForm.cancel")}</button><button className="btn primary" disabled={saving}>{t("credentialGroups.save")}</button></div>
    </form>
  );
}
