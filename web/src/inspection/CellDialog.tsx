import { useState } from "react";
import { extractApiError } from "../api/error";
import { fetchInspectionAnswers, type InspectionAnswers, type InspectionCell, type InspectionRun } from "../api/inspection";
import { isRecordGone } from "../api/quality";
import Modal from "../components/Modal";
import { useT } from "../i18n";
import { recordError } from "../quality/helpers";
import { verdictTone, zonedTime } from "./helpers";

interface Props {
  name: string;
  date: string;
  cell: InspectionCell;
  timezone: string;
  onClose: () => void;
}

// One channel's rounds of a day; a round's wrong answers load on demand.
export default function CellDialog({ name, date, cell, timezone, onClose }: Props) {
  const t = useT();
  return (
    <Modal title={t("inspection.cell.title", { name, date })} closeLabel={t("inspection.cell.close")} onClose={onClose} wide>
      <p className="inspection-cell-summary">
        <span className="mono">{cell.model}{cell.effort !== "none" ? ` (${cell.effort})` : ""}</span>
        <span className={`badge ${verdictTone(cell.verdict)}`}>{t(`inspection.verdict.${cell.verdict}`)}</span>
        <span>{t("inspection.cell.total", { correct: cell.correct, answered: cell.answered, failed: cell.failed })}</span>
      </p>
      <ol className="inspection-rounds">
        {cell.rounds.map((run) => <RoundItem key={run.round} run={run} cell={cell} timezone={timezone} />)}
      </ol>
    </Modal>
  );
}

function RoundItem({ run, cell, timezone }: { run: InspectionRun; cell: InspectionCell; timezone: string }) {
  const t = useT();
  const [answers, setAnswers] = useState<InspectionAnswers | null>(null);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState("");
  const load = async () => {
    setLoading(true);
    setError("");
    try {
      setAnswers(await fetchInspectionAnswers(run.round, cell));
    } catch (reason) {
      setError(isRecordGone(reason) ? t("inspection.cell.answersGone") : extractApiError(reason, t("inspection.cell.answersFailed")));
    } finally {
      setLoading(false);
    }
  };
  const misses = answers?.misses ?? [];
  return (
    <li className="inspection-round">
      <div className="inspection-round-head">
        <span className="mono">{zonedTime(run.started, timezone)}</span>
        <span className="badge">{t(run.scheduled ? "inspection.cell.scheduled" : "inspection.cell.manual")}</span>
        {run.skipped ? <span className="muted">{t(`inspection.skipped.${run.skipped}`)}</span> : (
          <span>{t("inspection.cell.round", { correct: run.correct, answered: run.answered })}{run.failed > 0 && <> · <span className="inspection-warn">{t("inspection.cell.failed", { count: run.failed })}</span></>}</span>
        )}
        {run.wrong_answers > 0 && !answers && (
          <button type="button" className="btn sm" disabled={loading} onClick={() => void load()}>{t("inspection.cell.showWrong", { count: run.wrong_answers })}</button>
        )}
      </div>
      {run.error && <div className="field-hint">{recordError(t, run.error)}</div>}
      {error && <div className="error" role="alert">{error}</div>}
      {answers && (misses.length === 0 ? <p className="muted">{t("inspection.cell.answersGone")}</p> : misses.map((miss, index) => (
        <div key={index} className="inspection-miss">
          <details>
            <summary>{miss.question < 0 ? t("inspection.cell.published", { expected: miss.expected }) : t("inspection.cell.question", { index: miss.question + 1, expected: miss.expected })}</summary>
            <pre className="code-block inspection-answer">{miss.prompt}</pre>
          </details>
          <pre className="code-block inspection-answer">{miss.answer}</pre>
        </div>
      )))}
    </li>
  );
}
