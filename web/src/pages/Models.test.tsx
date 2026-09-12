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

import { deleteModelDefinition, fetchModelDefinitions, importModelPrices, previewModelPrices } from "../api/modelDefinitions";
import { fetchCatalog } from "../api/models";
import Models from "./Models";

const models: ModelDefinition[] = [
  {
    name: "fast",
    targets: [
      { provider: "codex", group: "team", target_model: "gpt-5" },
      { provider: "xai", target_model: "grok" },
      { provider: "openai", target_model: "gpt-4.1" },
      { provider: "anthropic", target_model: "claude" },
    ],
    dispatch: "round-robin",
    billing_mode: "tokens",
    free: false,
    input_price_per_million: 1,
    output_price_per_million: 2,
    ref_count: 1,
    ref_keys: ["team-a"],
  },
  {
    name: "free-model",
    targets: [{ provider: "openai", target_model: "small" }],
    dispatch: "priority",
    billing_mode: "tokens",
    free: true,
    ref_count: 0,
  },
];

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
    source: "Models.dev", source_url: "https://models.dev/api.json", metadata_models: 1,
    matches: [{
      model: "gpt-5", matched_model: "gpt-5", match_type: "index_exact", source: "Models.dev",
      source_url: "https://models.dev/api.json", source_provider_id: "openai", source_provider_name: "OpenAI",
      prompt_price_per_1m: 3, completion_price_per_1m: 12, cache_read_price_per_1m: 0.75, cache_write_price_per_1m: 3.75,
    }, {
      model: "small", matched_model: "small", match_type: "index_exact", source: "Models.dev",
      source_url: "https://models.dev/api.json", source_provider_id: "openai", source_provider_name: "OpenAI",
      prompt_price_per_1m: 0.15, completion_price_per_1m: 0.6, cache_read_price_per_1m: 0.075,
    }],
    unmatched_models: [],
  });
  vi.mocked(fetchCatalog).mockResolvedValue([]);
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

describe("Models management", () => {
  it("shows targets, prices and reference protection", async () => {
    await renderPage();
    expect(container.textContent).toContain("codex · team / gpt-5");
    expect(container.textContent).toContain("models.priceTokenSummary");
    expect(container.querySelectorAll(".model-table tbody tr")).toHaveLength(2);
    const cards = container.querySelectorAll(".model-card");
    expect(cards).toHaveLength(2);
    expect(cards[0].querySelector<HTMLButtonElement>("button.danger")?.disabled).toBe(true);
    expect(cards[0].textContent).toContain("models.refs");
    expect(cards[1].querySelector<HTMLButtonElement>("button.danger")?.disabled).toBe(false);
    const moreTargets = cards[0].querySelector<HTMLButtonElement>(".chip.more")!;
    expect(moreTargets.getAttribute("aria-expanded")).toBe("false");
    await act(async () => moreTargets.click());
    expect(cards[0].textContent).toContain("anthropic / claude");
    expect(moreTargets.textContent).toBe("models.showLessTargets");
  });

  it("previews and applies Models.dev price sync, then refreshes model definitions", async () => {
    await renderPage();
    expect(container.querySelector(".model-import")).toBeNull();
    const openSync = [...container.querySelectorAll<HTMLButtonElement>("button")].find((button) => button.textContent === "models.syncPrices")!;
    await act(async () => { openSync.click(); await tick(); });
    expect(container.querySelector('[role="dialog"]')).not.toBeNull();
    const preview = [...container.querySelectorAll<HTMLButtonElement>(".model-import button")].find((button) => button.textContent === "models.pricingPreview")!;
    await act(async () => { preview.click(); await tick(); await tick(); });
    expect(previewModelPrices).toHaveBeenCalled();
    const buttons = [...container.querySelectorAll<HTMLButtonElement>(".model-import button")];
    const selections = container.querySelectorAll<HTMLInputElement>('.model-import input[type="checkbox"]');
    expect(selections).toHaveLength(2);
    await act(async () => selections[1].click());
    await act(async () => { buttons[1].click(); await tick(); });
    expect(importModelPrices).toHaveBeenNthCalledWith(1, expect.objectContaining({
      dry_run: true,
      matches: [expect.objectContaining({ model: "gpt-5" })],
    }));
    const priceInput = container.querySelector<HTMLInputElement>('.model-import input[type="number"]')!;
    await act(async () => Simulate.change(priceInput, { target: { value: "4" } } as never));
    expect(buttons[2].disabled).toBe(true);
    await act(async () => { buttons[1].click(); await tick(); });
    await act(async () => { buttons[2].click(); await tick(); });
    expect(importModelPrices).toHaveBeenNthCalledWith(3, expect.objectContaining({ dry_run: false }));
    expect(fetchModelDefinitions).toHaveBeenCalledTimes(3);
  });

  it("deletes an unreferenced model only after confirmation", async () => {
    await renderPage();
    const cards = container.querySelectorAll(".model-card");
    const deleteButton = cards[1].querySelector<HTMLButtonElement>("button.danger")!;
    await act(async () => deleteButton.click());
    expect(container.querySelector('[role="dialog"]')?.textContent).toContain("models.deleteConfirm");
    expect(deleteModelDefinition).not.toHaveBeenCalled();
    const confirmDelete = [...container.querySelectorAll<HTMLButtonElement>('[role="dialog"] button')].find((button) => button.textContent === "models.confirmDelete")!;
    await act(async () => { confirmDelete.click(); await tick(); });
    expect(deleteModelDefinition).toHaveBeenCalledWith("free-model");
    expect(fetchModelDefinitions).toHaveBeenCalledTimes(2);
  });
});
