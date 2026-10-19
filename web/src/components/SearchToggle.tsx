import { useEffect, useRef, useState } from "react";
import { extractApiError } from "../api/error";
import { SEARCH_APPLY_TIMEOUT_MS, SearchApplyTimeout, setSearchEnabled } from "../api/search";
import { useT } from "../i18n";
import type { SearchState } from "../types";
import { ConfirmDialog } from "./Modal";

type Phase =
  | { kind: "idle" }
  | { kind: "waiting"; target: boolean; seconds: number }
  | { kind: "done"; target: boolean }
  | { kind: "timeout"; target: boolean }
  | { kind: "error"; message: string };

// The search module switch, shared by the search and settings pages. After a
// confirmation, since it changes routing and may start paid probes, it saves
// search_enabled through CPA and then waits for the plugin to register (or
// drop) its search router; enabled and declared are the last known state.
// unavailable: the search module could not open its file, so the router is
// not declared whatever the switch says.
export default function SearchToggle({ enabled, declared, unavailable = false, onChanged }: {
  enabled: boolean;
  declared: boolean;
  unavailable?: boolean;
  onChanged: (state: SearchState) => void;
}) {
  const t = useT();
  const [phase, setPhase] = useState<Phase>({ kind: "idle" });
  const [confirming, setConfirming] = useState(false);
  const alive = useRef(true);
  useEffect(() => {
    alive.current = true;
    return () => { alive.current = false; };
  }, []);

  const waiting = phase.kind === "waiting";
  const checked = waiting ? phase.target : enabled;
  const toggle = async () => {
    const target = !enabled;
    setPhase({ kind: "waiting", target, seconds: 0 });
    try {
      const state = await setSearchEnabled(target, (seconds) => { if (alive.current) setPhase({ kind: "waiting", target, seconds }); }, () => !alive.current);
      if (!alive.current) return;
      setPhase({ kind: "done", target });
      onChanged(state);
    } catch (reason) {
      if (!alive.current) return;
      if (reason instanceof SearchApplyTimeout) {
        setPhase({ kind: "timeout", target });
        if (reason.last) onChanged(reason.last);
      } else {
        setPhase({ kind: "error", message: extractApiError(reason, t("search.toggleFailed")) });
      }
    }
  };

  const state = waiting || enabled !== declared ? "applying" : enabled ? "on" : "off";
  const max = SEARCH_APPLY_TIMEOUT_MS / 1000;
  return (
    <div className="search-toggle" data-testid="search-toggle">
      <div className="search-toggle-row">
        <label className="switch">
          <input type="checkbox" role="switch" checked={checked} disabled={waiting} onChange={() => setConfirming(true)} />
          <span className="track"><span className="thumb" /></span>
          <span>{t("search.toggleLabel")}</span>
        </label>
        <span className={"badge" + (state === "on" ? " ok" : state === "applying" ? " warn" : "")}>{t(`search.state.${state}`)}</span>
      </div>
      {!waiting && <p className="field-hint">{t(unavailable ? "overview.moduleUnavailable" : `search.stateHint.${state}`)}</p>}
      {waiting && <p className="muted" role="status">{t(phase.target ? "search.applyingOn" : "search.applyingOff", { seconds: phase.seconds, max })}</p>}
      {phase.kind === "done" && <p className="success" role="status">{t(phase.target ? "search.appliedOn" : "search.appliedOff")}</p>}
      {phase.kind === "timeout" && <p className="notice" role="alert">{t(phase.target ? "search.timeoutOn" : "search.timeoutOff", { max })}</p>}
      {phase.kind === "error" && <div className="error" role="alert">{phase.message}</div>}
      {confirming && (
        <ConfirmDialog
          title={t(enabled ? "search.confirmOffTitle" : "search.confirmOnTitle")}
          message={t(enabled ? "search.confirmOff" : "search.confirmOn")}
          cancelLabel={t("keyForm.cancel")}
          confirmLabel={t(enabled ? "search.confirmOffAction" : "search.confirmOnAction")}
          onCancel={() => setConfirming(false)}
          onConfirm={() => { setConfirming(false); void toggle(); }}
        />
      )}
    </div>
  );
}
