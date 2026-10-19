import { describe, expect, it } from "vitest";
import { mergeChannels } from "../channelMerge";
import type { ChannelSummary } from "../types";
import { nameByGroup } from "./channels";
import { fromAuthFiles, fromKeyList, fromOpenAICompatibility, fromPlugins } from "./cpa";

const SECRET = "sk-upstream-secret-value";
const summary: ChannelSummary = {
  attempts: 0, failures: 0, success_rate: 0, latency_p50_ms: 0, latency_p95_ms: 0, ttft_p50_ms: 0, ttft_p95_ms: 0, input_tokens: 0, output_tokens: 0, list_cost_usd: 0, upstream_cost_usd: 0,
};

describe("CPA response projections", () => {
  it("keep CPA's credential fields the UI shows and drop keys, proxies and headers", () => {
    const keys = fromKeyList("codex", {
      "codex-api-key": [{
        "api-key": SECRET, "base-url": "https://inferhub.net/v1", "proxy-url": `http://user:${SECRET}@proxy:8080`,
        headers: { Authorization: `Bearer ${SECRET}` }, models: [{ name: "gpt-6.1-sol", "display-name": "Sol" }, { name: "" }],
        "excluded-models": ["*"], priority: -1, weight: 3, "auth-index": "47c2d6c61505edc3",
      }],
    });
    expect(keys).toEqual([{
      kind: "codex", provider: "codex", name: "", authIndex: "47c2d6c61505edc3", baseUrl: "https://inferhub.net/v1", models: ["gpt-6.1-sol"],
      excludedModels: ["*"], priority: -1, weight: 3, prefix: "", disabled: false, position: 1,
    }]);

    const compat = fromOpenAICompatibility({
      "openai-compatibility": [{ name: "nbility", "base-url": "https://api.nbility.dev/v1", headers: { "X-Key": SECRET },
        "api-key-entries": [{ "api-key": SECRET, "auth-index": "a1" }, { "api-key": SECRET, "proxy-url": SECRET, weight: 2, "auth-index": "a2" }] }],
    });
    expect(compat.map((entry) => [entry.authIndex, entry.keyPosition, entry.weight])).toEqual([["a1", 1, undefined], ["a2", 2, 2]]);

    const files = fromAuthFiles({
      files: [{ id: "codex-x", auth_index: "b1", name: "codex-x", provider: "codex", label: "codex-apikey", account_type: "api_key", account: SECRET,
        id_token: { sub: SECRET }, path: "/root/.cli-proxy-api/codex-x.json", status: "active", unavailable: false, cooldowns: [{ scope: "model", model_key: "gpt-6", reason: "quota", retry_at: "2026-10-19T01:00:00Z", remaining_seconds: 30 }] }],
    });
    expect(files[0]).toMatchObject({ authIndex: "b1", label: "codex-apikey", cooldowns: [{ scope: "model", model: "gpt-6", reason: "quota", retryAt: "2026-10-19T01:00:00Z" }] });
    expect(JSON.stringify([keys, compat, files])).not.toContain(SECRET);
  });

  it("reads the interactions, meta and vertex lists, so their channels are not reported missing", () => {
    const meta = fromKeyList("meta", { "meta-api-key": [{ "api-key": SECRET, models: [{ name: "llama-5" }], "auth-index": "0123456789abcdef" }] });
    const rows = mergeChannels({
      config: meta,
      authFiles: [],
      channels: [{ auth_index: "0123456789abcdef", provider: "meta", meta: { auth_index: "0123456789abcdef" }, last_24h: summary, today: summary, balance: null, rules: 0 }],
      cpaListed: true,
    });
    expect(rows).toHaveLength(1);
    expect(rows[0]).toMatchObject({ origin: "config", state: "configured", provider: "meta", config: { models: ["llama-5"] } });
    // CPA reports interactions credentials as gemini-interactions.
    expect(fromKeyList("interactions", { "interactions-api-key": [{ "auth-index": "a1" }] })[0]).toMatchObject({ kind: "interactions", provider: "gemini-interactions" });
    expect(fromKeyList("vertex", { "vertex-api-key": [{ "base-url": "https://vertex.example/v1", "auth-index": "a2" }] })[0]).toMatchObject({ kind: "vertex", provider: "vertex", authIndex: "a2" });
  });

  it("reads the plugin list and tolerates missing lists", () => {
    expect(fromPlugins({ plugins: [{ id: "alpha-search-model", registered: true, metadata: { name: "alpha-search-model" } }, "x"] })).toEqual([
      { id: "alpha-search-model" }, { id: "" },
    ]);
    expect(fromKeyList("xai", { "xai-api-key": null })).toEqual([]);
    expect(fromAuthFiles({})).toEqual([]);
  });

  it("names config keys by their CPA config group and tells keys of one group apart", () => {
    const keys = fromKeyList("codex", { "codex-api-key": [
      { "base-url": "https://a/v1", "auth-index": "i1", models: [{ name: "gpt-image-2" }] },
      { "base-url": "https://a/v1", "auth-index": "i2", models: [{ name: "gpt-image-2" }] },
      { "base-url": "https://b/v1", "auth-index": "i3", models: [{ name: "gpt-6.1-sol" }] },
      { "base-url": "https://c/v1", "auth-index": "i4", models: [{ name: "gpt-6.1-sol" }] },
    ] });
    const named = nameByGroup(keys, { i1: "codex-6", i2: "codex-6", i3: "codex-18" });
    expect(named.map((entry) => [entry.name, entry.keyPosition])).toEqual([["codex-6", 1], ["codex-6", 2], ["codex-18", undefined], ["", undefined]]);
    const rows = mergeChannels({ config: named, authFiles: [], channels: [], cpaListed: true });
    expect(rows.map((row) => row.name)).toEqual(["codex-6 #1", "codex-6 #2", "codex-18", "gpt-6.1-sol"]);
  });
});
