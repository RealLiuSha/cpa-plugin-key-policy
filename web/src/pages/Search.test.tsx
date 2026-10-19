import { act } from "react";
import { createRoot, type Root } from "react-dom/client";
import { Simulate } from "react-dom/test-utils";
import { MemoryRouter } from "react-router-dom";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import type { SearchChannel, SearchChannelStatus, SearchState } from "../types";

(globalThis as unknown as { IS_REACT_ACT_ENVIRONMENT?: boolean }).IS_REACT_ACT_ENVIRONMENT = true;
vi.mock("../api/search", async (importOriginal) => ({ ...(await importOriginal<typeof import("../api/search")>()), fetchSearch: vi.fn(), saveSearch: vi.fn(), importSearch: vi.fn() }));
vi.mock("../api/cpa", () => ({ fetchCpaPlugins: vi.fn(async () => []), fetchPluginConfig: vi.fn() }));
vi.mock("../polling", () => ({ useVisiblePolling: vi.fn() }));
const { translate } = vi.hoisted(() => ({ translate: (key: string, vars?: Record<string, string | number>) => key + (vars ? ` ${Object.values(vars).join(" ")}` : "") }));
vi.mock("../i18n", () => ({ useT: () => translate }));
import Search, { nextSearchChannel } from "./Search";
import { fetchSearch } from "../api/search";
import { unsavedMessage } from "../store/unsaved";

const channel = (name: string, force_unhealthy = false, model = "{model}"): SearchChannel => ({ name, model, force_unhealthy });
const status = (name: string, healthy: boolean, forced = false, model = "{model}"): SearchChannelStatus => ({ name, model, healthy, forced, routed: 0 });

describe("nextSearchChannel", () => {
  // Saved: A forced, B and C healthy. Draft: A no longer forced, B forced.
  // Saving routes to A, whose saved status is unhealthy only because of the force.
  it("takes what is forced from the draft, not from the saved status", () => {
    const draft = [channel("A"), channel("B"), channel("C")];
    expect(nextSearchChannel(draft, 1, [status("A", false, true), status("B", true), status("C", true)])).toEqual({ name: "A", fallback: false });
  });

  it("counts a channel whose model template changed as new and healthy", () => {
    const draft = [channel("A", true), channel("B", false, "gpt-6-search")];
    expect(nextSearchChannel(draft, 0, [status("A", true), status("B", false)])).toEqual({ name: "B", fallback: false });
    expect(nextSearchChannel([channel("A", true), channel("B")], 0, [status("A", true), status("B", false)])).toEqual({ name: "A", fallback: true });
  });
});

describe("Search page", () => {
  let container: HTMLDivElement;
  let root: Root;
  const tick = () => new Promise((resolve) => setTimeout(resolve, 0));
  const state: SearchState = {
    enabled: true, declared: true,
    settings: { target_model: "gpt-6.1-sol", keep_models: [], channels: [channel("primary"), channel("backup")], probe: { interval_seconds: 60, timeout_seconds: 15, model: "", url: "", user_agent: "", idle_seconds: 0 } },
    status: { channels: [], routed_total: 0, probe_running: false, probe_key_stored: false },
  };

  beforeEach(() => {
    vi.mocked(fetchSearch).mockResolvedValue(state);
    container = document.createElement("div");
    document.body.appendChild(container);
    root = createRoot(container);
  });
  afterEach(async () => {
    await act(async () => root.unmount());
    container.remove();
  });

  it("names each channel's force switch and marks a draft unsaved until the page goes", async () => {
    await act(async () => { root.render(<MemoryRouter><Search /></MemoryRouter>); await tick(); });
    const switches = [...container.querySelectorAll<HTMLInputElement>(".search-channel input[type=checkbox]")];
    expect(switches.map((item) => item.getAttribute("aria-label"))).toEqual(["search.forceLabelFor primary", "search.forceLabelFor backup"]);

    expect(unsavedMessage()).toBeNull();
    const target = container.querySelector<HTMLInputElement>(".search-form input")!;
    await act(async () => { Simulate.change(target, { target: { value: "gpt-6.2" } as unknown as EventTarget & HTMLInputElement }); });
    expect(unsavedMessage()).toBe("search.leaveMessage");
    await act(async () => root.unmount());
    expect(unsavedMessage()).toBeNull();
    root = createRoot(container);
  });
});
