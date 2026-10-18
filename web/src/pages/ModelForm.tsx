import { isUnpriced, MAX_MULTIPLIER, modelPriceSummary, priceRowKey, validMultiplier } from "../components/modelPricing";
import { useCallback, useEffect, useState } from "react";
import { useNavigate, useSearchParams } from "react-router-dom";
import { fetchModelPrices, saveModelPrice } from "../api/modelPrices";
import { formatUSD } from "../format";
import type { ModelPrice, ModelPriceRow } from "../types";
import { extractApiError } from "../api/error";
import { useT } from "../i18n";

// Plugin-side validation bound; the form stops values the plugin would reject.
const MAX_PRICE = 10000;

const emptyPrice: ModelPrice = {
  name: "", provider: "", billing_mode: "tokens", billing_multiplier: 1,
  input_price_per_million: 0, output_price_per_million: 0, cache_read_price_per_million: 0, per_call_usd: 0,
};

type PriceField = "input_price_per_million" | "output_price_per_million" | "cache_read_price_per_million" | "cache_write_price_per_million" | "per_call_usd";

// Serves /models/new (optionally ?name= to prefill) and /models/edit?name=&provider=.
// A row's model id and provider qualifier are its identity, so editing keeps both.
export default function ModelForm({ mode }: { mode: "new" | "edit" }) {
  const [params] = useSearchParams();
  const navigate = useNavigate();
  const t = useT();
  const editing = mode === "edit";
  const editName = params.get("name") ?? "";
  const editProvider = params.get("provider") ?? "";
  const [model, setModel] = useState<ModelPrice>(() => (editing ? emptyPrice : { ...emptyPrice, name: params.get("name") ?? "" }));
  const [multiplierInput, setMultiplierInput] = useState("1");
  const [loading, setLoading] = useState(editing);
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState("");
  const [rows, setRows] = useState<ModelPriceRow[]>([]);
  const multiplier = Number(multiplierInput);
  const multiplierValid = validMultiplier(multiplierInput);
  const tokens = model.billing_mode === "tokens";
  // The plugin keys rows by id ignoring case and by provider, so a new entry
  // that matches a row updates it and keeps the row's spelling.
  const existing = editing ? undefined : rows.find((row) => priceRowKey(row.name, row.provider) === priceRowKey(model.name, model.provider));

  useEffect(() => {
    let alive = true;
    void fetchModelPrices()
      .then(({ models }) => {
        if (!alive) return;
        setRows(models);
        if (!editing) return;
        const found = models.find((row) => priceRowKey(row.name, row.provider) === priceRowKey(editName, editProvider));
        if (!found) setError(t("models.notFound"));
        else { setModel(found); setMultiplierInput(String(found.billing_multiplier ?? 1)); }
      })
      .catch((reason) => { if (alive && editing) setError(extractApiError(reason, t("models.loadFailed"))); })
      .finally(() => { if (alive) setLoading(false); });
    return () => { alive = false; };
  }, [editing, editName, editProvider, t]);

  const setPrice = (field: PriceField, value: string) => {
    const parsed = Number.parseFloat(value);
    setModel((previous) => ({
      ...previous,
      [field]: field === "cache_write_price_per_million" && value.trim() === "" ? undefined : Number.isFinite(parsed) ? parsed : 0,
    }));
  };

  const save = useCallback(async (event: React.FormEvent) => {
    event.preventDefault();
    setError("");
    if (!model.name.trim()) {
      setError(t("models.required"));
      return;
    }
    if (tokens && !multiplierValid) {
      setError(t("quota.invalidMultiplier"));
      return;
    }
    setSaving(true);
    try {
      await saveModelPrice({
        ...model,
        name: model.name.trim(),
        provider: model.provider.trim(),
        billing_multiplier: tokens ? multiplier : model.billing_multiplier ?? 1,
      });
      navigate("/models");
    } catch (reason) {
      setError(extractApiError(reason, t("models.saveFailed")));
    } finally {
      setSaving(false);
    }
  }, [model, multiplier, multiplierValid, navigate, t, tokens]);

  if (loading) return <div className="muted">{t("keys.loading")}</div>;
  const preview = { ...model, billing_multiplier: multiplierValid ? multiplier : 1 };
  const exampleBase = (model.input_price_per_million ?? 0) + (model.output_price_per_million ?? 0);
  const priceInput = (field: PriceField, extra: { placeholder?: string; value: number | string }) => (
    <input className="input" type="number" min="0" max={MAX_PRICE} step="any" placeholder={extra.placeholder} value={extra.value} onChange={(event) => setPrice(field, event.target.value)} />
  );
  return (
    <form className="form-page model-form" onSubmit={save}>
      <div className="page-head"><h1>{editing ? t("models.editTitle") : t("models.newTitle")}</h1></div>
      {error && <div className="error" role="alert">{error}</div>}
      <section className="card">
        <label>{t("models.modelId")}
          <input className="input mono" value={model.name} disabled={editing} placeholder={t("models.modelIdPlaceholder")} onChange={(event) => setModel((previous) => ({ ...previous, name: event.target.value }))} />
          <small className="field-hint">{t("models.modelIdHint")}</small>
        </label>
        <label>{t("models.providerQualifier")}
          <input className="input mono" value={model.provider} disabled={editing} placeholder={t("models.anyProvider")} onChange={(event) => setModel((previous) => ({ ...previous, provider: event.target.value }))} />
          <small className="field-hint">{t("models.providerQualifierHint")}</small>
        </label>
        {existing && <p className="notice info" role="note">{t("models.existingRow", { name: existing.name, provider: existing.provider || t("models.anyProvider") })}</p>}
      </section>
      <section className="card">
        <div className="seg-inline" role="radiogroup" aria-label={t("models.billing")}>
          <span className="field-label">{t("models.billing")}</span>
          <label className="check-row"><input type="radio" name="billing_mode" checked={tokens} onChange={() => setModel((previous) => ({ ...previous, billing_mode: "tokens" }))} />{t("models.tokens")}</label>
          <label className="check-row"><input type="radio" name="billing_mode" checked={!tokens} onChange={() => setModel((previous) => ({ ...previous, billing_mode: "per_call" }))} />{t("models.perCall")}</label>
        </div>
        {!tokens && <p className="field-hint per-call-hint" role="note">{t("models.perCallHint")}</p>}
        {tokens ? (
          <>
            <div className="field-row">
              <label>{t("models.inputPrice")}{priceInput("input_price_per_million", { value: model.input_price_per_million ?? 0 })}</label>
              <label>{t("models.outputPrice")}{priceInput("output_price_per_million", { value: model.output_price_per_million ?? 0 })}</label>
              <label>{t("models.cachePrice")}{priceInput("cache_read_price_per_million", { placeholder: t("models.sameAsInputPlaceholder"), value: model.cache_read_price_per_million || "" })}</label>
              <label>{t("models.cacheWritePrice")}{priceInput("cache_write_price_per_million", { placeholder: t("models.sameAsInputPlaceholder"), value: model.cache_write_price_per_million ?? "" })}</label>
            </div>
            <div className="model-multiplier">
              <label>{t("quota.multiplier")}<input className="input" type="number" min="1" max={MAX_MULTIPLIER} step="any" required value={multiplierInput} onChange={(event) => setMultiplierInput(event.target.value)} /></label>
              <div>
                <p className="muted">{t("quota.multiplierHelp")}</p>
                {multiplierValid && exampleBase > 0 && <strong>{t("quota.multiplierExample", { base: formatUSD(exampleBase), amount: formatUSD(exampleBase * multiplier) })}</strong>}
              </div>
            </div>
          </>
        ) : (
          <label>{t("models.perCallPrice")}{priceInput("per_call_usd", { value: model.per_call_usd ?? 0 })}</label>
        )}
        {isUnpriced(model) ? <p className="notice">{t("models.unpricedHint")}</p> : (
          <p className="model-effective-prices">{t("models.chargedPrices")}：{modelPriceSummary(preview, t)}</p>
        )}
        <p className="muted">{t("models.priceHint")}</p>
      </section>
      <div className="form-actions"><button type="button" className="btn" onClick={() => navigate("/models")}>{t("keyForm.cancel")}</button><button className="btn primary" disabled={saving}>{saving ? t("keyForm.submitting") : t("models.save")}</button></div>
    </form>
  );
}
