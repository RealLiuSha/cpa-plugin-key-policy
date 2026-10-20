import { useCallback, useEffect, useState } from "react";
import { Link } from "react-router-dom";
import { extractApiError } from "../api/error";
import { fetchSettings } from "../api/settings";
import { copyText } from "../clipboard";
import LedgerHealthList from "../components/LedgerHealth";
import SearchToggle from "../components/SearchToggle";
import { formatBytes, formatCount, formatDateTime, formatMicros } from "../format";
import { useT } from "../i18n";
import { labelOr } from "../labels";
import type { SettingsResponse } from "../types";

function Section({ title, children, testId }: { title: string; children: React.ReactNode; testId?: string }) {
  return (
    <section className="card settings-section" data-testid={testId}>
      <h2 className="usage-section-title">{title}</h2>
      {children}
    </section>
  );
}

export default function Settings() {
  const t = useT();
  const [settings, setSettings] = useState<SettingsResponse | null>(null);
  const [error, setError] = useState("");
  const [loading, setLoading] = useState(true);
  const [copy, setCopy] = useState<{ ok: boolean; text: string } | null>(null);

  const load = useCallback(async () => {
    setLoading(true);
    setError("");
    try {
      setSettings(await fetchSettings());
    } catch (reason) {
      setError(extractApiError(reason, t("settings.loadFailed")));
    } finally {
      setLoading(false);
    }
  }, [t]);
  useEffect(() => { void load(); }, [load]);

  const copyDiagnostics = async () => {
    if (!settings) return;
    const text = JSON.stringify(settings, null, 2);
    setCopy({ ok: await copyText(text), text });
  };

  const modules = settings?.modules;
  const moduleErrors = Object.entries(settings?.module_errors ?? {});
  return (
    <div className="page settings-page">
      <div className="page-head">
        <div>
          <h1>{t("settings.title")}</h1>
          <p className="muted">{settings ? t("settings.version", { name: settings.display_name || t("header.title"), version: settings.version, id: settings.plugin_id }) : t("settings.description")}</p>
        </div>
        <div className="page-head-actions">
          <button type="button" className="btn" disabled={!settings} onClick={() => void copyDiagnostics()}>{t("settings.copyDiagnostics")}</button>
          <button type="button" className="btn" disabled={loading} onClick={() => void load()}>{t(loading ? "quota.refreshing" : "keys.refresh")}</button>
        </div>
      </div>
      {copy && (copy.ok ? <p className="success" role="status">{t("settings.copied")}</p> : (
        <div role="status">
          <p className="notice">{t("channels.copyManual")}</p>
          <textarea className="input mono" rows={10} readOnly value={copy.text} aria-label={t("settings.copyDiagnostics")} onFocus={(event) => event.currentTarget.select()} />
        </div>
      ))}
      {error && (
        <div className="error stats-error" role="alert">
          <span>{error}</span>
          <button type="button" className="btn sm" onClick={() => void load()}>{t("stats.retry")}</button>
        </div>
      )}
      {!settings || !modules ? (loading && <div className="card muted" role="status">{t("keys.loading")}</div>) : (
        <>
          <Section title={t("settings.modulesTitle")} testId="settings-modules">
            <dl className="detail-facts settings-modules">
              <div>
                <dt><Link to="/search">{t("overview.moduleSearch")}</Link></dt>
                <dd>
                  <SearchToggle
                    enabled={modules.search.enabled}
                    declared={modules.search.declared}
                    unavailable={Boolean(settings.module_errors?.search)}
                    onChanged={(next) => setSettings((current) => current && { ...current, modules: { ...current.modules, search: { enabled: next.enabled, declared: next.declared } } })}
                  />
                </dd>
              </div>
              <div><dt><Link to="/channels">{t("overview.moduleAdapters")}</Link></dt><dd>{modules.adapters ? t("overview.adapterCount", { rules: modules.adapters.rules, enabled: modules.adapters.enabled_rules }) : t("overview.moduleUnavailable")}</dd></div>
              <div><dt><Link to="/channels">{t("settings.moduleChannels")}</Link></dt><dd>{modules.channels ? t("settings.channelCount", { tracked: modules.channels.tracked }) : t("overview.moduleUnavailable")}</dd></div>
            </dl>
            {moduleErrors.length > 0 && (
              <div className="notice" role="alert">
                <p>{t("settings.moduleErrorsLead")}</p>
                <ul className="overview-warnings">{moduleErrors.map(([name, message]) => <li key={name}><span className="mono">{name}</span>: {message}</li>)}</ul>
              </div>
            )}
          </Section>

          <Section title={t("settings.warningsTitle")} testId="settings-warnings">
            {settings.config_warnings.length === 0 ? <p className="muted">{t("overview.warningsClear")}</p> : (
              <ul className="notice overview-warnings">{settings.config_warnings.map((warning) => <li key={warning}>{warning}</li>)}</ul>
            )}
          </Section>

          <Section title={t("settings.hooksTitle")} testId="settings-hooks">
            <p className="field-hint">{t("settings.hooksHint")}</p>
            {settings.hooks.length === 0 ? <p className="muted">{t("settings.hooksEmpty")}</p> : (
              <div className="table-scroll">
                <table>
                  <thead><tr><th>{t("settings.col.method")}</th><th className="num">{t("settings.col.count")}</th><th className="num">p50</th><th className="num">p99</th><th className="num">{t("settings.col.max")}</th></tr></thead>
                  <tbody>
                    {settings.hooks.map((hook) => (
                      <tr key={hook.method}>
                        <td className="mono">{hook.method}</td>
                        <td className="num mono">{formatCount(hook.count)}</td>
                        <td className="num mono">{formatMicros(hook.p50_us)}</td>
                        <td className="num mono">{formatMicros(hook.p99_us)}</td>
                        <td className="num mono">{formatMicros(hook.max_us)}</td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              </div>
            )}
          </Section>

          <Section title={t("settings.filesTitle")} testId="settings-files">
            {settings.files.length === 0 ? <p className="muted">{t("settings.filesEmpty")}</p> : (
              <div className="table-scroll">
                <table>
                  <thead><tr><th>{t("settings.col.file")}</th><th>{t("settings.col.path")}</th><th className="num">{t("settings.col.size")}</th><th>{t("settings.col.modified")}</th></tr></thead>
                  <tbody>
                    {settings.files.map((file) => (
                      <tr key={file.path}>
                        <td>{file.name}</td>
                        <td className="mono settings-path">{file.path}</td>
                        <td className="num mono">{file.missing ? "—" : formatBytes(file.size)}</td>
                        <td>{file.missing ? t("settings.fileMissing") : formatDateTime(file.modified) || "—"}</td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              </div>
            )}
          </Section>

          <Section title={t("settings.runtimeTitle")} testId="settings-runtime">
            <dl className="detail-facts">
              <div><dt>{t("settings.uiSource")}</dt><dd>{t(settings.ui.source === "override" ? "settings.ui.override" : "settings.ui.embedded")}{settings.ui.sha256 && <span className="mono muted"> · sha256 {settings.ui.sha256.slice(0, 12)}</span>}</dd></div>
              <div><dt>{t("settings.owner")}</dt><dd>{t(settings.ownership.owner ? "settings.ownerYes" : "settings.ownerNo")}</dd></div>
              <div><dt>{t("settings.handoff")}</dt><dd>{labelOr(t, `settings.handoffState.${settings.ownership.handoff}`, settings.ownership.handoff || "—")}</dd></div>
            </dl>
            {!settings.ownership.owner && <p className="notice">{t(settings.ownership.handoff === "read_only" ? "settings.readOnly" : "settings.notOwner")}</p>}
          </Section>

          <Section title={t("stats.health.title")} testId="settings-health">
            {settings.health ? <LedgerHealthList health={settings.health} /> : <p className="muted">{t("stats.health.unavailable")}</p>}
          </Section>
        </>
      )}
    </div>
  );
}
