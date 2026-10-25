import { useEffect, useId, useState } from "react";
import { apiErrorCode, extractApiError } from "../api/error";
import { probeNativeSearch, saveNativeSearch } from "../api/search";
import { formatDateTime, formatMillis } from "../format";
import { useT } from "../i18n";
import { setUnsaved } from "../store/unsaved";
import type { ModuleStates, NativeChannelState, NativeMode, NativeSettings, NativeState } from "../types";
import { ConfirmDialog } from "./Modal";

// While a probe round runs the page follows its progress this often.
export const NATIVE_ROUND_REFRESH_MS = 3_000;
const MAX_INTERVAL_HOURS = 168;

const stateTone: Record<NativeChannelState, string> = {
  supported: "ok", unstable: "warn", stale: "warn", unknown: "", no_search: "danger", failing: "danger", rejected: "danger", disabled: "",
};
const modeTone: Record<NativeMode, string> = { native: "ok", fallback: "warn", excluded: "", off: "" };
// The answers to a check request that have their own message.
const probeErrors: Record<string, string> = { busy: "search.native.probeBusy", search_off: "search.native.probeOff", not_found: "search.native.probeNothing" };

type Confirm = { kind: "on" | "off" } | { kind: "disable"; authIndex: string };

// nativeModuleState is the overview and settings line for native search:
// how many catalog models search natively and how many fall back, once it is
// on and registered with CPA.
export function nativeModuleState(modules: ModuleStates, t: ReturnType<typeof useT>): string {
  const native = modules.native_search;
  if (!native) return t("overview.moduleUnavailable");
  if (!modules.search.enabled || !native.enabled) return t("overview.searchState.off");
  if (!native.declared) return t("overview.searchState.applying");
  return t("overview.nativeCount", { native: native.native_models, fallback: native.fallback_models });
}

// Native search on the search page: whether each Codex catalog model searches
// through the channel serving the conversation or falls back to alpha search,
// the probe evidence per channel, and the operator's switches. searchEnabled
// is the search module switch, which native search needs as well.
export default function NativeSearch({ native, searchEnabled, onChanged, onRefresh }: {
  native: NativeState;
  searchEnabled: boolean;
  onChanged: (next: NativeState) => void;
  onRefresh: () => void;
}) {
  const t = useT();
  const ids = useId();
  const settings = native.settings;
  const [busy, setBusy] = useState<string | null>(null);
  const [message, setMessage] = useState<{ ok: boolean; text: string } | null>(null);
  const [confirm, setConfirm] = useState<Confirm | null>(null);
  const [requireAll, setRequireAll] = useState(settings.require_all);
  const [interval, setIntervalHours] = useState(String(settings.interval_hours));

  // A saved change elsewhere (another tab, a refresh) resets the draft.
  useEffect(() => {
    setRequireAll(settings.require_all);
    setIntervalHours(String(settings.interval_hours));
  }, [settings.require_all, settings.interval_hours]);
  const round = native.round;
  useEffect(() => {
    if (!round) return undefined;
    const timer = window.setInterval(onRefresh, NATIVE_ROUND_REFRESH_MS);
    return () => window.clearInterval(timer);
  }, [round, onRefresh]);

  const name = (authIndex: string) => {
    const channel = native.channels.find((item) => item.auth_index === authIndex);
    return native.names[authIndex] || channel?.group || channel?.name || authIndex;
  };
  const save = async (patch: Partial<NativeSettings>, key: string, done: string) => {
    setBusy(key);
    setMessage(null);
    try {
      onChanged(await saveNativeSearch({ ...settings, ...patch }));
      setMessage({ ok: true, text: done });
    } catch (reason) {
      setMessage({ ok: false, text: extractApiError(reason, t("search.native.saveFailed")) });
    } finally {
      setBusy(null);
    }
  };
  const probe = async (authIndex?: string) => {
    setBusy(authIndex ? `probe:${authIndex}` : "probe");
    setMessage(null);
    try {
      onChanged(await probeNativeSearch(authIndex));
      setMessage({ ok: true, text: authIndex ? t("search.native.probeOneStarted", { name: name(authIndex) }) : t("search.native.probeStarted") });
    } catch (reason) {
      const known = probeErrors[apiErrorCode(reason) ?? ""];
      setMessage({ ok: false, text: known ? t(known) : extractApiError(reason, t("search.native.probeFailed")) });
    } finally {
      setBusy(null);
    }
  };
  const runConfirm = () => {
    const current = confirm;
    setConfirm(null);
    if (!current) return;
    if (current.kind === "disable") {
      void save({ disabled_channels: [...settings.disabled_channels, current.authIndex] }, `channel:${current.authIndex}`, t("search.native.channelDisabled", { name: name(current.authIndex) }));
    } else {
      void save({ enabled: current.kind === "on" }, "toggle", t(current.kind === "on" ? "search.native.turnedOn" : "search.native.turnedOff"));
    }
  };

  const hours = Number(interval);
  const intervalValid = Number.isInteger(hours) && hours >= 1 && hours <= MAX_INTERVAL_HOURS;
  const policyDirty = requireAll !== settings.require_all || interval !== String(settings.interval_hours);
  // The section links ask before they drop the draft.
  useEffect(() => setUnsaved("search-native", policyDirty ? "search.leaveMessage" : null), [policyDirty]);
  useEffect(() => () => setUnsaved("search-native", null), []);
  const savePolicy = (event: React.FormEvent) => {
    event.preventDefault();
    if (!intervalValid) return setMessage({ ok: false, text: t("search.native.intervalInvalid", { max: MAX_INTERVAL_HOURS }) });
    void save({ require_all: requireAll, interval_hours: hours }, "policy", t("search.native.policySaved"));
  };
  const working = busy !== null;
  const unavailable = Boolean(native.error);
  // Rounds run only while native search and the search module are on, and
  // only on channels CPA routes to.
  const canProbe = !working && !unavailable && !round && settings.enabled && searchEnabled && native.running;
  const probeable = native.channels.some((channel) => !channel.cpa_disabled);

  return (
    <section className="card native-search" aria-labelledby={`${ids}-title`} data-testid="native-search">
      <div className="native-head">
        <div>
          <h2 id={`${ids}-title`} className="usage-section-title">{t("search.native.title")}</h2>
          <p className="field-hint">{t("search.native.description")}</p>
        </div>
        <div className="native-head-actions">
          <label className="switch">
            <input type="checkbox" role="switch" checked={settings.enabled} disabled={working || unavailable} onChange={() => setConfirm({ kind: settings.enabled ? "off" : "on" })} />
            <span className="track"><span className="thumb" /></span>
            <span>{t("search.native.enabledLabel")}</span>
          </label>
          <button type="button" className="btn" disabled={!canProbe || !probeable} onClick={() => void probe()}>
            {t(busy === "probe" ? "search.native.probing" : "search.native.probeAll")}
          </button>
        </div>
      </div>

      {native.error && <div className="error" role="alert">{t("search.native.moduleError", { error: native.error })}</div>}
      {!unavailable && !searchEnabled && <p className="notice" role="note">{t("search.native.moduleOff")}</p>}
      {!unavailable && searchEnabled && !native.declared && <p className="notice" role="note">{t("search.native.notDeclared")}</p>}
      {native.credentials_error && <p className="notice" role="alert">{t("search.native.credentialsError", { error: native.credentials_error })}</p>}
      <p className="muted native-schedule" role="status">
        {round
          ? t("search.native.roundRunning", { kind: t(`search.native.round.${round.kind}`), done: round.done, total: round.total })
          : settings.enabled && native.running
            ? t("search.native.schedule", { last: formatDateTime(native.last_full_round) || t("search.never"), next: formatDateTime(native.next_full_round) || "—", hours: settings.interval_hours })
            : t("search.native.scheduleIdle", { last: formatDateTime(native.last_full_round) || t("search.never") })}
      </p>
      {message && <div className={message.ok ? "success" : "error"} role={message.ok ? "status" : "alert"}>{message.text}</div>}

      <h3 className="search-subtitle">{t("search.native.modelsTitle")}</h3>
      {native.models.length === 0 ? <div className="empty-state">{t("search.native.modelsEmpty")}</div> : (
        <div className="card table-wrap native-table">
          <table>
            <thead>
              <tr>
                <th>{t("search.native.col.model")}</th><th>{t("search.native.col.mode")}</th><th>{t("search.native.col.usable")}</th>
                <th>{t("search.native.col.gaps")}</th><th>{t("search.native.col.actions")}</th>
              </tr>
            </thead>
            <tbody>
              {native.models.map((model) => {
                const excluded = settings.excluded_models.some((item) => item.toLowerCase() === model.model.toLowerCase());
                return (
                  <tr key={model.model} data-testid={`native-model-${model.model}`}>
                    <td className="mono native-model"><strong>{model.model}</strong></td>
                    <td>
                      <span className={`badge ${modeTone[model.mode]}`}>{t(`search.native.mode.${model.mode}`)}</span>
                      {model.mode === "fallback" && model.reason && <div className="field-hint">{t(`search.native.reason.${model.reason}`)}</div>}
                    </td>
                    <td>{model.usable.length === 0 ? "—" : (
                      <span className="native-chips">{model.usable.map((index) => <span key={index} className="native-chip ok">{name(index)}</span>)}</span>
                    )}</td>
                    <td>{model.gaps.length === 0 ? <span className="muted">{t("search.native.noGaps")}</span> : (
                      <span className="native-chips">
                        {model.gaps.map((gap) => (
                          <span key={gap.auth_index} className={`native-chip ${stateTone[gap.state]}`}>{name(gap.auth_index)} · {t(`search.native.state.${gap.state}`)}</span>
                        ))}
                      </span>
                    )}</td>
                    <td className="native-row-actions">
                      <button type="button" className="btn sm" disabled={working || unavailable}
                        aria-label={t(excluded ? "search.native.includeFor" : "search.native.excludeFor", { model: model.model })}
                        onClick={() => void save(
                          { excluded_models: excluded ? settings.excluded_models.filter((item) => item.toLowerCase() !== model.model.toLowerCase()) : [...settings.excluded_models, model.model] },
                          `model:${model.model}`, t(excluded ? "search.native.modelIncluded" : "search.native.modelExcluded", { model: model.model }),
                        )}>
                        {t(excluded ? "search.native.include" : "search.native.exclude")}
                      </button>
                    </td>
                  </tr>
                );
              })}
            </tbody>
          </table>
        </div>
      )}
      <p className="notice info native-note" role="note">{t("search.native.gapNote")}</p>

      <h3 className="search-subtitle">{t("search.native.channelsTitle")}</h3>
      {native.channels.length === 0 ? <div className="empty-state">{t("search.native.channelsEmpty")}</div> : (
        <div className="card table-wrap native-table">
          <table>
            <thead>
              <tr>
                <th>{t("search.native.col.channel")}</th><th>{t("search.native.col.models")}</th><th>{t("search.native.col.state")}</th>
                <th>{t("search.native.col.lastProbe")}</th><th>{t("search.native.col.history")}</th><th>{t("search.native.col.rejections")}</th>
                <th>{t("search.native.col.actions")}</th>
              </tr>
            </thead>
            <tbody>
              {native.channels.map((channel) => {
                const label = name(channel.auth_index);
                const last = channel.probes[0];
                const disabled = settings.disabled_channels.includes(channel.auth_index);
                return (
                  <tr key={channel.auth_index} data-testid={`native-channel-${channel.auth_index}`}>
                    <td>
                      <strong>{label}</strong>
                      <div className="field-hint mono">{channel.name && channel.name !== label ? channel.name : channel.auth_index}</div>
                      {channel.cpa_disabled && <span className="badge">{t("search.native.cpaDisabled")}</span>}
                      {channel.unavailable && <span className="badge warn">{t("search.native.cooling")}</span>}
                    </td>
                    <td className="mono native-models">{channel.models.join(", ")}</td>
                    <td><span className={`badge ${stateTone[channel.state]}`}>{t(`search.native.state.${channel.state}`)}</span></td>
                    <td>
                      {last ? (
                        <>
                          {formatDateTime(last.at)}
                          <div className="field-hint">
                            {last.model} · {t(`search.native.result.${last.result}`)} · {formatMillis(last.latency_ms)}
                            {last.error && <span className="native-error"> · {last.error}</span>}
                          </div>
                        </>
                      ) : "—"}
                    </td>
                    <td>
                      {channel.probes.length === 0 ? "—" : (
                        <span className="native-history" role="img" aria-label={t("search.native.historyLabel", { results: channel.probes.map((probe) => t(`search.native.result.${probe.result}`)).join(", ") })}>
                          {[...channel.probes].reverse().map((probe, index) => (
                            <span key={index} className={`native-dot result-${probe.result}`} title={`${formatDateTime(probe.at)} · ${probe.model} · ${t(`search.native.result.${probe.result}`)}`} />
                          ))}
                        </span>
                      )}
                    </td>
                    <td>
                      {channel.rejection ? (
                        <>
                          {channel.rejection.count}
                          <div className="field-hint">{formatDateTime(channel.rejection.at)} · HTTP {channel.rejection.status}</div>
                        </>
                      ) : "—"}
                    </td>
                    <td className="native-row-actions">
                      <button type="button" className="btn sm" disabled={!canProbe || channel.cpa_disabled}
                        aria-label={t("search.native.probeOneFor", { name: label })} onClick={() => void probe(channel.auth_index)}>
                        {t(busy === `probe:${channel.auth_index}` ? "search.native.probing" : "search.native.probeOne")}
                      </button>
                      <button type="button" className={`btn sm${disabled ? "" : " danger-outline"}`} disabled={working || unavailable}
                        aria-label={t(disabled ? "search.native.enableFor" : "search.native.disableFor", { name: label })}
                        onClick={() => disabled
                          ? void save({ disabled_channels: settings.disabled_channels.filter((item) => item !== channel.auth_index) }, `channel:${channel.auth_index}`, t("search.native.channelEnabled", { name: label }))
                          : setConfirm({ kind: "disable", authIndex: channel.auth_index })}>
                        {t(disabled ? "search.native.enableChannel" : "search.native.disableChannel")}
                      </button>
                    </td>
                  </tr>
                );
              })}
            </tbody>
          </table>
        </div>
      )}

      <form className="native-policy" onSubmit={savePolicy} aria-label={t("search.native.policyTitle")}>
        <h3 className="search-subtitle">{t("search.native.policyTitle")}</h3>
        <fieldset className="native-coverage">
          <legend className="field-label">{t("search.native.coverageLabel")}</legend>
          <label className="check-row">
            <input type="radio" name={`${ids}-coverage`} checked={!requireAll} onChange={() => setRequireAll(false)} />
            <span>{t("search.native.coverageAny")}<span className="field-hint">{t("search.native.coverageAnyHint")}</span></span>
          </label>
          <label className="check-row">
            <input type="radio" name={`${ids}-coverage`} checked={requireAll} onChange={() => setRequireAll(true)} />
            <span>{t("search.native.coverageAll")}<span className="field-hint">{t("search.native.coverageAllHint")}</span></span>
          </label>
        </fieldset>
        <label className="native-interval">
          {t("search.native.intervalLabel")}
          <input className="input" type="number" min={1} max={MAX_INTERVAL_HOURS} step={1} value={interval} onChange={(event) => setIntervalHours(event.target.value)} />
          <span className="field-hint">{intervalValid ? t("search.native.intervalHint", { stale: hours * 2 + 1 }) : t("search.native.intervalInvalid", { max: MAX_INTERVAL_HOURS })}</span>
        </label>
        <div className="form-actions">
          <button type="submit" className="btn primary" disabled={working || unavailable || !policyDirty}>{t(busy === "policy" ? "keyForm.submitting" : "search.native.policySave")}</button>
          <button type="button" className="btn" disabled={working || !policyDirty} onClick={() => { setRequireAll(settings.require_all); setIntervalHours(String(settings.interval_hours)); }}>{t("search.discard")}</button>
          {policyDirty && <span className="muted">{t("search.unsaved")}</span>}
        </div>
      </form>

      {confirm && (
        <ConfirmDialog
          title={t(confirm.kind === "disable" ? "search.native.disableTitle" : confirm.kind === "on" ? "search.native.onTitle" : "search.native.offTitle")}
          message={confirm.kind === "disable" ? t("search.native.disableConfirm", { name: name(confirm.authIndex) }) : t(confirm.kind === "on" ? "search.native.onConfirm" : "search.native.offConfirm")}
          cancelLabel={t("keyForm.cancel")}
          confirmLabel={t(confirm.kind === "disable" ? "search.native.disableApply" : confirm.kind === "on" ? "search.native.onApply" : "search.native.offApply")}
          onCancel={() => setConfirm(null)}
          onConfirm={runConfirm}
        />
      )}
    </section>
  );
}
