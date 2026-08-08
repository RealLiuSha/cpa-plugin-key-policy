import { useEffect, useState, useCallback, useMemo } from "react";
import { useNavigate, useParams, useLocation } from "react-router-dom";
import { useT } from "../i18n";
import type {
  AliasMapping,
  AliasTarget,
  ClassifyRule,
  ClassifyPreviewResponse,
  CredentialDescriptor,
  PriceImportMatch,
  PriceImportResult,
} from "../types";
import {
  fetchAliases,
  upsertAlias,
  deleteAlias,
  fetchClassifyRules,
  upsertClassifyRule,
  deleteClassifyRule,
  reorderClassifyRules,
  classifyPreview,
  fetchCredentialDescriptors,
  importAliasPrices,
} from "../api/mappings";
import { getPriceTable, lookupPrice, type PriceTable } from "../store/modelPrices";

export default function Mapping() {
  const t = useT();
  const loc = useLocation();
  const [tab, setTab] = useState<"alias" | "classify">("alias");

  // Pick up returned state (new targets from ModelPick, etc.)
  useEffect(() => {
    if (loc.state?.mappingTab) setTab(loc.state.mappingTab);
  }, [loc.state]);

  return (
    <div className="map-page">
      <div className="map-page-head">
        <h1>{t("mapping.title")}</h1>
      </div>
      <div className="map-tabs">
        <button className={"map-tab" + (tab === "alias" ? " active" : "")} onClick={() => setTab("alias")}>
          {t("mapping.aliasTab")}
        </button>
        <button className={"map-tab" + (tab === "classify" ? " active" : "")} onClick={() => setTab("classify")}>
          {t("mapping.classifyTab")}
        </button>
      </div>
      {tab === "alias" ? <AliasListTab /> : <ClassifyTab />}
    </div>
  );
}

// --- helpers ---

/** tokens (default) with all three prices at 0 → unpriced. */
export function isUnpricedAlias(a: AliasMapping): boolean {
  if (a.billing_mode === "per_call") return false;
  return (a.input_price_per_million ?? 0) === 0
    && (a.output_price_per_million ?? 0) === 0
    && (a.cache_read_price_per_million ?? 0) === 0;
}

/** 1:1 pass-through: single target whose target_model equals the alias name. */
export function isPassThroughAlias(a: AliasMapping): boolean {
  if (!a.targets || a.targets.length !== 1) return false;
  return a.alias.toLowerCase() === (a.targets[0].target_model ?? "").toLowerCase();
}

// --- Alias List Tab ---

function AliasListTab() {
  const t = useT();
  const nav = useNavigate();
  const [aliases, setAliases] = useState<AliasMapping[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState("");
  const [filter, setFilter] = useState<"all" | "unpriced" | "orphan">("all");
  // Pass-through 1:1 aliases default expanded so the list is immediately scannable.
  const [passThroughOpen, setPassThroughOpen] = useState(true);
  const [importOpen, setImportOpen] = useState(false);

  const load = useCallback(async () => {
    setLoading(true);
    try {
      const list = await fetchAliases();
      setAliases(list);
    } catch (e: unknown) {
      setError(String(e));
    } finally {
      setLoading(false);
    }
  }, []);

  useEffect(() => { void load(); }, [load]);

  const handleDelete = async (aliasName: string) => {
    try {
      await deleteAlias(aliasName);
      await load();
    } catch (e: unknown) {
      setError(String(e));
    }
  };

  const filtered = useMemo(() => {
    return aliases.filter((a) => {
      const ref = a.ref_count ?? 0;
      if (filter === "unpriced") return isUnpricedAlias(a);
      if (filter === "orphan") return ref === 0;
      return true;
    });
  }, [aliases, filter]);

  const manual = filtered.filter((a) => !isPassThroughAlias(a));
  const passThrough = filtered.filter((a) => isPassThroughAlias(a));

  return (
    <>
      <div className="map-toolbar">
        <div className="map-toolbar-left">
          <div className="map-filter-seg" role="group">
            <button type="button" className={"map-filter-btn" + (filter === "all" ? " active" : "")} onClick={() => setFilter("all")}>
              {t("mapping.filterAll")}
            </button>
            <button type="button" className={"map-filter-btn" + (filter === "unpriced" ? " active" : "")} onClick={() => setFilter("unpriced")}>
              {t("mapping.filterUnpriced")}
            </button>
            <button type="button" className={"map-filter-btn" + (filter === "orphan" ? " active" : "")} onClick={() => setFilter("orphan")}>
              {t("mapping.filterOrphan")}
            </button>
          </div>
        </div>
        <div className="map-toolbar-right">
          <button className="btn" type="button" onClick={() => setImportOpen(true)}>
            {t("mapping.importPrices")}
          </button>
          <button className="btn primary" onClick={() => nav("/mapping/alias/new")}>
            + {t("mapping.newAlias")}
          </button>
        </div>
      </div>
      {error && <div className="error">{error}</div>}
      {loading ? (
        <div className="muted" style={{ padding: 20 }}>{t("keys.loading") || "Loading..."}</div>
      ) : aliases.length === 0 ? (
        <div className="muted" style={{ padding: 20 }}>No aliases</div>
      ) : (
        <>
          {manual.length > 0 && (
            <div className="alias-grid">
              {manual.map((a) => (
                <AliasCard
                  key={a.alias}
                  alias={a}
                  onDelete={handleDelete}
                  onEdit={(name) => nav(`/mapping/alias/${encodeURIComponent(name)}`)}
                />
              ))}
            </div>
          )}
          {passThrough.length > 0 && (
            <div className="alias-passthrough-section">
              <button
                type="button"
                className="alias-passthrough-toggle"
                onClick={() => setPassThroughOpen((v) => !v)}
                aria-expanded={passThroughOpen}
                data-testid="passthrough-toggle"
              >
                <span>{passThroughOpen ? "▾" : "▸"}</span>
                {t("mapping.passThroughFold", { n: passThrough.length })}
              </button>
              {passThroughOpen && (
                <div className="alias-grid">
                  {passThrough.map((a) => (
                    <AliasCard
                      key={a.alias}
                      alias={a}
                      onDelete={handleDelete}
                      onEdit={(name) => nav(`/mapping/alias/${encodeURIComponent(name)}`)}
                    />
                  ))}
                </div>
              )}
            </div>
          )}
          {manual.length === 0 && passThrough.length === 0 && (
            <div className="muted" style={{ padding: 20 }}>{t("mapping.filterEmpty")}</div>
          )}
        </>
      )}
      {importOpen && (
        <ImportPricesModal
          onClose={() => setImportOpen(false)}
          onApplied={async () => {
            await load();
          }}
        />
      )}
    </>
  );
}

function AliasCard({ alias, onDelete, onEdit }: { alias: AliasMapping; onDelete: (n: string) => void; onEdit: (n: string) => void }) {
  const t = useT();
  const refCount = alias.ref_count ?? 0;
  const unpriced = isUnpricedAlias(alias);
  const orphan = refCount === 0;
  return (
    <div className="alias-card" data-testid={`alias-card-${alias.alias}`}>
      <div className="alias-card-head">
        <span className="alias-card-name">{alias.alias}</span>
        <span className="alias-dispatch-badge">
          {alias.dispatch === "priority" ? t("mapping.alias.priority") : t("mapping.alias.roundRobin")}
        </span>
      </div>
      <div className="alias-badges">
        {unpriced && <span className="alias-badge unpriced" data-testid="badge-unpriced">{t("mapping.badgeUnpriced")}</span>}
        {orphan && <span className="alias-badge orphan" data-testid="badge-orphan">{t("mapping.badgeOrphan")}</span>}
      </div>
      <div className="alias-targets">
        {alias.targets.slice(0, 3).map((tgt, i) => (
          <div key={i} className="alias-target-row">
            <span>{tgt.provider} · {tgt.target_model}</span>
            {tgt.group && <span className="alias-target-group">{tgt.group}</span>}
          </div>
        ))}
        {alias.targets.length > 3 && (
          <div className="alias-target-row" style={{ opacity: 0.6 }}>
            {t("mapping.moreTargets", { n: alias.targets.length - 3 })}
          </div>
        )}
      </div>
      <div className="alias-pricing">
        {alias.billing_mode === "per_call" ? (
          <>{t("mapping.alias.perCallUnit")} ${alias.per_call_usd ?? 0}/{t("mapping.alias.perCallUnit")}</>
        ) : (
          <>{t("mapping.alias.input")} ${alias.input_price_per_million ?? 0} / {t("mapping.alias.output")} ${alias.output_price_per_million ?? 0} / {t("mapping.alias.cache")} ${alias.cache_read_price_per_million ?? 0} {t("mapping.alias.perMillion")}</>
        )}
      </div>
      <div className={"alias-refs" + (orphan ? " zero" : "")} data-testid="alias-refs">
        {refCount > 0 ? t("mapping.refs", { n: refCount }) : t("mapping.unreferenced")}
      </div>
      <div className="alias-actions">
        <button className="btn sm" onClick={() => onEdit(alias.alias)}>{t("mapping.edit")}</button>
        <button
          className="btn sm danger-outline"
          disabled={refCount > 0}
          title={refCount > 0 ? t("mapping.deleteBlocked", { n: refCount }) : ""}
          data-testid={`delete-${alias.alias}`}
          onClick={() => onDelete(alias.alias)}
        >
          {t("mapping.delete")}
        </button>
      </div>
    </div>
  );
}

// --- Import prices modal ---

/** Parse optional numeric field: missing/null → omit (do not coerce to 0). */
function optImportPrice(o: Record<string, unknown>, key: string): number | undefined {
  if (!(key in o) || o[key] === null || o[key] === undefined || o[key] === "") return undefined;
  const n = Number(o[key]);
  return Number.isFinite(n) ? n : undefined;
}

function parseImportPayload(raw: string): PriceImportMatch[] {
  const parsed = JSON.parse(raw) as unknown;
  if (!parsed || typeof parsed !== "object") throw new Error("invalid JSON");
  const root = parsed as Record<string, unknown>;
  const matches = root.matches;
  if (!Array.isArray(matches)) throw new Error("missing matches array");
  const out: PriceImportMatch[] = [];
  for (const item of matches) {
    if (!item || typeof item !== "object") continue;
    const o = item as Record<string, unknown>;
    const model = typeof o.model === "string" ? o.model.trim() : "";
    if (!model) continue;
    const row: PriceImportMatch = { model };
    const prompt = optImportPrice(o, "prompt_price_per_1m");
    const completion = optImportPrice(o, "completion_price_per_1m");
    const cacheRead = optImportPrice(o, "cache_read_price_per_1m");
    const cacheWrite = optImportPrice(o, "cache_write_price_per_1m");
    if (prompt !== undefined) row.prompt_price_per_1m = prompt;
    if (completion !== undefined) row.completion_price_per_1m = completion;
    if (cacheRead !== undefined) row.cache_read_price_per_1m = cacheRead;
    // Accepted for wire compat; server ignores.
    if (cacheWrite !== undefined) row.cache_write_price_per_1m = cacheWrite;
    // Need at least one token price field to import.
    if (prompt === undefined && completion === undefined && cacheRead === undefined) continue;
    out.push(row);
  }
  if (out.length === 0) throw new Error("no valid matches");
  return out;
}

function ImportPricesModal({ onClose, onApplied }: { onClose: () => void; onApplied: () => void | Promise<void> }) {
  const t = useT();
  const [text, setText] = useState("");
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);
  const [preview, setPreview] = useState<PriceImportResult | null>(null);
  const [result, setResult] = useState<PriceImportResult | null>(null);

  const runDryRun = async () => {
    setError("");
    setResult(null);
    setBusy(true);
    try {
      const matches = parseImportPayload(text);
      const res = await importAliasPrices({ dry_run: true, matches });
      setPreview(res);
    } catch (e: unknown) {
      setPreview(null);
      setError(String(e));
    } finally {
      setBusy(false);
    }
  };

  const runApply = async () => {
    if (!preview) return;
    setError("");
    setBusy(true);
    try {
      const matches = parseImportPayload(text);
      const res = await importAliasPrices({ dry_run: false, matches });
      setResult(res);
      setPreview(null);
      // Refresh list in parent but keep modal open so the user sees the summary.
      await onApplied();
    } catch (e: unknown) {
      setError(String(e));
    } finally {
      setBusy(false);
    }
  };

  const onFile = async (file: File | null) => {
    if (!file) return;
    try {
      const raw = await file.text();
      setText(raw);
      setPreview(null);
      setResult(null);
    } catch (e: unknown) {
      setError(String(e));
    }
  };

  return (
    <div className="map-modal-backdrop" data-testid="import-prices-modal">
      <div className="map-modal">
        <div className="map-modal-head">
          <h2>{t("mapping.importTitle")}</h2>
          <button type="button" className="btn sm" onClick={onClose}>{t("mapping.cancel")}</button>
        </div>
        <p className="muted">{t("mapping.importHint")}</p>
        <textarea
          className="map-import-textarea mono"
          data-testid="import-json"
          rows={8}
          value={text}
          onChange={(e) => { setText(e.target.value); setPreview(null); setResult(null); }}
          placeholder='{"matches":[{"model":"gpt-4o","prompt_price_per_1m":5,"completion_price_per_1m":30,"cache_read_price_per_1m":1.25}]}'
        />
        <div className="map-import-actions">
          <input
            type="file"
            accept="application/json,.json"
            data-testid="import-file"
            onChange={(e) => void onFile(e.target.files?.[0] ?? null)}
          />
          <button type="button" className="btn" disabled={busy || !text.trim()} onClick={() => void runDryRun()} data-testid="import-preview">
            {t("mapping.importPreview")}
          </button>
          {preview && (
            <button type="button" className="btn primary" disabled={busy} onClick={() => void runApply()} data-testid="import-apply">
              {t("mapping.importApply")}
            </button>
          )}
        </div>
        {error && <div className="error">{error}</div>}
        {preview && <ImportResultView title={t("mapping.importPreviewTitle")} result={preview} />}
        {result && <ImportResultView title={t("mapping.importResultTitle")} result={result} />}
      </div>
    </div>
  );
}

function ImportResultView({ title, result }: { title: string; result: PriceImportResult }) {
  const t = useT();
  return (
    <div className="map-import-result" data-testid="import-result">
      <h3>{title}</h3>
      <div className="muted">
        {t("mapping.importSummary", {
          applied: result.applied?.length ?? 0,
          unchanged: result.unchanged?.length ?? 0,
          skipped: result.skipped?.length ?? 0,
          keys: result.affected_keys?.length ?? 0,
        })}
      </div>
      {(result.applied?.length ?? 0) > 0 && (
        <table className="map-import-table">
          <thead>
            <tr>
              <th>{t("mapping.importColAlias")}</th>
              <th>{t("mapping.importColOld")}</th>
              <th>{t("mapping.importColNew")}</th>
              <th>{t("mapping.importColNote")}</th>
            </tr>
          </thead>
          <tbody>
            {result.applied.map((row) => (
              <tr key={row.alias}>
                <td className="mono">{row.alias}</td>
                <td className="mono">{row.old_input_price_per_million}/{row.old_output_price_per_million}/{row.old_cache_read_price_per_million}</td>
                <td className="mono">{row.new_input_price_per_million}/{row.new_output_price_per_million}/{row.new_cache_read_price_per_million}</td>
                <td>{row.note ?? ""}</td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
      {(result.skipped?.length ?? 0) > 0 && (
        <ul className="map-import-skipped">
          {result.skipped.map((s, i) => (
            <li key={i}>{s.alias || s.model}: {s.reason}</li>
          ))}
        </ul>
      )}
    </div>
  );
}

// --- Classify Tab ---

function ClassifyTab() {
  const t = useT();
  const nav = useNavigate();
  const [rules, setRules] = useState<ClassifyRule[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState("");
  const [previewData, setPreviewData] = useState<ClassifyPreviewResponse | null>(null);

  const load = useCallback(async () => {
    setLoading(true);
    try {
      const [list, descriptors] = await Promise.all([
        fetchClassifyRules(),
        fetchCredentialDescriptors().catch(() => [] as CredentialDescriptor[]),
      ]);
      setRules(list);
      const preview = await classifyPreview(descriptors).catch(() => null as ClassifyPreviewResponse | null);
      setPreviewData(preview);
    } catch (e: unknown) {
      setError(String(e));
    } finally {
      setLoading(false);
    }
  }, []);

  useEffect(() => { void load(); }, [load]);

  const handleReorder = async (names: string[]) => {
    try {
      await reorderClassifyRules(names);
      await load();
    } catch (e: unknown) {
      setError(String(e));
    }
  };

  const handleDelete = async (name: string) => {
    try {
      await deleteClassifyRule(name);
      await load();
    } catch (e: unknown) {
      setError(String(e));
    }
  };

  const moveRule = (idx: number, dir: -1 | 1) => {
    const newOrder = [...rules];
    const target = idx + dir;
    if (target < 0 || target >= newOrder.length) return;
    [newOrder[idx], newOrder[target]] = [newOrder[target], newOrder[idx]];
    void handleReorder(newOrder.map((r) => r.name));
  };

  return (
    <>
      <div className="map-toolbar">
        <button className="btn primary" onClick={() => nav("/mapping/rule/new")}>
          + {t("mapping.newRule")}
        </button>
      </div>
      {error && <div className="error">{error}</div>}
      {loading ? (
        <div className="muted" style={{ padding: 20 }}>Loading...</div>
      ) : (
        <div className="rule-list">
          <div className="rule-builtin-card">
            <h3>{t("mapping.rule.builtin")} ({t("mapping.rule.builtinReadOnly")})</h3>
            <div className="rule-builtin-row">
              <span className="info-icon">ⓘ</span>
              plan_type → {`<detected>`}
            </div>
            <div className="rule-builtin-row">
              <span className="info-icon">ⓘ</span>
              tier → {`<detected>`}
            </div>
            <div className="rule-builtin-desc">{t("mapping.rule.builtinDesc")}</div>
          </div>

          <div className="section-label" style={{ marginTop: 16 }}>{t("mapping.rule.custom")}</div>
          {rules.length === 0 ? (
            <div className="muted" style={{ padding: 20 }}>No custom rules</div>
          ) : (
            rules.map((rule, idx) => (
              <RuleCard
                key={rule.name}
                rule={rule}
                idx={idx}
                total={rules.length}
                onMoveUp={() => moveRule(idx, -1)}
                onMoveDown={() => moveRule(idx, 1)}
                onEdit={() => nav(`/mapping/rule/${encodeURIComponent(rule.name)}`)}
                onDelete={() => handleDelete(rule.name)}
                previewData={previewData}
              />
            ))
          )}
        </div>
      )}
    </>
  );
}

function RuleCard({
  rule, idx, total, onMoveUp, onMoveDown, onEdit, onDelete, previewData,
}: {
  rule: ClassifyRule;
  idx: number;
  total: number;
  onMoveUp: () => void;
  onMoveDown: () => void;
  onEdit: () => void;
  onDelete: () => void;
  previewData: ClassifyPreviewResponse | null;
}) {
  const t = useT();
  const [expanded, setExpanded] = useState(false);
  const [matchCount, setMatchCount] = useState<number | null>(null);
  const [matchedFiles, setMatchedFiles] = useState<string[]>([]);
  const [page, setPage] = useState(0);
  const pageSize = 50;

  useEffect(() => {
    const files = previewData?.groups[rule.group.toLowerCase()] ?? [];
    setMatchCount(files.length);
    setMatchedFiles(files);
  }, [previewData, rule.group]);

  const pageCount = Math.ceil(matchedFiles.length / pageSize);
  const pageFiles = matchedFiles.slice(page * pageSize, (page + 1) * pageSize);

  return (
    <div className="rule-card">
      <div className="rule-card-head">
        <div className="rule-card-order">
          <button onClick={onMoveUp} disabled={idx === 0} title={t("mapping.rule.moveUp")}>↑</button>
          <button onClick={onMoveDown} disabled={idx === total - 1} title={t("mapping.rule.moveDown")}>↓</button>
        </div>
        <div className="rule-card-main" onClick={() => setExpanded(!expanded)} style={{ cursor: "pointer" }}>
          <div className="rule-card-name">{rule.name}</div>
          <div className="rule-card-sub">{rule.field}: {rule.pattern} → {rule.group}</div>
        </div>
        <div className="rule-card-right">
          <span className={"rule-match-badge" + (matchCount === 0 ? " zero" : "")}>
            {matchCount !== null
              ? (matchCount > 0 ? t("mapping.rule.matchCount", { n: matchCount }) : t("mapping.rule.matchCountZero"))
              : "..."}
          </span>
          <label className="switch" title={t("mapping.rule.enabled")}>
            <input type="checkbox" checked={rule.enabled} readOnly />
            <span className="track"><span className="thumb" /></span>
          </label>
          <button className="btn sm" onClick={onEdit}>{t("mapping.edit")}</button>
          <button className="btn sm danger-outline" onClick={onDelete}>{t("mapping.delete")}</button>
        </div>
      </div>
      {expanded && (
        <div className="rule-detail">
          <div className="rule-detail-files">
            {pageFiles.length === 0 ? (
              <div className="muted" style={{ padding: 8 }}>{t("mapping.rule.noFiles")}</div>
            ) : (
              pageFiles.map((f, i) => (
                <div key={i} className="rule-detail-file">
                  <span>{f}</span>
                </div>
              ))
            )}
          </div>
          {pageCount > 1 && (
            <div className="rule-pager">
              <button onClick={() => setPage(Math.max(0, page - 1))} disabled={page === 0}>
                {t("mapping.rule.prevPage")}
              </button>
              <span className="page-info">{t("mapping.rule.pageInfo", { cur: page + 1, total: pageCount })}</span>
              <button onClick={() => setPage(Math.min(pageCount - 1, page + 1))} disabled={page >= pageCount - 1}>
                {t("mapping.rule.nextPage")}
              </button>
            </div>
          )}
        </div>
      )}
    </div>
  );
}

// --- Alias Edit Form ---

// Survives ModelPick remounts (and React Strict Mode) better than router
// state alone — without this, filling the alias name then picking targets
// wipes the name when AliasEditForm remounts empty.
const ALIAS_FORM_DRAFT_KEY = "cpa-key-policy:alias-form-draft";
const ALIAS_FORM_FROM_PICKER_KEY = "cpa-key-policy:alias-form-from-picker";

function readAliasFormDraft(): AliasMapping | null {
  try {
    const raw = sessionStorage.getItem(ALIAS_FORM_DRAFT_KEY);
    if (!raw) return null;
    return JSON.parse(raw) as AliasMapping;
  } catch {
    return null;
  }
}

function writeAliasFormDraft(draft: AliasMapping) {
  try {
    sessionStorage.setItem(ALIAS_FORM_DRAFT_KEY, JSON.stringify(draft));
  } catch {
    /* private mode / quota — router state is the fallback */
  }
}

function clearAliasFormDraft() {
  try {
    sessionStorage.removeItem(ALIAS_FORM_DRAFT_KEY);
    sessionStorage.removeItem(ALIAS_FORM_FROM_PICKER_KEY);
  } catch {
    /* ignore */
  }
}

/** Scope for the fromPicker mark: "new" or the alias being edited. */
function pickerScope(isNew: boolean, routeAliasName: string | undefined, formAlias?: string): string {
  if (isNew) return "new";
  const fromRoute = (routeAliasName ?? "").trim();
  if (fromRoute && fromRoute !== "new") {
    try {
      return decodeURIComponent(fromRoute);
    } catch {
      return fromRoute;
    }
  }
  return (formAlias ?? "").trim() || "new";
}

function markFromPicker(scope: string) {
  try {
    sessionStorage.setItem(ALIAS_FORM_FROM_PICKER_KEY, scope);
  } catch {
    /* ignore */
  }
}

/** True only when the stored mark matches this form's scope (not a global bool). */
function peekFromPicker(scope: string): boolean {
  try {
    const v = sessionStorage.getItem(ALIAS_FORM_FROM_PICKER_KEY);
    return !!v && v === scope;
  } catch {
    return false;
  }
}

/** Consume mark if it matches scope. Mismatched marks are left alone (other form's). */
function consumeFromPicker(scope: string): boolean {
  try {
    const v = sessionStorage.getItem(ALIAS_FORM_FROM_PICKER_KEY);
    if (v === scope) {
      sessionStorage.removeItem(ALIAS_FORM_FROM_PICKER_KEY);
      return true;
    }
    return false;
  } catch {
    return false;
  }
}

export function AliasEditForm() {
  const t = useT();
  const nav = useNavigate();
  const { aliasName } = useParams();
  const loc = useLocation();
  const isNew = aliasName === "new" || !aliasName;

  const locState = loc.state as { draftAlias?: AliasMapping; pickedTargets?: AliasTarget[] } | null;
  const returnDraft = locState?.draftAlias;
  const returnTargets = locState?.pickedTargets;
  // Scope the fromPicker mark to this form ("new" or alias name). A global
  // boolean leaked across forms: abandoned new+picker left the mark set, and
  // the next edit mounted the wrong draft and skipped fetch.
  const scope = pickerScope(isNew, aliasName);
  // Explicit picker-return signal: router state or *matching* session mark.
  // Do NOT treat "session draft has same alias name" as a draft — that was the
  // P0 bug that skipped fetch and left 0 prices.
  const fromPickerReturn = !!(returnDraft || returnTargets || peekFromPicker(scope));

  const [alias, setAlias] = useState<AliasMapping>(() => {
    if (fromPickerReturn) {
      const draft = returnDraft ?? readAliasFormDraft();
      // Only accept draft when it belongs to this form's scope.
      if (draft) {
        const draftOk = isNew
          || draft.alias.toLowerCase() === scope.toLowerCase()
          || !draft.alias; // empty name mid-new is ok
        if (draftOk || returnDraft || returnTargets) {
          return {
            ...draft,
            // Edit route wins for the name so a mismatched leftover never renames the form.
            alias: isNew ? (draft.alias ?? "") : scope,
            targets: returnTargets ?? draft.targets ?? [],
          };
        }
      }
    }
    // New form may still recover in-progress work from draft when scope is "new"
    // and the draft is not a leftover from a different edit session.
    if (isNew && peekFromPicker("new")) {
      const draft = returnDraft ?? readAliasFormDraft();
      if (draft) {
        return {
          ...draft,
          targets: returnTargets ?? draft.targets ?? [],
        };
      }
    }
    return {
      alias: isNew ? "" : scope,
      targets: [],
      dispatch: "round-robin",
      billing_mode: "tokens",
      input_price_per_million: 0,
      output_price_per_million: 0,
      cache_read_price_per_million: 0,
      per_call_usd: 0,
    };
  });
  const [error, setError] = useState("");
  const [saving, setSaving] = useState(false);
  const [priceTable, setPriceTable] = useState<PriceTable | null>(null);
  // Do not persist the 0-price mount shell for edit mode until server data (or
  // an explicit picker draft) is in place — otherwise a clean list→edit can
  // pollute sessionStorage with a zero-price draft for this alias name.
  const [draftPersistReady, setDraftPersistReady] = useState(isNew || fromPickerReturn);

  // Keep session draft in sync once we have real form data (picker return,
  // new form, or post-fetch edit). Gated by draftPersistReady.
  useEffect(() => {
    if (!draftPersistReady) return;
    writeAliasFormDraft(alias);
  }, [alias, draftPersistReady]);

  // Load existing alias if editing — ALWAYS fetch unless this mount is a
  // picker return for *this* alias (returnTargets / returnDraft / scoped mark).
  useEffect(() => {
    if (isNew) {
      // Consume a matching "new" mark so it cannot leak into a later edit.
      if (returnDraft || returnTargets || peekFromPicker("new")) {
        consumeFromPicker("new");
      }
      setDraftPersistReady(true);
      return;
    }
    if (returnDraft || returnTargets) {
      consumeFromPicker(scope);
      setDraftPersistReady(true);
      return;
    }
    if (consumeFromPicker(scope)) {
      // Draft already applied in useState; do not overwrite with server.
      setDraftPersistReady(true);
      return;
    }
    const name = scope;
    void fetchAliases().then((list) => {
      const found = list.find((a) => a.alias === name || a.alias.toLowerCase() === name.toLowerCase());
      if (found) setAlias(found);
      // Persist only after server hydration (or not-found), never the 0-shell.
      setDraftPersistReady(true);
    }).catch((e: unknown) => {
      setError(String(e));
      setDraftPersistReady(true);
    });
  }, [aliasName, isNew, returnDraft, returnTargets, scope]);

  // Apply targets returned from ModelPick (draft fields come from session/router).
  useEffect(() => {
    if (!returnTargets) return;
    setAlias((prev) => {
      const base = returnDraft ?? prev;
      return { ...base, targets: returnTargets };
    });
  }, [returnTargets, returnDraft]);

  // LiteLLM price table for recommend button (single-target only).
  useEffect(() => {
    let alive = true;
    void getPriceTable().then((tbl) => {
      if (alive) setPriceTable(tbl);
    });
    return () => { alive = false; };
  }, []);

  const leaveForm = (toMapping = true) => {
    clearAliasFormDraft();
    if (toMapping) nav("/mapping", { state: { mappingTab: "alias" } });
  };

  const handleSave = async () => {
    setSaving(true);
    setError("");
    try {
      await upsertAlias(alias);
      leaveForm(true);
    } catch (e: unknown) {
      setError(String(e));
    } finally {
      setSaving(false);
    }
  };

  const addTarget = () => {
    // Persist draft + mark fromPicker (scoped to this form) so remount after
    // picker keeps edits and does not re-fetch over them.
    writeAliasFormDraft(alias);
    markFromPicker(pickerScope(isNew, aliasName, alias.alias));
    const here = `/mapping/alias/${isNew ? "new" : encodeURIComponent(alias.alias || aliasName || "new")}`;
    nav("/mapping/pick-target", {
      state: {
        returnTo: here,
        currentTargets: alias.targets,
        draftAlias: alias,
      },
    });
  };

  const removeTarget = (idx: number) => {
    setAlias((prev) => ({ ...prev, targets: prev.targets.filter((_, i) => i !== idx) }));
  };

  const singleTarget = alias.targets.length === 1;
  const recommendHint = singleTarget && alias.billing_mode !== "per_call"
    ? lookupPrice(priceTable, alias.targets[0].target_model)
    : null;

  const applyRecommend = () => {
    if (!recommendHint) return;
    setAlias((prev) => ({
      ...prev,
      input_price_per_million: recommendHint.input_price_per_million,
      output_price_per_million: recommendHint.output_price_per_million,
      cache_read_price_per_million: recommendHint.cache_read_price_per_million,
    }));
  };

  return (
    <div className="map-form-page">
      <div className="map-form-card">
        <div className="map-form-head">
          <a className="back-link" onClick={() => leaveForm(true)}>
            ← {t("mapping.back")}
          </a>
          <h1>{isNew ? t("mapping.alias.newTitle") : t("mapping.alias.editTitle")}</h1>
        </div>
        <div className="map-form-row">
          <label>{t("mapping.alias.name")}</label>
          <input
            className="mono"
            value={alias.alias}
            onChange={(e) => setAlias({ ...alias, alias: e.target.value })}
            disabled={!isNew}
            placeholder="my-alias"
            data-testid="alias-name"
          />
        </div>
        <div className="map-form-row">
          <label>{t("mapping.alias.dispatch")}</label>
          <div className="map-dispatch-seg" role="group" aria-label={t("mapping.alias.dispatch")}>
            <button
              type="button"
              className={"map-dispatch-btn" + (alias.dispatch === "round-robin" ? " active" : "")}
              onClick={() => setAlias({ ...alias, dispatch: "round-robin" })}
            >
              <span className="map-dispatch-title">{t("mapping.alias.roundRobin")}</span>
              <span className="map-dispatch-desc">{t("mapping.alias.roundRobinDesc")}</span>
            </button>
            <button
              type="button"
              className={"map-dispatch-btn" + (alias.dispatch === "priority" ? " active" : "")}
              onClick={() => setAlias({ ...alias, dispatch: "priority" })}
            >
              <span className="map-dispatch-title">{t("mapping.alias.priority")}</span>
              <span className="map-dispatch-desc">{t("mapping.alias.priorityDesc")}</span>
            </button>
          </div>
        </div>
        <div className="map-form-row">
          <label>{t("mapping.alias.targets")}</label>
          <div className="map-form-targets" data-testid="alias-targets">
            {alias.targets.map((tgt, i) => (
              <div key={i} className="map-form-target-row">
                <span className="mono">{tgt.provider} · {tgt.target_model} {tgt.group ? `· ${tgt.group}` : ""}</span>
                <button className="remove-btn" onClick={() => removeTarget(i)}>×</button>
              </div>
            ))}
          </div>
          <button className="btn" onClick={addTarget}>+ {t("mapping.alias.addTarget")}</button>
        </div>
        <div className="map-form-row">
          <label>{t("mapping.alias.billing")}</label>
          <label className="switch" style={{ display: "inline-flex", alignItems: "center", gap: 8 }}>
            <input
              type="checkbox"
              checked={alias.billing_mode === "per_call"}
              onChange={(e) => setAlias({ ...alias, billing_mode: e.target.checked ? "per_call" : "tokens" })}
              data-testid="billing-mode"
            />
            <span className="track"><span className="thumb" /></span>
            <span>{alias.billing_mode === "per_call" ? t("mapping.alias.perCall") : t("mapping.alias.tokens")}</span>
          </label>
        </div>
        {alias.billing_mode === "tokens" ? (
          <>
            <div className="map-form-row">
              <label>{t("mapping.alias.input")} ($/1M)</label>
              <input
                className="mono"
                type="number"
                step="0.01"
                data-testid="price-input"
                value={alias.input_price_per_million ?? 0}
                onChange={(e) => setAlias({ ...alias, input_price_per_million: parseFloat(e.target.value) || 0 })}
              />
            </div>
            <div className="map-form-row">
              <label>{t("mapping.alias.output")} ($/1M)</label>
              <input
                className="mono"
                type="number"
                step="0.01"
                data-testid="price-output"
                value={alias.output_price_per_million ?? 0}
                onChange={(e) => setAlias({ ...alias, output_price_per_million: parseFloat(e.target.value) || 0 })}
              />
            </div>
            <div className="map-form-row">
              <label>{t("mapping.alias.cache")} ($/1M)</label>
              <input
                className="mono"
                type="number"
                step="0.01"
                data-testid="price-cache"
                value={alias.cache_read_price_per_million ?? 0}
                onChange={(e) => setAlias({ ...alias, cache_read_price_per_million: parseFloat(e.target.value) || 0 })}
              />
            </div>
            {recommendHint && (
              <div className="map-form-row">
                <button
                  type="button"
                  className="btn sm"
                  data-testid="recommend-price"
                  onClick={applyRecommend}
                  title={t("mapping.alias.recommendTitle")}
                >
                  {t("mapping.alias.recommend")}
                </button>
              </div>
            )}
          </>
        ) : (
          <div className="map-form-row">
            <label>{t("mapping.alias.perCallUnit")} ($/{t("mapping.alias.perCallUnit")})</label>
            <input
              className="mono"
              type="number"
              step="0.01"
              data-testid="price-per-call"
              value={alias.per_call_usd ?? 0}
              onChange={(e) => setAlias({ ...alias, per_call_usd: parseFloat(e.target.value) || 0 })}
            />
          </div>
        )}
        {error && <div className="error">{error}</div>}
        <div className="map-form-foot">
          <button className="btn primary" onClick={handleSave} disabled={saving}>
            {saving ? "..." : t("mapping.save")}
          </button>
          <button className="btn" onClick={() => leaveForm(true)}>
            {t("mapping.cancel")}
          </button>
        </div>
      </div>
    </div>
  );
}

// --- Rule Edit Form ---

export function RuleEditForm() {
  const t = useT();
  const nav = useNavigate();
  const { ruleName } = useParams();
  const isNew = ruleName === "new" || !ruleName;

  const [rule, setRule] = useState<ClassifyRule>({
    name: isNew ? "" : decodeURIComponent(ruleName),
    field: "plan_type",
    pattern: "",
    group: "",
    enabled: true,
  });
  const [customField, setCustomField] = useState("");
  const [regexError, setRegexError] = useState("");
  const [regexValid, setRegexValid] = useState(false);
  const [error, setError] = useState("");
  const [saving, setSaving] = useState(false);

  useEffect(() => {
    if (isNew) return;
    void fetchClassifyRules().then((list) => {
      const found = list.find((r) => r.name === decodeURIComponent(ruleName));
      if (found) {
        setRule(found);
        if (["filename", "provider", "plan_type", "tier"].includes(found.field)) {
          setCustomField("");
        } else {
          setCustomField(found.field);
          setRule((prev) => ({ ...prev, field: "custom" }));
        }
      }
    }).catch((e: unknown) => setError(String(e)));
  }, [ruleName, isNew]);

  useEffect(() => {
    if (!rule.pattern) {
      setRegexError("");
      setRegexValid(false);
      return;
    }
    try {
      new RegExp(rule.pattern);
      setRegexError("");
      setRegexValid(true);
    } catch (e: unknown) {
      setRegexError(String(e).replace(/^Error: /, ""));
      setRegexValid(false);
    }
  }, [rule.pattern]);

  const effectiveField = rule.field === "custom" ? customField : rule.field;

  const handleSave = async () => {
    if (!regexValid) {
      setError(t("mapping.rule.regexInvalid", { err: regexError }));
      return;
    }
    setSaving(true);
    setError("");
    try {
      await upsertClassifyRule({ ...rule, field: effectiveField });
      nav("/mapping", { state: { mappingTab: "classify" } });
    } catch (e: unknown) {
      setError(String(e));
    } finally {
      setSaving(false);
    }
  };

  return (
    <div className="map-form-page">
      <div className="map-form-card">
        <div className="map-form-head">
          <a className="back-link" onClick={() => nav("/mapping", { state: { mappingTab: "classify" } })}>
            ← {t("mapping.back")}
          </a>
          <h1>{isNew ? t("mapping.rule.newTitle") : t("mapping.rule.editTitle")}</h1>
        </div>
        <div className="map-form-row">
          <label>{t("mapping.rule.name")}</label>
          <input
            value={rule.name}
            onChange={(e) => setRule({ ...rule, name: e.target.value })}
            disabled={!isNew}
            placeholder="my-rule"
          />
        </div>
        <div className="map-form-row">
          <label>{t("mapping.rule.field")}</label>
          <select
            value={rule.field}
            onChange={(e) => setRule({ ...rule, field: e.target.value })}
          >
            <option value="filename">{t("mapping.rule.fieldFilename")}</option>
            <option value="provider">{t("mapping.rule.fieldProvider")}</option>
            <option value="plan_type">{t("mapping.rule.fieldPlanType")}</option>
            <option value="tier">{t("mapping.rule.fieldTier")}</option>
            <option value="custom">{t("mapping.rule.fieldCustom")}</option>
          </select>
        </div>
        {rule.field === "custom" && (
          <div className="map-form-row">
            <label>{t("mapping.rule.customField")}</label>
            <input
              className="mono"
              value={customField}
              onChange={(e) => setCustomField(e.target.value)}
              placeholder="custom_attribute_name"
            />
          </div>
        )}
        <div className="map-form-row">
          <label>{t("mapping.rule.regex")}</label>
          <input
            className="mono"
            value={rule.pattern}
            onChange={(e) => setRule({ ...rule, pattern: e.target.value })}
            placeholder="^team$"
          />
          {regexValid && <div className="regex-valid">✓ {t("mapping.rule.regexValid")}</div>}
          {regexError && <div className="regex-invalid">{t("mapping.rule.regexInvalid", { err: regexError })}</div>}
        </div>
        <div className="map-form-row">
          <label>{t("mapping.rule.group")}</label>
          <input
            className="mono"
            value={rule.group}
            onChange={(e) => setRule({ ...rule, group: e.target.value })}
            placeholder="team"
          />
        </div>
        <div className="map-form-row">
          <label className="switch" style={{ display: "inline-flex", alignItems: "center", gap: 8 }}>
            <input
              type="checkbox"
              checked={rule.enabled}
              onChange={(e) => setRule({ ...rule, enabled: e.target.checked })}
            />
            <span className="track"><span className="thumb" /></span>
            <span>{t("mapping.rule.enabled")}</span>
          </label>
        </div>
        {error && <div className="error">{error}</div>}
        <div className="map-form-foot">
          <button className="btn primary" onClick={handleSave} disabled={saving || !regexValid}>
            {saving ? "..." : t("mapping.save")}
          </button>
          <button className="btn" onClick={() => nav("/mapping", { state: { mappingTab: "classify" } })}>
            {t("mapping.cancel")}
          </button>
        </div>
      </div>
    </div>
  );
}
