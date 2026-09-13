import { modelPriceSummary } from "./modelPricing";
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
  keyListReturnTo?: string;
  showCurrentUsage?: boolean;
  dangerLabel?: string;
  onDanger?: () => void;
}

function parseNumber(value: string): number {
  const parsed = Number.parseFloat(value);
  return Number.isFinite(parsed) ? parsed : 0;
}


export default function KeyForm({
  initial,
  idReadOnly,
  submitLabel,
  onSubmit,
  onCancel,
  error,
  returnPath,
  keyListReturnTo,
  showCurrentUsage = false,
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
  const [modelQuery, setModelQuery] = useState("");
  const [showSelectedOnly, setShowSelectedOnly] = useState(false);
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
  const filteredDefinitions = useMemo(() => {
    const query = modelQuery.trim().toLowerCase();
    if (!query) return definitions;
    return definitions.filter((model) => model.name.toLowerCase().includes(query));
  }, [definitions, modelQuery]);
  const visibleDefinitions = useMemo(
    () => showSelectedOnly
      ? filteredDefinitions.filter((model) => selectedByName.has(model.name.toLowerCase()))
      : filteredDefinitions,
    [filteredDefinitions, selectedByName, showSelectedOnly],
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

  const selectVisibleModels = () => {
    setSelected((previous) => {
      const next = new Map(previous.map((ref) => [ref.name.toLowerCase(), ref]));
      for (const model of visibleDefinitions) {
        if (!next.has(model.name.toLowerCase())) next.set(model.name.toLowerCase(), { name: model.name, daily_limit_usd: 0 });
      }
      return [...next.values()];
    });
  };

  const clearVisibleModels = () => {
    const visibleNames = new Set(visibleDefinitions.map((model) => model.name.toLowerCase()));
    setSelected((previous) => previous.filter((ref) => !visibleNames.has(ref.name.toLowerCase())));
  };

  const canSelectVisible = visibleDefinitions.some((model) => !selectedByName.has(model.name.toLowerCase()));
  const canClearVisible = visibleDefinitions.some((model) => selectedByName.has(model.name.toLowerCase()));

  const createModel = () => {
    navigate("/models/new", {
      state: {
        returnTo: returnPath ?? window.location.pathname,
        draftKey: currentValues(),
        keyListReturnTo,
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
    <form className="card key-form" onSubmit={submit}>
      {(error || localError) && <div className="error">{error || localError}</div>}

      <section className="kf-section">
        <h2>{t("keyForm.mobile.sectionBasic")}</h2>
        <div className="form-grid">
          <label><span className="field-label">{t("keyForm.idLabel")}</span><input className="input" value={id} disabled={idReadOnly} placeholder={t("keyForm.idPlaceholder")} onChange={(event) => setID(event.target.value)} /></label>
          <label><span className="field-label">{t("keyForm.nameLabel")}</span><input className="input" value={name} placeholder={t("keyForm.namePlaceholder")} onChange={(event) => setName(event.target.value)} /></label>
          <label><span className="field-label">{t("keyForm.rpmLabel")}</span><input className="input" type="number" min="0" value={rpm} onChange={(event) => setRPM(parseNumber(event.target.value))} /></label>
          <fieldset className="check-field">
            <legend>{t("keyForm.statusLabel")}</legend>
            <label className="check-row"><input type="checkbox" checked={enabled} onChange={(event) => setEnabled(event.target.checked)} />{t("keyForm.enableKey")}</label>
          </fieldset>
        </div>
      </section>

      <section className="kf-section">
        <h2>{t("keyForm.mobile.sectionLimits")}</h2>
        <div className="form-grid">
          <label>
            <span className="field-label">{t("keyForm.dailyLimitLabel")}</span>
            <input className="input" type="number" min="0" step="0.01" value={dailyLimit} onChange={(event) => setDailyLimit(parseNumber(event.target.value))} />
            <small className="field-hint">{t("keyForm.dailyLimitHint")}</small>
            {showCurrentUsage && initial && <small className="kf-current-usage">{t("keyForm.currentUsage", { amount: initial.usage.daily_usd.toFixed(2) })}</small>}
          </label>
          <label>
            <span className="field-label">{t("keyForm.weeklyLimitLabel")}</span>
            <input className="input" type="number" min="0" step="0.01" value={weeklyLimit} onChange={(event) => setWeeklyLimit(parseNumber(event.target.value))} />
            <small className="field-hint">{t("keyForm.weeklyLimitHint")}</small>
            {showCurrentUsage && initial && <small className="kf-current-usage">{t("keyForm.currentUsage", { amount: initial.usage.weekly_usd.toFixed(2) })}</small>}
          </label>
          <label>
            <span className="field-label">{t("keyForm.monthlyLimitLabel")}</span>
            <input className="input" type="number" min="0" step="0.01" value={monthlyLimit} onChange={(event) => setMonthlyLimit(parseNumber(event.target.value))} />
            <small className="field-hint">{t("keyForm.monthlyLimitHint")}</small>
            {showCurrentUsage && initial && <small className="kf-current-usage">{t("keyForm.currentUsage", { amount: (initial.usage.monthly_usd ?? 0).toFixed(2) })}</small>}
          </label>
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
          <>
            <div className="model-definition-toolbar" role="group" aria-label={t("keyForm.modelToolsLabel")}>
              <input className="input model-definition-search" aria-label={t("keyForm.searchModelsPlaceholder")} value={modelQuery} placeholder={t("keyForm.searchModelsPlaceholder")} onChange={(event) => setModelQuery(event.target.value)} />
              <div className="model-definition-tools">
                <button type="button" className="btn sm" disabled={!canSelectVisible} onClick={selectVisibleModels}>{t("picker.selectAll")}</button>
                <button type="button" className="btn sm" disabled={!canClearVisible} onClick={clearVisibleModels}>{t("picker.clearAll")}</button>
                <button type="button" className={`btn sm${showSelectedOnly ? " active" : ""}`} aria-pressed={showSelectedOnly} onClick={() => setShowSelectedOnly((current) => !current)}>{t("keyForm.showSelectedOnly")}</button>
              </div>
              <span className="model-selection-count">{t("keyForm.selectedModelsSummary", { selected: selected.length, total: definitions.length })}</span>
            </div>
            {visibleDefinitions.length === 0 ? <div className="empty-state">{t("keyForm.noModelMatch")}</div> : <div className="model-definition-list">
            {visibleDefinitions.map((model) => {
              const ref = selectedByName.get(model.name.toLowerCase());
              return (
                <div className={"model-definition-row" + (ref ? " active" : "")} key={model.name}>
                  <label className="model-definition-main">
                    <input type="checkbox" checked={!!ref} onChange={() => toggleModel(model)} />
                    <span><strong>{model.name}</strong><small>{modelPriceSummary(model, t)}</small></span>
                  </label>
                  {ref && (
                    <label className="model-limit-field" title={t("keyForm.modelDailyLimit", { model: model.name })}>
                      <span>{t("keyForm.modelDailyLimitShort")}</span>
                      <input className="input" aria-label={t("keyForm.modelDailyLimit", { model: model.name })} type="number" min="0" step="0.01" value={ref.daily_limit_usd ?? 0} onChange={(event) => setModelDailyLimit(model.name, parseNumber(event.target.value))} />
                    </label>
                  )}
                </div>
              );
            })}
            </div>}
          </>
        )}
      </section>

      <section className="kf-section">
        <h2>{t("keyForm.mobile.sectionAccess")}</h2>
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
