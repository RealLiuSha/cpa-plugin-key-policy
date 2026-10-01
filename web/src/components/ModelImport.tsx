import { useEffect, useMemo, useState } from "react";
import { fetchCatalog } from "../api/models";
import { importModels } from "../api/modelDefinitions";
import { extractApiError } from "../api/error";
import { useT } from "../i18n";
import type { ModelDefinition, ModelImportResult } from "../types";

export interface ImportCandidate {
  model: string;
  providers: string[];
}

// One row per upstream model id; a model offered by several providers lets
// the operator choose which provider the public model routes to.
export function importCandidates(catalog: { provider: string; model: string }[]): ImportCandidate[] {
  const byModel = new Map<string, ImportCandidate>();
  for (const row of catalog) {
    const candidate = byModel.get(row.model) ?? { model: row.model, providers: [] };
    if (!candidate.providers.includes(row.provider)) candidate.providers.push(row.provider);
    byModel.set(row.model, candidate);
  }
  return [...byModel.values()].sort((a, b) => a.model.toLowerCase().localeCompare(b.model.toLowerCase()));
}

interface Props {
  existing: ModelDefinition[];
  onImported: () => Promise<void>;
  onSyncPrices: (names: string[]) => void;
  onDone: () => void;
}

export default function ModelImport({ existing, onImported, onSyncPrices, onDone }: Props) {
  const t = useT();
  const [candidates, setCandidates] = useState<ImportCandidate[]>([]);
  const [providerOf, setProviderOf] = useState<Record<string, string>>({});
  const [selected, setSelected] = useState<Set<string>>(new Set());
  const [query, setQuery] = useState("");
  const [loading, setLoading] = useState(true);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [result, setResult] = useState<ModelImportResult | null>(null);
  // A model is already defined when its name is taken or an existing public
  // model already routes to it.
  const taken = useMemo(() => {
    const names = new Set(existing.map((model) => model.name.toLowerCase()));
    const upstreams = new Set(existing.map((model) => `${model.provider.toLowerCase()}|${model.target_model}`));
    return (row: ImportCandidate) => names.has(row.model.toLowerCase()) || row.providers.some((provider) => upstreams.has(`${provider}|${row.model}`));
  }, [existing]);

  useEffect(() => {
    let alive = true;
    void fetchCatalog(new Set(existing.map((model) => model.provider)))
      .then((catalog) => {
        if (!alive) return;
        const rows = importCandidates(catalog);
        setCandidates(rows);
        setProviderOf(Object.fromEntries(rows.map((row) => [row.model, row.providers[0]])));
      })
      .catch((reason) => { if (alive) setError(extractApiError(reason, t("models.importCatalogFailed"))); })
      .finally(() => { if (alive) setLoading(false); });
    return () => { alive = false; };
    // The catalog is read once per dialog; existing models only shape the filter.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  const visible = useMemo(() => {
    const needle = query.trim().toLowerCase();
    return needle ? candidates.filter((row) => row.model.toLowerCase().includes(needle) || row.providers.some((provider) => provider.includes(needle))) : candidates;
  }, [candidates, query]);
  const importable = (row: ImportCandidate) => !taken(row);
  const visibleImportable = visible.filter(importable);

  const toggle = (model: string) => setSelected((current) => {
    const next = new Set(current);
    if (next.has(model)) next.delete(model); else next.add(model);
    return next;
  });
  const selectVisible = () => setSelected((current) => new Set([...current, ...visibleImportable.map((row) => row.model)]));
  const clearSelection = () => setSelected(new Set());

  const submit = async () => {
    setBusy(true);
    setError("");
    try {
      const imported = await importModels([...selected].map((model) => ({ provider: providerOf[model], target_model: model })));
      setResult(imported);
      await onImported();
    } catch (reason) {
      setError(extractApiError(reason, t("models.importApplyFailed")));
    } finally {
      setBusy(false);
    }
  };

  if (result) {
    const created = result.created.map((row) => row.name);
    return (
      <div className="model-import">
        <p className="success">{t("models.importDone", { count: created.length })}</p>
        {result.skipped.length > 0 && <p className="muted">{t("models.importSkipped", { names: result.skipped.map((row) => row.name).join("、") })}</p>}
        <div className="form-actions modal-actions">
          <button type="button" className="btn" onClick={onDone}>{t("models.later")}</button>
          {created.length > 0 && <button type="button" className="btn primary" onClick={() => onSyncPrices(created)}>{t("models.syncImported", { count: created.length })}</button>}
        </div>
      </div>
    );
  }

  return (
    <div className="model-import">
      <p className="muted">{t("models.importCatalogHint")}</p>
      {error && <div className="error" role="alert">{error}</div>}
      {loading ? <div className="muted">{t("picker.loading")}</div> : candidates.length === 0 ? (
        <div className="muted">{t("models.importEmptyCatalog")}</div>
      ) : (
        <>
          <div className="import-toolbar">
            <input className="input" aria-label={t("picker.searchPlaceholder")} placeholder={t("picker.searchPlaceholder")} value={query} onChange={(event) => setQuery(event.target.value)} />
            <button type="button" className="btn sm" disabled={visibleImportable.every((row) => selected.has(row.model))} onClick={selectVisible}>{t("picker.selectAll")}</button>
            <button type="button" className="btn sm" disabled={selected.size === 0} onClick={clearSelection}>{t("picker.clearAll")}</button>
          </div>
          <div className="choice-list import-list">
            {visible.length === 0 ? <div className="empty-state">{t("picker.noMatch")}</div> : visible.map((row) => {
              const exists = !importable(row);
              return (
                <div className={"import-item" + (selected.has(row.model) ? " active" : "") + (exists ? " disabled" : "")} key={row.model}>
                  <label className="check-row">
                    <input type="checkbox" disabled={exists} checked={selected.has(row.model)} onChange={() => toggle(row.model)} />
                    <span className="mono">{row.model}</span>
                  </label>
                  {row.providers.length > 1 && !exists ? (
                    <select className="input import-provider" aria-label={t("models.provider")} value={providerOf[row.model]} onChange={(event) => setProviderOf((current) => ({ ...current, [row.model]: event.target.value }))}>
                      {row.providers.map((provider) => <option key={provider} value={provider}>{provider}</option>)}
                    </select>
                  ) : <span className="badge">{row.providers.join(" / ")}</span>}
                  {exists && <span className="muted import-state">{t("models.alreadyDefined")}</span>}
                </div>
              );
            })}
          </div>
          <div className="form-actions modal-actions">
            <span className="muted">{t("models.importSelected", { count: selected.size })}</span>
            <span className="form-actions-spacer" />
            <button type="button" className="btn" onClick={onDone}>{t("keyForm.cancel")}</button>
            <button type="button" className="btn primary" disabled={busy || selected.size === 0} onClick={() => void submit()}>{busy ? t("keyForm.submitting") : t("models.importCount", { count: selected.size })}</button>
          </div>
        </>
      )}
    </div>
  );
}
