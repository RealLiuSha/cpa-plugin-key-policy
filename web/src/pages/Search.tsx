import { useCallback, useEffect, useId, useRef, useState } from "react";
import { fetchCpaPlugins, fetchPluginConfig } from "../api/cpa";
import { apiErrorCode, extractApiError } from "../api/error";
import { fetchSearch, importSearch, saveSearch } from "../api/search";
import { ConfirmDialog } from "../components/Modal";
import SearchToggle from "../components/SearchToggle";
import { formatCount, formatDateTime } from "../format";
import { useT } from "../i18n";
import { useVisiblePolling } from "../polling";
import { setUnsaved } from "../store/unsaved";
import type { SearchChannel, SearchChannelStatus, SearchSettings, SearchState } from "../types";

type Translate = ReturnType<typeof useT>;

export const SEARCH_REFRESH_MS = 15_000;
export const ALPHA_SEARCH_PLUGIN = "alpha-search-model";
const MIN_PROBE_INTERVAL = 10;

// Numbers are kept as typed so a half-typed value is not rewritten.
export interface SearchDraft {
  target_model: string;
  keep_models: string;
  channels: SearchChannel[];
  interval: string;
  timeout: string;
  probe_model: string;
  url: string;
  user_agent: string;
  idle: string;
  api_key: string;
  clear_key: boolean;
}

export function searchDraftOf(settings: SearchSettings): SearchDraft {
  const probe = settings.probe;
  return {
    target_model: settings.target_model,
    keep_models: settings.keep_models.join("\n"),
    channels: settings.channels.map((channel) => ({ ...channel })),
    interval: String(probe.interval_seconds),
    timeout: String(probe.timeout_seconds),
    probe_model: probe.model ?? "",
    url: probe.url,
    user_agent: probe.user_agent,
    idle: String(probe.idle_seconds),
    api_key: "",
    clear_key: false,
  };
}

const wholeNumber = (value: string, min: number) => {
  const number = Number(value);
  return Number.isInteger(number) && number >= min ? number : null;
};

export function searchSettingsFrom(draft: SearchDraft, t: Translate): { settings: SearchSettings } | { error: string } {
  if (!draft.target_model.trim()) return { error: t("search.targetRequired") };
  const channels = draft.channels.map((channel) => ({ ...channel, name: channel.name.trim(), model: channel.model.trim() }));
  if (channels.length === 0) return { error: t("search.channelsRequired") };
  if (channels.some((channel) => !channel.name || !channel.model)) return { error: t("search.channelIncomplete") };
  if (new Set(channels.map((channel) => channel.name.toLowerCase())).size !== channels.length) return { error: t("search.channelDuplicate") };
  const interval = wholeNumber(draft.interval, MIN_PROBE_INTERVAL);
  const timeout = wholeNumber(draft.timeout, 1);
  const idle = wholeNumber(draft.idle, 0);
  if (interval === null) return { error: t("search.intervalInvalid", { min: MIN_PROBE_INTERVAL }) };
  if (timeout === null || idle === null) return { error: t("search.secondsInvalid") };
  return {
    settings: {
      target_model: draft.target_model.trim(),
      keep_models: draft.keep_models.split(/[\n,]/).map((pattern) => pattern.trim()).filter(Boolean),
      channels,
      probe: {
        interval_seconds: interval,
        timeout_seconds: timeout,
        model: draft.probe_model.trim(),
        url: draft.url.trim(),
        user_agent: draft.user_agent.trim(),
        idle_seconds: idle,
        ...(draft.api_key.trim() ? { api_key: draft.api_key.trim() } : {}),
      },
    },
  };
}

// Where searches go once channel `index` is forced unhealthy: the first other
// channel that is neither forced nor failing its probe. With none left the
// router falls back to the first channel, forced or not. The draft decides
// what is forced; a status is the saved channel of that name and model, and
// its healthy already counts the saved force, so a channel forced there (not
// probed) or new to the draft counts as healthy.
export function nextSearchChannel(channels: SearchChannel[], index: number, statuses: SearchChannelStatus[]): { name: string; fallback: boolean } {
  const healthy = (channel: SearchChannel) => {
    const status = statuses.find((item) => item.name === channel.name && item.model === channel.model);
    return !status || status.forced || status.healthy;
  };
  const next = channels.find((channel, position) => position !== index && !channel.force_unhealthy && healthy(channel));
  return next ? { name: next.name, fallback: false } : { name: channels[0]?.name ?? "", fallback: true };
}

function ChannelEditor({ draft, statuses, onChange }: { draft: SearchDraft; statuses: SearchChannelStatus[]; onChange: (channels: SearchChannel[]) => void }) {
  const t = useT();
  const ids = useId();
  const [confirming, setConfirming] = useState<number | null>(null);
  const channels = draft.channels;
  const set = (index: number, patch: Partial<SearchChannel>) => onChange(channels.map((channel, position) => (position === index ? { ...channel, ...patch } : channel)));
  const move = (index: number, delta: number) => {
    const next = [...channels];
    const [item] = next.splice(index, 1);
    next.splice(index + delta, 0, item);
    setConfirming(null);
    onChange(next);
  };
  return (
    <fieldset className="search-channels">
      <legend className="field-label">{t("search.channelsLabel")}</legend>
      <p className="field-hint">{t("search.channelsHint")}</p>
      <ol className="search-channel-list">
        {channels.map((channel, index) => {
          const next = confirming === index ? nextSearchChannel(channels, index, statuses) : null;
          return (
            <li key={index} className="search-channel">
              <span className="search-channel-order mono" aria-hidden="true">{index + 1}</span>
              <div className="search-channel-fields">
                <div className="search-field">
                  <label htmlFor={`${ids}-name-${index}`} className="field-label">{t("search.channelName")}</label>
                  <input id={`${ids}-name-${index}`} className="input" value={channel.name} onChange={(event) => set(index, { name: event.target.value })} />
                </div>
                <div className="search-field">
                  <label htmlFor={`${ids}-model-${index}`} className="field-label">{t("search.channelTemplate")}</label>
                  <input id={`${ids}-model-${index}`} className="input mono" value={channel.model} onChange={(event) => set(index, { model: event.target.value })} placeholder="{model}" />
                </div>
                <label className="switch">
                  <input type="checkbox" checked={channel.force_unhealthy} aria-label={t("search.forceLabelFor", { name: channel.name || String(index + 1) })} onChange={() => {
                    if (channel.force_unhealthy) set(index, { force_unhealthy: false });
                    else setConfirming(index);
                  }} />
                  <span className="track"><span className="thumb" /></span>
                  <span>{t("search.forceLabel")}</span>
                </label>
                {next && (
                  <div className="notice search-force-confirm" role="alert">
                    <p>{next.fallback ? t("search.forceConfirmFallback", { name: next.name }) : t("search.forceConfirm", { name: next.name })}</p>
                    <div className="form-actions">
                      <button type="button" className="btn sm danger" onClick={() => { set(index, { force_unhealthy: true }); setConfirming(null); }}>{t("search.forceApply")}</button>
                      <button type="button" className="btn sm" onClick={() => setConfirming(null)}>{t("keyForm.cancel")}</button>
                    </div>
                  </div>
                )}
              </div>
              <div className="search-channel-actions">
                <button type="button" className="btn sm" disabled={index === 0} aria-label={t("search.moveUp", { name: channel.name || index + 1 })} onClick={() => move(index, -1)}>↑</button>
                <button type="button" className="btn sm" disabled={index === channels.length - 1} aria-label={t("search.moveDown", { name: channel.name || index + 1 })} onClick={() => move(index, 1)}>↓</button>
                <button type="button" className="btn sm danger-outline" disabled={channels.length === 1} aria-label={t("search.removeChannel", { name: channel.name || index + 1 })} onClick={() => { setConfirming(null); onChange(channels.filter((_, position) => position !== index)); }}>×</button>
              </div>
            </li>
          );
        })}
      </ol>
      <button type="button" className="btn sm" onClick={() => onChange([...channels, { name: "", model: "{model}", force_unhealthy: false }])}>+ {t("search.addChannel")}</button>
    </fieldset>
  );
}

function StatusTable({ state }: { state: SearchState }) {
  const t = useT();
  const status = state.status;
  return (
    <section className="stats-section" data-testid="search-status">
      <h2 className="usage-section-title">{t("search.statusTitle")}</h2>
      <p className="muted search-status-summary">
        {t("search.statusSummary", { routed: formatCount(status.routed_total), last: formatDateTime(status.last_search_at) || t("search.never") })}
        {" · "}{t(status.probe_running ? "search.probeRunning" : "search.probeStopped")}
      </p>
      {status.channels.length === 0 ? <div className="empty-state">{t("search.statusEmpty")}</div> : (
        <div className="card table-wrap search-status-table">
          <table>
            <thead>
              <tr>
                <th>{t("search.channelName")}</th><th>{t("search.channelTemplate")}</th><th>{t("search.col.health")}</th>
                <th className="num">{t("search.col.routed")}</th><th>{t("search.col.lastRouted")}</th><th>{t("search.col.lastProbe")}</th><th>{t("search.col.probeError")}</th>
              </tr>
            </thead>
            <tbody>
              {status.channels.map((channel) => (
                <tr key={channel.name}>
                  <td><strong>{channel.name}</strong></td>
                  <td className="mono">{channel.model}</td>
                  <td>
                    <span className={"badge" + (channel.forced ? " warn" : channel.healthy ? " ok" : " danger")}>{t(channel.forced ? "search.health.forced" : channel.healthy ? "search.health.healthy" : "search.health.unhealthy")}</span>
                  </td>
                  <td className="num mono">{formatCount(channel.routed)}</td>
                  <td>{formatDateTime(channel.last_routed_at) || "—"}</td>
                  <td>{formatDateTime(channel.last_probe_at) || "—"}</td>
                  <td className="search-probe-error">{channel.last_probe_error || "—"}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </section>
  );
}

export default function Search() {
  const t = useT();
  const ids = useId();
  const [state, setState] = useState<SearchState | null>(null);
  const [draft, setDraft] = useState<SearchDraft | null>(null);
  const [error, setError] = useState("");
  const [message, setMessage] = useState<{ ok: boolean; text: string } | null>(null);
  const [saving, setSaving] = useState(false);
  const [canImport, setCanImport] = useState(false);
  const [confirmImport, setConfirmImport] = useState(false);

  // Refreshes keep the form: only the switch and the live status follow. A
  // write bumps the generation when it starts and when it ends, so a refresh
  // that was already under way cannot bring back the settings it replaced.
  const generation = useRef(0);
  const refresh = useCallback(async () => {
    const mine = ++generation.current;
    try {
      const next = await fetchSearch();
      if (mine !== generation.current) return;
      setState(next);
      setDraft((current) => current ?? searchDraftOf(next.settings));
      setError("");
    } catch (reason) {
      if (mine === generation.current) setError(extractApiError(reason, t("search.loadFailed")));
    }
  }, [t]);
  useEffect(() => { void refresh(); }, [refresh]);
  useVisiblePolling(() => void refresh(), SEARCH_REFRESH_MS);
  useEffect(() => {
    let alive = true;
    fetchCpaPlugins()
      .then((plugins) => { if (alive) setCanImport(plugins.some((plugin) => plugin.id === ALPHA_SEARCH_PLUGIN)); })
      .catch(() => { /* No plugin list, no import. */ });
    return () => { alive = false; };
  }, []);

  const save = async (event: React.FormEvent) => {
    event.preventDefault();
    if (!draft) return;
    const result = searchSettingsFrom(draft, t);
    if ("error" in result) return setMessage({ ok: false, text: result.error });
    setSaving(true);
    setMessage(null);
    generation.current++;
    try {
      const saved = await saveSearch(result.settings, draft.clear_key);
      generation.current++;
      setState((current) => current && { ...current, ...saved });
      setDraft(searchDraftOf(saved.settings));
      setMessage({ ok: true, text: t("search.saved") });
    } catch (reason) {
      setMessage({ ok: false, text: extractApiError(reason, t("search.saveFailed")) });
    } finally {
      setSaving(false);
    }
  };
  const runImport = async () => {
    setConfirmImport(false);
    setSaving(true);
    setMessage(null);
    generation.current++;
    try {
      const imported = await importSearch(await fetchPluginConfig(ALPHA_SEARCH_PLUGIN));
      generation.current++;
      setDraft(searchDraftOf(imported));
      setState((current) => current && { ...current, settings: imported });
      setMessage({ ok: true, text: t("search.imported") });
      void refresh();
    } catch (reason) {
      // Without a configuration block there is nothing to import, and the
      // plugin keeps the current settings.
      setMessage({ ok: false, text: apiErrorCode(reason) === "nothing_to_import" ? t("search.importEmpty") : extractApiError(reason, t("search.importFailed")) });
    } finally {
      setSaving(false);
    }
  };

  const dirty = Boolean(state && draft && JSON.stringify(draft) !== JSON.stringify(searchDraftOf(state.settings)));
  // The section links ask before they drop the draft.
  useEffect(() => setUnsaved("search", dirty ? "search.leaveMessage" : null), [dirty]);
  useEffect(() => () => setUnsaved("search", null), []);
  const intervalSeconds = draft ? wholeNumber(draft.interval, MIN_PROBE_INTERVAL) : null;
  const keyStored = Boolean(state?.status.probe_key_stored);
  const update = (patch: Partial<SearchDraft>) => setDraft((current) => current && { ...current, ...patch });

  return (
    <div className="page search-page">
      <div className="page-head">
        <div><h1>{t("search.title")}</h1><p className="muted">{t("search.description")}</p></div>
        {canImport && (
          <div className="page-head-actions">
            <button type="button" className="btn" disabled={saving || !state} onClick={() => setConfirmImport(true)}>{t("search.import")}</button>
          </div>
        )}
      </div>
      {error && (
        <div className="error stats-error" role="alert">
          <span>{error}</span>
          <button type="button" className="btn sm" onClick={() => void refresh()}>{t("stats.retry")}</button>
        </div>
      )}
      {!state || !draft ? (!error && <div className="card muted" role="status">{t("keys.loading")}</div>) : (
        <>
          <section className="card">
            <SearchToggle enabled={state.enabled} declared={state.declared} onChanged={(next) => setState((current) => current && { ...current, enabled: next.enabled, declared: next.declared, status: next.status })} />
          </section>
          <StatusTable state={state} />
          <form className="card search-form" onSubmit={(event) => void save(event)} aria-label={t("search.settingsTitle")}>
            <h2 className="usage-section-title">{t("search.settingsTitle")}</h2>
            <div className="form-grid">
              <label>
                {t("search.targetModel")}
                <input className="input mono" value={draft.target_model} onChange={(event) => update({ target_model: event.target.value })} />
                <span className="field-hint">{t("search.targetHint")}</span>
              </label>
              <label>
                {t("search.keepModels")}
                <textarea className="input mono" rows={3} value={draft.keep_models} onChange={(event) => update({ keep_models: event.target.value })} placeholder="gpt-*" />
                <span className="field-hint">{t("search.keepHint")}</span>
              </label>
            </div>
            <ChannelEditor draft={draft} statuses={state.status.channels} onChange={(channels) => update({ channels })} />

            <h3 className="search-subtitle">{t("search.probeTitle")}</h3>
            <p className="field-hint">{t("search.probeHint")}</p>
            <div className="form-grid">
              <label>{t("search.probeInterval")}<input className="input" type="number" min={MIN_PROBE_INTERVAL} step={1} value={draft.interval} onChange={(event) => update({ interval: event.target.value })} /></label>
              <label>{t("search.probeTimeout")}<input className="input" type="number" min={1} step={1} value={draft.timeout} onChange={(event) => update({ timeout: event.target.value })} /></label>
              <label>{t("search.probeIdle")}<input className="input" type="number" min={0} step={1} value={draft.idle} onChange={(event) => update({ idle: event.target.value })} /><span className="field-hint">{t("search.probeIdleHint")}</span></label>
              <label>{t("search.probeModel")}<input className="input mono" value={draft.probe_model} onChange={(event) => update({ probe_model: event.target.value })} placeholder={draft.target_model} /><span className="field-hint">{t("search.probeModelHint")}</span></label>
              <label>{t("search.probeUrl")}<input className="input mono" value={draft.url} onChange={(event) => update({ url: event.target.value })} /></label>
              <label>{t("search.probeUserAgent")}<input className="input mono" value={draft.user_agent} onChange={(event) => update({ user_agent: event.target.value })} /></label>
            </div>
            <div className="form-grid">
              <label htmlFor={`${ids}-key`}>
                {t("search.probeKey")}
                <input id={`${ids}-key`} className="input" type="password" autoComplete="new-password" value={draft.api_key} disabled={draft.clear_key} onChange={(event) => update({ api_key: event.target.value })} placeholder={t(keyStored ? "search.probeKeyStored" : "search.probeKeyEmpty")} />
                <span className="field-hint">{t("search.probeKeyHint")}</span>
              </label>
              {keyStored && (
                <label className="check-row"><input type="checkbox" checked={draft.clear_key} onChange={(event) => update({ clear_key: event.target.checked, api_key: "" })} />{t("search.probeKeyClear")}</label>
              )}
            </div>
            <p className="notice info search-probe-cost" role="note">
              {draft.channels.length < 2 ? t("search.probeCostSingle")
                : !keyStored && !draft.api_key.trim() ? t("search.probeCostNoKey")
                : intervalSeconds ? t("search.probeCost", { count: formatCount(86400 / intervalSeconds), interval: intervalSeconds }) : t("search.intervalInvalid", { min: MIN_PROBE_INTERVAL })}
            </p>
            {message && <div className={message.ok ? "success" : "error"} role={message.ok ? "status" : "alert"}>{message.text}</div>}
            <div className="form-actions">
              <button type="submit" className="btn primary" disabled={saving}>{t(saving ? "keyForm.submitting" : "search.save")}</button>
              <button type="button" className="btn" disabled={saving || !dirty} onClick={() => { setDraft(searchDraftOf(state.settings)); setMessage(null); }}>{t("search.discard")}</button>
              {dirty && <span className="muted">{t("search.unsaved")}</span>}
            </div>
          </form>
        </>
      )}
      {confirmImport && (
        <ConfirmDialog
          title={t("search.importTitle")}
          message={t("search.importConfirm")}
          cancelLabel={t("keyForm.cancel")}
          confirmLabel={t("search.importApply")}
          onCancel={() => setConfirmImport(false)}
          onConfirm={() => void runImport()}
        />
      )}
    </div>
  );
}
