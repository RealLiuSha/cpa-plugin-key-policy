import { useEffect, useMemo, useState } from "react";
import { fetchCatalog } from "../api/models";
import { importModels } from "../api/modelPrices";
import { extractApiError } from "../api/error";
import { useT } from "../i18n";
import type { ModelImportResult, ModelPriceRow } from "../types";

export interface ImportCandidate {
  model: string;
  providers: string[];
}

// One row per upstream model id, listing the CPA providers that serve it.
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
  existing: ModelPriceRow[];
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
  // Any price row for the id, with or without a provider qualifier, counts as priced here.
  const taken = useMemo(() => {
    const names = new Set(existing.map((row) => row.name.toLowerCase()));
    return (row: ImportCandidate) => names.has(row.model.toLowerCase());
  }, [existing]);

  useEffect(() => {
    let alive = true;
    void fetchCatalog(new Set(existing.map((row) => row.provider).filter(Boolean)))
      .then((catalog) => { if (alive) setCandidates(importCandidates(catalog)); })
      .catch((reason) => { if (alive) setError(extractApiError(reason, t("models.importCatalogFailed"))); })
      .finally(() => { if (alive) setLoading(false); });
    return () => { alive = false; };
    // The catalog is read once per dialog; existing rows only shape the filter.
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
      const imported = await importModels([...selected].map((model) => ({ name: model, provider: providerOf[model] ?? "" })));
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
                  {exists ? <span className="badge">{row.providers.join(" / ")}</span> : (
                    <select className="input import-provider" aria-label={t("models.providerQualifier")} value={providerOf[row.model] ?? ""} onChange={(event) => setProviderOf((current) => ({ ...current, [row.model]: event.target.value }))}>
                      <option value="">{t("models.anyProvider")}</option>
                      {row.providers.map((provider) => <option key={provider} value={provider}>{t("models.onlyProvider", { provider })}</option>)}
                    </select>
                  )}
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
