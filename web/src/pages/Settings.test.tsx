import { act } from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { createRoot } from "react-dom/client";
import { MemoryRouter } from "react-router-dom";
import type { SettingsResponse } from "../types";

(globalThis as unknown as { IS_REACT_ACT_ENVIRONMENT?: boolean }).IS_REACT_ACT_ENVIRONMENT = true;

vi.mock("../api/settings", () => ({ fetchSettings: vi.fn() }));
const { translate } = vi.hoisted(() => ({ translate: (key: string, vars?: Record<string, string | number>) => key + (vars ? ` ${Object.values(vars).join(" ")}` : "") }));
vi.mock("../i18n", () => ({ useT: () => translate }));

import { fetchSettings } from "../api/settings";
import Settings from "./Settings";

const tick = () => new Promise((resolve) => setTimeout(resolve, 0));

describe("Settings page", () => {
  let container: HTMLDivElement;
  let root: ReturnType<typeof createRoot>;

  beforeEach(() => {
    container = document.createElement("div");
    document.body.appendChild(container);
    root = createRoot(container);
  });

  afterEach(() => {
    act(() => root.unmount());
    container.remove();
    vi.clearAllMocks();
  });

  // A module that cannot open its data file is missing from the answer; the
  // page names it and the reason instead of failing to render.
  it("shows the modules that could not open their files", async () => {
    vi.mocked(fetchSettings).mockResolvedValue({
      version: "0.18.0",
      plugin_id: "cpa-key-policy",
      display_name: "萝卜",
      modules: { search: { enabled: false, declared: false }, channels: { tracked: 2 } },
      module_errors: { adapters: "decode cpa-key-policy-adapters.json: unexpected EOF" },
      hooks: [],
      config_warnings: [],
      files: [{ name: "adapters", path: "/data/cpa-key-policy-adapters.json", size: 0, missing: true }],
      ui: { source: "embedded", sha256: "" },
      ownership: { owner: true, handoff: "idle" },
    } satisfies SettingsResponse);

    await act(async () => {
      root.render(<MemoryRouter><Settings /></MemoryRouter>);
      await tick();
    });

    const modules = container.querySelector('[data-testid="settings-modules"]');
    expect(modules?.textContent).toContain("overview.moduleUnavailable");
    expect(modules?.textContent).toContain("adapters: decode cpa-key-policy-adapters.json: unexpected EOF");
    expect(container.querySelector('[data-testid="settings-files"]')?.textContent).toContain("settings.fileMissing");
  });

  // Without its file the search router is never declared: the switch points
  // to the module error instead of "applying, usually a few seconds".
  it("points the search switch to the module error when search could not open", async () => {
    vi.mocked(fetchSettings).mockResolvedValue({
      version: "0.18.0",
      plugin_id: "cpa-key-policy",
      display_name: "萝卜",
      modules: { search: { enabled: true, declared: false }, channels: { tracked: 0 } },
      module_errors: { search: "decode cpa-key-policy-search.json: unexpected EOF" },
      hooks: [],
      config_warnings: [],
      files: [],
      ui: { source: "embedded", sha256: "" },
      ownership: { owner: false, handoff: "quiesced" },
    } satisfies SettingsResponse);

    await act(async () => {
      root.render(<MemoryRouter><Settings /></MemoryRouter>);
      await tick();
    });

    const toggle = container.querySelector('[data-testid="search-toggle"]');
    expect(toggle?.querySelector(".field-hint")?.textContent).toBe("overview.moduleUnavailable");
    expect(toggle?.textContent).not.toContain("search.stateHint.applying");
  });
});
