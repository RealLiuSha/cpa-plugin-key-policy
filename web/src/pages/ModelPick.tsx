import { useCallback, useState } from "react";
import { useLocation, useNavigate } from "react-router-dom";
import ModelPicker from "../components/ModelPicker";
import type { ModelTarget } from "../types";
import { useT } from "../i18n";

export function safeModelFormReturnPath(value: string | undefined): string {
  if (value === "/models/new") return value;
  if (value && /^\/models\/[^/?#]+\/edit$/.test(value)) return value;
  return "/models/new";
}

export default function ModelPick() {
  const navigate = useNavigate();
  const location = useLocation();
  const t = useT();
  const state = location.state as { currentTargets?: ModelTarget[]; returnTo?: string; draftModel?: unknown } | null;
  const [targets, setTargets] = useState<ModelTarget[]>(state?.currentTargets ?? []);
  const onChange = useCallback((next: ModelTarget[]) => setTargets(next), []);
  const returnTo = safeModelFormReturnPath(state?.returnTo);

  const finish = () => navigate(returnTo, { state: { pickedTargets: targets, draftModel: state?.draftModel } });
  const cancel = () => navigate(returnTo, { state: { pickedTargets: state?.currentTargets ?? [], draftModel: state?.draftModel } });

  return (
    <div className="model-pick-page">
      <div className="mp-head">
        <button type="button" className="btn sm" onClick={cancel}>{t("keyUsage.back")}</button>
        <h1>{t("picker.targetTitle")}</h1>
        <button type="button" className="btn primary sm" onClick={finish}>{t("picker.done", { count: targets.length })}</button>
      </div>
      <p className="muted">{t("picker.targetHint")}</p>
      <div className="card mp-list"><ModelPicker initial={state?.currentTargets} onChange={onChange} /></div>
    </div>
  );
}
