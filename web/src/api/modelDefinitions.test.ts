import { beforeEach, describe, expect, it, vi } from "vitest";

const client = vi.hoisted(() => ({
  get: vi.fn(),
  post: vi.fn(),
}));

vi.mock("./client", () => ({
  apiClient: () => client,
  pluginPath: (path: string) => `/plugin${path}`,
}));

import { importModels, previewModelPrices, upsertModelDefinition } from "./modelDefinitions";

describe("model definition management APIs", () => {
  beforeEach(() => {
    vi.clearAllMocks();
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
    await expect(previewModelPrices(["cpa/gpt-4o"])).resolves.toMatchObject({
      source: "Models.dev",
      matches: [expect.objectContaining({ model: "gpt-4o", cache_write_price_per_1m: 3.75 })],
      unmatched_models: ["mystery"],
    });
    expect(client.post).toHaveBeenCalledWith("/plugin/models/pricing-preview", { models: ["cpa/gpt-4o"] });
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

  it("posts only writable model fields", async () => {
    client.post.mockResolvedValue({ data: { model: { name: "fast" } } });
    await upsertModelDefinition({
      name: "fast",
      targets: [{ provider: "codex", target_model: "gpt" }],
      dispatch: "round-robin",
      billing_mode: "tokens",
      free: false,
      input_price_per_million: 1,
      output_price_per_million: 2,
      ref_count: 2,
      ref_keys: ["k1", "k2"],
    });
    expect(client.post).toHaveBeenCalledWith("/plugin/models", {
      billing_multiplier: 1,
      name: "fast",
      targets: [{ provider: "codex", target_model: "gpt" }],
      dispatch: "round-robin",
      billing_mode: "tokens",
      free: false,
      input_price_per_million: 1,
      output_price_per_million: 2,
      cache_read_price_per_million: undefined,
      per_call_usd: undefined,
    });
  });

  it("narrows import apply results", async () => {
    client.post.mockResolvedValue({
      data: { created: [{ name: "fast", action: "create" }], updated: [], skipped: [], conflicts: [], missing_price: [], affected_keys: ["k1"] },
    });
    await expect(importModels({ dry_run: false, items: [{ name: "fast", targets: [{ provider: "codex", target_model: "gpt" }] }] })).resolves.toEqual({
      created: [expect.objectContaining({ name: "fast", action: "create" })],
      updated: [], skipped: [], conflicts: [], missing_price: [], affected_keys: ["k1"],
    });
  });
});
