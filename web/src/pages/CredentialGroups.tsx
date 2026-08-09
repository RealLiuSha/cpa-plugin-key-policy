import { useEffect, useState } from "react";
import { Link } from "react-router-dom";
import { classifyPreview, deleteClassifyRule, fetchClassifyRules, fetchCredentialDescriptors, reorderClassifyRules } from "../api/credentialGroups";
import type { ClassifyPreviewResponse, ClassifyRule } from "../types";
import { extractApiError } from "../api/error";
import { useT } from "../i18n";

export default function CredentialGroups() {
  const t = useT();
  const [rules, setRules] = useState<ClassifyRule[]>([]);
  const [error, setError] = useState("");
  const [preview, setPreview] = useState<ClassifyPreviewResponse | null>(null);
  const [previewing, setPreviewing] = useState(false);

  const load = async () => {
    try { setRules(await fetchClassifyRules()); } catch (reason) { setError(extractApiError(reason, t("credentialGroups.loadFailed"))); }
  };
  useEffect(() => { void load(); }, []);

  const remove = async (name: string) => {
    if (!confirm(t("credentialGroups.deleteConfirm", { name }))) return;
    try { await deleteClassifyRule(name); await load(); } catch (reason) { setError(extractApiError(reason, t("credentialGroups.deleteFailed"))); }
  };

  const move = async (index: number, offset: number) => {
    const nextIndex = index + offset;
    if (nextIndex < 0 || nextIndex >= rules.length) return;
    const next = [...rules];
    [next[index], next[nextIndex]] = [next[nextIndex], next[index]];
    setRules(next);
    try { await reorderClassifyRules(next.map((rule) => rule.name)); } catch (reason) { setError(extractApiError(reason, t("credentialGroups.reorderFailed"))); await load(); }
  };

  const runPreview = async () => {
    setPreviewing(true);
    setError("");
    try {
      const descriptors = await fetchCredentialDescriptors();
      setPreview(await classifyPreview(descriptors, rules));
    } catch (reason) {
      setError(extractApiError(reason, t("credentialGroups.previewFailed")));
    } finally {
      setPreviewing(false);
    }
  };

  return (
    <div className="page credential-groups-page">
      <div className="page-head"><div><h1>{t("credentialGroups.title")}</h1><p className="muted">{t("credentialGroups.description")}</p></div><div className="row-actions"><button className="btn" disabled={previewing} onClick={() => void runPreview()}>{previewing ? t("credentialGroups.previewing") : t("credentialGroups.preview")}</button><Link className="btn primary" to="/credential-groups/new">{t("credentialGroups.new")}</Link></div></div>
      {error && <div className="error">{error}</div>}
      <div className="card">
        {rules.length === 0 ? <div className="muted">{t("credentialGroups.empty")}</div> : rules.map((rule, index) => (
          <div className="rule-row" key={rule.name}>
            <div><strong>{rule.name}</strong><div className="muted">{rule.field} / {rule.pattern} → {rule.group}</div>{preview && <div className="rule-preview-count">{t("credentialGroups.matchCount", { count: preview.group_counts[rule.group.toLowerCase()] ?? 0 })}</div>}</div>
            <div className="row-actions"><button className="btn sm" disabled={index === 0} onClick={() => void move(index, -1)}>↑</button><button className="btn sm" disabled={index === rules.length - 1} onClick={() => void move(index, 1)}>↓</button><Link className="btn sm" to={`/credential-groups/${encodeURIComponent(rule.name)}/edit`}>{t("credentialGroups.edit")}</Link><button className="btn sm danger" onClick={() => void remove(rule.name)}>{t("credentialGroups.delete")}</button></div>
          </div>
        ))}
      </div>
    </div>
  );
}
