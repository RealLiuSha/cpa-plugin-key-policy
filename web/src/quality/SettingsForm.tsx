import { useId, type ReactNode } from "react";
import { useT } from "../i18n";
import { fingerprintModeName, modelLabel, modelSuggestions, requestsPerCredential, type QualitySettings } from "./helpers";
import { EFFORTS, type QualityConfig, type QualityTest } from "./types";

interface Props {
  test: QualityTest;
  settings: QualitySettings;
  config: QualityConfig;
  // Model names from the price rows, as suggestions.
  models: string[];
  submitLabel: string;
  submitDisabled: boolean;
  onChange: (next: QualitySettings) => void;
  onSubmit: () => void;
}

// One test's settings; submitting starts it on the selected credentials.
export default function SettingsForm({ test, settings, config, models, submitLabel, submitDisabled, onChange, onSubmit }: Props) {
  const t = useT();
  const ids = useId();
  const listID = `${ids}-models`;
  const suggestions = modelSuggestions(models, test === "fingerprint" || test === "modeltrace");
  const perCredential = requestsPerCredential(test, settings, config);

  const modelField = (value: string, change: (model: string) => void) => (
    <label>
      {t("quality.settings.model")}
      <input className="input mono" list={listID} value={value} spellCheck={false} autoComplete="off" onChange={(event) => change(event.target.value)} />
      {(test === "fingerprint" || test === "modeltrace") && <span className="field-hint">{t("quality.settings.plainModelHint")}</span>}
    </label>
  );
  const effortField = (value: string, change: (effort: string) => void) => (
    <label>
      {t("quality.settings.effort")}
      <select value={value} onChange={(event) => change(event.target.value)}>
        {EFFORTS.map((effort) => <option key={effort} value={effort}>{effort === "none" ? t("quality.settings.effortNone") : effort}</option>)}
      </select>
      <span className="field-hint">{t("quality.settings.effortHint")}</span>
    </label>
  );
  const concurrencyField = (value: string, max: number, change: (concurrency: string) => void) => (
    <label>
      {t("quality.settings.concurrency")}
      <input className="input" type="number" min={1} max={max} step={1} value={value} onChange={(event) => change(event.target.value)} />
      <span className="field-hint">{t("quality.settings.concurrencyHint", { max })}</span>
    </label>
  );

  let fields: ReactNode = null;
  switch (test) {
    case "candy": {
      const candy = settings.candy;
      const set = (patch: Partial<QualitySettings["candy"]>) => onChange({ ...settings, candy: { ...candy, ...patch } });
      fields = (
        <>
          {modelField(candy.model, (model) => set({ model }))}
          {effortField(candy.effort, (effort) => set({ effort }))}
          <label>
            {t("quality.settings.runs")}
            <input className="input" type="number" min={1} max={config.candy.max_runs} step={1} value={candy.runs} onChange={(event) => set({ runs: event.target.value })} />
            <span className="field-hint">{t("quality.settings.runsHint", { max: config.candy.max_runs })}</span>
          </label>
        </>
      );
      break;
    }
    case "fingerprint": {
      const fingerprint = settings.fingerprint;
      const set = (patch: Partial<QualitySettings["fingerprint"]>) => onChange({ ...settings, fingerprint: { ...fingerprint, ...patch } });
      fields = (
        <>
          {modelField(fingerprint.model, (model) => set({ model }))}
          <label>
            {t("quality.settings.mode")}
            <select value={fingerprint.mode} onChange={(event) => set({ mode: event.target.value })}>
              {config.fingerprint.modes.map((mode) => (
                <option key={mode.id} value={mode.id}>{t("quality.settings.modeOption", { name: fingerprintModeName(t, config, mode.id), requests: mode.cells * mode.samples_per_cell })}</option>
              ))}
            </select>
          </label>
          {concurrencyField(fingerprint.concurrency, config.fingerprint.max_concurrency, (concurrency) => set({ concurrency }))}
        </>
      );
      break;
    }
    case "modeltrace": {
      const trace = settings.modeltrace;
      fields = (
        <>
          {modelField(trace.model, (model) => onChange({ ...settings, modeltrace: { ...trace, model } }))}
          {concurrencyField(trace.concurrency, config.modeltrace.max_concurrency, (concurrency) => onChange({ ...settings, modeltrace: { ...trace, concurrency } }))}
        </>
      );
      break;
    }
    case "pelican": {
      const pelican = settings.pelican;
      fields = (
        <>
          {modelField(pelican.model, (model) => onChange({ ...settings, pelican: { ...pelican, model } }))}
          {effortField(pelican.effort, (effort) => onChange({ ...settings, pelican: { ...pelican, effort } }))}
        </>
      );
      break;
    }
  }

  const requests = perCredential ?? "—";
  return (
    <form className="card quality-settings" aria-label={t("quality.settings.title")} onSubmit={(event) => { event.preventDefault(); onSubmit(); }}>
      <div className="form-grid">{fields}</div>
      <datalist id={listID}>{suggestions.map((name) => <option key={name} value={name} />)}</datalist>
      <div className="form-actions">
        <button type="submit" className="btn primary" disabled={submitDisabled}>{submitLabel}</button>
        <span className="muted quality-cost">
          {t(`quality.cost.${test}`, { requests })}
          {test === "candy" && <> · {t("quality.candy.rateScope", { model: modelLabel(settings.candy.model.trim() || "—", settings.candy.effort) })}</>}
        </span>
      </div>
    </form>
  );
}
