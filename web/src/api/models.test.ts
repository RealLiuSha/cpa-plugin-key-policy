import { describe, it, expect } from "vitest";
import {
  normalizeCatalog,
  filterByConfigured,
  fromAuthFileModels,
  fromAuthFiles,
  fromModelDefinitions,
  fromOpenAICompat,
} from "./models";

describe("normalizeCatalog", () => {
  it("lowercases providers, de-duplicates case-insensitively and sorts", () => {
    const out = normalizeCatalog([
      { provider: "Codex", models: ["GPT-5", "b"] },
      { provider: "codex", models: ["gpt-5"] },
      { provider: "claude", models: ["c", ""] },
      { provider: "", models: ["x"] },
    ]);
    expect(out).toEqual([
      { provider: "claude", model: "c" },
      { provider: "codex", model: "b" },
      { provider: "codex", model: "GPT-5" },
    ]);
  });
});

describe("current CPA response adapters", () => {
  // The live /auth-files/models response has no provider field and its model
  // objects carry an upstream "type"; the provider comes from the list endpoint.
  it("uses the auth-files provider with current per-file model ids", () => {
    const entry = fromAuthFileModels("codex", {
      models: [
        { display_name: "GPT 5.4", id: "gpt-5.4", owned_by: "openai", type: "openai" },
        { display_name: "GPT 5.5", id: "gpt-5.5", owned_by: "openai", type: "openai" },
      ],
    });
    expect(normalizeCatalog([entry])).toEqual([
      { provider: "codex", model: "gpt-5.4" },
      { provider: "codex", model: "gpt-5.5" },
    ]);
  });

  it("reads canonical auth-file metadata only", () => {
    expect(fromAuthFiles({ files: [{ name: "codex-team.json", provider: "Codex", id_token: { plan_type: "team" } }] }))
      .toEqual([{ name: "codex-team.json", provider: "codex" }]);
    expect(() => fromAuthFiles({ "auth-files": [{ id: "old", type: "codex" }] })).toThrow();
  });

  it("reads canonical openai compatibility model names only", () => {
    expect(fromOpenAICompat({
      "openai-compatibility": [{ name: "opencode", models: [{ name: "gpt-5" }] }],
    })).toEqual([{ provider: "opencode", models: ["gpt-5"] }]);
    expect(() => fromOpenAICompat({
      "openai-compatibility": [{ provider: "missing-name", models: ["gpt-5"] }],
    })).toThrow();
  });

  it("reads canonical static model definition ids and validates the channel", () => {
    expect(fromModelDefinitions("claude", {
      channel: "claude",
      models: [{ id: "claude-sonnet-4", display_name: "Sonnet" }],
    })).toEqual({ provider: "claude", models: ["claude-sonnet-4"], static: true });
    expect(() => fromModelDefinitions("claude", { channel: "gemini", definitions: [] })).toThrow();
  });
});

describe("filterByConfigured", () => {
  const entries = [
    { provider: "claude", models: ["sonnet"], static: true },
    { provider: "XAI", models: ["grok-4.6"], static: true },
    { provider: "codex", models: ["gpt-5"] },
  ];

  it("keeps credential-backed entries and static channels that are configured or in use", () => {
    expect(filterByConfigured(entries, new Set(["claude"]), new Set()).map((entry) => entry.provider)).toEqual(["claude", "codex"]);
    expect(filterByConfigured(entries, new Set(), new Set(["xai"])).map((entry) => entry.provider)).toEqual(["XAI", "codex"]);
  });
});
