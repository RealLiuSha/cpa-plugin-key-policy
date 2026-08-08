import { useCallback, useEffect, useState, type ReactNode } from "react";
import { Link, useNavigate } from "react-router-dom";
import type { KeyPublic, ModelRule, AliasMapping } from "../types";
import ModelPicker from "./ModelPicker";
import { fetchAliases } from "../api/mappings";
import { formatTierLabel } from "../api/models";
import { useT } from "../i18n";

export interface KeyFormValues {
  id: string;
  name: string;
  enabled: boolean;
  rpm: number;
  models: ModelRule[];
  daily_limit_usd: number;
  weekly_limit_usd: number;
  // Per-key override for GET /v1/models. CPA cannot filter the model list per
  // downstream key, so the only plugin-enforceable choice is binary: 401 (hide
  // the list) or allow (client sees the full global list). Default false.
  allow_models_endpoint?: boolean;
}

/** Meta passed to parent onSubmit so post-save UX (navigate / toast) can react. */
export interface KeyFormSubmitMeta {
  /** Newly selected aliases that are unpriced (tokens + three prices 0, or not yet in global table). */
  newUnpricedCount: number;
}

interface Props {
  initial?: KeyPublic;
  idReadOnly?: boolean;
  submitLabel: string;
  onSubmit: (v: KeyFormValues, meta: KeyFormSubmitMeta) => Promise<void>;
  onCancel: () => void;
  // top-level error to render
  error?: string;
  // route path for the standalone model-picker page (e.g. "/keys/new/models").
  // When set, the desktop form renders a chip box + "add model" button that
  // navigates here with the current models as router state. The picker page
  // navigates back with state.pickedModels, which the parent merges into
  // `initial` before re-rendering this form.
  pickPath?: string;
  // extra-danger button config for the footer (edit mode). When provided,
  // renders a danger-outline button on the far right of the footer.
  dangerLabel?: string;
  onDanger?: () => void;
}

// Stable identity for a selected model row (group|alias). Used for chip keys
// and dedupe; no longer used for price maps.
function modelKey(m: { alias: string; group?: string; provider?: string; target_model?: string }): string {
  const g = (m.group ?? "").toLowerCase();
  const p = (m.provider ?? "").toLowerCase();
  const t = (m.target_model ?? "").toLowerCase();
  return `${g}|${m.alias.toLowerCase()}|${p}|${t}`;
}

function parseNum(value: string): number {
  const n = parseFloat(value);
  return Number.isFinite(n) ? n : 0;
}

/** tokens (or empty default) with all three token prices at 0. */
export function isUnpricedAlias(a: Pick<AliasMapping, "billing_mode" | "input_price_per_million" | "output_price_per_million" | "cache_read_price_per_million">): boolean {
  if (a.billing_mode === "per_call") return false;
  return (a.input_price_per_million ?? 0) === 0
    && (a.output_price_per_million ?? 0) === 0
    && (a.cache_read_price_per_million ?? 0) === 0;
}

/** Strip price / billing fields so key submit never claims to set prices. */
export function modelsWithoutPrices(models: ModelRule[]): ModelRule[] {
  return models.map((m) => {
    const out: ModelRule = {
      alias: m.alias,
      provider: m.provider,
      target_model: m.target_model,
    };
    if (m.group) out.group = m.group;
    return out;
  });
}

/**
 * Count aliases present in `models` but not in `initialModels` that are unpriced
 * against the global alias table (missing alias ⇒ will be created at 0 = unpriced).
 */
export function countNewUnpricedAliases(
  models: ModelRule[],
  initialModels: ModelRule[] | undefined,
  globalAliases: AliasMapping[],
): number {
  const initialAliases = new Set((initialModels ?? []).map((m) => m.alias.toLowerCase()));
  const byName = new Map(globalAliases.map((a) => [a.alias.toLowerCase(), a]));
  let n = 0;
  const seen = new Set<string>();
  for (const m of models) {
    const lk = m.alias.toLowerCase();
    if (seen.has(lk) || initialAliases.has(lk)) continue;
    seen.add(lk);
    const global = byName.get(lk);
    if (!global || isUnpricedAlias(global)) n++;
  }
  return n;
}

export default function KeyForm({
  initial,
  idReadOnly,
  submitLabel,
  onSubmit,
  onCancel,
  error,
  pickPath,
  dangerLabel,
  onDanger,
}: Props) {
  const nav = useNavigate();
  const [id, setId] = useState(initial?.id ?? "");
  const [name, setName] = useState(initial?.name ?? "");
  const [enabled, setEnabled] = useState(initial?.enabled ?? true);
  const [rpm, setRpm] = useState(initial?.rpm ?? 0);
  const [dailyLimit, setDailyLimit] = useState(initial?.daily_limit_usd ?? 0);
  const [weeklyLimit, setWeeklyLimit] = useState(initial?.weekly_limit_usd ?? 0);
  const [allowModels, setAllowModels] = useState<boolean>(initial?.allow_models_endpoint ?? false);
  const t = useT();

  const [models, setModels] = useState<ModelRule[]>(initial?.models ?? []);
  const [busy, setBusy] = useState(false);
  const [localErr, setLocalErr] = useState("");

  // Global alias table: used for "existing aliases" chips and unpriced checks.
  // aliasesReady gates submit meta so we never count "all new = unpriced" while
  // the table is still inflight (empty array would over-count).
  const [globalAliases, setGlobalAliases] = useState<AliasMapping[]>([]);
  const [aliasesReady, setAliasesReady] = useState(false);
  useEffect(() => {
    let alive = true;
    void fetchAliases()
      .then((list) => {
        if (!alive) return;
        setGlobalAliases(list);
        setAliasesReady(true);
      })
      .catch(() => {
        // Failed load: still mark ready with empty table so submit is not stuck.
        // Missing aliases are treated as not-yet-created (0-price) — same as
        // after a successful empty list.
        if (!alive) return;
        setGlobalAliases([]);
        setAliasesReady(true);
      });
    return () => { alive = false; };
  }, []);

  const aliasByName = useCallback((aliasName: string): AliasMapping | undefined => {
    const lk = aliasName.toLowerCase();
    return globalAliases.find((a) => a.alias.toLowerCase() === lk);
  }, [globalAliases]);

  // aliasSelected reports whether every target of `a` is already in `models`.
  const aliasSelected = useCallback((a: AliasMapping) => {
    return a.targets.every((tgt) =>
      models.some((m) =>
        m.alias.toLowerCase() === a.alias.toLowerCase() &&
        m.provider.toLowerCase() === tgt.provider.toLowerCase() &&
        m.target_model.toLowerCase() === tgt.target_model.toLowerCase() &&
        (m.group ?? "").toLowerCase() === (tgt.group ?? "").toLowerCase(),
      ),
    );
  }, [models]);

  const toggleAlias = useCallback((a: AliasMapping) => {
    if (aliasSelected(a)) {
      setModels((prev) => prev.filter((m) =>
        !(m.alias.toLowerCase() === a.alias.toLowerCase()),
      ));
    } else {
      const newRules: ModelRule[] = a.targets.map((tgt) => ({
        alias: a.alias,
        provider: tgt.provider,
        target_model: tgt.target_model,
        group: tgt.group ?? "",
      }));
      setModels((prev) => {
        const filtered = prev.filter((m) => m.alias.toLowerCase() !== a.alias.toLowerCase());
        return [...filtered, ...newRules];
      });
    }
  }, [aliasSelected]);

  const handleModelsChange = useCallback((next: ModelRule[]) => {
    setModels(next);
  }, []);

  /** Unique alias names currently selected (for unpriced checks). */
  const selectedAliasNames = (() => {
    const names: string[] = [];
    const seen = new Set<string>();
    for (const m of models) {
      const lk = m.alias.toLowerCase();
      if (seen.has(lk)) continue;
      seen.add(lk);
      names.push(m.alias);
    }
    return names;
  })();

  const isAliasUnpriced = (aliasName: string): boolean => {
    const global = aliasByName(aliasName);
    if (!global) {
      // Not yet in global table → will be created at 0 price on save.
      return true;
    }
    return isUnpricedAlias(global);
  };

  const submit = async (e: React.FormEvent) => {
    e.preventDefault();
    setLocalErr("");
    if (!id.trim()) {
      setLocalErr(t("keyForm.idRequired"));
      return;
    }
    // Wait for global alias table so newUnpricedCount is not computed against
    // an empty inflight list (which would treat every new alias as unpriced).
    if (!aliasesReady) {
      setLocalErr(t("keyForm.aliasesLoading") || t("keys.loading") || "Loading...");
      return;
    }
    // Models without price fields — global alias table is the price authority.
    const stripped = modelsWithoutPrices(models);
    const newUnpriced = countNewUnpricedAliases(stripped, initial?.models, globalAliases);
    const meta: KeyFormSubmitMeta = { newUnpricedCount: newUnpriced };
    setBusy(true);
    try {
      await onSubmit({
        id: id.trim(),
        name: name.trim(),
        enabled,
        rpm,
        models: stripped,
        daily_limit_usd: dailyLimit,
        weekly_limit_usd: weeklyLimit,
        allow_models_endpoint: allowModels,
      }, meta);
      // After-save unpriced guidance is owned by KeyEdit/KeyNew via meta —
      // do not render a second banner here (avoids duplicate DOM notices).
    } catch (err) {
      const e2 = err as { response?: { data?: { error?: { message?: string } } }; message?: string };
      setLocalErr(e2.response?.data?.error?.message ?? e2.message ?? t("keyForm.submitFailed"));
    } finally {
      setBusy(false);
    }
  };

  const renderModelChips = () => (
    <div className="model-chips-box">
      {models.length === 0 && <span className="mc-empty">{t("keyForm.modelsEmpty")}</span>}
      {models.map((m) => {
        const unpriced = isAliasUnpriced(m.alias);
        return (
          <span key={modelKey(m)} className={"mc-chip" + (unpriced ? " mc-chip-unpriced" : "")}>
            {m.alias}{m.group ? " · " + formatTierLabel(t, m.group) : ""}
            {unpriced && (
              <Link
                className="mc-unpriced-link"
                to={`/mapping/alias/${encodeURIComponent(m.alias)}`}
                title={t("keyForm.unpricedTitle")}
                onClick={(ev) => ev.stopPropagation()}
              >
                {t("keyForm.unpricedBadge")}
              </Link>
            )}
            <button
              type="button"
              className="mc-x"
              onClick={() => {
                setModels((prev) => prev.filter((x) => modelKey(x) !== modelKey(m)));
              }}
              aria-label={t("keyForm.removeModel")}
            >
              ×
            </button>
          </span>
        );
      })}
      {pickPath && (
        <button type="button" className="mc-add" onClick={() => nav(pickPath, { state: { models } })}>
          + {t("keyForm.addModel")}
        </button>
      )}
    </div>
  );

  const section = (title: string, children: ReactNode) => (
    <section className="kf-section mobile-only">
      <div className="section-label">{title}</div>
      <div className="kf-section-card">{children}</div>
    </section>
  );

  return (
    <form className="card key-form" onSubmit={submit}>
      <div className="mobile-only kf-sections">
        {section(t("keyForm.mobile.sectionBasic"), (
          <>
            <div className="form-row">
              <label>{t("keyForm.idLabel")}</label>
              <input
                className={"input" + (idReadOnly ? " mono" : "")}
                value={id}
                onChange={(e) => setId(e.target.value)}
                readOnly={idReadOnly}
                placeholder={t("keyForm.idPlaceholder")}
                autoFocus={!idReadOnly}
              />
            </div>
            <div className="form-row">
              <label>{t("keyForm.nameLabel")}</label>
              <input
                className="input"
                value={name}
                onChange={(e) => setName(e.target.value)}
                placeholder={t("keyForm.namePlaceholder")}
              />
            </div>
            <div className="form-row kf-switch-row">
              <label className="switch">
                <input type="checkbox" checked={enabled} onChange={(e) => setEnabled(e.target.checked)} />
                <span className="track"><span className="thumb" /></span>
                <span>{t("keyForm.enableKey")}</span>
              </label>
            </div>
            <div className="form-row">
              <label>{t("keyForm.rpmLabel")}</label>
              <input
                className="input"
                type="number"
                min={0}
                value={rpm}
                onChange={(e) => setRpm(parseInt(e.target.value || "0", 10) || 0)}
              />
            </div>
          </>
        ))}
        {section(t("keyForm.mobile.sectionLimits"), (
          <>
            <div className="form-row">
              <label>{t("keyForm.dailyLimitLabel")}</label>
              <input
                className="input"
                type="number"
                min={0}
                step="0.01"
                value={dailyLimit}
                onChange={(e) => setDailyLimit(parseNum(e.target.value))}
              />
            </div>
            <div className="form-row">
              <label>{t("keyForm.weeklyLimitLabel")}</label>
              <input
                className="input"
                type="number"
                min={0}
                step="0.01"
                value={weeklyLimit}
                onChange={(e) => setWeeklyLimit(parseNum(e.target.value))}
              />
            </div>
          </>
        ))}
        {section(t("keyForm.mobile.sectionAccess"), (
          <>
            <label className="switch kf-access-switch" title={t("keyForm.allowModelsTitle")}>
              <input type="checkbox" checked={allowModels} onChange={(e) => setAllowModels(e.target.checked)} />
              <span className="track"><span className="thumb" /></span>
              <span>{t("keyForm.allowModelsLabel")}</span>
            </label>
            <p className="muted kf-hint">{t("keyForm.allowModelsHint")}</p>
          </>
        ))}
        <section className="kf-section mobile-only">
          <div className="section-label">{t("keyForm.mobile.sectionModels")}</div>
          {globalAliases.length > 0 && (
            <div className="form-row kf-alias-pick" style={{ marginBottom: 12 }}>
              <div className="kf-alias-chips">
                {globalAliases.map((a) => {
                  const on = aliasSelected(a);
                  return (
                    <button key={a.alias} type="button" className={"kf-alias-chip" + (on ? " selected" : "")} onClick={() => toggleAlias(a)}>
                      {a.alias}{a.targets.length > 1 ? ` (${a.targets.length})` : ""}
                    </button>
                  );
                })}
              </div>
            </div>
          )}
          <div className="form-row" style={{ marginBottom: 12 }}>
            {pickPath ? renderModelChips() : (
              <ModelPicker initial={initial?.models} onChange={handleModelsChange} />
            )}
          </div>
          {selectedAliasNames.some(isAliasUnpriced) && (
            <p className="muted kf-warn">⚠ {t("keyForm.unpricedHint")}</p>
          )}
        </section>
      </div>

      <div className="mobile-hidden">
        <div className="row2">
          <div className="form-row">
            <label>{t("keyForm.idLabel")}</label>
            <input
              className="input"
              value={id}
              onChange={(e) => setId(e.target.value)}
              readOnly={idReadOnly}
              placeholder={t("keyForm.idPlaceholder")}
              autoFocus={!idReadOnly}
            />
          </div>
          <div className="form-row">
            <label>{t("keyForm.nameLabel")}</label>
            <input
              className="input"
              value={name}
              onChange={(e) => setName(e.target.value)}
              placeholder={t("keyForm.namePlaceholder")}
            />
          </div>
        </div>
        <div className="row2">
          <div className="form-row">
            <label>{t("keyForm.rpmLabel")}</label>
            <input
              className="input"
              type="number"
              min={0}
              value={rpm}
              onChange={(e) => setRpm(parseInt(e.target.value || "0", 10) || 0)}
            />
          </div>
          <div className="form-row">
            <label>{t("keyForm.statusLabel")}</label>
            <label className="switch">
              <input
                type="checkbox"
                checked={enabled}
                onChange={(e) => setEnabled(e.target.checked)}
              />
              <span className="track"><span className="thumb" /></span>
              <span>{t("keyForm.enableKey")}</span>
            </label>
          </div>
        </div>

        <div className="row2">
          <div className="form-row">
            <label>{t("keyForm.dailyLimitLabel")}</label>
            <input
              className="input"
              type="number"
              min={0}
              step="0.01"
              value={dailyLimit}
              onChange={(e) => setDailyLimit(parseNum(e.target.value))}
            />
          </div>
          <div className="form-row">
            <label>{t("keyForm.weeklyLimitLabel")}</label>
            <input
              className="input"
              type="number"
              min={0}
              step="0.01"
              value={weeklyLimit}
              onChange={(e) => setWeeklyLimit(parseNum(e.target.value))}
            />
          </div>
        </div>

        <div className="form-row">
          <label className="switch" title={t("keyForm.allowModelsTitle")}>
            <input
              type="checkbox"
              checked={allowModels}
              onChange={(e) => setAllowModels(e.target.checked)}
            />
            <span className="track"><span className="thumb" /></span>
            <span>{t("keyForm.allowModelsLabel")}</span>
          </label>
          <span className="muted" style={{ fontSize: "0.85em", marginLeft: 8 }}>
            {t("keyForm.allowModelsHint")}
          </span>
        </div>

        {globalAliases.length > 0 && (
          <div className="form-row kf-alias-pick">
            <label>{t("keyForm.existingAliases")}</label>
            <div className="kf-alias-chips">
              {globalAliases.map((a) => {
                const on = aliasSelected(a);
                return (
                  <button
                    key={a.alias}
                    type="button"
                    className={"kf-alias-chip" + (on ? " selected" : "")}
                    onClick={() => toggleAlias(a)}
                    title={a.targets.map((tg) => `${tg.provider}·${tg.target_model}${tg.group ? `·${tg.group}` : ""}`).join("\n")}
                  >
                    {a.alias}{a.targets.length > 1 ? ` (${a.targets.length})` : ""}
                  </button>
                );
              })}
            </div>
          </div>
        )}

        <div className="form-row">
          <label>{t("keyForm.modelsLabel")}</label>
          {pickPath ? renderModelChips() : (
            <ModelPicker initial={initial?.models} onChange={handleModelsChange} />
          )}
          {selectedAliasNames.some(isAliasUnpriced) && (
            <p className="muted kf-warn" style={{ marginTop: 8 }}>⚠ {t("keyForm.unpricedHint")}</p>
          )}
        </div>
      </div>

      {(localErr || error) && <div className="error">{localErr || error}</div>}

      <div className="actions fp-foot">
        <button className="btn primary" type="submit" disabled={busy || !aliasesReady} data-testid="keyform-submit">
          {busy ? t("keyForm.submitting") : (!aliasesReady ? (t("keys.loading") || "...") : submitLabel)}
        </button>
        <button className="btn" type="button" onClick={onCancel}>{t("keyForm.cancel")}</button>
        {dangerLabel && onDanger && (
          <span className="fp-foot-right">
            <button type="button" className="btn danger-outline" onClick={onDanger}>{dangerLabel}</button>
          </span>
        )}
      </div>
    </form>
  );
}
