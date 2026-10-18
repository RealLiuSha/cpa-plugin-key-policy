import { AxiosError, AxiosHeaders, type AxiosResponse } from "axios";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { extractApiError } from "./error";

const client = vi.hoisted(() => ({ get: vi.fn() }));

vi.mock("./client", () => ({
  apiClient: () => client,
  pluginPath: (path: string) => `/plugin${path}`,
}));

import { exportStatsCSV, fetchStats, isRangeTooLarge } from "./stats";

// jsdom's Blob lacks text(), which every supported browser has.
Blob.prototype.text ??= function text(this: Blob) {
  return new Promise<string>((resolve) => {
    const reader = new FileReader();
    reader.onload = () => resolve(String(reader.result));
    reader.readAsText(this);
  });
};

function failure(status: number, body: string): AxiosError {
  const response = { status, data: new Blob([body], { type: "application/json" }), headers: {}, statusText: "", config: { headers: new AxiosHeaders() } } as AxiosResponse;
  return new AxiosError("Request failed with status code " + status, "ERR_BAD_REQUEST", undefined, undefined, response);
}

describe("stats API", () => {
  beforeEach(() => vi.clearAllMocks());

  it("reads /stats for the range and treats missing lists as empty", async () => {
    client.get.mockResolvedValue({ data: { timezone: "Asia/Shanghai", from: "2026-10-11", to: "2026-10-17", totals: {}, days: [], models: [], keys: [], keys_total: 0, keys_truncated: false, unpriced_models: null } });
    await expect(fetchStats(7)).resolves.toMatchObject({ from: "2026-10-11", unpriced_models: [] });
    expect(client.get).toHaveBeenCalledWith("/plugin/stats", { params: { days: 7 } });
  });

  it("downloads the CSV as a blob named after the range", async () => {
    const blob = new Blob(["date,key_id\n"], { type: "text/csv" });
    client.get.mockResolvedValue({ data: blob });
    await expect(exportStatsCSV("2026-09-18", "2026-10-17")).resolves.toEqual({ blob, filename: "cpa-key-policy-usage-2026-09-18_2026-10-17.csv" });
    expect(client.get).toHaveBeenCalledWith("/plugin/stats/export", { params: { from: "2026-09-18", to: "2026-10-17" }, responseType: "blob" });
  });

  it("decodes the JSON error body of a failed blob request and recognizes 413", async () => {
    client.get.mockRejectedValueOnce(failure(400, '{"error":{"code":"invalid_range","message":"from is not a date"}}'));
    const invalid = await exportStatsCSV("x", "2026-10-17").catch((error: unknown) => error);
    expect(extractApiError(invalid, "fallback")).toBe("from is not a date");
    expect(isRangeTooLarge(invalid)).toBe(false);

    client.get.mockRejectedValueOnce(failure(413, '{"error":{"code":"range_too_large","message":"too many rows"}}'));
    const tooLarge = await exportStatsCSV("2026-07-20", "2026-10-17").catch((error: unknown) => error);
    expect(isRangeTooLarge(tooLarge)).toBe(true);
    expect(isRangeTooLarge(new Error("offline"))).toBe(false);
  });
});
