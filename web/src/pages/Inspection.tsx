import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { Link, useSearchParams } from "react-router-dom";
import { fetchChannelData, type ChannelData } from "../api/channels";
import { apiErrorCode, extractApiError } from "../api/error";
import { fetchInspection, saveInspection, startInspection, type InspectionCell, type InspectionGroup, type InspectionSettings, type InspectionState } from "../api/inspection";
import { mergeChannels, type ChannelRow } from "../channelMerge";
import { useChannelModels, type ChannelModels } from "../channelModels";
import { ConfirmDialog } from "../components/Modal";
import { formatCount } from "../format";
import { useT } from "../i18n";
import CellDialog from "../inspection/CellDialog";
import GroupEditor from "../inspection/GroupEditor";
import { INSPECTION_DAYS, groupCells, groupKey, newChannels, questionsPerRound, verdictTone, zonedTime } from "../inspection/helpers";
import { channelPath } from "../navigation";
import { useVisiblePolling } from "../polling";
import { CODEX_CONTEXT_TOKENS } from "../quality/types";
import "../inspection/inspection.css";

// While a round runs its progress is read every few seconds.
export const INSPECTION_RUNNING_REFRESH_MS = 2_500;
export const INSPECTION_IDLE_REFRESH_MS = 30_000;
const MAX_TIMES = 6;
const MAX_RUNS = 10;

interface Detail {
  name: string;
  date: string;
  cell: InspectionCell;
}

export default function Inspection() {
  const t = useT();
  const [params, setParams] = useSearchParams();
  const days = INSPECTION_DAYS.find((value) => String(value) === params.get("days")) ?? INSPECTION_DAYS[0];
  const [state, setState] = useState<InspectionState | null>(null);
  const [channels, setChannels] = useState<ChannelData | null>(null);
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);
  const [message, setMessage] = useState<{ ok: boolean; text: string } | null>(null);
  // The group dialog: an index into the groups, or "new".
  const [editing, setEditing] = useState<number | "new" | null>(null);
  const [editError, setEditError] = useState("");
  const [removing, setRemoving] = useState<number | null>(null);
  const [confirmRun, setConfirmRun] = useState(false);
  const [detail, setDetail] = useState<Detail | null>(null);
  const requestID = useRef(0);

  const load = useCallback(async () => {
    const id = ++requestID.current;
    try {
      const next = await fetchInspection(days);
      if (id !== requestID.current) return;
      setState(next);
      setError("");
    } catch (reason) {
      if (id === requestID.current) setError(extractApiError(reason, t("inspection.loadFailed")));
    }
  }, [days, t]);
  const loadChannels = useCallback(async () => {
    try {
      setChannels(await fetchChannelData());
    } catch {
      // Channels are named by their auth_index until the list loads.
    }
  }, []);
  useEffect(() => {
    void load();
    return () => { requestID.current++; };
  }, [load]);
  useEffect(() => { void loadChannels(); }, [loadChannels]);
  const running = Boolean(state?.running);
  useVisiblePolling(() => {
    void load();
    if (!running) void loadChannels();
  }, running ? INSPECTION_RUNNING_REFRESH_MS : INSPECTION_IDLE_REFRESH_MS);

  const rows = useMemo((): ChannelRow[] => channels ? mergeChannels({ config: channels.cpa?.config ?? [], authFiles: channels.cpa?.authFiles ?? [], channels: channels.channels, cpaListed: channels.cpa !== null }) : [], [channels]);
  const byIndex = useMemo(() => new Map(rows.filter((row) => row.authIndex).map((row) => [row.authIndex, row])), [rows]);
  const models = useChannelModels(rows, channels?.ids ?? {});
  const nameOf = (authIndex: string) => byIndex.get(authIndex)?.name ?? authIndex;

  // Every change saves the whole settings at once.
  const save = async (next: InspectionSettings, done: string): Promise<boolean> => {
    setBusy(true);
    setMessage(null);
    try {
      const stored = await saveInspection(next);
      setState((current) => current && { ...current, settings: stored });
      setMessage({ ok: true, text: done });
      void load();
      return true;
    } catch (reason) {
      setMessage({ ok: false, text: extractApiError(reason, t("inspection.saveFailed")) });
      return false;
    } finally {
      setBusy(false);
    }
  };
  const saveGroup = async (group: InspectionGroup) => {
    if (!state || editing === null) return;
    const groups = editing === "new" ? [...state.settings.groups, group] : state.settings.groups.map((item, index) => (index === editing ? group : item));
    setEditError("");
    setBusy(true);
    try {
      const stored = await saveInspection({ ...state.settings, groups });
      setState({ ...state, settings: stored });
      setEditing(null);
      setMessage({ ok: true, text: t("inspection.groupSaved") });
      void load();
    } catch (reason) {
      setEditError(extractApiError(reason, t("inspection.saveFailed")));
    } finally {
      setBusy(false);
    }
  };
  const run = async () => {
    setConfirmRun(false);
    setBusy(true);
    setMessage(null);
    try {
      await startInspection();
      setMessage({ ok: true, text: t("inspection.started") });
    } catch (reason) {
      setMessage({ ok: false, text: apiErrorCode(reason) === "inspection_running" ? t("inspection.alreadyRunning") : extractApiError(reason, t("inspection.startFailed")) });
    } finally {
      setBusy(false);
      void load();
    }
  };

  const settings = state?.settings;
  const questions = settings ? questionsPerRound(settings) : 0;
  return (
    <div className="page inspection-page">
      <div className="page-head">
        <div><h1>{t("inspection.title")}</h1><p className="muted">{t("inspection.description")}</p></div>
        <div className="page-head-actions">
          <button type="button" className="btn primary" disabled={busy || running || questions === 0} onClick={() => setConfirmRun(true)}>{t("inspection.runNow")}</button>
          <button type="button" className="btn" onClick={() => { void load(); void loadChannels(); }}>{t("keys.refresh")}</button>
        </div>
      </div>
      {error && <div className="error stats-error" role="alert"><span>{error}</span><button type="button" className="btn sm" onClick={() => void load()}>{t("stats.retry")}</button></div>}
      {state?.storage_error && <p className="notice" role="status">{t("inspection.storageError", { error: state.storage_error })}</p>}
      {!state || !settings ? (!error && <div className="card muted" role="status">{t("keys.loading")}</div>) : (
        <>
          <Schedule state={state} busy={busy} onSave={(next) => save(next, t("inspection.scheduleSaved"))} />
          {message && <div className={message.ok ? "success" : "error"} role={message.ok ? "status" : "alert"}>{message.text}</div>}
          <div className="inspection-groups-head">
            <h2 className="usage-section-title">{t("inspection.groupsTitle")}</h2>
            <span className="spacer" />
            <div className="seg inspection-days" role="group" aria-label={t("inspection.daysLabel")}>
              {INSPECTION_DAYS.map((value) => (
                <button key={value} type="button" className={"seg-btn" + (value === days ? " active" : "")} aria-pressed={value === days}
                  onClick={() => { const next = new URLSearchParams(params); if (value === INSPECTION_DAYS[0]) next.delete("days"); else next.set("days", String(value)); setParams(next, { replace: true }); }}>
                  {t("inspection.days", { count: value })}
                </button>
              ))}
            </div>
            <button type="button" className="btn" disabled={busy} onClick={() => { setEditError(""); setEditing("new"); }}>{t("inspection.addGroup")}</button>
          </div>
          {settings.groups.length === 0 ? <div className="empty-state">{t("inspection.noGroups")}</div> : settings.groups.map((group, index) => (
            <GroupCard
              key={groupKey(group)}
              group={group}
              state={state}
              rows={rows}
              models={models}
              busy={busy}
              nameOf={nameOf}
              onEdit={() => { setEditError(""); setEditing(index); }}
              onRemove={() => setRemoving(index)}
              onAdd={(added) => void save({ ...settings, groups: settings.groups.map((item, i) => (i === index ? { ...item, channels: [...item.channels, ...added] } : item)) }, t("inspection.groupSaved"))}
              onIgnore={(ignored) => void save({ ...settings, groups: settings.groups.map((item, i) => (i === index ? { ...item, ignored: [...item.ignored, ...ignored] } : item)) }, t("inspection.groupSaved"))}
              onOpen={(date, cell) => setDetail({ name: nameOf(cell.auth_index), date, cell })}
            />
          ))}
        </>
      )}
      {editing !== null && settings && (
        <GroupEditor
          group={editing === "new" ? null : settings.groups[editing]}
          groups={settings.groups}
          rows={rows}
          models={models}
          busy={busy}
          error={editError}
          onCancel={() => setEditing(null)}
          onSave={(group) => void saveGroup(group)}
        />
      )}
      {removing !== null && settings && (
        <ConfirmDialog
          title={t("inspection.removeTitle")}
          message={t("inspection.removeMessage", { model: settings.groups[removing]?.model ?? "" })}
          cancelLabel={t("keyForm.cancel")}
          confirmLabel={t("inspection.remove")}
          onCancel={() => setRemoving(null)}
          onConfirm={() => { const index = removing; setRemoving(null); void save({ ...settings, groups: settings.groups.filter((_, i) => i !== index) }, t("inspection.groupRemoved")); }}
        />
      )}
      {confirmRun && settings && (
        <ConfirmDialog
          title={t("inspection.confirmTitle")}
          message={t("inspection.confirmMessage", { questions: formatCount(questions), tokens: formatCount(questions * CODEX_CONTEXT_TOKENS) })}
          cancelLabel={t("keyForm.cancel")}
          confirmLabel={t("inspection.runNow")}
          onCancel={() => setConfirmRun(false)}
          onConfirm={() => void run()}
        />
      )}
      {detail && state && <CellDialog name={detail.name} date={detail.date} cell={detail.cell} timezone={state.timezone} onClose={() => setDetail(null)} />}
    </div>
  );
}

// The schedule as typed: times of day, questions per channel, on or off.
function Schedule({ state, busy, onSave }: { state: InspectionState; busy: boolean; onSave: (next: InspectionSettings) => Promise<boolean> }) {
  const t = useT();
  const settings = state.settings;
  const [draft, setDraft] = useState({ enabled: settings.enabled, times: settings.times, runs: String(settings.runs) });
  const saved = JSON.stringify({ enabled: settings.enabled, times: settings.times, runs: String(settings.runs) });
  // The plugin keeps times sorted and distinct.
  const dirty = JSON.stringify({ ...draft, times: [...new Set(draft.times)].sort() }) !== saved;
  // A refresh shows what another tab saved unless this one is being edited.
  useEffect(() => {
    if (!dirty) setDraft(JSON.parse(saved));
  }, [saved]); // eslint-disable-line react-hooks/exhaustive-deps
  const runs = Number(draft.runs);
  const valid = Number.isInteger(runs) && runs >= 1 && runs <= MAX_RUNS && draft.times.length > 0 && draft.times.every(Boolean);
  const channels = settings.groups.reduce((sum, group) => sum + group.channels.length, 0);
  const perDay = valid ? channels * runs * draft.times.length : 0;
  const setTime = (index: number, value: string) => setDraft({ ...draft, times: draft.times.map((time, i) => (i === index ? value : time)) });
  return (
    <form className="card inspection-schedule" aria-label={t("inspection.scheduleTitle")} onSubmit={(event) => { event.preventDefault(); if (valid) void onSave({ ...settings, enabled: draft.enabled, times: draft.times, runs }); }}>
      <div className="inspection-schedule-row">
        <label className="switch">
          <input type="checkbox" checked={draft.enabled} onChange={(event) => setDraft({ ...draft, enabled: event.target.checked })} />
          <span className="track"><span className="thumb" /></span>
          <span>{t("inspection.enabled")}</span>
        </label>
        <span className="muted">
          {state.running ? t("inspection.runningProgress", { done: state.running.done, total: state.running.total })
            : state.next_at ? t("inspection.nextAt", { time: zonedTime(state.next_at, state.timezone, true) }) : t("inspection.off")}
        </span>
      </div>
      {state.running && <progress className="inspection-progress" max={state.running.total} value={state.running.done} />}
      <div className="inspection-schedule-row">
        <span className="inspection-label">{t("inspection.times", { timezone: state.timezone })}</span>
        {draft.times.map((time, index) => (
          <span key={index} className="inspection-time">
            <input className="input" type="time" aria-label={t("inspection.timeLabel", { index: index + 1 })} value={time} required onChange={(event) => setTime(index, event.target.value)} />
            {draft.times.length > 1 && <button type="button" className="btn sm" aria-label={t("inspection.removeTime", { time })} onClick={() => setDraft({ ...draft, times: draft.times.filter((_, i) => i !== index) })}>×</button>}
          </span>
        ))}
        {draft.times.length < MAX_TIMES && <button type="button" className="btn sm" onClick={() => setDraft({ ...draft, times: [...draft.times, "12:00"] })}>{t("inspection.addTime")}</button>}
      </div>
      <div className="inspection-schedule-row">
        <label className="inspection-runs">
          {t("inspection.runs")}
          <input className="input" type="number" min={1} max={MAX_RUNS} step={1} value={draft.runs} onChange={(event) => setDraft({ ...draft, runs: event.target.value })} />
        </label>
        <span className="field-hint">{t("inspection.cost", { questions: formatCount(perDay), tokens: formatCount(perDay * CODEX_CONTEXT_TOKENS) })}</span>
        <span className="spacer" />
        {dirty && <button type="button" className="btn" disabled={busy} onClick={() => setDraft(JSON.parse(saved))}>{t("inspection.reset")}</button>}
        <button type="submit" className="btn primary" disabled={busy || !dirty || !valid}>{t("inspection.saveSchedule")}</button>
      </div>
    </form>
  );
}

interface GroupCardProps {
  group: InspectionGroup;
  state: InspectionState;
  rows: ChannelRow[];
  models: ChannelModels | null;
  busy: boolean;
  nameOf: (authIndex: string) => string;
  onEdit: () => void;
  onRemove: () => void;
  onAdd: (authIndexes: string[]) => void;
  onIgnore: (authIndexes: string[]) => void;
  onOpen: (date: string, cell: InspectionCell) => void;
}

// One group: channels new since it was saved, then each channel's days.
function GroupCard({ group, state, rows, models, busy, nameOf, onEdit, onRemove, onAdd, onIgnore, onOpen }: GroupCardProps) {
  const t = useT();
  const fresh = newChannels(group, rows, models);
  const cells = groupCells(state.days, group);
  const today = state.days[0]?.date;
  return (
    <section className="card inspection-group" aria-label={group.model}>
      <div className="inspection-group-head">
        <h3 className="mono">{group.model}</h3>
        <span className="badge">{group.effort === "none" ? t("quality.settings.effortNone") : group.effort}</span>
        <span className="muted">{t("inspection.channelCount", { count: group.channels.length })}</span>
        <span className="spacer" />
        <button type="button" className="btn sm" disabled={busy} onClick={onEdit}>{t("inspection.edit")}</button>
        <button type="button" className="btn sm danger-outline" disabled={busy} onClick={onRemove}>{t("inspection.remove")}</button>
      </div>
      {fresh.length > 0 && (
        <div className="notice inspection-fresh" role="status">
          <span>{t("inspection.fresh", { count: fresh.length, model: group.model, names: fresh.map((row) => row.name).join("、") })}</span>
          <span className="spacer" />
          <button type="button" className="btn sm primary" disabled={busy} onClick={() => onAdd(fresh.map((row) => row.authIndex))}>{t("inspection.freshAdd")}</button>
          <button type="button" className="btn sm" disabled={busy} onClick={() => onIgnore(fresh.map((row) => row.authIndex))}>{t("inspection.freshIgnore")}</button>
        </div>
      )}
      <div className="table-wrap">
        <table className="inspection-table">
          <thead>
            <tr>
              <th>{t("inspection.channel")}</th>
              {state.days.map((day) => <th key={day.date} className="num">{day.date === today ? t("inspection.today") : day.date.slice(5)}</th>)}
            </tr>
          </thead>
          <tbody>
            {group.channels.map((authIndex) => (
              <tr key={authIndex}>
                <td><Link to={channelPath(authIndex)} className="channel-name">{nameOf(authIndex)}</Link></td>
                {state.days.map((day) => {
                  const cell = cells.get(day.date)?.get(authIndex);
                  return (
                    <td key={day.date} className="num">
                      {cell ? (
                        <button type="button" className={`inspection-cell ${verdictTone(cell.verdict)}`} title={t(`inspection.verdict.${cell.verdict}`)} onClick={() => onOpen(day.date, cell)}>
                          {cell.answered > 0 ? `${cell.correct}/${cell.answered}` : t(`inspection.verdict.${cell.verdict}`)}
                        </button>
                      ) : <span className="muted">—</span>}
                    </td>
                  );
                })}
              </tr>
            ))}
          </tbody>
        </table>
      </div>
    </section>
  );
}
