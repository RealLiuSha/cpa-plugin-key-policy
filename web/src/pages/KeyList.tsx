import { useCallback, useEffect, useState } from "react";
import { Link } from "react-router-dom";
import { listKeys } from "../api/keys";
import type { KeyPublic, ModelRule } from "../types";
import KeyMoreMenu from "../components/KeyMoreMenu";
import { MobileTabBar } from "../components/MobileChrome";
import PlainKeyModal from "../components/PlainKeyModal";
import { useT } from "../i18n";

/** Deduplicate model rules by alias (case-insensitive) for chip display. */
export function uniqueAliases(models: ModelRule[] | undefined): string[] {
  const out: string[] = [];
  const seen = new Set<string>();
  for (const m of models ?? []) {
    const key = (m.alias ?? "").toLowerCase();
    if (key && !seen.has(key)) {
      seen.add(key);
      out.push(m.alias);
    }
  }
  return out;
}

function fmtUsd(n: number): string {
  return "$" + (Number.isFinite(n) ? n.toFixed(2) : "0.00");
}

/** One usage window: "$used / $limit" or "$used / 不限" when limit is 0. */
function formatUsageWindow(
  used: number,
  limit: number,
  unlimitedLabel: string,
): string {
  return fmtUsd(used) + " / " + (limit > 0 ? fmtUsd(limit) : unlimitedLabel);
}

export default function KeyList() {
  const t = useT();
  const [keys, setKeys] = useState<KeyPublic[]>([]);
  const [error, setError] = useState("");
  const [loading, setLoading] = useState(true);
  const [plain, setPlain] = useState<string | null>(null);
  const [plainTitle, setPlainTitle] = useState("");

  // silent: refresh after reset/rotate/delete without swapping the whole list for "loading…".
  const load = useCallback(async (mode: "full" | "silent" = "full") => {
    if (mode === "full") setLoading(true);
    setError("");
    try {
      setKeys(await listKeys());
    } catch (e) {
      const err = e as { response?: { data?: { error?: { message?: string } } }; message?: string };
      setError(err.response?.data?.error?.message ?? err.message ?? t("keys.loadFailed"));
    } finally {
      if (mode === "full") setLoading(false);
    }
  }, [t]);

  const refreshSilent = useCallback(() => load("silent"), [load]);

  useEffect(() => {
    void load("full");
  }, [load]);

  return (
    <div className="key-list">
      <div className="fp-head mobile-hidden" style={{ margin: "0 0 16px" }}>
        <h1>{t("header.keyList")}</h1>
        <div className="fp-actions">
          <button className="btn sm" onClick={() => void load("full")}>{t("keys.refresh")}</button>
        </div>
      </div>
      {error && <div className="error">{error}</div>}
      {loading ? (
        <div className="muted">{t("keys.loading")}</div>
      ) : keys.length === 0 ? (
        <div className="card muted">{t("keys.empty")}</div>
      ) : (
        <>
          <div className="card table-wrap key-list-table mobile-hidden">
            <table>
              <thead>
                <tr>
                  <th>{t("keys.colIdName")}</th>
                  <th>{t("keys.colStatus")}</th>
                  <th>{t("keys.colPreview")}</th>
                  <th>{t("keys.colRpm")}</th>
                  <th>{t("keys.colUsage")}</th>
                  <th>{t("keys.colModels")}</th>
                  <th>{t("keys.colAliases")}</th>
                  <th>{t("keys.colActions")}</th>
                </tr>
              </thead>
              <tbody>
                {keys.map((k) => (
                  <KeyTableRow
                    key={k.id}
                    k={k}
                    onResetComplete={refreshSilent}
                    onRotated={(plainKey) => {
                      setPlain(plainKey);
                      setPlainTitle(t("keys.rotated"));
                    }}
                    onDeleted={refreshSilent}
                  />
                ))}
              </tbody>
            </table>
          </div>
          <div className="key-list-cards mobile-only">
            {keys.map((k) => (
              <KeyMobileCard
                key={k.id}
                k={k}
                onResetComplete={refreshSilent}
                onRotated={(plainKey) => {
                  setPlain(plainKey);
                  setPlainTitle(t("keys.rotated"));
                }}
                onDeleted={refreshSilent}
              />
            ))}
          </div>
        </>
      )}

      <Link to="/keys/new" className="fab" aria-label={t("keys.newKey")}>+</Link>
      <MobileTabBar active="keys" />

      {plain && (
        <PlainKeyModal
          plainKey={plain}
          title={plainTitle}
          onClose={() => setPlain(null)}
        />
      )}
    </div>
  );
}

function KeyActions({
  k,
  onResetComplete,
  onRotated,
  onDeleted,
}: {
  k: KeyPublic;
  onResetComplete: () => void | Promise<void>;
  onRotated: (plainKey: string) => void;
  onDeleted: () => void | Promise<void>;
}) {
  const t = useT();
  const id = encodeURIComponent(k.id);
  return (
    <div className="key-actions">
      <Link to={`/keys/${id}/edit`} className="btn sm">{t("keys.edit")}</Link>
      <Link to={`/keys/${id}/usage`} className="btn sm">{t("keys.detail")}</Link>
      <KeyMoreMenu
        keyId={k.id}
        onResetComplete={onResetComplete}
        onRotated={onRotated}
        onDeleted={onDeleted}
      />
    </div>
  );
}

function KeyTableRow({
  k,
  onResetComplete,
  onRotated,
  onDeleted,
}: {
  k: KeyPublic;
  onResetComplete: () => void | Promise<void>;
  onRotated: (plainKey: string) => void;
  onDeleted: () => void | Promise<void>;
}) {
  const t = useT();
  const aliases = uniqueAliases(k.models);
  const shown = aliases.slice(0, 3);
  const more = Math.max(0, aliases.length - 3);
  const unlimited = t("usage.unlimited");
  const daily = formatUsageWindow(k.usage.daily_usd, k.usage.daily_limit_usd, unlimited);
  const weekly = formatUsageWindow(k.usage.weekly_usd, k.usage.weekly_limit_usd, unlimited);

  return (
    <tr className={k.enabled ? undefined : "row-disabled"}>
      <td>
        <div className="key-id">{k.id}</div>
        {k.name ? <div className="muted key-name">{k.name}</div> : null}
      </td>
      <td>
        <span className={"tag " + (k.enabled ? "on" : "off")}>
          {k.enabled ? t("keys.enabled") : t("keys.disabled")}
        </span>
      </td>
      <td className="mono">{k.key_preview}</td>
      <td>{k.rpm > 0 ? k.rpm : t("usage.unlimited")}</td>
      <td className="key-usage-cell">
        <div>{t("usage.today")} {daily}</div>
        <div className="muted">{t("usage.thisWeek")} {weekly}</div>
      </td>
      <td>{aliases.length}</td>
      <td>
        {shown.length > 0 ? (
          <div className="key-alias-chips">
            {shown.map((a) => (
              <span key={a} className="chip">{a}</span>
            ))}
            {more > 0 && <span className="chip more">+{more}</span>}
          </div>
        ) : (
          <span className="muted">—</span>
        )}
      </td>
      <td className="key-actions-cell">
        <KeyActions
          k={k}
          onResetComplete={onResetComplete}
          onRotated={onRotated}
          onDeleted={onDeleted}
        />
      </td>
    </tr>
  );
}

/**
 * Mobile card: same ops as the desktop row (edit / detail / more). No whole-card
 * navigation and no swipe-to-revoke — those conflicted with the desktop IA.
 */
function KeyMobileCard({
  k,
  onResetComplete,
  onRotated,
  onDeleted,
}: {
  k: KeyPublic;
  onResetComplete: () => void | Promise<void>;
  onRotated: (plainKey: string) => void;
  onDeleted: () => void | Promise<void>;
}) {
  const t = useT();
  const aliases = uniqueAliases(k.models);
  const shownChips = aliases.slice(0, 2);
  const moreCount = Math.max(0, aliases.length - 2);
  const limit = k.usage.daily_limit_usd > 0 ? k.usage.daily_limit_usd : 0;
  const pct = limit > 0 ? Math.min(100, (k.usage.daily_usd / limit) * 100) : 0;
  const over = limit > 0 && k.usage.daily_usd >= limit;

  return (
    <div className={"keycard" + (k.enabled ? "" : " disabled") + (over ? " over" : "")}>
      <div className="kc-head">
        <span className="kc-dot" />
        <span className="kc-name">{k.name || k.id}</span>
      </div>
      <div className="kc-preview">{k.key_preview}</div>
      {limit > 0 ? (
        <>
          <div className="kc-bar"><span style={{ width: pct + "%" }} /></div>
          <div className="kc-meta">
            <span>{fmtUsd(k.usage.daily_usd)} / {fmtUsd(limit)}</span>
            <span>{aliases.length} {t("keys.mobile.modelsSuffix")}</span>
          </div>
        </>
      ) : (
        <div className="kc-meta">
          <span>{fmtUsd(k.usage.daily_usd)} · {t("keys.mobile.noLimit")}</span>
          <span>{aliases.length} {t("keys.mobile.modelsSuffix")}</span>
        </div>
      )}
      {shownChips.length > 0 && (
        <div className="kc-chips">
          {shownChips.map((a) => <span key={a} className="chip">{a}</span>)}
          {moreCount > 0 && <span className="chip more">+{moreCount}</span>}
        </div>
      )}
      <div className="kc-actions">
        <KeyActions
          k={k}
          onResetComplete={onResetComplete}
          onRotated={onRotated}
          onDeleted={onDeleted}
        />
      </div>
    </div>
  );
}
