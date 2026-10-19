import { useCallback, useEffect, useId, useMemo, useRef, useState } from "react";
import axios from "axios";
import { deleteAdapter, fetchAdapters, isChannelNotSeen, isTryTimeout, previewAdapter, saveAdapter, tryAdapter, TRY_TIMEOUT_MS, type AdapterPreview, type AdapterTryResult } from "../api/adapters";
import { extractApiError } from "../api/error";
import { ConfirmDialog } from "./Modal";
import { formatCount, formatDateTime, formatMillis } from "../format";
import { useT } from "../i18n";
import { labelOr } from "../labels";
import type { AdapterRule, AdapterRuleType } from "../types";

type Translate = ReturnType<typeof useT>;

// The form keeps paths one per line and the value as JSON text.
export interface AdapterDraft {
  id: string;
  type: string;
  paths: string;
  field: string;
  value: string;
  overwrite: boolean;
  enabled: boolean;
  note: string;
}

export function draftOf(rule: AdapterRule | null, types: AdapterRuleType[]): AdapterDraft {
  if (!rule) return { id: "", type: types[0]?.type ?? "", paths: "", field: "", value: "", overwrite: false, enabled: true, note: "" };
  return {
    id: rule.id, type: rule.type, paths: (rule.paths ?? []).join("\n"), field: rule.field ?? "",
    value: rule.value === undefined ? "" : JSON.stringify(rule.value, null, 2),
    overwrite: Boolean(rule.overwrite), enabled: rule.enabled, note: rule.note ?? "",
  };
}

// The rule a draft describes, or the reason it is not valid yet. A type
// without default paths needs explicit paths.
export function ruleFromDraft(draft: AdapterDraft, channel: string, types: AdapterRuleType[], t: Translate): { rule: AdapterRule } | { error: string } {
  const type = types.find((item) => item.type === draft.type);
  if (!type) return { error: t("adapters.typeRequired") };
  const paths = draft.paths.split("\n").map((path) => path.trim()).filter(Boolean);
  if (paths.some((path) => !path.startsWith("/"))) return { error: t("adapters.pathsInvalid") };
  if (paths.length === 0 && type.default_paths.length === 0) return { error: t("adapters.pathsRequired") };
  const field = draft.field.trim();
  if (type.needs_field && !field) return { error: t("adapters.fieldRequired") };
  let value: unknown;
  if (type.needs_value) {
    if (!draft.value.trim()) return { error: t("adapters.valueRequired") };
    try {
      value = JSON.parse(draft.value);
    } catch {
      return { error: t("adapters.valueInvalid") };
    }
  }
  return {
    rule: {
      id: draft.id, channel, type: type.type, paths, enabled: draft.enabled,
      ...(type.needs_field ? { field } : {}),
      ...(type.needs_value ? { value, overwrite: draft.overwrite } : {}),
      ...(draft.note.trim() ? { note: draft.note.trim() } : {}),
    },
  };
}

// A preview or a real request needs a JSON object as the sample body.
function sampleObject(text: string): Record<string, unknown> | null {
  try {
    const body: unknown = JSON.parse(text);
    return body && typeof body === "object" && !Array.isArray(body) ? body as Record<string, unknown> : null;
  } catch {
    return null;
  }
}

type TryState =
  | { kind: "idle" }
  | { kind: "confirm" }
  | { kind: "sending" }
  | { kind: "done"; result: AdapterTryResult }
  | { kind: "error"; message: string };

// What a saved rule sends back: hits and updated_at belong to the server.
function writable(rule: AdapterRule): AdapterRule {
  const { hits: _hits, updated_at: _updated, ...rest } = rule;
  return rest;
}

function typeLabel(type: string, t: Translate): string {
  return labelOr(t, `adapters.type.${type}`, type);
}

function RuleForm({ channel, types, initial, onSaved, onCancel, onDirty }: {
  channel: string;
  types: AdapterRuleType[];
  initial: AdapterRule | null;
  onSaved: () => void;
  onCancel: () => void;
  onDirty: (dirty: boolean) => void;
}) {
  const t = useT();
  const ids = useId();
  const [draft, setDraft] = useState(() => draftOf(initial, types));
  const [error, setError] = useState("");
  const [saving, setSaving] = useState(false);
  const [sample, setSample] = useState("");
  const [preview, setPreview] = useState<AdapterPreview | null>(null);
  const [previewError, setPreviewError] = useState("");
  const [previewing, setPreviewing] = useState(false);
  const [trying, setTrying] = useState<TryState>({ kind: "idle" });
  // The confirmation and the result open below the form, out of view in a
  // short detail pane; bring them in.
  const tryPanel = useRef<HTMLDivElement>(null);
  useEffect(() => {
    if (trying.kind === "confirm" || trying.kind === "done") tryPanel.current?.scrollIntoView?.({ block: "nearest" });
  }, [trying.kind]);
  // Leaving the form, or editing the sample it was sent with, closes a real
  // request still waiting, which makes CPA cancel it upstream; its result
  // would no longer match the sample shown.
  const tryAbort = useRef<AbortController | null>(null);
  useEffect(() => () => tryAbort.current?.abort(), []);
  // A preview answers for the rule and sample it was sent with; editing either
  // drops the shown preview and any answer still on its way.
  const previewRun = useRef(0);
  const dropPreview = () => {
    previewRun.current += 1;
    setPreview(null);
  };
  const type = types.find((item) => item.type === draft.type);
  const update = (patch: Partial<AdapterDraft>) => {
    setDraft((current) => ({ ...current, ...patch }));
    dropPreview();
  };
  // The sample body is not part of the rule and never counts as a change.
  const pristine = useMemo(() => JSON.stringify(draftOf(initial, types)), [initial, types]);
  const dirty = JSON.stringify(draft) !== pristine;
  useEffect(() => onDirty(dirty), [dirty, onDirty]);
  useEffect(() => () => onDirty(false), [onDirty]);
  // A real request always uses the saved rule.
  const unsaved = initial !== null && dirty;

  const submit = async (event: React.FormEvent) => {
    event.preventDefault();
    const result = ruleFromDraft(draft, channel, types, t);
    if ("error" in result) {
      setError(result.error);
      return;
    }
    setSaving(true);
    setError("");
    try {
      await saveAdapter(result.rule);
      onSaved();
    } catch (reason) {
      setError(extractApiError(reason, t("adapters.saveFailed")));
    } finally {
      setSaving(false);
    }
  };

  const runPreview = async () => {
    setPreview(null);
    const result = ruleFromDraft(draft, channel, types, t);
    if ("error" in result) {
      setPreviewError(result.error);
      return;
    }
    const body = sampleObject(sample);
    if (!body) {
      setPreviewError(t("adapters.previewBodyInvalid"));
      return;
    }
    const run = previewRun.current;
    setPreviewing(true);
    setPreviewError("");
    try {
      const answer = await previewAdapter(result.rule, body);
      if (previewRun.current === run) setPreview(answer);
    } catch (reason) {
      if (previewRun.current === run) setPreviewError(extractApiError(reason, t("adapters.previewFailed")));
    } finally {
      setPreviewing(false);
    }
  };

  const askTry = () => {
    if (!sampleObject(sample)) {
      setTrying({ kind: "error", message: t("adapters.previewBodyInvalid") });
      return;
    }
    setTrying({ kind: "confirm" });
  };
  const runTry = async () => {
    const body = sampleObject(sample);
    if (!initial || !body) return;
    const controller = new AbortController();
    tryAbort.current = controller;
    setTrying({ kind: "sending" });
    try {
      setTrying({ kind: "done", result: await tryAdapter(initial.id, body, controller.signal) });
    } catch (reason) {
      if (axios.isCancel(reason)) return;
      const message = isChannelNotSeen(reason) ? t("adapters.tryNotSeen")
        : isTryTimeout(reason) ? t("adapters.tryTimeout", { minutes: TRY_TIMEOUT_MS / 60_000 })
        : extractApiError(reason, t("adapters.tryFailed"));
      setTrying({ kind: "error", message });
    } finally {
      if (tryAbort.current === controller) tryAbort.current = null;
    }
  };

  return (
    <form className="card adapter-form" onSubmit={(event) => void submit(event)} aria-label={t(initial ? "adapters.editTitle" : "adapters.newTitle")}>
      <h3>{t(initial ? "adapters.editTitle" : "adapters.newTitle")}</h3>
      <label htmlFor={`${ids}-type`} className="field-label">{t("adapters.typeLabel")}</label>
      <select id={`${ids}-type`} className="input" value={draft.type} onChange={(event) => update({ type: event.target.value })}>
        {types.map((item) => <option key={item.type} value={item.type}>{typeLabel(item.type, t)}</option>)}
      </select>
      {type && <p className="field-hint">{t(`adapters.typeHint.${type.type}`)}</p>}
      <label htmlFor={`${ids}-paths`} className="field-label">{t("adapters.pathsLabel")}</label>
      <textarea id={`${ids}-paths`} className="input mono" rows={2} value={draft.paths} onChange={(event) => update({ paths: event.target.value })} placeholder={type?.default_paths.join("\n") || "/v1/chat/completions"} />
      <p className="field-hint">{type && type.default_paths.length > 0 ? t("adapters.pathsDefaultHint", { paths: type.default_paths.join(", ") }) : t("adapters.pathsHint")}</p>
      {type?.needs_field && (
        <>
          <label htmlFor={`${ids}-field`} className="field-label">{t("adapters.fieldLabel")}</label>
          <input id={`${ids}-field`} className="input mono" value={draft.field} onChange={(event) => update({ field: event.target.value })} placeholder="reasoning_effort" />
        </>
      )}
      {type?.needs_value && (
        <>
          <label htmlFor={`${ids}-value`} className="field-label">{t("adapters.valueLabel")}</label>
          <textarea id={`${ids}-value`} className="input mono" rows={2} value={draft.value} onChange={(event) => update({ value: event.target.value })} placeholder='"low"' />
          <p className="field-hint">{t("adapters.valueHint")}</p>
          <label className="check-row"><input type="checkbox" checked={draft.overwrite} onChange={(event) => update({ overwrite: event.target.checked })} />{t("adapters.overwriteLabel")}</label>
        </>
      )}
      <label htmlFor={`${ids}-note`} className="field-label">{t("adapters.noteLabel")}</label>
      <input id={`${ids}-note`} className="input" maxLength={200} value={draft.note} onChange={(event) => update({ note: event.target.value })} />
      <label className="check-row"><input type="checkbox" checked={draft.enabled} onChange={(event) => update({ enabled: event.target.checked })} />{t("adapters.enabledLabel")}</label>

      <div className="adapter-preview">
        <label htmlFor={`${ids}-sample`} className="field-label">{t("adapters.previewLabel")}</label>
        <textarea id={`${ids}-sample`} className="input mono" rows={5} value={sample} onChange={(event) => { tryAbort.current?.abort(); setSample(event.target.value); dropPreview(); setTrying({ kind: "idle" }); }} placeholder='{"model": "gpt-image-2", "images": [{"image_url": "data:image/png;base64,..."}]}' />
        <p className="field-hint">{t("adapters.previewHint")}</p>
        <div className="form-actions">
          <button type="button" className="btn sm" disabled={previewing || !sample.trim()} onClick={() => void runPreview()}>{t(previewing ? "adapters.previewing" : "adapters.preview")}</button>
          <button type="button" className="btn sm danger-outline" disabled={!initial || !sample.trim() || trying.kind === "sending" || trying.kind === "confirm"} onClick={askTry}>{t("adapters.try")}</button>
          {!initial && <span className="field-hint">{t("adapters.trySaveFirst")}</span>}
        </div>
        {previewError && <div className="error" role="alert">{previewError}</div>}
        {preview && (
          <div role="status">
            <p className={preview.changed ? "success" : "muted"}>{t(preview.changed ? "adapters.previewChanged" : "adapters.previewUnchanged")}</p>
            <pre className="code-block">{JSON.stringify(preview.body, null, 2)}</pre>
          </div>
        )}
        {trying.kind === "confirm" && (
          <div className="notice adapter-try-confirm" role="alert" ref={tryPanel}>
            <p>{t("adapters.tryConfirm")}</p>
            {unsaved && <p>{t("adapters.trySavedVersion")}</p>}
            <div className="form-actions">
              <button type="button" className="btn sm danger" onClick={() => void runTry()}>{t("adapters.tryApply")}</button>
              <button type="button" className="btn sm" onClick={() => setTrying({ kind: "idle" })}>{t("keyForm.cancel")}</button>
            </div>
          </div>
        )}
        {trying.kind === "sending" && <p className="muted" role="status">{t("adapters.trying")}</p>}
        {trying.kind === "error" && <div className="error" role="alert">{trying.message}</div>}
        {trying.kind === "done" && (
          <div role="status" data-testid="adapter-try-result" ref={tryPanel}>
            <p>
              <span className={"badge " + (trying.result.status_code >= 200 && trying.result.status_code < 300 ? "ok" : "danger")}>{trying.result.status_code || "—"}</span>
              {" "}{t("adapters.tryLatency", { latency: formatMillis(trying.result.latency_ms) })}
              {trying.result.auth_id && <span className="muted"> · <span className="mono">{trying.result.auth_id}</span></span>}
            </p>
            <pre className="code-block">{trying.result.body || t("adapters.tryEmpty")}</pre>
            {trying.result.truncated && <p className="field-hint">{t("adapters.tryTruncated")}</p>}
          </div>
        )}
      </div>

      {error && <div className="error" role="alert">{error}</div>}
      <div className="form-actions">
        <button type="submit" className="btn primary" disabled={saving}>{t(saving ? "keyForm.submitting" : "adapters.save")}</button>
        <button type="button" className="btn" disabled={saving} onClick={onCancel}>{t("keyForm.cancel")}</button>
      </div>
    </form>
  );
}

// The adapter rules of one channel: list, switch on and off, edit, delete,
// and try a rule on a sample request body.
export default function ChannelAdapters({ channel, onChanged, onDirty }: { channel: string; onChanged: () => void; onDirty: (dirty: boolean) => void }) {
  const t = useT();
  const [rules, setRules] = useState<AdapterRule[] | null>(null);
  const [types, setTypes] = useState<AdapterRuleType[]>([]);
  const [error, setError] = useState("");
  const [editing, setEditing] = useState<AdapterRule | "new" | null>(null);
  const [busy, setBusy] = useState("");
  const [pendingDelete, setPendingDelete] = useState<AdapterRule | null>(null);
  // Switching a rule changes this channel's live requests at once, so it is
  // confirmed like a delete.
  const [pendingToggle, setPendingToggle] = useState<AdapterRule | null>(null);

  const load = useCallback(async () => {
    try {
      const data = await fetchAdapters();
      setRules(data.rules.filter((rule) => rule.channel === channel));
      setTypes(data.types);
      setError("");
    } catch (reason) {
      setError(extractApiError(reason, t("adapters.loadFailed")));
    }
  }, [channel, t]);
  useEffect(() => {
    setRules(null);
    setEditing(null);
    void load();
  }, [load]);

  // Saving or deleting closes the open form, so other rules wait until it is
  // saved or cancelled.
  const changed = async () => {
    setEditing(null);
    await load();
    onChanged();
  };
  const toggle = async () => {
    if (!pendingToggle) return;
    const rule = pendingToggle;
    setBusy(rule.id);
    try {
      await saveAdapter({ ...writable(rule), enabled: !rule.enabled });
      setPendingToggle(null);
      await changed();
    } catch (reason) {
      setPendingToggle(null);
      setError(extractApiError(reason, t("adapters.saveFailed")));
    } finally {
      setBusy("");
    }
  };
  const remove = async () => {
    if (!pendingDelete) return;
    setBusy(pendingDelete.id);
    try {
      await deleteAdapter(pendingDelete.id);
      setPendingDelete(null);
      await changed();
    } catch (reason) {
      setPendingDelete(null);
      setError(extractApiError(reason, t("adapters.deleteFailed")));
    } finally {
      setBusy("");
    }
  };

  const defaultsOf = (type: string) => types.find((item) => item.type === type)?.default_paths ?? [];
  const pathsOf = (rule: AdapterRule) => (rule.paths?.length ? rule.paths : defaultsOf(rule.type)).join(", ");
  return (
    <div className="channel-adapters">
      <p className="muted">{t("adapters.description")}</p>
      {error && <div className="error" role="alert">{error}</div>}
      {rules === null ? (!error && <div className="muted" role="status">{t("keys.loading")}</div>) : (
        <>
          {rules.length === 0 && editing === null && <div className="empty-state">{t("adapters.empty")}</div>}
          <ul className="adapter-list">
            {rules.map((rule) => (
              <li key={rule.id} className="card adapter-rule">
                {editing !== null && editing !== "new" && editing.id === rule.id ? (
                  <RuleForm channel={channel} types={types} initial={rule} onSaved={() => void changed()} onCancel={() => setEditing(null)} onDirty={onDirty} />
                ) : (
                  <>
                    <div className="adapter-rule-head">
                      <strong>{typeLabel(rule.type, t)}</strong>
                      <label className="switch">
                        <input type="checkbox" checked={rule.enabled} disabled={busy === rule.id || editing !== null} onChange={() => setPendingToggle(rule)} aria-label={t("adapters.toggleLabel", { type: typeLabel(rule.type, t), paths: pathsOf(rule) })} />
                        <span className="track"><span className="thumb" /></span>
                        <span>{t(rule.enabled ? "adapters.on" : "adapters.off")}</span>
                      </label>
                    </div>
                    <dl className="detail-facts">
                      <div><dt>{t("adapters.pathsLabel")}</dt><dd className="mono">{rule.paths?.length ? rule.paths.join(", ") : t("adapters.defaultPaths", { paths: defaultsOf(rule.type).join(", ") })}</dd></div>
                      {rule.field && <div><dt>{t("adapters.fieldLabel")}</dt><dd className="mono">{rule.field}</dd></div>}
                      {rule.value !== undefined && rule.type === "set_field" && <div><dt>{t("adapters.valueLabel")}</dt><dd className="mono">{JSON.stringify(rule.value)}{rule.overwrite ? ` · ${t("adapters.overwrites")}` : ""}</dd></div>}
                      <div><dt>{t("adapters.hitsLabel")}</dt><dd>{rule.hits?.count ? t("adapters.hits", { count: formatCount(rule.hits.count), time: formatDateTime(rule.hits.last_at) || "—" }) : t("adapters.noHits")}</dd></div>
                    </dl>
                    {rule.note && <p className="muted adapter-note">{rule.note}</p>}
                    <div className="card-actions">
                      <button type="button" className="btn sm" disabled={editing !== null} onClick={() => setEditing(rule)}>{t("keys.edit")}</button>
                      <button type="button" className="btn sm danger-outline" disabled={busy === rule.id || editing !== null} onClick={() => setPendingDelete(rule)}>{t("keys.delete")}</button>
                    </div>
                  </>
                )}
              </li>
            ))}
          </ul>
          {editing === "new" ? (
            <RuleForm channel={channel} types={types} initial={null} onSaved={() => void changed()} onCancel={() => setEditing(null)} onDirty={onDirty} />
          ) : (
            <button type="button" className="btn" disabled={editing !== null || types.length === 0} onClick={() => setEditing("new")}>+ {t("adapters.new")}</button>
          )}
        </>
      )}
      {pendingDelete && (
        <ConfirmDialog
          title={t("adapters.deleteTitle")}
          message={t("adapters.deleteConfirm", { type: typeLabel(pendingDelete.type, t) })}
          cancelLabel={t("keyForm.cancel")}
          confirmLabel={t("models.confirmDelete")}
          busy={busy === pendingDelete.id}
          onCancel={() => setPendingDelete(null)}
          onConfirm={() => void remove()}
        />
      )}
      {pendingToggle && (
        <ConfirmDialog
          title={t(pendingToggle.enabled ? "adapters.toggleOffTitle" : "adapters.toggleOnTitle")}
          message={t(pendingToggle.enabled ? "adapters.toggleOffConfirm" : "adapters.toggleOnConfirm", { type: typeLabel(pendingToggle.type, t), paths: pathsOf(pendingToggle) })}
          cancelLabel={t("keyForm.cancel")}
          confirmLabel={t(pendingToggle.enabled ? "adapters.toggleOffAction" : "adapters.toggleOnAction")}
          busy={busy === pendingToggle.id}
          onCancel={() => setPendingToggle(null)}
          onConfirm={() => void toggle()}
        />
      )}
    </div>
  );
}
