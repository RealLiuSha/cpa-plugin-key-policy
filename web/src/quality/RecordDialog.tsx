import { useEffect, useState, type ReactNode } from "react";
import { extractApiError } from "../api/error";
import { fetchQualityRecord, isRecordGone } from "../api/quality";
import Modal from "../components/Modal";
import { formatDateTime } from "../format";
import { getLocale, useT } from "../i18n";
import { labelOr } from "../labels";
import { fingerprintModeName, fingerprintStatus, fingerprintWarnings, modelLabel, nearestModel, probability, recordError, statistic, traceMatches } from "./helpers";
import { CandyVerdict, FingerprintOutcome, Metrics, PelicanVerdict, TraceOutcome } from "./results";
import type { CandyResult, DetailTarget, FingerprintResult, PelicanResult, QualityConfig, QualityTest, TraceAttribution, TraceResult } from "./types";

type Loaded =
  | { test: "candy"; record: CandyResult }
  | { test: "fingerprint"; record: FingerprintResult }
  | { test: "modeltrace"; record: TraceResult }
  | { test: "pelican"; record: PelicanResult };

async function loadRecord(test: QualityTest, authID: string, id: string): Promise<Loaded> {
  switch (test) {
    case "candy": return { test, record: await fetchQualityRecord(test, authID, id) };
    case "fingerprint": return { test, record: await fetchQualityRecord(test, authID, id) };
    case "modeltrace": return { test, record: await fetchQualityRecord(test, authID, id) };
    case "pelican": return { test, record: await fetchQualityRecord(test, authID, id) };
  }
}

function RecordMeta({ time, model, children }: { time: string; model: string; children?: ReactNode }) {
  return (
    <div className="quality-dialog-meta">
      {children}
      <time dateTime={time}>{formatDateTime(time) || "—"}</time>
      <span className="mono">{model}</span>
    </div>
  );
}

function CandyDetail({ record }: { record: CandyResult }) {
  const t = useT();
  return (
    <>
      <RecordMeta time={record.time} model={modelLabel(record.model, record.effort)}><CandyVerdict record={record} /></RecordMeta>
      {!record.skipped && <Metrics usage={record} />}
      {record.error ? <div className="error" role="alert">{recordError(t, record.error)}</div> : <pre className="code-block quality-answer">{record.answer || t("quality.candy.noAnswer")}</pre>}
    </>
  );
}

function FingerprintDetail({ record, config }: { record: FingerprintResult; config: QualityConfig }) {
  const t = useT();
  const attribution = record.attribution;
  const comparisons = attribution?.comparisons ?? [];
  const nearest = nearestModel(record);
  const [baseline, setBaseline] = useState(() => (comparisons.some((item) => item.model === nearest) ? nearest : comparisons[0]?.model ?? ""));
  const cells = comparisons.find((item) => item.model === baseline)?.cells ?? [];
  const partial = record.status === "cancelled" || record.status === "failed";
  return (
    <>
      <RecordMeta time={record.time} model={record.model}>
        <span className="badge">{t("quality.fingerprint.modeName", { mode: fingerprintModeName(t, config, record.mode) })}</span>
      </RecordMeta>
      <FingerprintOutcome record={record} />
      {partial && <p className="field-hint">{t("quality.fingerprint.partial")}</p>}
      {/* The plugin writes the conclusion in Simplified Chinese; the status
          and its hint above say the same in every language. */}
      {attribution?.message && getLocale() === "zh-CN" && <p className="field-hint">{attribution.message}</p>}
      {record.error && <div className="error" role="alert">{recordError(t, record.error)}</div>}
      {fingerprintStatus(record) !== "skipped" && (
        <>
          <dl className="detail-facts">
            <div><dt>{t("quality.fingerprint.collected")}</dt><dd className="mono">{record.done} / {record.total}</dd></div>
            <div><dt>{t("quality.fingerprint.valid")}</dt><dd className="mono">{record.valid}</dd></div>
            <div><dt>{t("quality.fingerprint.failures")}</dt><dd className="mono">{record.errors}</dd></div>
            <div><dt>{t("quality.usage")}</dt><dd><Metrics usage={record} /></dd></div>
          </dl>
          <section className="detail-section">
            <h3>{t("quality.fingerprint.comparisons")}</h3>
            <div className="table-scroll">
              <table className="quality-detail-table">
                <thead>
                  <tr>
                    <th>{t("quality.fingerprint.baseline")}</th>
                    <th className="num">{t("quality.fingerprint.jsd")}</th>
                    <th className="num">{t("quality.fingerprint.pValue")}</th>
                    <th className="num">{t("quality.fingerprint.probes")}</th>
                    <th>{t("quality.fingerprint.distance")}</th>
                  </tr>
                </thead>
                <tbody>
                  {comparisons.length === 0 ? <tr><td colSpan={5} className="muted">{t("quality.fingerprint.noComparisons")}</td></tr> : comparisons.map((item) => (
                    <tr key={item.model} className={item.model === nearest ? "nearest" : undefined}>
                      <td className="mono">
                        {item.model}
                        {item.model === record.model && <> · {t("quality.fingerprint.selected")}</>}
                        {item.model === nearest && <> · {t("quality.fingerprint.nearest")}</>}
                      </td>
                      <td className="num mono">{statistic(item.mean_jsd)}</td>
                      <td className="num mono">{statistic(item.p_value)}</td>
                      <td className="num mono">{item.cells?.length ?? 0}</td>
                      <td>{labelOr(t, `quality.fingerprint.verdict.${item.verdict}`, item.verdict)}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
            <p className="field-hint">{t("quality.fingerprint.alphaNote", { alpha: statistic(attribution?.alpha) })}</p>
          </section>
          <section className="detail-section">
            <h3>{t("quality.fingerprint.stability")}</h3>
            <p>{t("quality.fingerprint.selfJsd", { value: statistic(attribution?.self_jsd) })}</p>
            {typeof attribution?.reference_p_value === "number" && <p>{t("quality.fingerprint.referenceP", { value: statistic(attribution.reference_p_value) })}</p>}
            {fingerprintWarnings(t, attribution).map((warning) => <p key={warning} className="notice">{warning}</p>)}
          </section>
          <section className="detail-section">
            <div className="detail-section-head">
              <h3>{t("quality.fingerprint.probesTitle")}</h3>
              {comparisons.length > 0 && (
                <label className="quality-inline-field">
                  {t("quality.fingerprint.compareWith")}
                  <select value={baseline} onChange={(event) => setBaseline(event.target.value)}>
                    {comparisons.map((item) => <option key={item.model} value={item.model}>{item.model}</option>)}
                  </select>
                </label>
              )}
            </div>
            <div className="table-scroll">
              <table className="quality-detail-table">
                <thead>
                  <tr>
                    <th>{t("quality.fingerprint.probe")}</th>
                    <th className="num">{t("quality.fingerprint.jsd")}</th>
                    <th className="num">{t("quality.fingerprint.validOwn")}</th>
                    <th className="num">{t("quality.fingerprint.validBaseline")}</th>
                  </tr>
                </thead>
                <tbody>
                  {cells.length === 0 ? <tr><td colSpan={4} className="muted">{t("quality.fingerprint.noProbes")}</td></tr> : cells.map((cell) => {
                    const [task, language] = cell.cell.split(":");
                    return (
                      <tr key={cell.cell}>
                        <td>{labelOr(t, `quality.fingerprint.probeName.${task}`, task)} · {labelOr(t, `quality.fingerprint.language.${language}`, language ?? "")}</td>
                        <td className="num mono">{statistic(cell.jsd)}</td>
                        <td className="num mono">{cell.valid_a}</td>
                        <td className="num mono">{cell.valid_b}</td>
                      </tr>
                    );
                  })}
                </tbody>
              </table>
            </div>
          </section>
        </>
      )}
    </>
  );
}

function TraceAttributionView({ record, attribution, config }: { record: TraceResult; attribution: TraceAttribution; config: QualityConfig }) {
  const t = useT();
  const matches = traceMatches(record);
  const families = attribution.family_probabilities ?? [];
  const candidates = attribution.results ?? [];
  const diagnostics = attribution.diagnostics ?? [];
  return (
    <>
      <dl className="detail-facts">
        <div>
          <dt>{matches === null ? t("quality.modeltrace.closest") : t(matches ? "quality.modeltrace.match" : "quality.modeltrace.mismatch")}</dt>
          <dd><strong className="mono">{attribution.prediction}</strong> <span className="mono">{probability(attribution.probability)}</span></dd>
        </div>
        <div><dt>{t("quality.modeltrace.family")}</dt><dd>{attribution.family_prediction_name || "—"} <span className="mono">{probability(attribution.family_probability)}</span></dd></div>
        <div><dt>{t("quality.modeltrace.used")}</dt><dd className="mono">{attribution.used_outputs} / {config.modeltrace.requests}</dd></div>
      </dl>
      {families.length > 0 && (
        <section className="detail-section">
          <h3>{t("quality.modeltrace.families")}</h3>
          <div className="quality-families">
            {families.map((family) => <span key={family.family} className="badge">{family.display_name || family.family}&nbsp;<span className="mono">{probability(family.probability)}</span></span>)}
          </div>
        </section>
      )}
      {candidates.length > 0 && (
        <section className="detail-section">
          <div className="detail-section-head"><h3>{t("quality.modeltrace.ranking")}</h3><span className="muted">{t("quality.modeltrace.probability")}</span></div>
          <ol className="quality-ranking">
            {candidates.map((candidate, index) => (
              <li key={candidate.model}>
                <span className="mono muted">{String(index + 1).padStart(2, "0")}</span>
                <span className="mono quality-rank-name">{candidate.display_name || candidate.model}</span>
                <span className="quality-bar" aria-hidden="true"><span style={{ width: `${Math.max(0, Math.min(100, candidate.probability * 100))}%` }} /></span>
                <span className="mono num">{probability(candidate.probability)}</span>
              </li>
            ))}
          </ol>
        </section>
      )}
      {diagnostics.length > 0 && (
        <section className="detail-section">
          <h3>{t("quality.modeltrace.diagnostics")}</h3>
          <div className="table-scroll">
            <table className="quality-detail-table">
              <thead>
                <tr>
                  <th>{t("quality.modeltrace.challengeColumn")}</th>
                  <th className="num">{t("quality.modeltrace.parsedColumn")}</th>
                  <th className="num">{t("quality.modeltrace.minimumColumn")}</th>
                  <th>{t("quality.modeltrace.acceptedColumn")}</th>
                </tr>
              </thead>
              <tbody>
                {diagnostics.map((item) => (
                  <tr key={item.index}>
                    <td>{t("quality.modeltrace.challenge", { index: item.index + 1 })}</td>
                    <td className="num mono">{item.parsed_numbers}</td>
                    <td className="num mono">{item.minimum_numbers}</td>
                    <td><span className={item.accepted ? "badge ok" : "badge warn"}>{t(item.accepted ? "quality.modeltrace.accepted" : "quality.modeltrace.rejected")}</span></td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        </section>
      )}
    </>
  );
}

function TraceDetail({ record, config }: { record: TraceResult; config: QualityConfig }) {
  const t = useT();
  const attribution = record.attribution;
  const samples = record.samples ?? [];
  const requests = samples.reduce((total, sample) => total + (sample.attempts || 1), 0);
  const staleBank = Boolean(record.bank_revision && config.modeltrace.bank_revision && record.bank_revision !== config.modeltrace.bank_revision);
  return (
    <>
      <RecordMeta time={record.time} model={record.model}>
        {record.concurrency ? <span className="badge">{t("quality.modeltrace.concurrency", { count: record.concurrency })}</span> : null}
      </RecordMeta>
      <Metrics usage={record} />
      {attribution && record.status !== "completed" && <p className="field-hint">{labelOr(t, `quality.modeltrace.status.${record.status}`, record.status)}</p>}
      {attribution ? <TraceAttributionView record={record} attribution={attribution} config={config} /> : <TraceOutcome record={record} />}
      {record.error && <div className="error" role="alert">{recordError(t, record.error)}</div>}
      {staleBank && <p className="notice info">{t("quality.modeltrace.staleBank")}</p>}
      {samples.length > 0 && (
        <section className="detail-section">
          <div className="detail-section-head"><h3>{t("quality.modeltrace.samples")}</h3><span className="muted">{t("quality.modeltrace.requests", { count: requests })}</span></div>
          {samples.map((sample, index) => (
            <details key={index} className="quality-sample">
              <summary>
                <span>{t("quality.modeltrace.challenge", { index: index + 1 })}</span>
                <span className="muted">
                  {t("quality.modeltrace.parsed", { parsed: sample.parsed_numbers, expected: sample.expected_count })}
                  {(sample.attempts ?? 1) > 1 && <> · {t("quality.modeltrace.retries", { count: (sample.attempts ?? 1) - 1 })}</>}
                </span>
                <span className={sample.accepted ? "badge ok" : "badge warn"}>{t(sample.accepted ? "quality.modeltrace.accepted" : "quality.modeltrace.rejected")}</span>
              </summary>
              <pre className="code-block">{sample.prompt}</pre>
              {sample.error && <div className="error">{recordError(t, sample.error)}</div>}
              {sample.text && <pre className="code-block">{sample.text}</pre>}
            </details>
          ))}
        </section>
      )}
    </>
  );
}

function PelicanDetail({ record }: { record: PelicanResult }) {
  const t = useT();
  return (
    <>
      <RecordMeta time={record.time} model={modelLabel(record.model, record.effort)}><PelicanVerdict record={record} /></RecordMeta>
      {!record.skipped && <Metrics usage={record} />}
      {record.error ? <div className="error" role="alert">{recordError(t, record.error)}</div> : record.html ? (
        // The page is the model's: without allow-same-origin its scripts run
        // in an opaque origin, away from this page, its storage and the
        // management key.
        <iframe className="quality-frame" sandbox="allow-scripts" srcDoc={record.html} title={t("quality.pelican.frameTitle")} />
      ) : <div className="empty-state">{t("quality.pelican.noPage")}</div>}
    </>
  );
}

function LoadedRecord({ test, authID, id, config }: { test: QualityTest; authID: string; id: string; config: QualityConfig }) {
  const t = useT();
  const [loaded, setLoaded] = useState<Loaded | null>(null);
  const [error, setError] = useState("");
  useEffect(() => {
    let alive = true;
    loadRecord(test, authID, id).then(
      (value) => { if (alive) setLoaded(value); },
      (reason: unknown) => { if (alive) setError(isRecordGone(reason) ? t("quality.detail.gone") : extractApiError(reason, t("quality.detail.failed"))); },
    );
    return () => { alive = false; };
  }, [test, authID, id, t]);
  if (error) return <div className="error" role="alert">{error}</div>;
  if (!loaded) return <div className="muted" role="status">{t("keys.loading")}</div>;
  switch (loaded.test) {
    case "candy": return <CandyDetail record={loaded.record} />;
    case "fingerprint": return <FingerprintDetail record={loaded.record} config={config} />;
    case "modeltrace": return <TraceDetail record={loaded.record} config={config} />;
    case "pelican": return <PelicanDetail record={loaded.record} />;
  }
}

// One record in full, loaded from GET /quality/record.
export default function RecordDialog({ target, name, config, onClose }: { target: DetailTarget; name: string; config: QualityConfig; onClose: () => void }) {
  const t = useT();
  return (
    <Modal title={t(`quality.detail.${target.test}`)} closeLabel={t("models.close")} onClose={onClose} wide>
      <div className="quality-dialog">
        <p className="muted quality-dialog-name">{name}</p>
        <LoadedRecord test={target.test} authID={target.credential.id} id={target.id} config={config} />
      </div>
    </Modal>
  );
}
