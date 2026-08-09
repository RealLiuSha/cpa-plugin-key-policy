import { beforeEach, describe, expect, it, vi } from "vitest";

const client = vi.hoisted(() => ({
  get: vi.fn(),
  post: vi.fn(),
  patch: vi.fn(),
  delete: vi.fn(),
}));

vi.mock("./client", () => ({
  apiClient: () => client,
  pluginPath: (path: string) => `/plugin${path}`,
}));

import { createKey, fetchKeyHistory, fetchKeyUsage, patchKey } from "./keys";

describe("current key API client", () => {
  beforeEach(() => vi.clearAllMocks());

  it("sends only public-model references in create and patch requests", async () => {
    const request = {
      id: "team-a",
      models: [{ name: "fast", daily_limit_usd: 3 }],
      daily_limit_usd: 10,
    };
    client.post.mockResolvedValue({ data: { plain_key: "secret" } });
    client.patch.mockResolvedValue({ data: { key: { id: "team-a" } } });

    await createKey(request);
    await patchKey(request);

    expect(client.post).toHaveBeenCalledWith("/plugin/keys", request);
    expect(client.patch).toHaveBeenCalledWith("/plugin/keys", request);
    expect(JSON.stringify(request)).not.toContain("target_model");
    expect(JSON.stringify(request)).not.toContain("price_per_million");
  });

  it("reads model usage and by-model history from current endpoints", async () => {
    client.get
      .mockResolvedValueOnce({ data: { key_id: "team-a", models: [{ name: "fast" }] } })
      .mockResolvedValueOnce({ data: { key_id: "team-a", days: [{ date: "2026-08-09", by_model: { fast: { total_usd: 1 } } }] } });

    await expect(fetchKeyUsage("team-a")).resolves.toMatchObject({ models: [{ name: "fast" }] });
    await expect(fetchKeyHistory("team-a", 7)).resolves.toMatchObject({ days: [{ by_model: { fast: { total_usd: 1 } } }] });
    expect(client.get).toHaveBeenNthCalledWith(1, "/plugin/keys/usage", { params: { id: "team-a" } });
    expect(client.get).toHaveBeenNthCalledWith(2, "/plugin/keys/history", { params: { id: "team-a", days: 7 } });
  });
});
