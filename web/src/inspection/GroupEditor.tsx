import { useState } from "react";
import type { InspectionGroup } from "../api/inspection";
import type { ChannelRow } from "../channelMerge";
import { ALL, channelType, channelTypes, filterChannels, modelCounts, servesModel, type ChannelModels } from "../channelModels";
import ChannelFilter from "../components/ChannelFilter";
import { ChannelStateBadge } from "../components/ChannelState";
import Modal from "../components/Modal";
import { useT } from "../i18n";
import { DEFAULT_EFFORT, EFFORTS } from "../quality/types";
import { groupKey } from "./helpers";

interface Props {
  // The group edited, or null for a new one.
  group: InspectionGroup | null;
  groups: InspectionGroup[];
  rows: ChannelRow[];
  models: ChannelModels | null;
  busy: boolean;
  error: string;
  onCancel: () => void;
  onSave: (group: InspectionGroup) => void;
}

// Picks a model, then the channels asked with it: every channel serving it
// starts checked, and those left unchecked are remembered as ignored, so the
// page later offers only channels new since.
export default function GroupEditor({ group, groups, rows, models, busy, error, onCancel, onSave }: Props) {
  const t = useT();
  const [type, setType] = useState(ALL);
  const [model, setModel] = useState(group?.model ?? ALL);
  const [effort, setEffort] = useState(group?.effort ?? DEFAULT_EFFORT);
  const [checked, setChecked] = useState<ReadonlySet<string>>(() => new Set(group?.channels ?? []));
  const [problem, setProblem] = useState("");

  const chosen = model !== ALL;
  const serving = chosen ? rows.filter((row) => row.authIndex && servesModel(row.authIndex, model, models)) : [];
  // Channels the group asks that no longer serve the model stay listed, so
  // they can be unchecked.
  const listed = [...serving, ...rows.filter((row) => checked.has(row.authIndex) && !serving.includes(row))];
  const visible = type === ALL ? listed : listed.filter((row) => channelType(row) === type);

  const pick = (nextType: string, nextModel: string) => {
    setType(nextType);
    setProblem("");
    if (nextModel !== model) {
      setModel(nextModel);
      setChecked(new Set(nextModel === ALL ? [] : filterChannels(rows, nextType, nextModel, models).map((row) => row.authIndex).filter(Boolean)));
    }
  };
  const toggle = (authIndex: string, on: boolean) => setChecked((current) => {
    const next = new Set(current);
    if (on) next.add(authIndex);
    else next.delete(authIndex);
    return next;
  });
  const save = () => {
    const channels = listed.map((row) => row.authIndex).filter((index) => checked.has(index));
    if (!chosen) return setProblem(t("inspection.editor.chooseModel"));
    if (channels.length === 0) return setProblem(t("inspection.editor.chooseChannels"));
    const key = groupKey({ model, effort });
    if (groups.some((other) => other !== group && groupKey(other) === key)) return setProblem(t("inspection.editor.duplicate", { model, effort }));
    onSave({ model, effort, channels, ignored: serving.map((row) => row.authIndex).filter((index) => !checked.has(index)) });
  };

  const typed = filterChannels(rows, type, ALL, models).map((row) => row.authIndex);
  return (
    <Modal title={t(group ? "inspection.editor.editTitle" : "inspection.editor.newTitle")} closeLabel={t("keyForm.cancel")} onClose={onCancel} dismissible={!busy} wide>
      <div className="inspection-editor">
        <ChannelFilter types={channelTypes(rows)} type={type} models={models && modelCounts(typed, models)} model={model} onType={(value) => pick(value, model)} onModel={(value) => pick(type, value)} />
        <label className="inspection-effort">
          {t("quality.settings.effort")}
          <select value={effort} onChange={(event) => { setEffort(event.target.value); setProblem(""); }}>
            {EFFORTS.map((value) => <option key={value} value={value}>{value === "none" ? t("quality.settings.effortNone") : value}</option>)}
          </select>
        </label>
        <p className="field-hint">{t("inspection.editor.hint")}</p>
        {!chosen ? <div className="empty-state">{t("inspection.editor.chooseModel")}</div> : visible.length === 0 ? (
          <div className="empty-state">{t("inspection.editor.noChannels")}</div>
        ) : (
          <ul className="inspection-pick">
            {visible.map((row) => (
              <li key={row.key}>
                <label className="check-row">
                  <input type="checkbox" checked={checked.has(row.authIndex)} onChange={(event) => toggle(row.authIndex, event.target.checked)} />
                  <span className="channel-name">{row.name}</span>
                </label>
                <span className="mono muted">{row.provider}</span>
                <ChannelStateBadge row={row} />
                {!serving.includes(row) && <span className="badge warn">{t("inspection.editor.notServing")}</span>}
              </li>
            ))}
          </ul>
        )}
        {(problem || error) && <div className="error" role="alert">{problem || error}</div>}
        <div className="form-actions modal-actions">
          <span className="muted">{t("inspection.editor.count", { checked: listed.filter((row) => checked.has(row.authIndex)).length, total: listed.length })}</span>
          <span className="spacer" />
          <button type="button" className="btn" disabled={busy} onClick={onCancel}>{t("keyForm.cancel")}</button>
          <button type="button" className="btn primary" disabled={busy} onClick={save}>{t("inspection.editor.save")}</button>
        </div>
      </div>
    </Modal>
  );
}
