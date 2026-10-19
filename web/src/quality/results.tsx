import type { ReactNode } from "react";
import { formatDateTime, formatMillis, formatPercent } from "../format";
import { useT } from "../i18n";
import { labelOr } from "../labels";
import {
  candyKind, candyRate, fingerprintModeName, fingerprintStatus, fingerprintTone, inScope, lastOf, modelLabel, nearestModel, probability, recordError, tokenCount, traceMatches,
  type CandyKind, type QualitySettings, type Tone,
} from "./helpers";
import type { CandyResult, CollectionPhase, DetailTarget, FingerprintResult, PelicanResult, QualityConfig, QualityCredential, QualityTest, TraceResult, Usage } from "./types";

type Translate = ReturnType<typeof useT>;

const badgeClass = (tone: Tone) => `badge ${tone}`.trim();

export function Metrics({ usage }: { usage: Usage }) {
  const t = useT();
  const reasoning = usage.reasoning_tokens ?? 0;
  return (
    <span className="quality-metrics">
      {t("quality.metrics", { duration: formatMillis(usage.duration_ms ?? 0), input: tokenCount(usage.input_tokens), output: tokenCount(usage.output_tokens) })}
      {reasoning > 0 && <> · {t("quality.reasoning", { tokens: tokenCount(reasoning) })}</>}
    </span>
  );
}

const CANDY_TONE: Record<CandyKind, Tone> = { ok: "ok", bad: "danger", err: "warn", skip: "" };
const CANDY_MARK: Record<CandyKind, string> = { ok: "✓", bad: "✗", err: "!", skip: "–" };

export function CandyVerdict({ record }: { record: CandyResult }) {
  const t = useT();
  const kind = candyKind(record);
  return <span className={badgeClass(CANDY_TONE[kind])}>{t(`quality.candy.${kind}`)}</span>;
}

type PelicanKind = "ok" | "err" | "skip";
const pelicanKind = (record: PelicanResult): PelicanKind => (record.skipped ? "skip" : record.error ? "err" : "ok");
const PELICAN_TONE: Record<PelicanKind, Tone> = { ok: "ok", err: "danger", skip: "" };

export function PelicanVerdict({ record }: { record: PelicanResult }) {
  const t = useT();
  const kind = pelicanKind(record);
  return <span className={badgeClass(PELICAN_TONE[kind])}>{t(`quality.pelican.${kind}`)}</span>;
}

export function FingerprintOutcome({ record }: { record: FingerprintResult }) {
  const t = useT();
  const status = fingerprintStatus(record);
  const model = <span className="mono">{record.model}</span>;
  let detail: ReactNode = null;
  switch (status) {
    case "consistent":
    case "no_baseline":
    case "cancelled":
      detail = model;
      break;
    case "substitution":
      detail = <>{model} → <strong className="mono">{nearestModel(record) || "—"}</strong></>;
      break;
    case "different":
      detail = t("quality.fingerprint.differentHint");
      break;
    case "ambiguous":
    case "unstable":
      detail = t(record.mode === "strict" ? "quality.fingerprint.retryLater" : "quality.fingerprint.retryStrict");
      break;
    case "insufficient":
      detail = t("quality.fingerprint.recollect");
      break;
    case "failed":
      detail = t("quality.fingerprint.failedHint");
      break;
    case "skipped":
      detail = recordError(t, record.error) || t("quality.unsupported");
      break;
  }
  return (
    <div className="quality-result">
      <span className={badgeClass(fingerprintTone(status))}>{labelOr(t, `quality.fingerprint.status.${status || "idle"}`, status)}</span>
      {detail && <span className="quality-detail">{detail}</span>}
    </div>
  );
}

export function TraceOutcome({ record }: { record: TraceResult }) {
  const t = useT();
  const attribution = record.attribution;
  const stopped = record.status === "failed" || record.status === "cancelled" || record.status === "skipped";
  if (stopped || !attribution) {
    const status = stopped ? record.status : "idle";
    const hint = record.status === "failed" ? t("quality.modeltrace.failedHint") : record.status === "skipped" ? recordError(t, record.error) || t("quality.unsupported") : "";
    return (
      <div className="quality-result">
        <span className={badgeClass(record.status === "failed" ? "danger" : "")}>{t(`quality.modeltrace.status.${status}`)}</span>
        {hint && <span className="quality-detail">{hint}</span>}
      </div>
    );
  }
  const matches = traceMatches(record);
  return (
    <div className="quality-result">
      <span className={badgeClass(matches === null ? "" : matches ? "ok" : "warn")}>
        {matches === null ? labelOr(t, `quality.modeltrace.status.${record.status}`, record.status) : t(matches ? "quality.modeltrace.match" : "quality.modeltrace.mismatch")}
      </span>
      <span className="quality-detail">
        <strong className="mono">{attribution.prediction}</strong> <span className="mono">{probability(attribution.probability)}</span>
        {record.status === "partial" && <> · {t("quality.modeltrace.partial")}</>}
      </span>
    </div>
  );
}

function Progress({ label, done, total, model, stopping }: { label: string; done?: number; total?: number; model?: string; stopping?: boolean }) {
  const t = useT();
  return (
    <div className="quality-result">
      <span className="badge">{label}{total ? <span className="mono">&nbsp;{done ?? 0}/{total}</span> : null}</span>
      {total ? <progress className="quality-progress" value={done ?? 0} max={total} /> : null}
      {model && <span className="mono quality-detail">{model}</span>}
      {stopping && <span className="field-hint">{t("quality.stoppingHint")}</span>}
    </div>
  );
}

const phaseLabel = (t: Translate, phase?: CollectionPhase) => labelOr(t, `quality.phase.${phase || "collecting"}`, phase || "");

function NotTested() {
  const t = useT();
  return <span className="muted">{t("quality.notTested")}</span>;
}

function SingleResult({ verdict, record }: { verdict: ReactNode; record: CandyResult | PelicanResult }) {
  return (
    <div className="quality-result">
      {verdict}
      <span className="mono quality-detail">{modelLabel(record.model, record.effort)}</span>
      {!record.skipped && <Metrics usage={record} />}
    </div>
  );
}

// The test in progress, else the latest record.
export function LatestResult({ test, credential, config }: { test: QualityTest; credential: QualityCredential; config: QualityConfig }) {
  const t = useT();
  switch (test) {
    case "candy": {
      const progress = credential.running;
      if (progress) return <Progress label={t("quality.phase.testing")} done={progress.done} total={progress.total} />;
      const last = lastOf(credential.results);
      return last ? <SingleResult verdict={<CandyVerdict record={last} />} record={last} /> : <NotTested />;
    }
    case "fingerprint": {
      const progress = credential.fingerprint_running;
      if (progress) return <Progress label={phaseLabel(t, progress.phase)} done={progress.done} total={progress.total} model={progress.model} stopping={progress.phase === "cancelling"} />;
      const last = lastOf(credential.fingerprints);
      return last ? <FingerprintOutcome record={last} /> : <NotTested />;
    }
    case "modeltrace": {
      const progress = credential.modeltrace_running;
      if (progress) return <Progress label={phaseLabel(t, progress.phase)} done={progress.done} total={config.modeltrace.requests} model={progress.model} stopping={progress.phase === "cancelling"} />;
      const last = lastOf(credential.modeltraces);
      return last ? <TraceOutcome record={last} /> : <NotTested />;
    }
    case "pelican": {
      const progress = credential.pelican_running;
      if (progress) {
        const cancelling = progress.phase === "cancelling";
        return <Progress label={t(cancelling ? "quality.phase.cancelling" : "quality.phase.generating")} model={progress.model} stopping={cancelling} />;
      }
      const last = lastOf(credential.pelicans);
      return last ? <SingleResult verdict={<PelicanVerdict record={last} />} record={last} /> : <NotTested />;
    }
  }
}

function CandyMarks({ credential, model, effort, onOpen }: { credential: QualityCredential; model: string; effort: string; onOpen: (target: DetailTarget) => void }) {
  const t = useT();
  return (
    <div className="quality-marks" role="group" aria-label={t("quality.candy.recent", { count: credential.results.length })}>
      {credential.results.map((record, index) => {
        const kind = candyKind(record);
        const label = `${formatDateTime(record.time)} · ${modelLabel(record.model, record.effort)} · ${t(`quality.candy.${kind}`)}`;
        return (
          <button key={`${record.time}|${index}`} type="button" className={`quality-mark ${kind}${inScope(record, model, effort) ? "" : " dim"}`} title={label} aria-label={label}
            onClick={() => onOpen({ test: "candy", credential, id: record.time })}>
            {CANDY_MARK[kind]}
          </button>
        );
      })}
    </div>
  );
}

// Candy: the correct rate at the chosen model and effort, then every recent
// answer; the others: what the latest record was run with.
export function ResultSummary({ test, credential, settings, config, onOpen }: { test: QualityTest; credential: QualityCredential; settings: QualitySettings; config: QualityConfig; onOpen: (target: DetailTarget) => void }) {
  const t = useT();
  switch (test) {
    case "candy": {
      const model = settings.candy.model.trim();
      const { correct, graded } = candyRate(credential.results, model, settings.candy.effort);
      return (
        <div className="quality-summary">
          <span title={t("quality.candy.rateScope", { model: modelLabel(model, settings.candy.effort) })}>
            {graded > 0 ? t("quality.candy.rate", { rate: formatPercent(correct / graded), correct, graded }) : credential.results.length > 0 ? t("quality.candy.noRate") : "—"}
          </span>
          {credential.results.length > 0 && <CandyMarks credential={credential} model={model} effort={settings.candy.effort} onOpen={onOpen} />}
        </div>
      );
    }
    case "fingerprint": {
      const last = lastOf(credential.fingerprints);
      const mode = credential.fingerprint_running?.mode || last?.mode;
      return (
        <div className="quality-summary">
          {mode && <span>{t("quality.fingerprint.modeName", { mode: fingerprintModeName(t, config, mode) })}</span>}
          {last && !credential.fingerprint_running && <time className="muted" dateTime={last.time}>{formatDateTime(last.time) || "—"}</time>}
        </div>
      );
    }
    case "modeltrace": {
      const last = lastOf(credential.modeltraces);
      const model = credential.modeltrace_running?.model || last?.model;
      return (
        <div className="quality-summary">
          {model && <span className="mono">{model}</span>}
          {last && !credential.modeltrace_running && <time className="muted" dateTime={last.time}>{formatDateTime(last.time) || "—"}</time>}
        </div>
      );
    }
    case "pelican": {
      const last = lastOf(credential.pelicans);
      return (
        <div className="quality-summary">
          {last && pelicanKind(last) === "ok" && (
            <button type="button" className="btn sm" onClick={() => onOpen({ test: "pelican", credential, id: last.id })}>{t("quality.pelican.view")}</button>
          )}
          {last && <time className="muted" dateTime={last.time}>{formatDateTime(last.time) || "—"}</time>}
        </div>
      );
    }
  }
}

function HistoryItem({ time, model, usage, children }: { time: string; model: string; usage?: Usage; children: ReactNode }) {
  return (
    <li className="quality-history-item">
      <div className="quality-history-meta">
        <time dateTime={time}>{formatDateTime(time) || "—"}</time>
        <span className="mono">{model}</span>
        {usage && <Metrics usage={usage} />}
      </div>
      <div className="quality-history-body">{children}</div>
    </li>
  );
}

const newestFirst = <T,>(records: T[]) => [...records].reverse();

export function HistoryItems({ test, credential, config, onOpen }: { test: QualityTest; credential: QualityCredential; config: QualityConfig; onOpen: (target: DetailTarget) => void }) {
  const t = useT();
  const details = (target: QualityTest, id: string, label = t("quality.details")) => (
    <button type="button" className="btn sm" onClick={() => onOpen({ test: target, credential, id })}>{label}</button>
  );
  switch (test) {
    case "candy":
      return <>{newestFirst(credential.results).map((record, index) => (
        <HistoryItem key={`${record.time}|${index}`} time={record.time} model={modelLabel(record.model, record.effort)} usage={record.skipped ? undefined : record}>
          <CandyVerdict record={record} />
          {details("candy", record.time, t("quality.candy.viewAnswer"))}
        </HistoryItem>
      ))}</>;
    case "fingerprint":
      return <>{newestFirst(credential.fingerprints).map((record) => (
        <HistoryItem key={record.id} time={record.time} model={record.model} usage={record.status === "skipped" ? undefined : record}>
          <span className="badge">{t("quality.fingerprint.modeName", { mode: fingerprintModeName(t, config, record.mode) })}</span>
          <FingerprintOutcome record={record} />
          {details("fingerprint", record.id)}
        </HistoryItem>
      ))}</>;
    case "modeltrace":
      return <>{newestFirst(credential.modeltraces).map((record) => (
        <HistoryItem key={record.id} time={record.time} model={record.model} usage={record.status === "skipped" ? undefined : record}>
          <TraceOutcome record={record} />
          {details("modeltrace", record.id)}
        </HistoryItem>
      ))}</>;
    case "pelican":
      return <>{newestFirst(credential.pelicans).map((record) => (
        <HistoryItem key={record.id} time={record.time} model={modelLabel(record.model, record.effort)} usage={record.skipped ? undefined : record}>
          <PelicanVerdict record={record} />
          {record.error || record.skipped ? record.error && <span className="quality-detail">{recordError(t, record.error)}</span> : details("pelican", record.id, t("quality.pelican.view"))}
        </HistoryItem>
      ))}</>;
  }
}
