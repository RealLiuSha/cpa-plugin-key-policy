import { modelKey } from "./channelModels";
import { formatCount, formatPercent } from "./format";
import type { ChannelModelSummary } from "./types";

// Signals of what an upstream really serves, read from real traffic: the
// reasoning tokens per answer, the cached share of the prompt and the pace
// of generation. A channel that swaps in a cheaper model or effort, or skips
// the provider's prompt cache, shows them lower than its peers serving the
// same model. Providers count them differently, so only channels of one
// model are compared, and only with enough traffic.

export type QualityMetric = "reasoning" | "cache" | "speed";
export const QUALITY_METRICS: readonly QualityMetric[] = ["reasoning", "cache", "speed"];

// Fewer successful attempts than this in the window say nothing either way.
export const MIN_COMPARED_ATTEMPTS = 20;
// A value under this share of the peers' median is pointed out.
export const LOW_SHARE = 0.6;
// Medians below these are noise: a model that hardly reasons, a prompt that
// is hardly ever cached.
const MEANINGFUL_MEDIAN: Record<QualityMetric, number> = { reasoning: 50, cache: 0.1, speed: 1 };

export function metricValue(summary: ChannelModelSummary, metric: QualityMetric): number {
  switch (metric) {
    case "reasoning": return summary.reasoning_per_attempt;
    case "cache": return summary.cache_hit_rate;
    case "speed": return summary.tokens_per_second;
  }
}

export function formatMetricValue(metric: QualityMetric, value: number): string {
  switch (metric) {
    case "reasoning": return formatCount(Math.round(value));
    case "cache": return formatPercent(value);
    case "speed": return value > 0 ? `${value.toFixed(value >= 100 ? 0 : 1)}/s` : "—";
  }
}

// "—" without a successful attempt, or a cache share without prompt tokens.
export function formatMetric(summary: ChannelModelSummary | undefined, metric: QualityMetric): string {
  if (!summary || successes(summary) <= 0 || (metric === "cache" && summary.prompt_tokens <= 0)) return "—";
  return formatMetricValue(metric, metricValue(summary, metric));
}

export const successes = (summary?: ChannelModelSummary) => (summary ? summary.attempts - summary.failures : 0);

function median(values: number[]): number {
  const sorted = [...values].sort((a, b) => a - b);
  const middle = Math.floor(sorted.length / 2);
  return sorted.length % 2 ? sorted[middle] : (sorted[middle - 1] + sorted[middle]) / 2;
}

// The peers' median of each metric over the channels with enough traffic,
// when at least two have it.
export function peerMedians(summaries: (ChannelModelSummary | undefined)[]): Partial<Record<QualityMetric, number>> {
  const compared = summaries.filter((summary): summary is ChannelModelSummary => successes(summary) >= MIN_COMPARED_ATTEMPTS);
  const out: Partial<Record<QualityMetric, number>> = {};
  if (compared.length < 2) return out;
  for (const metric of QUALITY_METRICS) {
    const value = median(compared.map((summary) => metricValue(summary, metric)));
    if (value >= MEANINGFUL_MEDIAN[metric]) out[metric] = value;
  }
  return out;
}

// Whether a channel's metric is well below its peers'.
export function isLow(summary: ChannelModelSummary | undefined, metric: QualityMetric, medians: Partial<Record<QualityMetric, number>>): boolean {
  const peer = medians[metric];
  return peer !== undefined && successes(summary) >= MIN_COMPARED_ATTEMPTS && metricValue(summary!, metric) < peer * LOW_SHARE;
}

export const modelSummary = (summaries: ChannelModelSummary[] | undefined, model: string) =>
  summaries?.find((summary) => summary.model === modelKey(model));
