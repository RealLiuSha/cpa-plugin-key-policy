import { beforeEach, describe, expect, it, vi } from "vitest";

const client = vi.hoisted(() => ({
  get: vi.fn(),
  post: vi.fn(),
}));

vi.mock("./client", () => ({
  apiClient: () => client,
  pluginPath: (path: string) => `/plugin${path}`,
}));

import { fetchCatalog } from "./models";

function mockCurrentCPAGet(path: string, config?: { params?: { name?: string } }) {
  if (path === "/v0/management/openai-compatibility") {
    return Promise.resolve({ data: { "openai-compatibility": [] } });
  }
  if (path.endsWith("-api-key")) {
    const field = path.slice(path.lastIndexOf("/") + 1);
    return Promise.resolve({ data: { [field]: [] } });
  }
  if (path === "/v0/management/auth-files") {
    return Promise.resolve({
      data: { files: [{ name: "codex-team.json", provider: "codex", id_token: { plan_type: "team" } }] },
    });
  }
  if (path === "/v0/management/auth-files/models") {
    expect(config?.params?.name).toBe("codex-team.json");
    return Promise.resolve({ data: { models: [{ id: "gpt-5.4", type: "openai" }] } });
  }
  if (path.startsWith("/v0/management/model-definitions/")) {
    const channel = path.slice(path.lastIndexOf("/") + 1);
    const models = channel === "codex" ? [{ id: "gpt-5.4" }, { id: "gpt-5.5" }] : channel === "xai" ? [{ id: "grok-4.6" }] : [];
    return Promise.resolve({ data: { channel, models } });
  }
  return Promise.reject(new Error(`unexpected GET ${path}`));
}

describe("fetchCatalog current CPA contract", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    client.get.mockImplementation(mockCurrentCPAGet);
  });

  it("merges auth-file models with the configured channel definitions", async () => {
    await expect(fetchCatalog()).resolves.toEqual([
      { provider: "codex", model: "gpt-5.4" },
      { provider: "codex", model: "gpt-5.5" },
    ]);
    expect(client.post).not.toHaveBeenCalled();
  });

  it("keeps channels that existing models route to", async () => {
    await expect(fetchCatalog(new Set(["XAI"]))).resolves.toContainEqual({ provider: "xai", model: "grok-4.6" });
  });
});
