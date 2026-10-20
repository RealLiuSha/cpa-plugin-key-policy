import { describe, expect, it } from "vitest";
import type { CpaAuthFile, CpaConfigCredential } from "./api/cpa";
import { failureRate, groupChannels, mergeChannels } from "./channelMerge";
import type { ChannelSummary, ChannelView } from "./types";

const summary = (attempts = 0, failures = 0): ChannelSummary => ({
  attempts, failures, success_rate: attempts ? 1 - failures / attempts : 0, latency_p50_ms: 0, latency_p95_ms: 0,
  ttft_p50_ms: 0, ttft_p95_ms: 0, input_tokens: 0, output_tokens: 0, list_cost_usd: 0,
});
const view = (authIndex: string, overrides: Partial<ChannelView> = {}): ChannelView => ({
  auth_index: authIndex, meta: { auth_index: authIndex }, last_24h: summary(10, 1), today: summary(), rules: 0, ...overrides,
});
const key = (overrides: Partial<CpaConfigCredential>): CpaConfigCredential => ({
  kind: "codex", provider: "codex", name: "", authIndex: "", baseUrl: "https://inferhub.net/v1", models: ["gpt-6.1-sol"],
  excludedModels: [], prefix: "", disabled: false, position: 1, ...overrides,
});
const file = (overrides: Partial<CpaAuthFile>): CpaAuthFile => ({
  id: "xai-a.json", authIndex: "", name: "xai-a.json", provider: "xai", label: "a@example.com", status: "active", statusMessage: "",
  disabled: false, unavailable: false, nextRetryAfter: "", cooldowns: [], success: 0, failed: 0, ...overrides,
});
const byKey = (rows: ReturnType<typeof mergeChannels>) => new Map(rows.map((row) => [row.key, row]));

describe("mergeChannels", () => {
  it("joins config keys with the plugin's data by auth_index and names them", () => {
    const rows = byKey(mergeChannels({
      config: [
        key({ authIndex: "aaaa000000000001" }),
        key({ authIndex: "aaaa000000000002", position: 2 }),
        key({ authIndex: "aaaa000000000003", position: 3, excludedModels: ["*"] }),
        key({ position: 4 }),
        key({ authIndex: "aaaa000000000005", position: 5, models: [] }),
        key({ authIndex: "aaaa000000000006", position: 6, models: ["m1", "m2", "m3", "m4"] }),
      ],
      authFiles: [],
      channels: [view("aaaa000000000001"), view("aaaa000000000002", { meta: { auth_index: "aaaa000000000002", display_name: " Inferhub main ", vendor: "Inferhub" } })],
      cpaListed: true,
    }));
    // CPA's key lists carry no group name: an unnamed key is named by its models.
    expect(rows.get("aaaa000000000001")).toMatchObject({ origin: "config", name: "gpt-6.1-sol", vendor: "inferhub.net", state: "configured" });
    expect(rows.get("aaaa000000000005")).toMatchObject({ name: "aaaa000000000005" });
    expect(rows.get("aaaa000000000006")).toMatchObject({ name: "m1, m2 +2" });
    expect(rows.get("aaaa000000000001")?.view?.last_24h.attempts).toBe(10);
    expect(rows.get("aaaa000000000002")).toMatchObject({ name: "Inferhub main", vendor: "Inferhub" });
    expect(rows.get("aaaa000000000003")).toMatchObject({ state: "excluded" });
    // CPA has no auth_index for the fourth key yet: a config-only row.
    expect(rows.get("config:codex:4")).toMatchObject({ authIndex: "", name: "gpt-6.1-sol", state: "configured", view: undefined });
    expect(rows.size).toBe(6);
  });

  it("expands OpenAI-compatible key entries and keeps a disabled provider", () => {
    const rows = mergeChannels({
      config: [
        key({ kind: "openai-compatibility", provider: "nbility", name: "nbility", authIndex: "bbbb000000000001", keyPosition: 1, baseUrl: "https://api.nbility.dev/v1" }),
        key({ kind: "openai-compatibility", provider: "nbility", name: "nbility", authIndex: "bbbb000000000002", keyPosition: 2, baseUrl: "https://api.nbility.dev/v1" }),
        key({ kind: "openai-compatibility", provider: "old", name: "old", position: 2, disabled: true }),
      ],
      authFiles: [],
      channels: [],
      cpaListed: true,
    });
    expect(rows.map((row) => [row.key, row.name, row.state])).toEqual([
      ["bbbb000000000001", "nbility #1", "configured"],
      ["bbbb000000000002", "nbility #2", "configured"],
      ["config:openai-compatibility:2", "old", "disabled"],
    ]);
  });

  it("names the row a duplicate config key shares its auth_index with", () => {
    // Same base URL and key: CPA gives both providers one auth_index.
    const rows = mergeChannels({
      config: [
        key({ kind: "openai-compatibility", provider: "zy-a", name: "zy-a", authIndex: "aaaa000000000001" }),
        key({ kind: "openai-compatibility", provider: "zy-b", name: "zy-b", authIndex: "aaaa000000000001", position: 2 }),
      ],
      authFiles: [],
      channels: [],
      cpaListed: true,
    });
    expect(rows.map((row) => [row.key, row.authIndex, row.sharedWith])).toEqual([
      ["aaaa000000000001", "aaaa000000000001", undefined],
      ["config:openai-compatibility:2", "", "zy-a"],
    ]);
  });

  it("takes OAuth state from the auth file and lists channels CPA no longer has", () => {
    const sources = {
      config: [key({ authIndex: "cccc000000000001" })],
      authFiles: [
        file({ authIndex: "dddd000000000001" }),
        file({ id: "b.json", authIndex: "dddd000000000002", label: "", name: "b.json", unavailable: true, nextRetryAfter: "2026-10-19T01:00:00Z" }),
        file({ id: "c.json", authIndex: "dddd000000000003", cooldowns: [{ scope: "model", model: "grok-5", reason: "quota", retryAt: "2026-10-19T01:00:00Z" }] }),
        file({ id: "d.json", authIndex: "dddd000000000004", disabled: true }),
        file({ id: "e.json", authIndex: "dddd000000000005", status: "error", unavailable: true }),
        // An auth file sharing a config key's auth_index adds its live state to that row.
        file({ id: "codex:apikey:1a2b3c4d5e6f", authIndex: "cccc000000000001", unavailable: true }),
      ],
      channels: [view("dddd000000000001"), view("eeee000000000001", { provider: "claude", base_url: "https://gone.example/v1" })],
      cpaListed: true,
    };
    const rows = byKey(mergeChannels(sources));
    expect(rows.get("dddd000000000001")).toMatchObject({ origin: "oauth", name: "a@example.com", vendor: "xai", state: "active" });
    expect(rows.get("dddd000000000001")?.view).toBeDefined();
    expect(rows.get("dddd000000000002")).toMatchObject({ name: "b.json", state: "unavailable" });
    expect(rows.get("dddd000000000003")?.state).toBe("cooldown");
    expect(rows.get("dddd000000000004")?.state).toBe("disabled");
    expect(rows.get("dddd000000000005")?.state).toBe("error");
    expect(rows.get("cccc000000000001")).toMatchObject({ origin: "config", state: "unavailable" });
    expect(rows.get("eeee000000000001")).toMatchObject({ origin: "plugin", state: "missing", vendor: "gone.example", provider: "claude" });
    expect(rows.size).toBe(7);

    // Without CPA's lists nothing can be called missing.
    expect(mergeChannels({ ...sources, config: [], authFiles: [], cpaListed: false }).map((row) => row.state)).toEqual(["unknown", "unknown"]);
  });
});

describe("channel helpers", () => {
  it("groups by vendor in name order", () => {
    const rows = mergeChannels({
      config: [key({ authIndex: "f2", baseUrl: "https://b.example/v1" }), key({ authIndex: "f1", position: 2, baseUrl: "https://b.example/v1" })],
      authFiles: [file({ authIndex: "f3" })],
      channels: [view("f2", { meta: { auth_index: "f2", display_name: "Alpha" } }), view("f1", { meta: { auth_index: "f1", display_name: "Zulu" } })],
      cpaListed: true,
    });
    expect(groupChannels(rows).map((group) => [group.vendor, group.rows.map((item) => item.name)])).toEqual([
      ["b.example", ["Alpha", "Zulu"]],
      ["xai", ["a@example.com"]],
    ]);
  });

  it("computes failure rates", () => {
    expect(failureRate(summary(20, 5))).toBe(0.25);
    expect(failureRate(summary())).toBeNull();
    expect(failureRate(undefined)).toBeNull();
  });
});
