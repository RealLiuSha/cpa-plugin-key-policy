import { useEffect, useId, useRef } from "react";
import { Link } from "react-router-dom";
import type { ChannelRow } from "../channelMerge";
import ChannelFilter from "../components/ChannelFilter";
import { formatDateTime } from "../format";
import { useT } from "../i18n";
import { channelPath } from "../navigation";
import {
  credentialLabel, credentialTypes, displayName, providerType, recordCount, runningTest, selectable, statusText, stopping, testable,
  type QualitySettings,
} from "./helpers";
import { HistoryItems, LatestResult, ResultSummary } from "./results";
import type { DetailTarget, QualityConfig, QualityCredential, QualityTest } from "./types";

type Translate = ReturnType<typeof useT>;

interface ListProps {
  test: QualityTest;
  credentials: QualityCredential[];
  visible: QualityCredential[];
  type: string;
  // The models the credentials of the type serve, null while they are
  // looked up, and the one the list is narrowed to.
  models: { model: string; count: number }[] | null;
  model: string;
  selected: ReadonlySet<string>;
  expanded: ReadonlySet<string>;
  channels: Map<string, ChannelRow>;
  settings: QualitySettings;
  config: QualityConfig;
  // An action is in flight.
  acting: boolean;
  onType: (type: string) => void;
  onModel: (model: string) => void;
  onSelect: (ids: string[], checked: boolean) => void;
  onToggle: (id: string) => void;
  onRun: (credential: QualityCredential) => void;
  onCancel: (credential: QualityCredential) => void;
  onOpen: (target: DetailTarget) => void;
  onClear: () => void;
}

function coolingReason(t: Translate, credential: QualityCredential): string {
  const retry = formatDateTime(credential.next_retry_after);
  return [statusText(credential.status_message), retry && t("quality.state.retryAt", { time: retry })].filter(Boolean).join(" · ") || t("quality.state.coolingUnknown");
}

// Why a credential cannot start a test now; "" when it can.
function blocker(t: Translate, credential: QualityCredential): string {
  if (credential.disabled) return t("quality.blocked.disabled");
  if (credential.unavailable) return t("quality.blocked.cooling", { reason: coolingReason(t, credential) });
  const running = runningTest(credential);
  return running ? t("quality.blocked.busy", { test: t(`quality.busy.${running}`) }) : "";
}

type RowProps = Omit<ListProps, "credentials" | "visible" | "type" | "models" | "model" | "selected" | "expanded" | "onType" | "onModel" | "onClear"> & {
  credential: QualityCredential;
  checked: boolean;
  open: boolean;
};

function CredentialRow({ test, credential, checked, open, channels, settings, config, acting, onSelect, onToggle, onRun, onCancel, onOpen }: RowProps) {
  const t = useT();
  const historyID = useId();
  const channel = credential.auth_index ? channels.get(credential.auth_index) : undefined;
  // An API key's group leads, with its masked key beside it.
  const name = displayName(credential, channels);
  const label = credentialLabel(credential);
  const running = runningTest(credential);
  const reason = blocker(t, credential);
  const count = recordCount(credential, test);
  const halting = running === test && stopping(credential, test);
  return (
    <li className={"quality-row" + (testable(credential) ? "" : " inactive")}>
      <div className="quality-row-main">
        <input type="checkbox" className="quality-select" aria-label={t("quality.select", { name })} title={reason || undefined}
          checked={checked} disabled={!selectable(credential)} onChange={(event) => onSelect([credential.id], event.target.checked)} />
        <div className="quality-credential">
          {channel ? <Link className="channel-name" to={channelPath(channel.authIndex)}>{name}</Link> : <span className="channel-name">{name}</span>}
          {name !== label && <span className="mono muted">{label}</span>}
          {credential.base_url && <span className="quality-url muted" title={credential.base_url}>{credential.base_url.replace(/^https:\/\//i, "")}</span>}
          <div className="quality-badges">
            <span className="badge">{providerType(credential)} · {t(`quality.source.${credential.source}`)}</span>
            {credential.plan_type && <span className="badge">{credential.plan_type}</span>}
            <span className="badge mono">P{credential.priority ?? 0}</span>
            {credential.disabled && <span className="badge">{t("quality.state.disabled")}</span>}
            {credential.unavailable && <span className="badge warn" title={coolingReason(t, credential)}>{t("quality.state.cooling")}</span>}
            {running && running !== test && <span className="badge">{t(`quality.busy.${running}`)}</span>}
          </div>
        </div>
        <LatestResult test={test} credential={credential} config={config} />
        <ResultSummary test={test} credential={credential} settings={settings} config={config} onOpen={onOpen} />
        <div className="quality-actions">
          {running === test ? (
            <button type="button" className="btn sm" disabled={acting || halting} onClick={() => onCancel(credential)}>{t(halting ? "quality.phase.cancelling" : "quality.cancel")}</button>
          ) : (
            <button type="button" className="btn sm" disabled={acting || Boolean(reason)} title={reason || undefined} onClick={() => onRun(credential)}>{t("quality.run")}</button>
          )}
          <button type="button" className="btn sm" aria-expanded={open} aria-controls={historyID} onClick={() => onToggle(credential.id)}>{t("quality.historyToggle", { count })}</button>
        </div>
      </div>
      {open && (
        <section id={historyID} className="quality-history" aria-label={t("quality.historyTitle", { name })}>
          {count === 0 ? <p className="muted">{t("quality.historyEmpty")}</p> : <ol><HistoryItems test={test} credential={credential} config={config} onOpen={onOpen} /></ol>}
        </section>
      )}
    </li>
  );
}

// The credentials of the current filter with one test's results. Only
// enabled credentials that are not cooling down or busy can be selected.
export default function CredentialList(props: ListProps) {
  const { test, credentials, visible, type, models, model, selected, expanded, acting, onType, onModel, onSelect, onClear } = props;
  const t = useT();
  const selectAll = useRef<HTMLInputElement>(null);
  const candidates = visible.filter(selectable);
  const chosen = candidates.filter((credential) => selected.has(credential.id)).length;
  const all = candidates.length > 0 && chosen === candidates.length;
  useEffect(() => {
    if (selectAll.current) selectAll.current.indeterminate = chosen > 0 && !all;
  }, [chosen, all]);
  const hasRecords = credentials.some((credential) => recordCount(credential, test) > 0);

  return (
    <section className="card quality-list" aria-label={t("quality.listLabel")}>
      <div className="quality-list-head">
        <label className="check-row">
          <input ref={selectAll} type="checkbox" checked={all} disabled={candidates.length === 0} onChange={(event) => onSelect(candidates.map((credential) => credential.id), event.target.checked)} />
          {t("quality.selectAll")}
        </label>
        <ChannelFilter types={credentialTypes(credentials, type)} type={type} models={models} model={model} onType={onType} onModel={onModel} />
        <span className="muted">{t("quality.count", { shown: visible.length, testable: candidates.length })}</span>
        <span className="spacer" />
        <button type="button" className="btn sm danger-outline" disabled={acting || !hasRecords} onClick={onClear}>{t("quality.clearTest", { test: t(`quality.tab.${test}`) })}</button>
      </div>
      {visible.length === 0 ? <div className="empty-state quality-empty">{t("quality.empty")}</div> : (
        <ul className="quality-rows">
          {visible.map((credential) => (
            <CredentialRow key={credential.id} {...props} credential={credential} checked={selectable(credential) && selected.has(credential.id)} open={expanded.has(credential.id)} />
          ))}
        </ul>
      )}
    </section>
  );
}
