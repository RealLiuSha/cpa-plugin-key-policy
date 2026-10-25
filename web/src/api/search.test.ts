import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import type { SearchState } from "../types";

const client = vi.hoisted(() => ({ get: vi.fn(), patch: vi.fn() }));
vi.mock("./client", () => ({ apiClient: () => client, pluginPath: (path: string) => `/plugin${path}`, PLUGIN_ID: "cpa-key-policy" }));

import { SEARCH_APPLY_TIMEOUT_MS, SearchApplyTimeout, nativeState, setSearchEnabled } from "./search";

const state = (enabled: boolean, declared: boolean): SearchState => ({
  enabled, declared,
  settings: { target_model: "gpt-6.1-sol", keep_models: ["gpt-*"], channels: [{ name: "primary", model: "{model}", force_unhealthy: false }], probe: { interval_seconds: 60, timeout_seconds: 15, model: "", url: "http://127.0.0.1:8317/v1/alpha/search", user_agent: "probe", idle_seconds: 0 } },
  status: { channels: [], routed_total: 0, probe_running: false, probe_key_stored: false },
  native: nativeState(),
});

beforeEach(() => {
  vi.useFakeTimers();
  vi.clearAllMocks();
  client.patch.mockResolvedValue({ data: {} });
});
afterEach(() => vi.useRealTimers());

describe("setSearchEnabled", () => {
  it("saves search_enabled through CPA and waits out the plugin restart until the router is declared", async () => {
    client.get
      .mockRejectedValueOnce(Object.assign(new Error("Service Unavailable"), { response: { status: 503 } }))
      .mockResolvedValueOnce({ data: state(true, false) })
      .mockResolvedValue({ data: state(true, true) });
    const waits: number[] = [];
    const done = setSearchEnabled(true, (seconds) => waits.push(seconds));
    await vi.advanceTimersByTimeAsync(3_000);
    await expect(done).resolves.toMatchObject({ enabled: true, declared: true });
    expect(client.patch).toHaveBeenCalledWith("/v0/management/plugins/cpa-key-policy/config", { search_enabled: true });
    expect(client.get).toHaveBeenCalledTimes(3);
    expect(waits).toEqual([1, 2]);
  });

  it("stops at once when the search module could not open its file", async () => {
    const unavailable = Object.assign(new Error("Service Unavailable"), { response: { status: 503, data: { error: { code: "module_unavailable", message: "search is unavailable; see the settings page" } } } });
    client.get.mockRejectedValue(unavailable);
    const done = setSearchEnabled(true).catch((error: unknown) => error);
    await vi.advanceTimersByTimeAsync(1_000);
    expect(await done).toBe(unavailable);
    expect(client.get).toHaveBeenCalledTimes(1);
  });

  it("gives up after the deadline with the last state it read", async () => {
    client.get.mockResolvedValue({ data: state(false, true) });
    const done = setSearchEnabled(false).catch((error: unknown) => error);
    await vi.advanceTimersByTimeAsync(SEARCH_APPLY_TIMEOUT_MS + 1_000);
    const error = await done;
    expect(error).toBeInstanceOf(SearchApplyTimeout);
    expect((error as SearchApplyTimeout).last).toMatchObject({ enabled: false, declared: true });
    expect(client.get).toHaveBeenCalledTimes(SEARCH_APPLY_TIMEOUT_MS / 1_000);
  });
});

describe("nativeState", () => {
  it("fills what a plugin without native search leaves out, so the page can still render", () => {
    const filled = nativeState({ models: [{ model: "gpt-6-sol", mode: "native" } as never], channels: [{ auth_index: "a", name: "a", state: "supported" } as never] });
    expect(filled.settings.excluded_models).toEqual([]);
    expect(filled.models[0].gaps).toEqual([]);
    expect(filled.channels[0].probes).toEqual([]);
    expect(filled.names).toEqual({});
  });
});
