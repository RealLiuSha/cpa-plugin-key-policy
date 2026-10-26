import { act } from "react";
import { createRoot, type Root } from "react-dom/client";
import { Simulate } from "react-dom/test-utils";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import type { NativeState } from "../types";

(globalThis as unknown as { IS_REACT_ACT_ENVIRONMENT?: boolean }).IS_REACT_ACT_ENVIRONMENT = true;
vi.mock("../api/search", async (importOriginal) => ({ ...(await importOriginal<typeof import("../api/search")>()), saveNativeSearch: vi.fn(), probeNativeSearch: vi.fn(), updateNativeExclusion: vi.fn() }));
const { translate } = vi.hoisted(() => ({ translate: (key: string, vars?: Record<string, string | number>) => key + (vars ? ` ${Object.values(vars).join(" ")}` : "") }));
vi.mock("../i18n", () => ({ useT: () => translate }));
import NativeSearch, { NATIVE_ROUND_REFRESH_MS } from "./NativeSearch";
import { nativeState, probeNativeSearch, saveNativeSearch, updateNativeExclusion } from "../api/search";

const state = (patch: Partial<NativeState> = {}): NativeState => nativeState({
  settings: { enabled: true, require_all: false, auto_exclude: true, excluded_models: [], disabled_channels: [], interval_hours: 12 },
  running: true, declared: true,
  names: { "zeu-index": "zeu-main" },
  models: [
    { model: "gpt-6-sol", mode: "native", usable: ["zeu-index"], gaps: [{ auth_index: "kimera-index", state: "unstable" }] },
    { model: "gpt-6-astra", mode: "fallback", usable: [], gaps: [], reason: "no_channel" },
  ],
  channels: [
    { auth_index: "zeu-index", name: "sk-zeu…1234", group: "codex-15", models: ["gpt-6-sol"], model_states: { "gpt-6-sol": "supported" }, state: "supported",
      probes: [{ at: "2026-10-24T01:00:00Z", model: "gpt-6-sol", result: "search", latency_ms: 4200, searches: 1 }] },
    { auth_index: "kimera-index", name: "sk-kim…5678", group: "codex-11", models: ["gpt-6-sol"], model_states: { "gpt-6-sol": "unstable" }, state: "unstable", cpa_disabled: true,
      probes: [{ at: "2026-10-24T01:00:00Z", model: "gpt-6-sol", result: "no_search", latency_ms: 9000 }, { at: "2026-10-23T13:00:00Z", model: "gpt-6-sol", result: "search", latency_ms: 70000 }] },
  ],
  ...patch,
});

describe("NativeSearch", () => {
  let container: HTMLDivElement;
  let root: Root;
  const render = async (native: NativeState, onChanged = vi.fn(), onRefresh = vi.fn()) => {
    await act(async () => { root.render(<NativeSearch native={native} searchEnabled onChanged={onChanged} onRefresh={onRefresh} />); });
    return { onChanged, onRefresh };
  };
  const button = (label: string) => [...container.querySelectorAll<HTMLButtonElement>("button")].find((item) => item.getAttribute("aria-label") === label || item.textContent === label)!;

  beforeEach(() => {
    vi.clearAllMocks();
    container = document.createElement("div");
    document.body.appendChild(container);
    root = createRoot(container);
  });
  afterEach(async () => {
    await act(async () => root.unmount());
    container.remove();
    vi.useRealTimers();
  });

  it("shows how each model searches and on which channels sessions go without", async () => {
    await render(state());
    const sol = container.querySelector("[data-testid='native-model-gpt-6-sol']")!;
    expect(sol.textContent).toContain("search.native.mode.native");
    expect(sol.textContent).toContain("zeu-main");
    expect(sol.textContent).toContain("codex-11 · search.native.state.unstable");
    const astra = container.querySelector("[data-testid='native-model-gpt-6-astra']")!;
    expect(astra.textContent).toContain("search.native.reason.no_channel");
    const kimera = container.querySelector("[data-testid='native-channel-kimera-index']")!;
    expect(kimera.querySelectorAll(".native-dot")).toHaveLength(2);
    expect(kimera.textContent).toContain("search.native.result.no_search");
    // CPA routes nothing to a channel it disabled, so it cannot be checked.
    expect(button("search.native.probeOneFor codex-11").disabled).toBe(true);
    expect(button("search.native.probeOneFor zeu-main").disabled).toBe(false);
  });

  it("disables a channel only after confirming it", async () => {
    const saved = state({ settings: { enabled: true, require_all: false, auto_exclude: true, excluded_models: [], disabled_channels: ["kimera-index"], interval_hours: 12 } });
    vi.mocked(saveNativeSearch).mockResolvedValue(saved);
    const { onChanged } = await render(state());
    await act(async () => { Simulate.click(button("search.native.disableFor codex-11")); });
    await act(async () => { Simulate.click(button("keyForm.cancel")); });
    expect(saveNativeSearch).not.toHaveBeenCalled();

    await act(async () => { Simulate.click(button("search.native.disableFor codex-11")); });
    await act(async () => { Simulate.click(button("search.native.disableApply")); });
    expect(saveNativeSearch).toHaveBeenCalledWith(expect.objectContaining({ disabled_channels: ["kimera-index"], enabled: true }));
    expect(onChanged).toHaveBeenCalledWith(saved);
    expect(container.querySelector("[role=status].success")?.textContent).toBe("search.native.channelDisabled codex-11");
  });

  it("lists what automatic exclusion took off which channel and lets the operator keep a channel serving", async () => {
    const excluded = state({
      models: [{ model: "gpt-6-sol", mode: "native", usable: ["zeu-index"], gaps: [] }],
      channels: [{ auth_index: "kimera-index", name: "sk-kim…5678", group: "codex-11", models: ["gpt-6-astra", "gpt-6.1-sol"], state: "unstable",
        model_states: { "gpt-6-astra": "supported", "gpt-6.1-sol": "no_search" }, probes: [] }],
      exclusions: [{ auth_index: "kimera-index", name: "sk-kim…5678", group: "codex-11", model: "gpt-6-sol", state: "active", since: "2026-10-25T12:00:00Z",
        trials: 1, retry_at: "2026-10-27T12:00:00Z", no_search: 2 }],
    });
    const released = state({ exclusions: [{ ...excluded.exclusions[0], state: "kept", kept_by: "operator", kept_at: "2026-10-26T00:00:00Z" }] });
    vi.mocked(updateNativeExclusion).mockResolvedValue(released);
    const { onChanged } = await render(excluded);
    // The model no longer lists the channel among its gaps, but as excluded.
    expect(container.querySelector("[data-testid='native-model-gpt-6-sol']")?.textContent).toContain("codex-11 · search.native.exclusion.state.active");
    // The channel's state for each model, where it differs from the whole.
    const channel = container.querySelector("[data-testid='native-channel-kimera-index']")!;
    expect(channel.textContent).toContain("gpt-6-astra · search.native.state.supported");
    expect(channel.textContent).toContain("gpt-6.1-sol · search.native.state.no_search");
    const row = container.querySelector("[data-testid='native-exclusion-kimera-index:gpt-6-sol']")!;
    expect(row.textContent).toContain("search.native.exclusion.state.active");
    expect(row.textContent).toContain("search.native.exclusion.evidence 2");
    expect(row.textContent).toContain("search.native.exclusion.trials 1");

    await act(async () => { Simulate.click(button("search.native.releaseFor codex-11 gpt-6-sol")); });
    expect(updateNativeExclusion).toHaveBeenCalledWith("kimera-index", "gpt-6-sol", "release");
    expect(onChanged).toHaveBeenCalledWith(released);
    expect(container.querySelector("[role=status].success")?.textContent).toBe("search.native.exclusionReleased codex-11 gpt-6-sol");

    // Switching it off gives every excluded model back, so it asks first.
    await act(async () => { Simulate.change(container.querySelector<HTMLInputElement>(".native-exclusions-head input")!); });
    expect(saveNativeSearch).not.toHaveBeenCalled();
    vi.mocked(saveNativeSearch).mockResolvedValue(state());
    await act(async () => { Simulate.click(button("search.native.autoOffApply")); });
    expect(saveNativeSearch).toHaveBeenCalledWith(expect.objectContaining({ auto_exclude: false, enabled: true }));
  });

  it("starts a check round, says when one already runs, and follows its progress", async () => {
    vi.mocked(probeNativeSearch).mockRejectedValueOnce(Object.assign(new Error("Conflict"), { response: { status: 409, data: { error: { code: "busy", message: "busy" } } } }));
    await render(state());
    await act(async () => { Simulate.click(button("search.native.probeAll")); });
    expect(probeNativeSearch).toHaveBeenCalledWith(undefined);
    expect(container.querySelector("[role=alert].error")?.textContent).toBe("search.native.probeBusy");

    vi.useFakeTimers();
    const { onRefresh } = await render(state({ round: { kind: "full", started: "2026-10-24T01:00:00Z", done: 1, total: 2 } }));
    expect(container.querySelector(".native-schedule")?.textContent).toContain("search.native.round.full 1 2");
    expect(button("search.native.probeAll").disabled).toBe(true);
    await act(async () => { vi.advanceTimersByTime(NATIVE_ROUND_REFRESH_MS); });
    expect(onRefresh).toHaveBeenCalledTimes(1);
  });
});
