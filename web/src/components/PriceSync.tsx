import { useEffect, useMemo, useState } from "react";
import { fetchModelDefinitions, importModelPrices, previewModelPrices } from "../api/modelDefinitions";
import { extractApiError } from "../api/error";
import { basePriceSummary, isUnpriced } from "./modelPricing";
import { useT } from "../i18n";
import type { ModelDefinition, PriceImportMatch, PriceImportResult, PricingPreviewMatch } from "../types";

export interface SyncRow {
  model: ModelDefinition;
  // The name sent back to the plugin: the upstream id when Models.dev knows
  // it, otherwise the public name (the plugin matches either).
  key: string;
  match: PricingPreviewMatch;
}

export function syncRows(models: ModelDefinition[], matches: PricingPreviewMatch[]): { rows: SyncRow[]; unmatched: ModelDefinition[] } {
  const byName = new Map(matches.map((match) => [match.model.toLowerCase(), match]));
  const rows: SyncRow[] = [];
  const unmatched: ModelDefinition[] = [];
  for (const model of models) {
    const match = byName.get(model.target_model.toLowerCase()) ?? byName.get(model.name.toLowerCase());
    if (match) rows.push({ model, key: match.model, match });
    else unmatched.push(model);
  }
  return { rows, unmatched };
}

function pricesDiffer(model: ModelDefinition, match: PricingPreviewMatch): boolean {
  return (model.input_price_per_million ?? 0) !== match.prompt_price_per_1m
    || (model.output_price_per_million ?? 0) !== match.completion_price_per_1m
    || (model.cache_read_price_per_million ?? 0) !== match.cache_read_price_per_1m
    || (match.cache_write_price_per_1m !== undefined && model.cache_write_price_per_million !== match.cache_write_price_per_1m);
}

interface Props {
  // Public model names to tick; empty means "every model whose price changed".
  preselect: string[];
  onApplied: () => Promise<void>;
  onDone: () => void;
}

export default function PriceSync({ preselect, onApplied, onDone }: Props) {
  const t = useT();
  const [rows, setRows] = useState<SyncRow[]>([]);
  const [unmatched, setUnmatched] = useState<ModelDefinition[]>([]);
  const [perCall, setPerCall] = useState<ModelDefinition[]>([]);
  const [edits, setEdits] = useState<Record<string, PriceImportMatch>>({});
  const [selected, setSelected] = useState<Set<string>>(new Set());
  const [loading, setLoading] = useState(true);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [result, setResult] = useState<PriceImportResult | null>(null);

  useEffect(() => {
    let alive = true;
    void (async () => {
      try {
        const models = await fetchModelDefinitions();
        const tokenModels = models.filter((model) => model.billing_mode === "tokens");
        const names = [...new Set(tokenModels.flatMap((model) => [model.target_model, model.name]))];
        const preview = await previewModelPrices(names);
        if (!alive) return;
        const wanted = new Set(preselect.map((name) => name.toLowerCase()));
        const next = syncRows(tokenModels, preview.matches);
        setRows(next.rows);
        setUnmatched(next.unmatched.filter((model) => wanted.size === 0 || wanted.has(model.name.toLowerCase())));
        setPerCall(models.filter((model) => model.billing_mode === "per_call" && (wanted.size === 0 || wanted.has(model.name.toLowerCase()))));
        setEdits(Object.fromEntries(next.rows.map((row) => [row.model.name, {
          model: row.key,
          prompt_price_per_1m: row.match.prompt_price_per_1m,
          completion_price_per_1m: row.match.completion_price_per_1m,
          cache_read_price_per_1m: row.match.cache_read_price_per_1m,
          cache_write_price_per_1m: row.match.cache_write_price_per_1m,
        }])));
        setSelected(new Set(next.rows
          .filter((row) => wanted.size > 0 ? wanted.has(row.model.name.toLowerCase()) : isUnpriced(row.model) || pricesDiffer(row.model, row.match))
          .map((row) => row.model.name)));
      } catch (reason) {
        if (alive) setError(extractApiError(reason, t("models.pricingUnavailable")));
      } finally {
        if (alive) setLoading(false);
      }
    })();
    return () => { alive = false; };
    // One preview per dialog; preselect only seeds the initial selection.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  const visibleRows = useMemo(() => {
    if (preselect.length === 0) return rows;
    const wanted = new Set(preselect.map((name) => name.toLowerCase()));
    return rows.filter((row) => wanted.has(row.model.name.toLowerCase()));
  }, [preselect, rows]);

  const setPrice = (name: string, field: keyof Omit<PriceImportMatch, "model">, raw: string) => {
    const value = raw.trim() === "" ? undefined : Number(raw);
    setEdits((current) => ({ ...current, [name]: { ...current[name], [field]: value !== undefined && Number.isFinite(value) ? value : field === "cache_write_price_per_1m" ? undefined : 0 } }));
  };
  const toggle = (name: string) => setSelected((current) => {
    const next = new Set(current);
    if (next.has(name)) next.delete(name); else next.add(name);
    return next;
  });

  const apply = async () => {
    setBusy(true);
    setError("");
    try {
      const matches = visibleRows.filter((row) => selected.has(row.model.name)).map((row) => edits[row.model.name]);
      setResult(await importModelPrices({ dry_run: false, matches }));
      await onApplied();
    } catch (reason) {
      setError(extractApiError(reason, t("models.importInvalid")));
    } finally {
      setBusy(false);
    }
  };

  if (loading) return <div className="muted">{t("models.pricingLoading")}</div>;
  if (result) {
    return (
      <div className="model-import">
        <p className="success">{t("models.importSummary", { applied: result.applied.length, unchanged: result.unchanged.length, keys: result.affected_keys.length })}</p>
        <div className="form-actions modal-actions"><button type="button" className="btn primary" onClick={onDone}>{t("quota.done")}</button></div>
      </div>
    );
  }
  const count = visibleRows.filter((row) => selected.has(row.model.name)).length;
  return (
    <div className="model-import">
      <p className="muted">{t("models.syncHint")}</p>
      {error && <div className="error" role="alert">{error}</div>}
      {visibleRows.length > 0 && <div className="choice-list sync-list">
        {visibleRows.map((row) => {
          const edit = edits[row.model.name];
          return (
            <div className={"sync-item" + (selected.has(row.model.name) ? " active" : "")} key={row.model.name}>
              <label className="check-row">
                <input type="checkbox" checked={selected.has(row.model.name)} onChange={() => toggle(row.model.name)} />
                <strong>{row.model.name}</strong>
                <span className="muted">{t("models.pricingMatch", { model: row.match.matched_model, provider: row.match.source_provider_name })}</span>
              </label>
              <small className="muted">{t("models.currentPrice")}：{basePriceSummary(row.model, t)}</small>
              <div className="field-row">
                <label>{t("models.inputPrice")}<input className="input" type="number" min="0" step="any" value={edit.prompt_price_per_1m ?? 0} onChange={(event) => setPrice(row.model.name, "prompt_price_per_1m", event.target.value)} /></label>
                <label>{t("models.outputPrice")}<input className="input" type="number" min="0" step="any" value={edit.completion_price_per_1m ?? 0} onChange={(event) => setPrice(row.model.name, "completion_price_per_1m", event.target.value)} /></label>
                <label>{t("models.cachePrice")}<input className="input" type="number" min="0" step="any" value={edit.cache_read_price_per_1m ?? 0} onChange={(event) => setPrice(row.model.name, "cache_read_price_per_1m", event.target.value)} /></label>
                <label>{t("models.cacheWritePrice")}<input className="input" type="number" min="0" step="any" placeholder={t("models.sameAsInputPlaceholder")} value={edit.cache_write_price_per_1m ?? ""} onChange={(event) => setPrice(row.model.name, "cache_write_price_per_1m", event.target.value)} /></label>
              </div>
            </div>
          );
        })}
      </div>}
      {visibleRows.length === 0 && unmatched.length === 0 && perCall.length === 0 && !error && <div className="muted">{t("models.syncNothing")}</div>}
      {unmatched.length > 0 && <p className="notice">{t("models.pricingUnmatched", { models: unmatched.map((model) => model.name).join("、") })}</p>}
      {perCall.length > 0 && <p className="muted">{t("models.syncPerCallNote", { models: perCall.map((model) => model.name).join("、") })}</p>}
      <div className="form-actions modal-actions">
        <button type="button" className="btn" onClick={onDone}>{t("keyForm.cancel")}</button>
        <button type="button" className="btn primary" disabled={busy || count === 0} onClick={() => void apply()}>{busy ? t("keyForm.submitting") : t("models.applyCount", { count })}</button>
      </div>
    </div>
  );
}
