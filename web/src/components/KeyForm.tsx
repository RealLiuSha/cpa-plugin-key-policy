import { useEffect, useMemo, useState } from "react";
import { useNavigate } from "react-router-dom";
import type { KeyModelRef, KeyPublic, KeyWriteRequest, ModelDefinition } from "../types";
import { fetchModelDefinitions } from "../api/modelDefinitions";
import { extractApiError } from "../api/error";
import { useT } from "../i18n";

export interface KeyFormValues {
  id: string;
  name: string;
  enabled: boolean;
  rpm: number;
  models: KeyModelRef[];
  daily_limit_usd: number;
  weekly_limit_usd: number;
  monthly_limit_usd: number;
  allow_models_endpoint?: boolean;
}

export function keyWriteRequestFromForm(values: KeyFormValues): KeyWriteRequest {
  return {
    id: values.id,
    name: values.name.trim() || undefined,
    enabled: values.enabled,
    rpm: values.rpm,
    models: values.models,
    daily_limit_usd: values.daily_limit_usd,
    weekly_limit_usd: values.weekly_limit_usd,
    monthly_limit_usd: values.monthly_limit_usd,
    allow_models_endpoint: values.allow_models_endpoint,
  };
}

interface Props {
  initial?: KeyPublic;
  idReadOnly?: boolean;
  submitLabel: string;
  onSubmit: (values: KeyFormValues) => Promise<void>;
  onCancel: () => void;
  error?: string;
  returnPath?: string;
  dangerLabel?: string;
  onDanger?: () => void;
}

function parseNumber(value: string): number {
  const parsed = Number.parseFloat(value);
  return Number.isFinite(parsed) ? parsed : 0;
}

function priceSummary(model: ModelDefinition, translate: (key: string, variables?: Record<string, string | number>) => string): string {
  if (model.free) return translate("models.free");
  if (model.billing_mode === "per_call") return translate("models.pricePerCallSummary", { price: model.per_call_usd ?? 0 });
  return translate("models.priceTokenSummary", {
    input: model.input_price_per_million ?? 0,
    output: model.output_price_per_million ?? 0,
    cache: model.cache_read_price_per_million ?? 0,
  });
}

export default function KeyForm({
  initial,
  idReadOnly,
  submitLabel,
  onSubmit,
  onCancel,
  error,
  returnPath,
  dangerLabel,
  onDanger,
}: Props) {
  const navigate = useNavigate();
  const t = useT();
  const [id, setID] = useState(initial?.id ?? "");
  const [name, setName] = useState(initial?.name ?? "");
  const [enabled, setEnabled] = useState(initial?.enabled ?? true);
  const [rpm, setRPM] = useState(initial?.rpm ?? 0);
  const [dailyLimit, setDailyLimit] = useState(initial?.daily_limit_usd ?? 0);
  const [weeklyLimit, setWeeklyLimit] = useState(initial?.weekly_limit_usd ?? 0);
  const [monthlyLimit, setMonthlyLimit] = useState(initial?.monthly_limit_usd ?? 0);
  const [allowModels, setAllowModels] = useState(initial?.allow_models_endpoint ?? false);
  const [selected, setSelected] = useState<KeyModelRef[]>(initial?.models ?? []);
  const [definitions, setDefinitions] = useState<ModelDefinition[]>([]);
  const [loadingModels, setLoadingModels] = useState(true);
  const [busy, setBusy] = useState(false);
  const [localError, setLocalError] = useState("");

  useEffect(() => {
    let alive = true;
    void fetchModelDefinitions()
      .then((models) => {
        if (alive) setDefinitions(models);
      })
      .catch((reason) => {
        if (alive) setLocalError(extractApiError(reason, t("picker.loadFailed")));
      })
      .finally(() => {
        if (alive) setLoadingModels(false);
      });
    return () => { alive = false; };
  }, [t]);

  const selectedByName = useMemo(
    () => new Map(selected.map((ref) => [ref.name.toLowerCase(), ref])),
    [selected],
  );

  const currentValues = (): KeyFormValues => ({
    id: id.trim(), name, enabled, rpm, models: selected,
    daily_limit_usd: dailyLimit, weekly_limit_usd: weeklyLimit,
    monthly_limit_usd: monthlyLimit, allow_models_endpoint: allowModels,
  });

  const toggleModel = (model: ModelDefinition) => {
    const key = model.name.toLowerCase();
    setSelected((previous) => {
      if (previous.some((ref) => ref.name.toLowerCase() === key)) {
        return previous.filter((ref) => ref.name.toLowerCase() !== key);
      }
      return [...previous, { name: model.name, daily_limit_usd: 0 }];
    });
  };

  const setModelDailyLimit = (modelName: string, value: number) => {
    setSelected((previous) => previous.map((ref) =>
      ref.name.toLowerCase() === modelName.toLowerCase()
        ? { ...ref, daily_limit_usd: value }
        : ref,
    ));
  };

  const createModel = () => {
    navigate("/models/new", {
      state: {
        returnTo: returnPath ?? window.location.pathname,
        draftKey: currentValues(),
      },
    });
  };

  const submit = async (event: React.FormEvent) => {
    event.preventDefault();
    setLocalError("");
    if (!id.trim()) {
      setLocalError(t("keyForm.idRequired"));
      return;
    }
    if ([rpm, dailyLimit, weeklyLimit, monthlyLimit, ...selected.map((ref) => ref.daily_limit_usd ?? 0)].some((value) => value < 0)) {
      setLocalError(t("keyForm.modelLimitInvalid"));
      return;
    }
    setBusy(true);
    try {
      await onSubmit(currentValues());
    } catch (reason) {
      setLocalError(extractApiError(reason, t("keyForm.submitFailed")));
    } finally {
      setBusy(false);
    }
  };

  return (
    <form className="key-form" onSubmit={submit}>
      {(error || localError) && <div className="error">{error || localError}</div>}

      <section className="kf-section">
        <h2>{t("keyForm.mobile.sectionBasic")}</h2>
        <div className="form-grid">
          <label>{t("keyForm.idLabel")}<input className="input" value={id} disabled={idReadOnly} onChange={(event) => setID(event.target.value)} /></label>
          <label>{t("keyForm.nameLabel")}<input className="input" value={name} onChange={(event) => setName(event.target.value)} /></label>
          <label>{t("keyForm.rpmLabel")}<input className="input" type="number" min="0" value={rpm} onChange={(event) => setRPM(parseNumber(event.target.value))} /></label>
          <label className="check-row"><input type="checkbox" checked={enabled} onChange={(event) => setEnabled(event.target.checked)} />{t("keyForm.enableKey")}</label>
        </div>
      </section>

      <section className="kf-section">
        <h2>{t("keyForm.mobile.sectionLimits")}</h2>
        <div className="form-grid">
          <label>{t("keyForm.dailyLimitLabel")}<input className="input" type="number" min="0" step="0.01" value={dailyLimit} onChange={(event) => setDailyLimit(parseNumber(event.target.value))} /></label>
          <label>{t("keyForm.weeklyLimitLabel")}<input className="input" type="number" min="0" step="0.01" value={weeklyLimit} onChange={(event) => setWeeklyLimit(parseNumber(event.target.value))} /></label>
          <label>{t("keyForm.monthlyLimitLabel")}<input className="input" type="number" min="0" step="0.01" value={monthlyLimit} onChange={(event) => setMonthlyLimit(parseNumber(event.target.value))} /></label>
        </div>
      </section>

      <section className="kf-section">
        <div className="section-title-row">
          <div>
            <h2>{t("keyForm.availableModels")}</h2>
            <p className="muted">{t("keyForm.availableModelsHint")}</p>
          </div>
          <button type="button" className="btn sm" onClick={createModel}>{t("keyForm.newModel")}</button>
        </div>
        {loadingModels ? <div className="muted">{t("picker.loading")}</div> : definitions.length === 0 ? (
          <div className="muted">{t("keyForm.noDefinedModels")}</div>
        ) : (
          <div className="model-definition-list">
            {definitions.map((model) => {
              const ref = selectedByName.get(model.name.toLowerCase());
              return (
                <div className={"model-definition-row" + (ref ? " active" : "")} key={model.name}>
                  <label className="model-definition-main">
                    <input type="checkbox" checked={!!ref} onChange={() => toggleModel(model)} />
                    <span><strong>{model.name}</strong><small>{priceSummary(model, t)}</small></span>
                  </label>
                  {ref && (
                    <label className="model-limit-field">
                      {t("keyForm.modelDailyLimit", { model: model.name })}
                      <input className="input" type="number" min="0" step="0.01" value={ref.daily_limit_usd ?? 0} onChange={(event) => setModelDailyLimit(model.name, parseNumber(event.target.value))} />
                    </label>
                  )}
                </div>
              );
            })}
          </div>
        )}
      </section>

      <section className="kf-section">
        <label className="check-row" title={t("keyForm.allowModelsTitle")}>
          <input type="checkbox" checked={allowModels} onChange={(event) => setAllowModels(event.target.checked)} />
          {t("keyForm.allowModelsLabel")}
        </label>
        <p className="muted">{t("keyForm.allowModelsHint")}</p>
      </section>

      <div className="form-actions">
        {dangerLabel && onDanger && <button type="button" className="btn danger" onClick={onDanger}>{dangerLabel}</button>}
        <span className="form-actions-spacer" />
        <button type="button" className="btn" onClick={onCancel}>{t("keyForm.cancel")}</button>
        <button className="btn primary" disabled={busy}>{busy ? t("keyForm.submitting") : submitLabel}</button>
      </div>
    </form>
  );
}
