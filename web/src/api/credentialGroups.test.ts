import { beforeEach, describe, expect, it, vi } from "vitest";

const client = vi.hoisted(() => ({ get: vi.fn() }));

vi.mock("./client", () => ({
  apiClient: () => client,
  pluginPath: (path: string) => `/plugin${path}`,
}));

import { fetchCredentialDescriptors } from "./credentialGroups";

describe("credential descriptor CPA contract", () => {
  beforeEach(() => vi.clearAllMocks());

  it("uses the canonical auth filename, provider and flat plan claim", async () => {
    client.get.mockResolvedValue({ data: { files: [{
      id: "runtime-auth-id",
      name: "codex-team.json",
      provider: "codex",
      id_token: { plan_type: "team" },
    }] } });

    await expect(fetchCredentialDescriptors()).resolves.toEqual([{
      id: "codex-team.json",
      provider: "codex",
      attributes: { plan_type: "team" },
    }]);
  });
});
