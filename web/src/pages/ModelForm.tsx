import { useCallback, useEffect, useState } from "react";
import { useLocation, useNavigate, useParams } from "react-router-dom";
import { fetchModelDefinitions, upsertModelDefinition } from "../api/modelDefinitions";
import type { KeyFormValues } from "../components/KeyForm";
import type { ModelDefinition, ModelTarget } from "../types";
import { extractApiError } from "../api/error";
import { useT } from "../i18n";

type ModelDraft = Partial<ModelDefinition> & { returnTo?: string; draftKey?: KeyFormValues };

const emptyModel: ModelDefinition = {
  name: "", targets: [], dispatch: "round-robin", billing_mode: "tokens", free: false,
  input_price_per_million: 0, output_price_per_million: 0, cache_read_price_per_million: 0, per_call_usd: 0,
};

export function safeKeyReturnPath(value: string | undefined): string | undefined {
  if (value === "/keys/new") return value;
  if (value && /^\/keys\/[^/?#]+\/edit$/.test(value)) return value;
  return undefined;
}

export default function ModelForm() {
  const { name } = useParams<{ name?: string }>();
  const navigate = useNavigate();
  const location = useLocation();
  const t = useT();
  const state = location.state as { pickedTargets?: ModelTarget[]; draftModel?: ModelDraft; returnTo?: string; draftKey?: KeyFormValues } | null;
  const draft = state?.draftModel;
  const keyReturnTo = safeKeyReturnPath(draft?.returnTo ?? state?.returnTo);
  const draftKey = draft?.draftKey ?? state?.draftKey;
  const [model, setModel] = useState<ModelDefinition>(() => ({ ...emptyModel, ...draft, targets: state?.pickedTargets ?? draft?.targets ?? [] }));
  const [loading, setLoading] = useState(!!name && !draft);
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState("");

  useEffect(() => {
    if (!name || draft) return;
    void fetchModelDefinitions()
      .then((models) => {
        const found = models.find((candidate) => candidate.name.toLowerCase() === decodeURIComponent(name).toLowerCase());
        if (!found) setError(t("models.notFound")); else setModel(found);
      })
      .catch((reason) => setError(extractApiError(reason, t("models.loadFailed"))))
      .finally(() => setLoading(false));
  }, [draft, name, t]);

  const setNumber = (field: keyof Pick<ModelDefinition, "input_price_per_million" | "output_price_per_million" | "cache_read_price_per_million" | "cache_write_price_per_million" | "per_call_usd">, value: string) => {
    const parsed = Number.parseFloat(value);
    setModel((previous) => ({ ...previous, [field]: field === "cache_write_price_per_million" && value.trim() === "" ? undefined : Number.isFinite(parsed) ? parsed : 0 }));
  };

  const pickTargets = () => {
    const returnTo = name ? `/models/${encodeURIComponent(name)}/edit` : "/models/new";
    const context: ModelDraft = { ...model, returnTo: keyReturnTo, draftKey };
    navigate("/models/pick-target", { state: { currentTargets: model.targets, returnTo, draftModel: context } });
  };

  const cancel = () => {
    if (keyReturnTo) {
      navigate(keyReturnTo, { state: { draftKey } });
      return;
    }
    navigate("/models");
  };

  const save = useCallback(async (event: React.FormEvent) => {
    event.preventDefault();
    setError("");
    if (!model.name.trim() || model.targets.length === 0) {
      setError(t("models.required"));
      return;
    }
    setSaving(true);
    try {
      await upsertModelDefinition({ ...model, name: model.name.trim() });
      if (keyReturnTo) navigate(keyReturnTo, { state: { createdModel: model.name.trim(), draftKey } });
      else navigate("/models");
    } catch (reason) {
      setError(extractApiError(reason, t("models.saveFailed")));
    } finally {
      setSaving(false);
    }
  }, [draftKey, keyReturnTo, model, navigate, t]);

  if (loading) return <div className="muted">{t("keys.loading")}</div>;
  return (
    <form className="form-page model-form" onSubmit={save}>
      <div className="page-head"><h1>{name ? t("models.editTitle") : t("models.newTitle")}</h1></div>
      {error && <div className="error">{error}</div>}
      <section className="card">
        <p className="muted">{t("models.formHint")}</p>
        <label>{t("models.callName")}<input className="input" value={model.name} disabled={!!name} onChange={(event) => setModel((previous) => ({ ...previous, name: event.target.value }))} /></label>
        <div className="field-row">
          <label>{t("models.dispatch")}<select className="input" value={model.dispatch} onChange={(event) => setModel((previous) => ({ ...previous, dispatch: event.target.value as ModelDefinition["dispatch"] }))}><option value="round-robin">{t("models.roundRobin")}</option><option value="priority">{t("models.priority")}</option></select></label>
          <label>{t("models.billing")}<select className="input" value={model.billing_mode} disabled={model.free} onChange={(event) => setModel((previous) => ({ ...previous, billing_mode: event.target.value as ModelDefinition["billing_mode"] }))}><option value="tokens">{t("models.tokens")}</option><option value="per_call">{t("models.perCall")}</option></select></label>
          <label className="check-row"><input type="checkbox" checked={model.free} onChange={(event) => setModel((previous) => ({ ...previous, free: event.target.checked, input_price_per_million: 0, output_price_per_million: 0, cache_read_price_per_million: 0, cache_write_price_per_million: undefined, per_call_usd: 0 }))} />{t("models.free")}</label>
        </div>
        {!model.free && model.billing_mode === "tokens" && <div className="field-row"><label>{t("models.inputPrice")}<input className="input" type="number" min="0" step="0.001" value={model.input_price_per_million ?? 0} onChange={(event) => setNumber("input_price_per_million", event.target.value)} /></label><label>{t("models.outputPrice")}<input className="input" type="number" min="0" step="0.001" value={model.output_price_per_million ?? 0} onChange={(event) => setNumber("output_price_per_million", event.target.value)} /></label><label>{t("models.cachePrice")}<input className="input" type="number" min="0" step="0.001" value={model.cache_read_price_per_million ?? 0} onChange={(event) => setNumber("cache_read_price_per_million", event.target.value)} /></label><label>{t("models.cacheWritePrice")}<input className="input" type="number" min="0" step="0.001" value={model.cache_write_price_per_million ?? ""} onChange={(event) => setNumber("cache_write_price_per_million", event.target.value)} /></label></div>}
        {!model.free && model.billing_mode === "per_call" && <label>{t("models.perCallPrice")}<input className="input" type="number" min="0" step="0.001" value={model.per_call_usd ?? 0} onChange={(event) => setNumber("per_call_usd", event.target.value)} /></label>}
        <p className="muted">{t("models.priceHint")}</p>
      </section>
      <section className="card">
        <div className="section-title-row"><div><h2>{t("models.availableTargets")}</h2><p className="muted">{t("models.targetsHint")}</p></div><button type="button" className="btn sm" onClick={pickTargets}>{t("models.chooseTargets")}</button></div>
        <div className="chip-row">{model.targets.map((target) => <span className="chip" key={`${target.provider}|${target.group ?? ""}|${target.target_model}`}>{target.provider}{target.group ? ` · ${target.group}` : ""} / {target.target_model}</span>)}</div>
      </section>
      <div className="form-actions"><button type="button" className="btn" onClick={cancel}>{t("keyForm.cancel")}</button><button className="btn primary" disabled={saving}>{saving ? t("keyForm.submitting") : t("models.save")}</button></div>
    </form>
  );
}
