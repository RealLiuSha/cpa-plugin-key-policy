import { useEffect, useState } from "react";
import { fetchModelDefinitions, importModelPrices, previewModelPrices } from "../api/modelDefinitions";
import { extractApiError } from "../api/error";
import { useT } from "../i18n";
import type { ModelDefinition, PriceImportMatch, PriceImportResult, PricingPreviewMatch } from "../types";

interface ModelPriceImportProps {
  onApplied: () => Promise<void>;
}

export default function ModelPriceImport({ onApplied }: ModelPriceImportProps) {
  const t = useT();
  const [models, setModels] = useState<ModelDefinition[]>([]);
  const [matches, setMatches] = useState<PricingPreviewMatch[]>([]);
  const [unmatched, setUnmatched] = useState<string[]>([]);
  const [edits, setEdits] = useState<Record<string, PriceImportMatch>>({});
  const [selected, setSelected] = useState<Record<string, boolean>>({});
  const [result, setResult] = useState<PriceImportResult | null>(null);
  const [dryRunReady, setDryRunReady] = useState(false);
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);

  useEffect(() => {
    void fetchModelDefinitions().then(setModels).catch((reason) => setError(extractApiError(reason, t("models.loadFailed"))));
  }, [t]);

  const loadPreview = async () => {
    setBusy(true);
    setError("");
    setResult(null);
    setDryRunReady(false);
    try {
      const names = models.flatMap((model) => model.targets.map((target) => target.target_model)).concat(models.map((model) => model.name));
      const preview = await previewModelPrices([...new Set(names)]);
      setMatches(preview.matches);
      setUnmatched(preview.unmatched_models);
      const next: Record<string, PriceImportMatch> = {};
      const nextSelected: Record<string, boolean> = {};
      for (const match of preview.matches) {
        next[match.model] = {
          model: match.model,
          prompt_price_per_1m: match.prompt_price_per_1m,
          completion_price_per_1m: match.completion_price_per_1m,
          cache_read_price_per_1m: match.cache_read_price_per_1m,
          cache_write_price_per_1m: match.cache_write_price_per_1m,
        };
        nextSelected[match.model] = true;
      }
      setEdits(next);
      setSelected(nextSelected);
    } catch (reason) {
      setError(extractApiError(reason, t("models.pricingUnavailable")));
    } finally {
      setBusy(false);
    }
  };

  const runImport = async (dryRun: boolean) => {
    setBusy(true);
    setError("");
    try {
      const payload = Object.values(edits).filter((match) => selected[match.model]);
      const next = await importModelPrices({ dry_run: dryRun, matches: payload });
      setResult(next);
      setDryRunReady(dryRun);
      if (!dryRun) await onApplied();
    } catch (reason) {
      setError(extractApiError(reason, t("models.importInvalid")));
    } finally {
      setBusy(false);
    }
  };

  const updateEdit = (model: string, patch: Partial<PriceImportMatch>) => {
    setEdits((current) => ({ ...current, [model]: { ...current[model], ...patch } }));
    setResult(null);
    setDryRunReady(false);
  };

  const updateCacheWrite = (model: string, raw: string) => {
    const value = raw.trim() === "" ? undefined : Number(raw);
    updateEdit(model, { cache_write_price_per_1m: Number.isFinite(value) ? value : undefined });
  };

  const updateSelected = (model: string, checked: boolean) => {
    setSelected((current) => ({ ...current, [model]: checked }));
    setResult(null);
    setDryRunReady(false);
  };

  const selectedCount = Object.values(selected).filter(Boolean).length;

  return (
    <div className="model-import">
      <p className="muted">{t("models.syncHint")}</p>
      <div className="card-actions">
        <button type="button" className="btn sm" disabled={busy} onClick={() => void loadPreview()}>{t("models.pricingPreview")}</button>
        <button type="button" className="btn sm" disabled={busy || selectedCount === 0} onClick={() => void runImport(true)}>{t("models.importPreview")}</button>
        <button type="button" className="btn sm primary" disabled={busy || !dryRunReady || selectedCount === 0} onClick={() => void runImport(false)}>{t("models.importApply")}</button>
      </div>
      {error && <div className="error">{error}</div>}
      {matches.map((match) => {
        const edit = edits[match.model];
        return (
          <div className="import-row" key={match.model}>
            <label className="check-row"><input type="checkbox" checked={selected[match.model] ?? false} onChange={(event) => updateSelected(match.model, event.target.checked)} /><strong>{match.model}</strong></label>
            <div className="muted">{t("models.pricingMatch", { model: match.matched_model, provider: match.source_provider_name, type: match.match_type })}</div>
            <div className="field-row">
              <label>{t("models.inputPrice")}<input className="input" type="number" value={edit?.prompt_price_per_1m ?? 0} onChange={(event) => updateEdit(match.model, { prompt_price_per_1m: Number(event.target.value) || 0 })} /></label>
              <label>{t("models.outputPrice")}<input className="input" type="number" value={edit?.completion_price_per_1m ?? 0} onChange={(event) => updateEdit(match.model, { completion_price_per_1m: Number(event.target.value) || 0 })} /></label>
              <label>{t("models.cachePrice")}<input className="input" type="number" value={edit?.cache_read_price_per_1m ?? 0} onChange={(event) => updateEdit(match.model, { cache_read_price_per_1m: Number(event.target.value) || 0 })} /></label>
              <label>{t("models.cacheWritePrice")}<input className="input" type="number" value={edit?.cache_write_price_per_1m ?? ""} onChange={(event) => updateCacheWrite(match.model, event.target.value)} /></label>
            </div>
          </div>
        );
      })}
      {unmatched.length > 0 && <div className="muted">{t("models.pricingUnmatched", { models: unmatched.join(", ") })}</div>}
      {result && <div className="muted">{t("models.importSummary", { applied: result.applied.length, unchanged: result.unchanged.length, skipped: result.skipped.length, keys: result.affected_keys.length })}</div>}
    </div>
  );
}
