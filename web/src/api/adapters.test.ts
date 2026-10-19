import { AxiosError, CanceledError } from "axios";
import { beforeEach, describe, expect, it, vi } from "vitest";

const client = vi.hoisted(() => ({ post: vi.fn() }));
vi.mock("./client", () => ({ apiClient: () => client, pluginPath: (path: string) => `/plugin${path}` }));

import { isTryTimeout, tryAdapter } from "./adapters";

describe("tryAdapter", () => {
  beforeEach(() => vi.clearAllMocks());

  it("bounds the real call to five minutes and passes the caller's abort signal", async () => {
    client.post.mockResolvedValue({ data: { status_code: 200, latency_ms: 12, body: "{}", truncated: false } });
    const { signal } = new AbortController();
    await expect(tryAdapter("r1", { model: "gpt-image-2" }, signal)).resolves.toMatchObject({ status_code: 200 });
    expect(client.post).toHaveBeenCalledWith("/plugin/adapters/try", { rule_id: "r1", body: { model: "gpt-image-2" } }, { timeout: 300_000, signal });
  });

  it("tells axios' timeout apart from an abort", () => {
    expect(isTryTimeout(new AxiosError("timeout of 300000ms exceeded", AxiosError.ECONNABORTED))).toBe(true);
    expect(isTryTimeout(new AxiosError("timeout", AxiosError.ETIMEDOUT))).toBe(true);
    expect(isTryTimeout(new CanceledError())).toBe(false);
    expect(isTryTimeout(new Error("timeout"))).toBe(false);
  });
});
