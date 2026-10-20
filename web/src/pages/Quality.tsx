import { useCallback, useEffect, useId, useMemo, useRef, useState } from "react";
import { useSearchParams } from "react-router-dom";
import { fetchChannelData, type ChannelData } from "../api/channels";
import { extractApiError } from "../api/error";
import { fetchModelPrices } from "../api/modelPrices";
import { cancelQuality, clearQualityResults, fetchQuality, runQuality } from "../api/quality";
import { mergeChannels, type ChannelRow } from "../channelMerge";
import { ALL, modelCounts, servesModel, useChannelModels } from "../channelModels";
import { ConfirmDialog } from "../components/Modal";
import { formatCount } from "../format";
import { useT } from "../i18n";
import { useVisiblePolling } from "../polling";
import CredentialList from "../quality/CredentialList";
import RecordDialog from "../quality/RecordDialog";
import SettingsForm from "../quality/SettingsForm";
import {
  DEFAULT_CREDENTIAL_TYPE, defaultSettings, displayName, ofType, parseCredentialType, planRun, recordCount, runningTest, selectable, withModel,
  type PlannedRun, type QualitySettings,
} from "../quality/helpers";
import { CODEX_CONTEXT_TOKENS, QUALITY_TESTS, type DetailTarget, type QualityConfig, type QualityCredential, type QualityState, type QualityTest } from "../quality/types";
import "../quality/quality.css";

// While a test runs its progress is read every few seconds; otherwise the
// page refreshes like the channel list.
export const QUALITY_RUNNING_REFRESH_MS = 2_500;
export const QUALITY_IDLE_REFRESH_MS = 30_000;

const CANDY_EVAL_URL = "https://github.com/haowang02/cpa-plugin-codex-candy-eval";
const MODELTRACE_URL = "https://github.com/xqy2006/ModelTrace";
const FINGERPRINT_PAPER_URL = "https://arxiv.org/abs/2607.10252";

type PerTest = Record<QualityTest, ReadonlySet<string>>;
const emptyPerTest = (): PerTest => ({ candy: new Set(), fingerprint: new Set(), modeltrace: new Set(), pelican: new Set() });

interface PendingRun extends PlannedRun {
  test: QualityTest;
  credentials: number;
}

function Intro({ test, config }: { test: QualityTest; config: QualityConfig }) {
  const t = useT();
  const prompt = test === "candy" ? config.candy.prompt : test === "pelican" ? config.pelican.prompt : "";
  return (
    <div className="quality-intro">
      <p>{t(`quality.intro.${test}`, { answer: config.candy.answer || "21" })}</p>
      {test === "fingerprint" && (
        <p className="field-hint">
          {t("quality.intro.fingerprintSource")}{" "}
          <a href={FINGERPRINT_PAPER_URL} target="_blank" rel="noopener noreferrer">One Token Is Enough</a>
        </p>
      )}
      {prompt && (
        <details className="quality-prompt">
          <summary>{t("quality.prompt")}</summary>
          <pre className="code-block">{prompt}</pre>
        </details>
      )}
    </div>
  );
}

export default function Quality() {
  const t = useT();
  const ids = useId();
  const [params, setParams] = useSearchParams();
  const tab: QualityTest = QUALITY_TESTS.find((value) => value === params.get("tab")) ?? "candy";
  const type = parseCredentialType(params.get("type"));
  const model = params.get("model") ?? ALL;
  const [state, setState] = useState<QualityState | null>(null);
  const [channels, setChannels] = useState<ChannelData | null>(null);
  const [models, setModels] = useState<string[]>([]);
  const [settings, setSettings] = useState<QualitySettings | null>(null);
  const [error, setError] = useState("");
  const [refreshing, setRefreshing] = useState(false);
  const [acting, setActing] = useState(false);
  const [message, setMessage] = useState<{ ok: boolean; text: string } | null>(null);
  const [selected, setSelected] = useState<PerTest>(emptyPerTest);
  const [expanded, setExpanded] = useState<PerTest>(emptyPerTest);
  const [pending, setPending] = useState<PendingRun | null>(null);
  const [clearing, setClearing] = useState<QualityTest | "all" | null>(null);
  const [detail, setDetail] = useState<DetailTarget | null>(null);
  const requestID = useRef(0);
  const mounted = useRef(true);

  const load = useCallback(async () => {
    const id = ++requestID.current;
    try {
      const next = await fetchQuality();
      if (id !== requestID.current) return;
      setState(next);
      setSettings((current) => current ?? defaultSettings(next.config));
      setError("");
    } catch (reason) {
      if (id === requestID.current) setError(extractApiError(reason, t("quality.loadFailed")));
    }
  }, [t]);

  // Channel names only: CPA's credential lists are several requests, so
  // they follow the idle refresh, not the progress polling.
  const loadChannels = useCallback(async () => {
    try {
      const next = await fetchChannelData();
      if (mounted.current) setChannels(next);
    } catch {
      // Credentials keep their CPA names.
    }
  }, []);

  useEffect(() => {
    mounted.current = true;
    void load();
    void loadChannels();
    fetchModelPrices().then(
      (prices) => { if (mounted.current) setModels(prices.models.map((row) => row.name)); },
      () => { /* No suggestions without price rows. */ },
    );
    return () => {
      mounted.current = false;
      requestID.current++;
    };
  }, [load, loadChannels]);

  const running = state?.auths.some((credential) => runningTest(credential) !== null) ?? false;
  useVisiblePolling(() => {
    void load();
    if (!running) void loadChannels();
  }, running ? QUALITY_RUNNING_REFRESH_MS : QUALITY_IDLE_REFRESH_MS);

  const refresh = async () => {
    setRefreshing(true);
    try {
      await Promise.all([load(), loadChannels()]);
    } finally {
      setRefreshing(false);
    }
  };

  const channelList = useMemo((): ChannelRow[] => channels ? mergeChannels({ config: channels.cpa?.config ?? [], authFiles: channels.cpa?.authFiles ?? [], channels: channels.channels, cpaListed: channels.cpa !== null }) : [], [channels]);
  const channelRows = useMemo(() => new Map(channelList.filter((row) => row.authIndex).map((row) => [row.authIndex, row])), [channelList]);
  const channelModels = useChannelModels(channelList, channels?.ids ?? {});

  const credentials = state?.auths ?? [];
  const typed = credentials.filter((credential) => ofType(credential, type));
  const visible = typed.filter((credential) => servesModel(credential.auth_index ?? "", model, channelModels));
  const candidates = visible.filter(selectable);
  const chosen = candidates.filter((credential) => selected[tab].has(credential.id));
  // Nothing selected runs every testable credential of the filter.
  const targets = chosen.length > 0 ? chosen : candidates;
  const hasRecords = credentials.some((credential) => QUALITY_TESTS.some((test) => recordCount(credential, test) > 0));

  const setParam = (name: string, value: string, fallback: string) => {
    const next = new URLSearchParams(params);
    if (value === fallback) next.delete(name);
    else next.set(name, value);
    setParams(next, { replace: true });
  };
  const changeType = (value: string) => {
    setSelected(emptyPerTest());
    setParam("type", value, DEFAULT_CREDENTIAL_TYPE);
  };
  // Narrowing the list to a model also asks every test with it.
  const changeModel = (value: string) => {
    setSelected(emptyPerTest());
    setParam("model", value, ALL);
    if (value !== ALL) setSettings((current) => current && withModel(current, value));
  };
  const select = (list: string[], checked: boolean) => setSelected((current) => {
    const next = new Set(current[tab]);
    for (const id of list) {
      if (checked) next.add(id);
      else next.delete(id);
    }
    return { ...current, [tab]: next };
  });
  const toggle = (id: string) => setExpanded((current) => {
    const next = new Set(current[tab]);
    if (!next.delete(id)) next.add(id);
    return { ...current, [tab]: next };
  });

  const perform = async (work: () => Promise<string | null>, failure: string) => {
    setActing(true);
    setMessage(null);
    try {
      const text = await work();
      if (text) setMessage({ ok: true, text });
    } catch (reason) {
      setMessage({ ok: false, text: extractApiError(reason, failure) });
    } finally {
      setActing(false);
      void load();
    }
  };

  // Every run is confirmed first: the requests use the credentials' quota.
  const askRun = (test: QualityTest, list: QualityCredential[]) => {
    if (!state || !settings || list.length === 0) return;
    const plan = planRun(test, settings, state.config, list.map((credential) => credential.id), t);
    if ("error" in plan) {
      setMessage({ ok: false, text: plan.error });
      return;
    }
    setMessage(null);
    setPending({ ...plan, test, credentials: list.length });
  };
  const startRun = (run: PendingRun) => {
    setPending(null);
    void perform(async () => {
      const result = await runQuality(run.body);
      setSelected((current) => ({ ...current, [run.test]: new Set<string>() }));
      const started = t("quality.started", { count: result.started });
      return result.busy > 0 ? `${started} ${t("quality.busySkipped", { count: result.busy })}` : started;
    }, t("quality.startFailed"));
  };
  const cancel = (credential: QualityCredential) => {
    void perform(async () => {
      await cancelQuality(tab, [credential.id]);
      return null;
    }, t("quality.cancelFailed"));
  };
  const clear = (scope: QualityTest | "all") => {
    setClearing(null);
    void perform(async () => {
      await clearQualityResults(scope);
      return t("quality.cleared");
    }, t("quality.clearFailed"));
  };

  const confirmMessage = (run: PendingRun) => {
    const requests = run.credentials * run.perCredential;
    const cost = run.test === "fingerprint"
      ? t("quality.confirm.fingerprint", { credentials: run.credentials, requests: formatCount(requests), perCredential: run.perCredential })
      : t("quality.confirm.codex", { credentials: run.credentials, requests: formatCount(requests), tokens: formatCount(requests * CODEX_CONTEXT_TOKENS) });
    return `${cost}\n${t("quality.about")}`;
  };

  return (
    <div className="page quality-page">
      <div className="page-head">
        <div><h1>{t("quality.title")}</h1><p className="muted">{t("quality.description")}</p></div>
        <div className="page-head-actions">
          <button type="button" className="btn danger-outline" disabled={acting || (!hasRecords && !state?.storage_error)} onClick={() => setClearing("all")}>{t("quality.clearAll")}</button>
          <button type="button" className="btn" disabled={refreshing} onClick={() => void refresh()}>{t(refreshing ? "quota.refreshing" : "keys.refresh")}</button>
        </div>
      </div>
      {error && (
        <div className="error stats-error" role="alert">
          <span>{error}</span>
          <button type="button" className="btn sm" onClick={() => void refresh()}>{t("stats.retry")}</button>
        </div>
      )}
      {state?.storage_error && <p className="notice" role="status">{t("quality.storageError", { error: state.storage_error })}</p>}
      {state?.config_error && <p className="notice" role="status">{t("quality.configError", { error: state.config_error })}</p>}
      {!state || !settings ? (!error && <div className="card muted" role="status">{t("keys.loading")}</div>) : (
        <>
          <div className="seg quality-tabs" role="tablist" aria-label={t("quality.tabsLabel")}>
            {QUALITY_TESTS.map((value) => (
              <button key={value} type="button" role="tab" id={`${ids}-tab-${value}`} aria-controls={`${ids}-panel`} aria-selected={tab === value}
                className={"seg-btn" + (tab === value ? " active" : "")} onClick={() => { setMessage(null); setParam("tab", value, "candy"); }}>
                {t(`quality.tab.${value}`)}
              </button>
            ))}
          </div>
          <section id={`${ids}-panel`} role="tabpanel" aria-labelledby={`${ids}-tab-${tab}`} className="quality-panel">
            <Intro test={tab} config={state.config} />
            <SettingsForm
              test={tab}
              settings={settings}
              config={state.config}
              models={models}
              submitLabel={t(chosen.length > 0 ? "quality.runSelected" : "quality.runAll", { count: targets.length })}
              submitDisabled={acting || targets.length === 0}
              onChange={setSettings}
              onSubmit={() => askRun(tab, targets)}
            />
            {message && <div className={message.ok ? "success" : "error"} role={message.ok ? "status" : "alert"}>{message.text}</div>}
            <CredentialList
              test={tab}
              credentials={credentials}
              visible={visible}
              type={type}
              models={channelModels && modelCounts(typed.map((credential) => credential.auth_index ?? ""), channelModels)}
              model={model}
              selected={selected[tab]}
              expanded={expanded[tab]}
              channels={channelRows}
              settings={settings}
              config={state.config}
              acting={acting}
              onType={changeType}
              onModel={changeModel}
              onSelect={select}
              onToggle={toggle}
              onRun={(credential) => askRun(tab, [credential])}
              onCancel={cancel}
              onOpen={setDetail}
              onClear={() => setClearing(tab)}
            />
          </section>
        </>
      )}
      <footer className="quality-credits muted">
        {t("quality.credits")}{" "}
        <a href={CANDY_EVAL_URL} target="_blank" rel="noopener noreferrer">candy-eval</a> (MIT, Hao Wang) ·{" "}
        <a href={MODELTRACE_URL} target="_blank" rel="noopener noreferrer">ModelTrace</a> (MIT, xqy2006)
      </footer>
      {pending && (
        <ConfirmDialog
          title={t("quality.confirm.title", { test: t(`quality.tab.${pending.test}`) })}
          message={confirmMessage(pending)}
          cancelLabel={t("keyForm.cancel")}
          confirmLabel={t("quality.confirm.start")}
          onCancel={() => setPending(null)}
          onConfirm={() => startRun(pending)}
        />
      )}
      {clearing && (
        <ConfirmDialog
          title={t("quality.clearTitle")}
          message={clearing === "all" ? t(state?.storage_error ? "quality.clearCorruptMessage" : "quality.clearAllMessage") : t("quality.clearMessage", { test: t(`quality.tab.${clearing}`) })}
          cancelLabel={t("keyForm.cancel")}
          confirmLabel={t("quality.clearConfirm")}
          onCancel={() => setClearing(null)}
          onConfirm={() => clear(clearing)}
        />
      )}
      {detail && state && <RecordDialog target={detail} name={displayName(detail.credential, channelRows)} config={state.config} onClose={() => setDetail(null)} />}
    </div>
  );
}
