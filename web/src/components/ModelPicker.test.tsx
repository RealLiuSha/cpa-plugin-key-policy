import { act } from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { createRoot } from "react-dom/client";
import { Simulate } from "react-dom/test-utils";
import type { ModelTarget } from "../types";

(globalThis as unknown as { IS_REACT_ACT_ENVIRONMENT?: boolean }).IS_REACT_ACT_ENVIRONMENT = true;

interface FakeGroup {
  provider: string;
  group?: string;
  models: string[];
}

vi.mock("../api/models", () => ({
  fetchCatalog: vi.fn(),
  formatTierLabel: (_translate: (key: string) => string, group: string) => group,
  groupByCatalog: (catalog: { provider: string; model: string; group?: string }[]): FakeGroup[] => {
    const groups = new Map<string, FakeGroup>();
    for (const item of catalog) {
      const key = `${item.provider}|${item.group ?? ""}`;
      const group = groups.get(key) ?? { provider: item.provider, ...(item.group ? { group: item.group } : {}), models: [] };
      group.models.push(item.model);
      groups.set(key, group);
    }
    return [...groups.values()];
  },
}));
vi.mock("../i18n", () => ({ useT: () => (key: string) => key }));

import { fetchCatalog } from "../api/models";
import ModelPicker from "./ModelPicker";

const tick = () => new Promise((resolve) => setTimeout(resolve, 0));
let container: HTMLDivElement;
let root: ReturnType<typeof createRoot>;

beforeEach(() => {
  container = document.createElement("div");
  document.body.appendChild(container);
});

afterEach(() => {
  act(() => root.unmount());
  container.remove();
  vi.clearAllMocks();
});

describe("ModelPicker v3 targets", () => {
  it("does not clear preselected targets while the catalog is loading", async () => {
    let resolveCatalog: (value: { provider: string; model: string }[]) => void = () => {};
    vi.mocked(fetchCatalog).mockImplementation(() => new Promise((resolve) => { resolveCatalog = resolve; }));
    const initial: ModelTarget[] = [{ provider: "xai", target_model: "grok-fast" }];
    const calls: ModelTarget[][] = [];

    await act(async () => {
      root = createRoot(container);
      root.render(<ModelPicker initial={initial} onChange={(targets) => calls.push(targets)} />);
      await tick();
    });
    expect(calls).toEqual([]);

    await act(async () => {
      resolveCatalog([{ provider: "xai", model: "grok-fast" }]);
      await tick();
    });
    expect(calls.at(-1)).toEqual(initial);
  });

  it("returns provider, credential group and upstream model as one target", async () => {
    vi.mocked(fetchCatalog).mockResolvedValue([
      { provider: "codex", group: "free", model: "gpt-5" },
      { provider: "codex", group: "team", model: "gpt-5" },
    ]);
    const calls: ModelTarget[][] = [];
    await act(async () => {
      root = createRoot(container);
      root.render(<ModelPicker onChange={(targets) => calls.push(targets)} />);
      await tick();
      await tick();
    });

    const checkboxes = container.querySelectorAll<HTMLInputElement>('input[type="checkbox"]');
    expect(checkboxes).toHaveLength(2);
    await act(async () => { checkboxes[1].click(); await tick(); });
    expect(calls.at(-1)).toEqual([{ provider: "codex", group: "team", target_model: "gpt-5" }]);
  });

  it("preserves a selected target no longer returned by the catalog", async () => {
    vi.mocked(fetchCatalog).mockResolvedValue([{ provider: "codex", group: "team", model: "gpt-5" }]);
    const initial: ModelTarget[] = [{ provider: "codex", target_model: "legacy-model" }];
    const calls: ModelTarget[][] = [];
    await act(async () => {
      root = createRoot(container);
      root.render(<ModelPicker initial={initial} onChange={(targets) => calls.push(targets)} />);
      await tick();
      await tick();
    });
    expect(calls.at(-1)).toContainEqual(initial[0]);
  });

  it("shows an explicit no-match state and supports selecting visible targets", async () => {
    vi.mocked(fetchCatalog).mockResolvedValue([
      { provider: "codex", group: "team", model: "gpt-5" },
      { provider: "xai", group: "plus", model: "grok" },
    ]);
    const initial: ModelTarget[] = [{ provider: "xai", group: "plus", target_model: "grok" }];
    const calls: ModelTarget[][] = [];
    await act(async () => {
      root = createRoot(container);
      root.render(<ModelPicker initial={initial} onChange={(targets) => calls.push(targets)} />);
      await tick();
      await tick();
    });

    const search = container.querySelector<HTMLInputElement>('input[placeholder="picker.searchPlaceholder"]')!;
    await act(async () => Simulate.change(search, { target: { value: "codex" } } as never));
    const selectAll = [...container.querySelectorAll<HTMLButtonElement>("button")].find((button) => button.textContent === "picker.selectAll")!;
    await act(async () => { selectAll.click(); await tick(); });
    expect(calls.at(-1)).toEqual([
      { provider: "codex", group: "team", target_model: "gpt-5" },
      { provider: "xai", group: "plus", target_model: "grok" },
    ]);

    const clearAll = [...container.querySelectorAll<HTMLButtonElement>("button")].find((button) => button.textContent === "picker.clearAll")!;
    await act(async () => { clearAll.click(); await tick(); });
    expect(calls.at(-1)).toEqual(initial);

    await act(async () => Simulate.change(search, { target: { value: "missing" } } as never));
    expect(container.textContent).toContain("picker.noMatch");
  });
});
