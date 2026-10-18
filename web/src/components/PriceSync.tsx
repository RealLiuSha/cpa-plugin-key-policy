import { useEffect, useMemo, useRef, useState } from "react";
import { fetchModelPrices, importModelPrices, previewModelPrices, saveModelPrice } from "../api/modelPrices";
import { extractApiError } from "../api/error";
import { basePriceSummary, MAX_MULTIPLIER, validMultiplier } from "./modelPricing";
import { useT } from "../i18n";
import type { ModelPriceRow, PriceImportMatch, PriceImportResult, PricingPreviewMatch } from "../types";

// Prices are synced per model id: the plugin updates every token-billed row with
// that id, whatever its provider qualifier. An id gets a new row, qualified for
// no provider, when it has no row at all, or when it was seen without a price
// and none of its rows is unqualified: only such a row prices those calls
// whichever provider served them.
export interface SyncCandidate {
  // The id shown, previewed and created: the spelling the caller asked for,
  // which for an unpriced model is the one its calls carry.
  name: string;
  rows: ModelPriceRow[];
  create: boolean;
}

export interface SyncRow extends SyncCandidate {
  match: PricingPreviewMatch;
}

const lower = (value: string) => value.toLowerCase();

export function syncCandidates(prices: ModelPriceRow[], wanted: string[], unpriced: boolean): SyncCandidate[] {
  const byName = new Map<string, SyncCandidate>();
  for (const row of prices) {
    if (row.billing_mode !== "tokens") continue;
    const candidate = byName.get(lower(row.name)) ?? { name: row.name, rows: [], create: false };
    candidate.rows.push(row);
    byName.set(lower(row.name), candidate);
  }
  const known = new Set(prices.map((row) => lower(row.name)));
  for (const name of wanted) {
    const candidate = byName.get(lower(name));
    if (candidate) {
      if (unpriced && !candidate.rows.some((row) => row.provider === "")) Object.assign(candidate, { name, create: true });
    } else if (!known.has(lower(name))) {
      byName.set(lower(name), { name, rows: [], create: true });
    }
  }
  return [...byName.values()];
}

export function syncRows(candidates: SyncCandidate[], matches: PricingPreviewMatch[]): { rows: SyncRow[]; unmatched: string[] } {
  const byName = new Map(matches.map((match) => [lower(match.model), match]));
  const rows: SyncRow[] = [];
  const unmatched: string[] = [];
  for (const candidate of candidates) {
    const match = byName.get(lower(candidate.name));
    if (match) rows.push({ ...candidate, match });
    else unmatched.push(candidate.name);
  }
  return { rows, unmatched };
}

function pricesDiffer(row: ModelPriceRow, match: PricingPreviewMatch): boolean {
  return (row.input_price_per_million ?? 0) !== match.prompt_price_per_1m
    || (row.output_price_per_million ?? 0) !== match.completion_price_per_1m
    || (row.cache_read_price_per_million ?? 0) !== match.cache_read_price_per_1m
    || (match.cache_write_price_per_1m !== undefined && row.cache_write_price_per_million !== match.cache_write_price_per_1m);
}

interface Props {
  // Model ids to tick; empty means "every row that is unpriced or whose price changed".
  preselect: string[];
  // Set when preselect lists models seen without a price, so that each gets a
  // row that prices its calls.
  unpriced?: boolean;
  onApplied: () => Promise<void>;
  onDone: () => void;
}

export default function PriceSync({ preselect, unpriced = false, onApplied, onDone }: Props) {
  const t = useT();
  const [rows, setRows] = useState<SyncRow[]>([]);
  const [unmatched, setUnmatched] = useState<string[]>([]);
  const [perCall, setPerCall] = useState<string[]>([]);
  const [edits, setEdits] = useState<Record<string, PriceImportMatch>>({});
  // Multipliers of the rows to create; a new row of an id that already has
  // rows starts from their multiplier.
  const [multipliers, setMultipliers] = useState<Record<string, string>>({});
  const [selected, setSelected] = useState<Set<string>>(new Set());
  const [loading, setLoading] = useState(true);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [result, setResult] = useState<{ prices: PriceImportResult; created: number } | null>(null);
  const selectAll = useRef<HTMLInputElement>(null);
  const wanted = useMemo(() => new Set(preselect.map(lower)), [preselect]);
  const isWanted = (name: string) => wanted.size === 0 || wanted.has(lower(name));

  useEffect(() => {
    let alive = true;
    void (async () => {
      try {
        const prices = (await fetchModelPrices()).models;
        const candidates = syncCandidates(prices, preselect, unpriced);
        const preview = await previewModelPrices(candidates.map((candidate) => candidate.name));
        if (!alive) return;
        const next = syncRows(candidates, preview.matches);
        setRows(next.rows);
        setUnmatched(next.unmatched.filter(isWanted));
        setPerCall([...new Set(prices.filter((row) => row.billing_mode === "per_call" && isWanted(row.name)).map((row) => row.name))]);
        setEdits(Object.fromEntries(next.rows.map((row) => [lower(row.name), {
          model: row.name,
          prompt_price_per_1m: row.match.prompt_price_per_1m,
          completion_price_per_1m: row.match.completion_price_per_1m,
          cache_read_price_per_1m: row.match.cache_read_price_per_1m,
          cache_write_price_per_1m: row.match.cache_write_price_per_1m,
        }])));
        setMultipliers(Object.fromEntries(next.rows.filter((row) => row.create).map((row) => [lower(row.name), String(row.rows[0]?.billing_multiplier ?? 1)])));
        setSelected(new Set(next.rows
          .filter((row) => wanted.size > 0 ? wanted.has(lower(row.name)) : row.rows.some((price) => !price.priced || pricesDiffer(price, row.match)))
          .map((row) => lower(row.name))));
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

  const visibleRows = useMemo(() => rows.filter((row) => wanted.size === 0 || wanted.has(lower(row.name))), [rows, wanted]);
  const count = visibleRows.filter((row) => selected.has(lower(row.name))).length;
  useEffect(() => {
    if (selectAll.current) selectAll.current.indeterminate = count > 0 && count < visibleRows.length;
  }, [count, visibleRows.length]);

  const setPrice = (name: string, field: keyof Omit<PriceImportMatch, "model">, raw: string) => {
    const value = raw.trim() === "" ? undefined : Number(raw);
    setEdits((current) => ({ ...current, [name]: { ...current[name], [field]: value !== undefined && Number.isFinite(value) ? value : field === "cache_write_price_per_1m" ? undefined : 0 } }));
  };
  const toggle = (name: string) => setSelected((current) => {
    const next = new Set(current);
    if (next.has(name)) next.delete(name); else next.add(name);
    return next;
  });
  const toggleAll = () => setSelected(count === visibleRows.length ? new Set() : new Set(visibleRows.map((row) => lower(row.name))));

  // New rows are written whole, with their multiplier; then the prices go to
  // every existing row of the chosen ids.
  const apply = async () => {
    const chosen = visibleRows.filter((row) => selected.has(lower(row.name)));
    const creating = chosen.filter((row) => row.create);
    if (creating.some((row) => !validMultiplier(multipliers[lower(row.name)] ?? ""))) {
      setError(t("quota.invalidMultiplier"));
      return;
    }
    setBusy(true);
    setError("");
    let created = 0;
    try {
      for (const row of creating) {
        const edit = edits[lower(row.name)];
        await saveModelPrice({
          name: row.name,
          provider: "",
          billing_mode: "tokens",
          billing_multiplier: Number(multipliers[lower(row.name)]),
          input_price_per_million: edit.prompt_price_per_1m ?? 0,
          output_price_per_million: edit.completion_price_per_1m ?? 0,
          cache_read_price_per_million: edit.cache_read_price_per_1m ?? 0,
          cache_write_price_per_million: edit.cache_write_price_per_1m,
        });
        created += 1;
      }
      const updates = chosen.filter((row) => row.rows.length > 0).map((row) => edits[lower(row.name)]);
      const prices = updates.length > 0 ? await importModelPrices({ dry_run: false, matches: updates }) : { applied: [], unchanged: [], skipped: [] };
      setResult({ prices, created });
      await onApplied();
    } catch (reason) {
      setError(extractApiError(reason, t("models.importInvalid")));
      if (created > 0) await onApplied();
    } finally {
      setBusy(false);
    }
  };

  if (loading) return <div className="muted">{t("models.pricingLoading")}</div>;
  if (result) {
    return (
      <div className="model-import">
        <p className="success">{t("models.importSummary", { applied: result.prices.applied.length, unchanged: result.prices.unchanged.length })}</p>
        {result.created > 0 && <p className="muted">{t("models.syncCreated", { count: result.created })}</p>}
        <div className="form-actions modal-actions"><button type="button" className="btn primary" onClick={onDone}>{t("quota.done")}</button></div>
      </div>
    );
  }
  return (
    <div className="model-import">
      <p className="muted">{t("models.syncHint")}</p>
      {error && <div className="error" role="alert">{error}</div>}
      {visibleRows.length > 1 && (
        <label className="check-row sync-select-all">
          <input ref={selectAll} type="checkbox" checked={count === visibleRows.length} onChange={toggleAll} />
          {t("models.selectAll", { selected: count, total: visibleRows.length })}
        </label>
      )}
      {visibleRows.length > 0 && <div className="choice-list sync-list">
        {visibleRows.map((row) => {
          const key = lower(row.name);
          const edit = edits[key];
          return (
            <div className={"sync-item" + (selected.has(key) ? " active" : "")} key={key}>
              <label className="check-row">
                <input type="checkbox" checked={selected.has(key)} onChange={() => toggle(key)} />
                <strong className="mono">{row.name}</strong>
                <span className="muted">{t("models.pricingMatch", { model: row.match.matched_model, provider: row.match.source_provider_name })}</span>
              </label>
              {row.create && <small className="muted">{row.rows.length === 0
                ? t("models.syncCreatesRow")
                : t("models.syncCreatesUnqualified", { providers: row.rows.map((price) => price.provider).join("、") })}</small>}
              {row.rows.map((price) => (
                <small className="muted" key={lower(price.provider)}>{t("models.currentPrice")} · {price.provider || t("models.anyProvider")}：{basePriceSummary(price, t)}</small>
              ))}
              <div className="field-row">
                <label>{t("models.inputPrice")}<input className="input" type="number" min="0" step="any" value={edit.prompt_price_per_1m ?? 0} onChange={(event) => setPrice(key, "prompt_price_per_1m", event.target.value)} /></label>
                <label>{t("models.outputPrice")}<input className="input" type="number" min="0" step="any" value={edit.completion_price_per_1m ?? 0} onChange={(event) => setPrice(key, "completion_price_per_1m", event.target.value)} /></label>
                <label>{t("models.cachePrice")}<input className="input" type="number" min="0" step="any" value={edit.cache_read_price_per_1m ?? 0} onChange={(event) => setPrice(key, "cache_read_price_per_1m", event.target.value)} /></label>
                <label>{t("models.cacheWritePrice")}<input className="input" type="number" min="0" step="any" placeholder={t("models.sameAsInputPlaceholder")} value={edit.cache_write_price_per_1m ?? ""} onChange={(event) => setPrice(key, "cache_write_price_per_1m", event.target.value)} /></label>
                {row.create && <label>{t("models.createMultiplier")}<input className="input" type="number" min="1" max={MAX_MULTIPLIER} step="any" value={multipliers[key] ?? ""} onChange={(event) => setMultipliers((current) => ({ ...current, [key]: event.target.value }))} /></label>}
              </div>
            </div>
          );
        })}
      </div>}
      {visibleRows.length === 0 && unmatched.length === 0 && perCall.length === 0 && !error && <div className="muted">{t("models.syncNothing")}</div>}
      {unmatched.length > 0 && <p className="notice">{t("models.pricingUnmatched", { models: unmatched.join("、") })}</p>}
      {perCall.length > 0 && <p className="muted">{t("models.syncPerCallNote", { models: perCall.join("、") })}</p>}
      <div className="form-actions modal-actions">
        <button type="button" className="btn" onClick={onDone}>{t("keyForm.cancel")}</button>
        <button type="button" className="btn primary" disabled={busy || count === 0} onClick={() => void apply()}>{busy ? t("keyForm.submitting") : t("models.applyCount", { count })}</button>
      </div>
    </div>
  );
}
