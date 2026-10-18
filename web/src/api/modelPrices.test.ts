import { beforeEach, describe, expect, it, vi } from "vitest";

const client = vi.hoisted(() => ({
  get: vi.fn(),
  post: vi.fn(),
  delete: vi.fn(),
}));

vi.mock("./client", () => ({
  apiClient: () => client,
  pluginPath: (path: string) => `/plugin${path}`,
}));

import { deleteModelPrice, fetchModelPrices, importModels, previewModelPrices, saveModelPrice } from "./modelPrices";

describe("model price management APIs", () => {
  beforeEach(() => {
    vi.clearAllMocks();
  });

  it("reads price rows together with the unpriced models list", async () => {
    client.get.mockResolvedValue({
      data: {
        models: [{ name: "gpt-5.6", provider: "", billing_mode: "tokens", priced: true }],
        unpriced: [{ name: "gpt-6-terra", calls: 120, last_seen: "2026-10-16" }],
      },
    });
    await expect(fetchModelPrices()).resolves.toEqual({
      models: [{ name: "gpt-5.6", provider: "", billing_mode: "tokens", priced: true }],
      unpriced: [{ name: "gpt-6-terra", calls: 120, last_seen: "2026-10-16" }],
    });
    client.get.mockResolvedValue({ data: { models: [] } });
    await expect(fetchModelPrices()).resolves.toEqual({ models: [], unpriced: [] });
  });

  it("narrows Models.dev preview responses", async () => {
    client.post.mockResolvedValue({
      data: {
        source: "Models.dev",
        source_url: "https://models.dev/api.json",
        metadata_models: 1,
        matches: [{
          model: "gpt-4o", matched_model: "gpt-4o", match_type: "index_suffix",
          source: "Models.dev", source_url: "https://models.dev/api.json",
          source_provider_id: "openai", source_provider_name: "OpenAI",
          prompt_price_per_1m: 2.5, completion_price_per_1m: 10,
          cache_read_price_per_1m: 1.25, cache_write_price_per_1m: 3.75,
        }],
        unmatched_models: ["mystery"],
      },
    });
    await expect(previewModelPrices(["gpt-4o"])).resolves.toMatchObject({
      source: "Models.dev",
      matches: [expect.objectContaining({ model: "gpt-4o", cache_write_price_per_1m: 3.75 })],
      unmatched_models: ["mystery"],
    });
    expect(client.post).toHaveBeenCalledWith("/plugin/models/pricing-preview", { models: ["gpt-4o"] });
  });

  it("preserves an unconfigured cache-write price", async () => {
    client.post.mockResolvedValue({
      data: {
        source: "Models.dev", source_url: "https://models.dev/api.json", metadata_models: 1,
        matches: [{
          model: "gpt-mini", matched_model: "gpt-mini", match_type: "index_exact",
          prompt_price_per_1m: 0.15, completion_price_per_1m: 0.6,
        }],
        unmatched_models: [],
      },
    });
    const preview = await previewModelPrices(["gpt-mini"]);
    expect(preview.matches[0].cache_write_price_per_1m).toBeUndefined();
  });

  it("posts only writable price fields, with the provider qualifier", async () => {
    client.post.mockResolvedValue({ data: { model: { name: "gpt-5", priced: true } } });
    await saveModelPrice({
      name: "gpt-5",
      provider: "codex",
      billing_mode: "tokens",
      input_price_per_million: 1,
      output_price_per_million: 2,
    });
    expect(client.post).toHaveBeenCalledWith("/plugin/models", {
      name: "gpt-5",
      provider: "codex",
      billing_mode: "tokens",
      billing_multiplier: 1,
      input_price_per_million: 1,
      output_price_per_million: 2,
      cache_read_price_per_million: 0,
      per_call_usd: 0,
    });
  });

  it("deletes one price row by name and provider", async () => {
    client.delete.mockResolvedValue({ data: { deleted: true } });
    await deleteModelPrice({ name: "gpt-5", provider: "" });
    expect(client.delete).toHaveBeenCalledWith("/plugin/models", { data: { name: "gpt-5", provider: "" } });
  });

  it("imports catalog models as named price rows and narrows the result", async () => {
    client.post.mockResolvedValue({
      data: { created: [{ name: "grok-4.7" }], skipped: [{ name: "grok-4.6", reason: "exists" }] },
    });
    await expect(importModels([{ name: "grok-4.7", provider: "" }])).resolves.toEqual({
      created: [{ name: "grok-4.7", reason: undefined }],
      skipped: [{ name: "grok-4.6", reason: "exists" }],
    });
    expect(client.post).toHaveBeenCalledWith("/plugin/models/import", { items: [{ name: "grok-4.7", provider: "" }] });
  });
});
