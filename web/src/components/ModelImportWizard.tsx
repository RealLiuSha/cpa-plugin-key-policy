import { useEffect, useMemo, useState } from "react";
import { fetchCatalog } from "../api/models";
import { fetchModelDefinitions, importModels, previewModelPrices } from "../api/modelDefinitions";
import { extractApiError } from "../api/error";
import { useT } from "../i18n";
import type { CatalogModel, ModelDefinition, ModelImportItem, ModelImportResult, ModelTarget, PricingPreviewMatch } from "../types";

export interface ImportDraft {
  modelId: string;
  selected: boolean;
  publicName: string;
  targetKey: string;
  dispatch: "round-robin" | "priority";
  overwrite: boolean;
  input: number;
  output: number;
  cacheRead: number;
  cacheWrite?: number;
  missingPrice: boolean;
  match?: PricingPreviewMatch;
}

function candidateKey(candidate: CatalogModel): string {
  return `${candidate.provider}|${candidate.group ?? ""}|${candidate.model}`;
}

function targetFromKey(key: string, modelId: string): ModelTarget {
  const [provider, group] = key.split("|");
  const target: ModelTarget = { provider, target_model: modelId };
  if (group) target.group = group;
  return target;
}

export function groupCatalogCandidates(catalog: CatalogModel[]): Map<string, CatalogModel[]> {
  const groups = new Map<string, CatalogModel[]>();
  for (const row of catalog) {
    const current = groups.get(row.model) ?? [];
    current.push(row);
    groups.set(row.model, current);
  }
  return groups;
}

export function buildImportItems(drafts: ImportDraft[]): ModelImportItem[] {
  return drafts.filter((draft) => draft.selected).map((draft) => ({
    name: draft.publicName.trim() || draft.modelId,
    targets: [targetFromKey(draft.targetKey, draft.modelId)],
    dispatch: draft.dispatch,
    overwrite: draft.overwrite,
    input_price_per_million: draft.input,
    output_price_per_million: draft.output,
    cache_read_price_per_million: draft.cacheRead,
    ...(draft.cacheWrite === undefined ? {} : { cache_write_price_per_million: draft.cacheWrite }),
    missing_price: draft.missingPrice,
  }));
}

interface ModelImportWizardProps {
  onApplied: () => Promise<void>;
}

export default function ModelImportWizard({ onApplied }: ModelImportWizardProps) {
  const t = useT();
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState("");
  const [groups, setGroups] = useState<Map<string, CatalogModel[]>>(new Map());
  const [existing, setExisting] = useState<ModelDefinition[]>([]);
  const [drafts, setDrafts] = useState<ImportDraft[]>([]);
  const [dryRun, setDryRun] = useState<ModelImportResult | null>(null);
  const [busy, setBusy] = useState(false);

  useEffect(() => {
    void (async () => {
      setLoading(true);
      setError("");
      try {
        const [catalog, models] = await Promise.all([fetchCatalog(), fetchModelDefinitions()]);
        const grouped = groupCatalogCandidates(catalog);
        setGroups(grouped);
        setExisting(models);
        const next: ImportDraft[] = [];
        for (const [modelId, candidates] of grouped) {
          const first = candidates[0];
          const found = models.find((model) => model.name.toLowerCase() === modelId.toLowerCase());
          next.push({
            modelId,
            selected: !found,
            publicName: modelId,
            targetKey: candidateKey(first),
            dispatch: "round-robin",
            overwrite: false,
            input: 0,
            output: 0,
            cacheRead: 0,
            cacheWrite: undefined,
            missingPrice: true,
          });
        }
        setDrafts(next);
      } catch (reason) {
        setError(extractApiError(reason, t("models.importCatalogFailed")));
      } finally {
        setLoading(false);
      }
    })();
  }, [t]);

  const existingByName = useMemo(() => {
    const map = new Map<string, ModelDefinition>();
    for (const model of existing) map.set(model.name.toLowerCase(), model);
    return map;
  }, [existing]);

  const loadPrices = async () => {
    setBusy(true);
    setError("");
    setDryRun(null);
    try {
      const selected = drafts.filter((draft) => draft.selected);
      const preview = await previewModelPrices(selected.map((draft) => draft.modelId));
      const byModel = new Map(preview.matches.map((match) => [match.model, match]));
      setDrafts((current) => current.map((draft) => {
        if (!draft.selected) return draft;
        const match = byModel.get(draft.modelId);
        if (!match) return { ...draft, missingPrice: true, match: undefined };
        return {
          ...draft,
          missingPrice: !(match.prompt_price_per_1m > 0 || match.completion_price_per_1m > 0 || match.cache_read_price_per_1m > 0 || (match.cache_write_price_per_1m ?? 0) > 0),
          match,
          input: match.prompt_price_per_1m,
          output: match.completion_price_per_1m,
          cacheRead: match.cache_read_price_per_1m,
          cacheWrite: match.cache_write_price_per_1m,
        };
      }));
    } catch (reason) {
      setError(extractApiError(reason, t("models.pricingUnavailable")));
    } finally {
      setBusy(false);
    }
  };

  const run = async (apply: boolean) => {
    const priced = drafts.map((draft) => {
      if (!draft.selected) return draft;
      const hasPositivePrice = draft.input > 0 || draft.output > 0 || draft.cacheRead > 0 || (draft.cacheWrite ?? 0) > 0;
      return { ...draft, missingPrice: !hasPositivePrice };
    });
    setDrafts(priced);
    const items = buildImportItems(priced);
    if (items.some((item) => item.missing_price)) {
      setError(t("models.importMissingPrice"));
      return;
    }
    setBusy(true);
    setError("");
    try {
      const result = await importModels({ dry_run: !apply, items });
      setDryRun(result);
      if (apply) await onApplied();
    } catch (reason) {
      setError(extractApiError(reason, t("models.importApplyFailed")));
    } finally {
      setBusy(false);
    }
  };

  const updateDraft = (modelId: string, patch: Partial<ImportDraft>) => {
    setDrafts((current) => current.map((draft) => draft.modelId === modelId ? { ...draft, ...patch } : draft));
    setDryRun(null);
  };

  if (loading) return <div className="muted">{t("keys.loading")}</div>;
  if (drafts.length === 0) return <div className="muted">{t("models.importEmptyCatalog")}</div>;

  return (
    <div className="model-import-wizard">
      <p className="muted">{t("models.importCatalogHint")}</p>
      {error && <div className="error">{error}</div>}
      <div className="import-rows">
        {drafts.map((draft) => {
          const candidates = groups.get(draft.modelId) ?? [];
          const existingModel = existingByName.get(draft.publicName.toLowerCase()) ?? existingByName.get(draft.modelId.toLowerCase());
          return (
            <div className="import-row" key={draft.modelId} data-testid={`import-row-${draft.modelId}`}>
              <label className="check-row">
                <input type="checkbox" checked={draft.selected} onChange={(event) => updateDraft(draft.modelId, { selected: event.target.checked })} />
                <span className="mono">{draft.modelId}</span>
              </label>
              <label>{t("models.callName")}
                <input className="input" value={draft.publicName} onChange={(event) => updateDraft(draft.modelId, { publicName: event.target.value })} />
              </label>
              {candidates.length > 1 ? (
                <fieldset>
                  <legend>{t("models.importChooseTarget")}</legend>
                  {candidates.map((candidate) => {
                    const key = candidateKey(candidate);
                    return (
                      <label className="check-row" key={key}>
                        <input type="radio" name={`target-${draft.modelId}`} checked={draft.targetKey === key} onChange={() => updateDraft(draft.modelId, { targetKey: key })} />
                        {candidate.provider}{candidate.group ? ` · ${candidate.group}` : ""} / {candidate.model}
                      </label>
                    );
                  })}
                </fieldset>
              ) : (
                <div className="muted">{candidates[0]?.provider}{candidates[0]?.group ? ` · ${candidates[0].group}` : ""} / {draft.modelId}</div>
              )}
              {existingModel && (
                <label className="check-row">
                  <input type="checkbox" checked={draft.overwrite} onChange={(event) => updateDraft(draft.modelId, { overwrite: event.target.checked, selected: true })} />
                  {t("models.importOverwrite", { count: existingModel.ref_count ?? 0, keys: (existingModel.ref_keys ?? []).join(", ") })}
                </label>
              )}
              {draft.selected && (
                <>
                <label>{t("models.dispatch")}<select className="input" value={draft.dispatch} onChange={(event) => updateDraft(draft.modelId, { dispatch: event.target.value as ImportDraft["dispatch"] })}><option value="round-robin">{t("models.roundRobin")}</option><option value="priority">{t("models.priority")}</option></select></label>
                <div className="field-row">
                  <label>{t("models.inputPrice")}<input className="input" type="number" value={draft.input} onChange={(event) => updateDraft(draft.modelId, { input: Number(event.target.value) || 0, missingPrice: false })} /></label>
                  <label>{t("models.outputPrice")}<input className="input" type="number" value={draft.output} onChange={(event) => updateDraft(draft.modelId, { output: Number(event.target.value) || 0, missingPrice: false })} /></label>
                  <label>{t("models.cachePrice")}<input className="input" type="number" value={draft.cacheRead} onChange={(event) => updateDraft(draft.modelId, { cacheRead: Number(event.target.value) || 0 })} /></label>
                  <label>{t("models.cacheWritePrice")}<input className="input" type="number" value={draft.cacheWrite ?? ""} onChange={(event) => updateDraft(draft.modelId, { cacheWrite: event.target.value.trim() === "" ? undefined : Number(event.target.value) || 0 })} /></label>
                </div>
                </>
              )}
              {draft.match && <div className="muted">{t("models.pricingMatch", { model: draft.match.matched_model, provider: draft.match.source_provider_name, type: draft.match.match_type })}</div>}
              {draft.selected && draft.missingPrice && <div className="error">{t("models.importMissingPrice")}</div>}
            </div>
          );
        })}
      </div>
      <div className="card-actions">
        <button type="button" className="btn sm" disabled={busy} onClick={() => void loadPrices()}>{t("models.pricingPreview")}</button>
        <button type="button" className="btn sm" disabled={busy} onClick={() => void run(false)}>{t("models.importPreview")}</button>
        <button type="button" className="btn sm primary" disabled={busy || !dryRun || dryRun.conflicts.length > 0 || dryRun.missing_price.length > 0} onClick={() => void run(true)}>{t("models.importApply")}</button>
      </div>
      {dryRun && <div className="import-results">
        <div className="muted">{t("models.importBatchSummary", { created: dryRun.created.length, updated: dryRun.updated.length, skipped: dryRun.skipped.length, conflicts: dryRun.conflicts.length, missing: dryRun.missing_price.length })}</div>
        {[...dryRun.created, ...dryRun.updated, ...dryRun.skipped, ...dryRun.conflicts, ...dryRun.missing_price].map((row, index) => (
          <div className="muted" key={`${row.name}-${row.action}-${index}`}>{t("models.importResultRow", { name: row.name, action: row.action, reason: row.reason ?? "-", keys: row.affected_keys?.join(", ") || "-" })}</div>
        ))}
      </div>}
    </div>
  );
}
