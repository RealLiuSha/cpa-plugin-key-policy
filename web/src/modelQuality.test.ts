import { describe, expect, it } from "vitest";
import { formatMetric, isLow, modelSummary, peerMedians } from "./modelQuality";
import type { ChannelModelSummary } from "./types";

const summary = (attempts: number, reasoning: number, cache: number, speed: number): ChannelModelSummary => ({
  model: "gpt-6.1-sol", attempts, failures: 0, success_rate: 1, prompt_tokens: 1000, cached_tokens: cache * 1000, generated_tokens: 100,
  reasoning_tokens: reasoning * attempts, cache_hit_rate: cache, reasoning_per_attempt: reasoning, tokens_per_second: speed, ttft_avg_ms: 900,
});

describe("model quality comparison", () => {
  it("marks a channel well below the median of its peers with enough traffic", () => {
    const peers = [summary(100, 800, 0.8, 60), summary(50, 760, 0.75, 55), summary(40, 300, 0.1, 58), summary(5, 10, 0, 1)];
    const medians = peerMedians(peers);
    // The 5-attempt channel is not a peer: medians of the other three.
    expect(medians).toEqual({ reasoning: 760, cache: 0.75, speed: 58 });
    expect(isLow(peers[2], "reasoning", medians)).toBe(true);
    expect(isLow(peers[2], "cache", medians)).toBe(true);
    expect(isLow(peers[2], "speed", medians)).toBe(false);
    expect(isLow(peers[3], "reasoning", medians)).toBe(false);
    // One channel alone has no peers, and a model that hardly reasons is not compared on it.
    expect(peerMedians([peers[0]])).toEqual({});
    expect(peerMedians([summary(30, 10, 0.5, 40), summary(30, 12, 0.5, 40)]).reasoning).toBeUndefined();
    expect(formatMetric(peers[0], "cache")).toBe("80%");
    // The statistics key a model in lower case without its reasoning suffix.
    expect(modelSummary(peers, "GPT-6.1-Sol(high)")).toBe(peers[0]);
    expect(formatMetric(undefined, "speed")).toBe("—");
  });
});
