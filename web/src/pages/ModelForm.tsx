import { keyListReturnPath } from "../navigation";
import { isUnpriced, modelPriceSummary } from "../components/modelPricing";
import { useCallback, useEffect, useState } from "react";
import { useLocation, useNavigate, useParams } from "react-router-dom";
import { fetchModelDefinitions, upsertModelDefinition } from "../api/modelDefinitions";
import UpstreamPicker from "../components/UpstreamPicker";
import type { KeyFormValues } from "../components/KeyForm";
import type { ModelDefinition } from "../types";
import { extractApiError } from "../api/error";
import { useT } from "../i18n";

const emptyModel: ModelDefinition = {
  name: "", provider: "", target_model: "", billing_mode: "tokens", billing_multiplier: 1,
  input_price_per_million: 0, output_price_per_million: 0, cache_read_price_per_million: 0, per_call_usd: 0,
};

type PriceField = "input_price_per_million" | "output_price_per_million" | "cache_read_price_per_million" | "cache_write_price_per_million" | "per_call_usd";

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
  const state = location.state as { returnTo?: string; draftKey?: KeyFormValues; keyListReturnTo?: string } | null;
  const keyReturnTo = safeKeyReturnPath(state?.returnTo);
  const draftKey = state?.draftKey;
  const keyListReturnTo = keyListReturnPath(state);
  const [model, setModel] = useState<ModelDefinition>(emptyModel);
  const [multiplierInput, setMultiplierInput] = useState("1");
  const [picking, setPicking] = useState(false);
  const [loading, setLoading] = useState(!!name);
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState("");
  const multiplier = Number(multiplierInput);
  const multiplierValid = multiplierInput.trim() !== "" && Number.isFinite(multiplier) && multiplier >= 1;
  const tokens = model.billing_mode === "tokens";

  useEffect(() => {
    if (!name) return;
    let alive = true;
    void fetchModelDefinitions()
      .then((models) => {
        if (!alive) return;
        const found = models.find((candidate) => candidate.name.toLowerCase() === decodeURIComponent(name).toLowerCase());
        if (!found) setError(t("models.notFound"));
        else { setModel(found); setMultiplierInput(String(found.billing_multiplier ?? 1)); }
      })
      .catch((reason) => { if (alive) setError(extractApiError(reason, t("models.loadFailed"))); })
      .finally(() => { if (alive) setLoading(false); });
    return () => { alive = false; };
  }, [name, t]);

  const setPrice = (field: PriceField, value: string) => {
    const parsed = Number.parseFloat(value);
    setModel((previous) => ({
      ...previous,
      [field]: field === "cache_write_price_per_million" && value.trim() === "" ? undefined : Number.isFinite(parsed) ? parsed : 0,
    }));
  };

  const cancel = () => {
    if (keyReturnTo) navigate(keyReturnTo, { state: { draftKey, keyListReturnTo } });
    else navigate("/models");
  };

  const save = useCallback(async (event: React.FormEvent) => {
    event.preventDefault();
    setError("");
    if (!model.name.trim() || !model.provider.trim() || !model.target_model.trim()) {
      setError(t("models.required"));
      return;
    }
    if (tokens && !multiplierValid) {
      setError(t("quota.invalidMultiplier"));
      return;
    }
    setSaving(true);
    try {
      await upsertModelDefinition({ ...model, name: model.name.trim(), billing_multiplier: tokens ? multiplier : model.billing_multiplier ?? 1 });
      if (keyReturnTo) navigate(keyReturnTo, { state: { createdModel: model.name.trim(), draftKey, keyListReturnTo } });
      else navigate("/models");
    } catch (reason) {
      setError(extractApiError(reason, t("models.saveFailed")));
    } finally {
      setSaving(false);
    }
  }, [draftKey, keyReturnTo, keyListReturnTo, model, multiplier, multiplierValid, navigate, t, tokens]);

  if (loading) return <div className="muted">{t("keys.loading")}</div>;
  const preview = { ...model, billing_multiplier: multiplierValid ? multiplier : 1 };
  const exampleBase = (model.input_price_per_million ?? 0) + (model.output_price_per_million ?? 0);
  return (
    <form className="form-page model-form" onSubmit={save}>
      <div className="page-head"><h1>{name ? t("models.editTitle") : t("models.newTitle")}</h1></div>
      {error && <div className="error" role="alert">{error}</div>}
      <section className="card">
        <label>{t("models.callName")}
          <input className="input" value={model.name} disabled={!!name} placeholder={t("models.callNamePlaceholder")} onChange={(event) => setModel((previous) => ({ ...previous, name: event.target.value }))} />
          <small className="field-hint">{t("models.callNameHint")}</small>
        </label>
        <div className="upstream-row">
          <label>{t("models.provider")}<input className="input mono" value={model.provider} placeholder="xai" onChange={(event) => setModel((previous) => ({ ...previous, provider: event.target.value }))} /></label>
          <label>{t("models.targetModel")}<input className="input mono" value={model.target_model} placeholder="grok-4.6" onChange={(event) => setModel((previous) => ({ ...previous, target_model: event.target.value }))} /></label>
          <button type="button" className="btn" onClick={() => setPicking(true)}>{t("models.chooseUpstream")}</button>
        </div>
        <small className="field-hint">{t("models.upstreamHint")}</small>
      </section>
      <section className="card">
        <div className="seg-inline" role="radiogroup" aria-label={t("models.billing")}>
          <span className="field-label">{t("models.billing")}</span>
          <label className="check-row"><input type="radio" name="billing_mode" checked={tokens} onChange={() => setModel((previous) => ({ ...previous, billing_mode: "tokens" }))} />{t("models.tokens")}</label>
          <label className="check-row"><input type="radio" name="billing_mode" checked={!tokens} onChange={() => setModel((previous) => ({ ...previous, billing_mode: "per_call" }))} />{t("models.perCall")}</label>
        </div>
        {tokens ? (
          <>
            <div className="field-row">
              <label>{t("models.inputPrice")}<input className="input" type="number" min="0" step="any" value={model.input_price_per_million ?? 0} onChange={(event) => setPrice("input_price_per_million", event.target.value)} /></label>
              <label>{t("models.outputPrice")}<input className="input" type="number" min="0" step="any" value={model.output_price_per_million ?? 0} onChange={(event) => setPrice("output_price_per_million", event.target.value)} /></label>
              <label>{t("models.cachePrice")}<input className="input" type="number" min="0" step="any" placeholder={t("models.sameAsInputPlaceholder")} value={model.cache_read_price_per_million || ""} onChange={(event) => setPrice("cache_read_price_per_million", event.target.value)} /></label>
              <label>{t("models.cacheWritePrice")}<input className="input" type="number" min="0" step="any" placeholder={t("models.sameAsInputPlaceholder")} value={model.cache_write_price_per_million ?? ""} onChange={(event) => setPrice("cache_write_price_per_million", event.target.value)} /></label>
            </div>
            <div className="model-multiplier">
              <label>{t("quota.multiplier")}<input className="input" type="number" min="1" step="any" required value={multiplierInput} onChange={(event) => setMultiplierInput(event.target.value)} /></label>
              <div>
                <p className="muted">{t("quota.multiplierHelp")}</p>
                {multiplierValid && exampleBase > 0 && <strong>{t("quota.multiplierExample", { base: exampleBase.toFixed(2), amount: (exampleBase * multiplier).toFixed(2) })}</strong>}
              </div>
            </div>
          </>
        ) : (
          <label>{t("models.perCallPrice")}<input className="input" type="number" min="0" step="any" value={model.per_call_usd ?? 0} onChange={(event) => setPrice("per_call_usd", event.target.value)} /></label>
        )}
        {isUnpriced(model) ? <p className="notice">{t("models.unpricedHint")}</p> : (
          <p className="model-effective-prices">{t("models.chargedPrices")}：{modelPriceSummary(preview, t)}</p>
        )}
        <p className="muted">{t("models.priceHint")}</p>
      </section>
      <div className="form-actions"><button type="button" className="btn" onClick={cancel}>{t("keyForm.cancel")}</button><button className="btn primary" disabled={saving}>{saving ? t("keyForm.submitting") : t("models.save")}</button></div>
      {picking && (
        <UpstreamPicker
          current={model.provider ? { provider: model.provider, target_model: model.target_model } : undefined}
          onClose={() => setPicking(false)}
          onPick={(choice) => {
            setModel((previous) => ({ ...previous, provider: choice.provider, target_model: choice.model, name: previous.name.trim() ? previous.name : choice.model }));
            setPicking(false);
          }}
        />
      )}
    </form>
  );
}
