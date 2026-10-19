import { useCallback, useEffect, useId, useMemo, useRef, useState } from "react";
import { fetchChannelDetail, patchChannel, refreshChannelBalance } from "../api/channels";
import { apiErrorCode, extractApiError } from "../api/error";
import type { ChannelRow } from "../channelMerge";
import { copyText } from "../clipboard";
import { formatCount, formatDateTime, formatMillis, formatPercent, formatUSD, timeValue } from "../format";
import { useT } from "../i18n";
import type { BalanceKind, ChannelBalance, ChannelDetail as Detail, ChannelMeta, ChannelPatch, ChannelSummary } from "../types";
import ChannelAdapters from "./ChannelAdapters";
import { BalanceValue, ChannelStateBadge, FailurePill } from "./ChannelState";
import HourlyChart from "./HourlyChart";

type Translate = ReturnType<typeof useT>;

export const DETAIL_TABS = ["overview", "adapters", "cost", "notes"] as const;
export type DetailTab = (typeof DETAIL_TABS)[number];

const BALANCE_KINDS: BalanceKind[] = ["", "sub2api", "openai_billing"];

function statusCodes(summary: ChannelSummary): string {
  return Object.entries(summary.status_codes ?? {}).sort(([a], [b]) => a.localeCompare(b)).map(([code, n]) => `${code} ×${n}`).join(", ");
}

// The vendor's logs are in another time zone, so the report names it.
function zonedTime(date: Date): string {
  return date.toLocaleString(undefined, { timeZoneName: "short" });
}

// Plain text for an upstream vendor: the channel, its last 24 hours and the
// recent failed attempts. No key or auth_index is included.
export function feedbackReport(name: string, baseUrl: string, detail: Detail, t: Translate, now = new Date()): string {
  const day = detail.last_24h;
  const lines = [
    t("channels.feedback.channel", { name, url: baseUrl || "—" }),
    t("channels.feedback.window", { time: zonedTime(now) }),
    t("channels.feedback.counts", { attempts: day.attempts, failures: day.failures, rate: day.attempts > 0 ? formatPercent(day.success_rate) : "—" }),
  ];
  const codes = statusCodes(day);
  if (codes) lines.push(t("channels.feedback.codes", { codes }));
  lines.push("", t("channels.feedback.errors"));
  if (detail.recent_errors.length === 0) lines.push(t("channels.feedback.noErrors"));
  for (const item of detail.recent_errors) {
    const at = timeValue(item.at);
    lines.push([at === null ? item.at : zonedTime(new Date(at)), item.status_code || "—", item.model || "—", (item.snippet ?? "").replace(/\s+/g, " ").trim() || "—"].join(" · "));
  }
  return lines.join("\n");
}

function SummaryTable({ detail, costs }: { detail: Detail; costs: boolean }) {
  const t = useT();
  const rows: [string, (s: ChannelSummary) => string][] = [
    ["channels.summary.attempts", (s) => formatCount(s.attempts)],
    ["channels.summary.failures", (s) => formatCount(s.failures)],
    ["channels.summary.successRate", (s) => (s.attempts > 0 ? formatPercent(s.success_rate) : "—")],
    ["channels.summary.latency", (s) => `${formatMillis(s.latency_p50_ms)} / ${formatMillis(s.latency_p95_ms)}`],
    ["channels.summary.ttft", (s) => `${formatMillis(s.ttft_p50_ms)} / ${formatMillis(s.ttft_p95_ms)}`],
    ["channels.summary.tokens", (s) => `${formatCount(s.input_tokens)} / ${formatCount(s.output_tokens)}`],
    ["channels.summary.listCost", (s) => formatUSD(s.list_cost_usd)],
  ];
  if (costs) rows.push(["channels.summary.upstreamCost", (s) => formatUSD(s.upstream_cost_usd)]);
  return (
    <div className="table-scroll">
      <table className="channel-summary">
        <thead><tr><th>{t("channels.summary.metric")}</th><th className="num">{t("channels.summary.last24h")}</th><th className="num">{t("channels.summary.last7d")}</th></tr></thead>
        <tbody>
          {rows.map(([key, value]) => (
            <tr key={key}><td>{t(key)}</td><td className="num mono">{value(detail.last_24h)}</td><td className="num mono">{value(detail.last_7d)}</td></tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}

// CPA owns these fields; they are shown, never edited here.
function CpaFacts({ row }: { row: ChannelRow }) {
  const t = useT();
  const { config, auth } = row;
  const facts: [string, string][] = [[t("channels.cpa.type"), row.provider || "—"]];
  if (config) {
    facts.push([t("channels.cpa.baseUrl"), config.baseUrl || "—"]);
    facts.push([t("channels.cpa.models"), config.models.join(", ") || t("channels.cpa.allModels")]);
    if (config.excludedModels.length) facts.push([t("channels.cpa.excluded"), config.excludedModels.join(", ")]);
    if (config.priority !== undefined) facts.push([t("channels.cpa.priority"), String(config.priority)]);
    if (config.weight !== undefined) facts.push([t("channels.cpa.weight"), String(config.weight)]);
    if (config.prefix) facts.push([t("channels.cpa.prefix"), config.prefix]);
  }
  if (auth) {
    facts.push([t("channels.cpa.file"), auth.name || auth.id]);
    if (auth.statusMessage) facts.push([t("channels.cpa.statusMessage"), auth.statusMessage]);
    if (auth.nextRetryAfter && formatDateTime(auth.nextRetryAfter)) facts.push([t("channels.cpa.nextRetry"), formatDateTime(auth.nextRetryAfter)]);
    if (auth.cooldowns.length) facts.push([t("channels.cpa.cooldowns"), auth.cooldowns.map((item) => `${item.model || t("channels.cpa.wholeCredential")} → ${formatDateTime(item.retryAt) || "—"}`).join("; ")]);
    facts.push([t("channels.cpa.counts"), t("channels.cpa.countsValue", { success: formatCount(auth.success), failed: formatCount(auth.failed) })]);
  }
  if (!config && !auth && row.view?.base_url) facts.push([t("channels.cpa.baseUrl"), row.view.base_url]);
  return (
    <section className="detail-section">
      <h3>{t("channels.cpa.title")}</h3>
      <dl className="detail-facts">
        {facts.map(([label, value]) => <div key={label}><dt>{label}</dt><dd className="mono">{value}</dd></div>)}
      </dl>
      <p className="field-hint">{row.origin === "plugin" ? t(row.state === "missing" ? "channels.cpa.missingHint" : "channels.cpa.unknownHint") : t("channels.cpa.ownedHint")}</p>
    </section>
  );
}

function OverviewTab({ row, detail, missing }: { row: ChannelRow; detail: Detail | null; missing: boolean }) {
  const t = useT();
  const [copy, setCopy] = useState<{ ok: boolean; text: string } | null>(null);
  const costs = (detail?.meta.cost_ratio ?? 0) > 0;
  const copyFeedback = async () => {
    if (!detail) return;
    const text = feedbackReport(row.name, row.baseUrl, detail, t);
    setCopy({ ok: await copyText(text), text });
  };
  return (
    <>
      <CpaFacts row={row} />
      {missing ? <div className="empty-state">{t("channels.noUsage")}</div> : detail && (
        <>
          <section className="detail-section">
            <h3>{t("channels.summaryTitle")}</h3>
            <SummaryTable detail={detail} costs={costs} />
          </section>
          <section className="detail-section">
            <h3>{t("channels.hoursTitle")}</h3>
            {detail.hours.length ? <HourlyChart hours={detail.hours} label={t("channels.hoursTitle")} /> : <p className="muted">{t("channels.hoursEmpty")}</p>}
          </section>
          <section className="detail-section">
            <h3>{t("channels.codesTitle")}</h3>
            {statusCodes(detail.last_24h) ? <p className="mono">{statusCodes(detail.last_24h)}</p> : <p className="muted">{t("channels.codesEmpty")}</p>}
          </section>
          <section className="detail-section">
            <div className="detail-section-head">
              <h3>{t("channels.errorsTitle")}</h3>
              <button type="button" className="btn sm" onClick={() => void copyFeedback()}>{t("channels.copyFeedback")}</button>
            </div>
            {copy && (copy.ok ? <p className="success" role="status">{t("channels.copied")}</p> : (
              <div role="status">
                <p className="notice">{t("channels.copyManual")}</p>
                <textarea className="input mono" rows={8} readOnly value={copy.text} aria-label={t("channels.copyFeedback")} onFocus={(event) => event.currentTarget.select()} />
              </div>
            ))}
            {detail.recent_errors.length === 0 ? <p className="muted">{t("channels.errorsEmpty")}</p> : (
              <ul className="channel-errors">
                {detail.recent_errors.map((item, index) => (
                  <li key={`${item.at}-${index}`}>
                    <div><time className="muted" dateTime={item.at}>{formatDateTime(item.at) || item.at}</time> <span className="badge danger">{item.status_code || "—"}</span> <span className="mono">{item.model}</span></div>
                    {item.snippet && <div className="mono channel-error-snippet">{item.snippet}</div>}
                  </li>
                ))}
              </ul>
            )}
          </section>
        </>
      )}
    </>
  );
}

function CostTab({ authIndex, meta, balance, onSaved, onRefreshed, onDirty }: { authIndex: string; meta: ChannelMeta; balance: ChannelBalance | null; onSaved: (meta: ChannelMeta) => void; onRefreshed: () => void; onDirty: (dirty: boolean) => void }) {
  const t = useT();
  const ids = useId();
  const [ratio, setRatio] = useState(String(meta.cost_ratio ?? 0));
  const [kind, setKind] = useState<BalanceKind>(meta.balance?.kind ?? "");
  const [threshold, setThreshold] = useState(String(meta.balance?.threshold_usd ?? 0));
  // The fields as loaded or last saved, compared as typed: "0.50" saved as
  // 0.5 is not a change.
  const [baseline, setBaseline] = useState({ ratio, kind, threshold });
  const [current, setCurrent] = useState<ChannelBalance | null>(balance);
  const [message, setMessage] = useState<{ ok: boolean; text: string } | null>(null);
  const [busy, setBusy] = useState(false);
  const savedKind = meta.balance?.kind ?? "";
  const dirty = ratio !== baseline.ratio || kind !== baseline.kind || threshold !== baseline.threshold;

  useEffect(() => setCurrent(balance), [balance]);
  useEffect(() => onDirty(dirty), [dirty, onDirty]);

  const save = async (event: React.FormEvent) => {
    event.preventDefault();
    const costRatio = Number(ratio);
    const thresholdUSD = Number(threshold);
    if (!Number.isFinite(costRatio) || costRatio < 0 || costRatio > 10) return setMessage({ ok: false, text: t("channels.cost.ratioInvalid") });
    if (!Number.isFinite(thresholdUSD) || thresholdUSD < 0) return setMessage({ ok: false, text: t("channels.cost.thresholdInvalid") });
    setBusy(true);
    setMessage(null);
    try {
      const patch: ChannelPatch = { auth_index: authIndex, cost_ratio: costRatio, balance: { kind, threshold_usd: thresholdUSD } };
      onSaved(await patchChannel(patch));
      setBaseline({ ratio, kind, threshold });
      setMessage({ ok: true, text: t("channels.saved") });
    } catch (reason) {
      setMessage({ ok: false, text: extractApiError(reason, t("channels.saveFailed")) });
    } finally {
      setBusy(false);
    }
  };
  const refresh = async () => {
    setBusy(true);
    setMessage(null);
    try {
      setCurrent(await refreshChannelBalance(authIndex));
      onRefreshed();
    } catch (reason) {
      setMessage({ ok: false, text: extractApiError(reason, t("channels.cost.refreshFailed")) });
    } finally {
      setBusy(false);
    }
  };
  return (
    <form className="detail-form" onSubmit={(event) => void save(event)}>
      <label htmlFor={`${ids}-ratio`} className="field-label">{t("channels.cost.ratio")}</label>
      <input id={`${ids}-ratio`} className="input" type="number" min={0} max={10} step="any" inputMode="decimal" value={ratio} onChange={(event) => setRatio(event.target.value)} />
      <p className="field-hint">{t("channels.cost.ratioHint")}</p>
      <label htmlFor={`${ids}-kind`} className="field-label">{t("channels.cost.source")}</label>
      <select id={`${ids}-kind`} className="input" value={kind} onChange={(event) => setKind(event.target.value as BalanceKind)}>
        {BALANCE_KINDS.map((value) => <option key={value || "none"} value={value}>{t(`channels.cost.kind.${value || "none"}`)}</option>)}
      </select>
      <p className="field-hint">{t(`channels.cost.kindHint.${kind || "none"}`)}</p>
      {kind && (
        <>
          <label htmlFor={`${ids}-threshold`} className="field-label">{t("channels.cost.threshold")}</label>
          <input id={`${ids}-threshold`} className="input" type="number" min={0} step="any" inputMode="decimal" value={threshold} onChange={(event) => setThreshold(event.target.value)} />
          <p className="field-hint">{t("channels.cost.thresholdHint")}</p>
        </>
      )}
      <div className="detail-section">
        <h3>{t("channels.cost.current")}</h3>
        <p><BalanceValue balance={current} />{current?.at && <span className="muted"> · {t("channels.balanceAt", { time: formatDateTime(current.at) })}</span>}</p>
        {current?.error && <p className="error">{current.error}</p>}
        {savedKind ? (
          <button type="button" className="btn sm" disabled={busy} onClick={() => void refresh()}>{t("channels.cost.refresh")}</button>
        ) : <p className="field-hint">{t("channels.cost.noSource")}</p>}
      </div>
      {message && <div className={message.ok ? "success" : "error"} role={message.ok ? "status" : "alert"}>{message.text}</div>}
      <div className="form-actions"><button type="submit" className="btn primary" disabled={busy}>{t(busy ? "keyForm.submitting" : "channels.save")}</button></div>
    </form>
  );
}

function NotesTab({ authIndex, meta, onSaved, onDirty }: { authIndex: string; meta: ChannelMeta; onSaved: (meta: ChannelMeta) => void; onDirty: (dirty: boolean) => void }) {
  const t = useT();
  const ids = useId();
  const [name, setName] = useState(meta.display_name ?? "");
  const [vendor, setVendor] = useState(meta.vendor ?? "");
  const [note, setNote] = useState(meta.note ?? "");
  const [baseline, setBaseline] = useState({ name, vendor, note });
  const [message, setMessage] = useState<{ ok: boolean; text: string } | null>(null);
  const [busy, setBusy] = useState(false);
  const dirty = name !== baseline.name || vendor !== baseline.vendor || note !== baseline.note;
  useEffect(() => onDirty(dirty), [dirty, onDirty]);
  const save = async (event: React.FormEvent) => {
    event.preventDefault();
    setBusy(true);
    setMessage(null);
    try {
      onSaved(await patchChannel({ auth_index: authIndex, display_name: name.trim(), vendor: vendor.trim(), note: note.trim() }));
      setBaseline({ name, vendor, note });
      setMessage({ ok: true, text: t("channels.saved") });
    } catch (reason) {
      setMessage({ ok: false, text: extractApiError(reason, t("channels.saveFailed")) });
    } finally {
      setBusy(false);
    }
  };
  return (
    <form className="detail-form" onSubmit={(event) => void save(event)}>
      <label htmlFor={`${ids}-name`} className="field-label">{t("channels.notes.name")}</label>
      <input id={`${ids}-name`} className="input" maxLength={200} value={name} onChange={(event) => setName(event.target.value)} />
      <p className="field-hint">{t("channels.notes.nameHint")}</p>
      <label htmlFor={`${ids}-vendor`} className="field-label">{t("channels.notes.vendor")}</label>
      <input id={`${ids}-vendor`} className="input" maxLength={200} value={vendor} onChange={(event) => setVendor(event.target.value)} />
      <p className="field-hint">{t("channels.notes.vendorHint")}</p>
      <label htmlFor={`${ids}-note`} className="field-label">{t("channels.notes.note")}</label>
      <textarea id={`${ids}-note`} className="input" rows={4} maxLength={200} value={note} onChange={(event) => setNote(event.target.value)} />
      {message && <div className={message.ok ? "success" : "error"} role={message.ok ? "status" : "alert"}>{message.text}</div>}
      <div className="form-actions"><button type="submit" className="btn primary" disabled={busy}>{t(busy ? "keyForm.submitting" : "channels.save")}</button></div>
    </form>
  );
}

// One channel: an overview of its health, its adapter rules, cost and
// balance settings, and notes. A desktop side panel; a full view on phones.
// The parent keys it by channel, so a new channel starts from scratch. A tab
// stays mounted once opened, so switching tabs keeps what was typed and not
// saved; onDirty tells the parent whether any tab holds such changes.
export default function ChannelDetail({ row, tab, onTab, onClose, onChanged, onDirty }: {
  row: ChannelRow;
  tab: DetailTab;
  onTab: (tab: DetailTab) => void;
  onClose: () => void;
  onChanged: () => void;
  onDirty: (dirty: boolean) => void;
}) {
  const t = useT();
  const ids = useId();
  const authIndex = row.authIndex;
  const [detail, setDetail] = useState<Detail | null>(null);
  const [missing, setMissing] = useState(false);
  const [error, setError] = useState("");
  const [meta, setMeta] = useState<ChannelMeta>(row.view?.meta ?? { auth_index: authIndex });
  const opened = useRef(new Set<DetailTab>());
  opened.current.add(tab);
  const [unsaved, setUnsaved] = useState<Partial<Record<DetailTab, boolean>>>({});
  const reportDirty = useMemo(() => Object.fromEntries(DETAIL_TABS.map((value) => [value, (dirty: boolean) => {
    setUnsaved((current) => (Boolean(current[value]) === dirty ? current : { ...current, [value]: dirty }));
  }])) as Record<DetailTab, (dirty: boolean) => void>, []);
  const anyUnsaved = DETAIL_TABS.some((value) => unsaved[value]);
  useEffect(() => onDirty(anyUnsaved), [anyUnsaved, onDirty]);
  useEffect(() => () => onDirty(false), [onDirty]);

  const load = useCallback(async (alive: () => boolean = () => true) => {
    setError("");
    try {
      const next = await fetchChannelDetail(authIndex);
      if (!alive()) return;
      setDetail(next);
      setMeta(next.meta);
      setMissing(false);
    } catch (reason) {
      if (!alive()) return;
      // The plugin has no record of a credential that never served a request.
      if (apiErrorCode(reason) === "not_found") setMissing(true);
      else setError(extractApiError(reason, t("channels.detailFailed")));
    }
  }, [authIndex, t]);

  useEffect(() => {
    let alive = true;
    void load(() => alive);
    return () => { alive = false; };
  }, [load]);

  const saved = (next: ChannelMeta) => {
    setMeta(next);
    onChanged();
    void load();
  };

  return (
    <aside className="card channel-detail" aria-labelledby={`${ids}-title`}>
      <div className="channel-detail-head">
        <button type="button" className="btn sm mobile-only channel-detail-back" onClick={onClose}>{t("keyUsage.back")}</button>
        <div className="channel-detail-title">
          <h2 id={`${ids}-title`}>{meta.display_name?.trim() || row.name}</h2>
          <div className="channel-detail-sub"><span className="mono">{authIndex}</span><ChannelStateBadge row={row} /><FailurePill summary={detail?.last_24h ?? row.view?.last_24h} /></div>
        </div>
        <button type="button" className="modal-close mobile-hidden" aria-label={t("models.close")} onClick={onClose}>×</button>
      </div>
      <div className="seg channel-tabs" role="tablist" aria-label={t("channels.tabsLabel")}>
        {DETAIL_TABS.map((value) => (
          <button key={value} type="button" role="tab" id={`${ids}-tab-${value}`} aria-controls={opened.current.has(value) ? `${ids}-panel-${value}` : undefined} aria-selected={tab === value}
            className={"seg-btn" + (tab === value ? " active" : "") + (unsaved[value] ? " unsaved" : "")}
            aria-label={unsaved[value] ? t("channels.tabUnsaved", { tab: t(`channels.tab.${value}`) }) : undefined}
            title={unsaved[value] ? t("channels.tabUnsaved", { tab: t(`channels.tab.${value}`) }) : undefined}
            onClick={() => onTab(value)}>
            {t(`channels.tab.${value}`)}
          </button>
        ))}
      </div>
      {DETAIL_TABS.filter((value) => opened.current.has(value)).map((value) => (
        <div key={value} role="tabpanel" id={`${ids}-panel-${value}`} aria-labelledby={`${ids}-tab-${value}`} className="channel-detail-body" hidden={tab !== value}>
          {tab === value && error && (
            <div className="error stats-error" role="alert">
              <span>{error}</span>
              <button type="button" className="btn sm" onClick={() => void load()}>{t("stats.retry")}</button>
            </div>
          )}
          {value === "overview" && (detail || missing || error ? <OverviewTab row={row} detail={detail} missing={missing} /> : <div className="muted" role="status">{t("keys.loading")}</div>)}
          {value === "adapters" && <ChannelAdapters channel={authIndex} onChanged={onChanged} onDirty={reportDirty.adapters} />}
          {value === "cost" && <CostTab authIndex={authIndex} meta={meta} balance={detail?.balance ?? row.view?.balance ?? null} onSaved={saved} onRefreshed={onChanged} onDirty={reportDirty.cost} />}
          {value === "notes" && <NotesTab authIndex={authIndex} meta={meta} onSaved={saved} onDirty={reportDirty.notes} />}
        </div>
      ))}
    </aside>
  );
}
