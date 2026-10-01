import { act } from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { createRoot } from "react-dom/client";
import { Simulate } from "react-dom/test-utils";
import { MemoryRouter } from "react-router-dom";
import type { ModelDefinition } from "../types";

(globalThis as unknown as { IS_REACT_ACT_ENVIRONMENT?: boolean }).IS_REACT_ACT_ENVIRONMENT = true;

vi.mock("../api/modelDefinitions", () => ({
  fetchModelDefinitions: vi.fn(),
  deleteModelDefinition: vi.fn(),
  importModelPrices: vi.fn(),
  previewModelPrices: vi.fn(),
  importModels: vi.fn(),
}));
vi.mock("../api/models", () => ({
  fetchCatalog: vi.fn(),
}));
const { translate } = vi.hoisted(() => ({ translate: (key: string) => key }));
vi.mock("../i18n", () => ({ useT: () => translate }));

import { deleteModelDefinition, fetchModelDefinitions, importModelPrices, importModels, previewModelPrices } from "../api/modelDefinitions";
import { fetchCatalog } from "../api/models";
import Models from "./Models";

const models: ModelDefinition[] = [
  {
    name: "fast", provider: "codex", target_model: "gpt-5", billing_mode: "tokens", billing_multiplier: 1.2,
    input_price_per_million: 1, output_price_per_million: 2, ref_count: 1, ref_keys: ["team-a"],
  },
  { name: "fresh", provider: "openai", target_model: "small", billing_mode: "tokens", billing_multiplier: 1, ref_count: 0 },
];

const match = (model: string, input: number) => ({
  model, matched_model: model, match_type: "index_exact", source: "Models.dev", source_url: "https://models.dev/api.json",
  source_provider_id: "openai", source_provider_name: "OpenAI", prompt_price_per_1m: input, completion_price_per_1m: input * 4, cache_read_price_per_1m: 0,
});

const tick = () => new Promise((resolve) => setTimeout(resolve, 0));
let container: HTMLDivElement;
let root: ReturnType<typeof createRoot>;

beforeEach(() => {
  container = document.createElement("div");
  document.body.appendChild(container);
  vi.mocked(fetchModelDefinitions).mockResolvedValue(models);
  vi.mocked(deleteModelDefinition).mockResolvedValue(undefined);
  vi.mocked(importModelPrices).mockResolvedValue({ applied: [], unchanged: [], skipped: [], affected_keys: [] });
  vi.mocked(previewModelPrices).mockResolvedValue({
    source: "Models.dev", source_url: "https://models.dev/api.json", metadata_models: 2,
    matches: [match("gpt-5", 3), match("small", 0.15)], unmatched_models: [],
  });
  vi.mocked(fetchCatalog).mockResolvedValue([
    { provider: "codex", model: "gpt-5" },
    { provider: "xai", model: "grok-4.7" },
    { provider: "openai", model: "grok-4.7" },
  ]);
  vi.mocked(importModels).mockResolvedValue({ created: [{ name: "grok-4.7" }], skipped: [] });
});

afterEach(() => {
  act(() => root.unmount());
  container.remove();
  vi.clearAllMocks();
});

async function renderPage() {
  await act(async () => {
    root = createRoot(container);
    root.render(<MemoryRouter><Models /></MemoryRouter>);
    await tick();
    await tick();
  });
}

const button = (label: string, scope: ParentNode = container) =>
  [...scope.querySelectorAll<HTMLButtonElement>("button")].find((candidate) => candidate.textContent === label)!;

describe("Models management", () => {
  it("shows the upstream, base price, multiplier and unpriced state", async () => {
    await renderPage();
    const rows = container.querySelectorAll(".model-table tbody tr");
    expect(rows).toHaveLength(2);
    expect(rows[0].textContent).toContain("codex / gpt-5");
    expect(rows[0].textContent).toContain("×1.2");
    expect(rows[0].textContent).toContain("models.chargedPrices");
    expect(rows[1].textContent).toContain("models.unpriced");
    expect(container.querySelector(".notice-row")?.textContent).toContain("models.unpricedNotice");
    const cards = container.querySelectorAll(".model-card");
    expect(cards[0].querySelector("button.danger-outline")).toBeNull();
    expect(cards[1].querySelector("button.danger-outline")).not.toBeNull();
  });

  it("syncs prices for the unpriced models in one step", async () => {
    await renderPage();
    await act(async () => { button("models.syncPrices", container.querySelector(".notice-row")!).click(); await tick(); await tick(); });
    expect(previewModelPrices).toHaveBeenCalledWith(["gpt-5", "fast", "small", "fresh"]);
    const items = container.querySelectorAll(".sync-item");
    expect(items).toHaveLength(1);
    expect(items[0].textContent).toContain("fresh");
    const price = items[0].querySelector<HTMLInputElement>('input[type="number"]')!;
    await act(async () => Simulate.change(price, { target: { value: "0.2" } } as never));
    await act(async () => { button("models.applyCount").click(); await tick(); });
    expect(importModelPrices).toHaveBeenCalledWith({
      dry_run: false,
      matches: [expect.objectContaining({ model: "small", prompt_price_per_1m: 0.2, completion_price_per_1m: 0.6 })],
    });
    expect(fetchModelDefinitions).toHaveBeenCalledTimes(3);
  });

  it("imports only the chosen catalog models, then offers price sync", async () => {
    await renderPage();
    await act(async () => { button("models.importFromCpa").click(); await tick(); await tick(); });
    const items = container.querySelectorAll(".import-item");
    expect(items).toHaveLength(2);
    expect(items[0].querySelector<HTMLInputElement>("input")!.disabled).toBe(true);
    expect(container.querySelectorAll<HTMLInputElement>('.import-item input[type="checkbox"]:checked')).toHaveLength(0);
    await act(async () => items[1].querySelector<HTMLInputElement>("input")!.click());
    const provider = items[1].querySelector<HTMLSelectElement>("select")!;
    await act(async () => Simulate.change(provider, { target: { value: "openai" } } as never));
    await act(async () => { button("models.importCount").click(); await tick(); });
    expect(importModels).toHaveBeenCalledWith([{ provider: "openai", target_model: "grok-4.7" }]);
    expect(container.textContent).toContain("models.importDone");
    await act(async () => { button("models.syncImported").click(); await tick(); await tick(); });
    expect(container.querySelector('[role="dialog"] h2')?.textContent).toBe("models.syncTitle");
  });

  it("deletes an unreferenced model only after confirmation", async () => {
    await renderPage();
    const cards = container.querySelectorAll(".model-card");
    await act(async () => cards[1].querySelector<HTMLButtonElement>("button.danger-outline")!.click());
    expect(container.querySelector('[role="dialog"]')?.textContent).toContain("models.deleteConfirm");
    expect(deleteModelDefinition).not.toHaveBeenCalled();
    await act(async () => { button("models.confirmDelete", container.querySelector('[role="dialog"]')!).click(); await tick(); });
    expect(deleteModelDefinition).toHaveBeenCalledWith("fresh");
    expect(fetchModelDefinitions).toHaveBeenCalledTimes(2);
  });
});
