import { formatCount } from "../format";
import { useT } from "../i18n";
import type { LedgerHealth } from "../types";

export const HEALTH_COUNTERS = [
  "flush_failures", "skipped_failed", "ignored_unknown_key", "ignored_no_model", "ignored_empty_usage",
  "clamped_tokens", "invalid_usage", "unmetered_per_call", "unpriced_calls",
] as const satisfies readonly (keyof LedgerHealth)[];

// Process-lifetime ledger counters: the last flush, its error if any, and
// every counter, zero ones dimmed.
export default function LedgerHealthList({ health }: { health: LedgerHealth }) {
  const t = useT();
  const flushError = health.last_flush_error ?? "";
  return (
    <>
      <p className="muted stats-health-hint">{t("stats.health.hint")}</p>
      <dl className="stats-health-list">
        <div>
          <dt>{t("stats.health.lastFlushAt")}</dt>
          <dd>{health.last_flush_at ? new Date(health.last_flush_at).toLocaleString() : t("stats.health.never")}</dd>
        </div>
        {flushError && (
          <div className="stats-health-error" data-testid="stats-flush-error">
            <dt>{t("stats.health.lastFlushError")}</dt>
            <dd className="mono">{flushError}</dd>
          </div>
        )}
        {HEALTH_COUNTERS.map((field) => (
          <div key={field} className={health[field] > 0 ? "" : "zero"} data-field={field}>
            <dt>{t(`stats.health.field.${field}`)}</dt>
            <dd className="mono">{formatCount(health[field])}</dd>
          </div>
        ))}
      </dl>
    </>
  );
}
