import { failureRate, type ChannelRow, type ChannelState } from "../channelMerge";
import { formatDateTime, formatPercent } from "../format";
import { useT } from "../i18n";
import type { ChannelSummary } from "../types";

const STATE_TONE: Record<ChannelState, string> = {
  active: "ok", configured: "", disabled: "", excluded: "", error: "danger", unavailable: "danger", cooldown: "warn", missing: "warn", unknown: "",
};

// CPA's side of the channel: config state, or the auth file's live state.
export function ChannelStateBadge({ row }: { row: ChannelRow }) {
  const t = useT();
  const auth = row.auth;
  const retry = auth?.nextRetryAfter ? formatDateTime(auth.nextRetryAfter) : "";
  const title = [auth?.statusMessage, retry && t("channels.retryAt", { time: retry })].filter(Boolean).join(" · ") || undefined;
  return <span className={`badge ${STATE_TONE[row.state]}`.trim()} title={title}>{t(`channels.state.${row.state}`)}</span>;
}

// Failed share of the last 24 hours: warn from 5 %, danger from 20 %.
export const FAILURE_WARN = 0.05;
export const FAILURE_DANGER = 0.2;

export function FailurePill({ summary }: { summary?: ChannelSummary }) {
  const t = useT();
  const rate = failureRate(summary);
  if (rate === null || !(summary!.failures > 0)) return null;
  const tone = rate >= FAILURE_DANGER ? " danger" : rate >= FAILURE_WARN ? " warn" : "";
  return <span className={"badge" + tone} title={t("channels.failuresTitle", { failures: summary!.failures, attempts: summary!.attempts })}>{t("channels.failurePill", { rate: formatPercent(rate) })}</span>;
}
