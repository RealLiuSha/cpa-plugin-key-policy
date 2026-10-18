import { act } from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { createRoot } from "react-dom/client";
import { Simulate } from "react-dom/test-utils";
import { MemoryRouter } from "react-router-dom";
import type { ModelPriceRow } from "../types";

(globalThis as unknown as { IS_REACT_ACT_ENVIRONMENT?: boolean }).IS_REACT_ACT_ENVIRONMENT = true;

vi.mock("../api/modelPrices", () => ({
  fetchModelPrices: vi.fn(),
  deleteModelPrice: vi.fn(),
  importModelPrices: vi.fn(),
  previewModelPrices: vi.fn(),
  importModels: vi.fn(),
  saveModelPrice: vi.fn(),
}));
vi.mock("../api/models", () => ({
  fetchCatalog: vi.fn(),
}));
const { translate } = vi.hoisted(() => ({ translate: (key: string, vars?: Record<string, string | number>) => key + (vars ? ` ${Object.values(vars).join(" ")}` : "") }));
vi.mock("../i18n", () => ({ useT: () => translate }));

import { deleteModelPrice, fetchModelPrices, importModelPrices, importModels, previewModelPrices, saveModelPrice } from "../api/modelPrices";
import { fetchCatalog } from "../api/models";
import Models from "./Models";

const rows: ModelPriceRow[] = [
  {
    name: "gpt-5", provider: "codex", billing_mode: "tokens", billing_multiplier: 1.2,
    input_price_per_million: 1, output_price_per_million: 2, priced: true,
  },
  { name: "small", provider: "", billing_mode: "tokens", billing_multiplier: 1, priced: false },
  { name: "veo", provider: "", billing_mode: "per_call", per_call_usd: 0.5, priced: true },
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
  vi.mocked(fetchModelPrices).mockResolvedValue({
    models: rows,
    unpriced: [{ name: "gpt-6-terra", calls: 1200, last_seen: "2026-10-16" }, { name: "mystery", calls: 3, last_seen: "2026-10-01", has_qualified_rows: true }],
  });
  vi.mocked(deleteModelPrice).mockResolvedValue(undefined);
  vi.mocked(importModelPrices).mockResolvedValue({ applied: [], unchanged: [], skipped: [] });
  vi.mocked(previewModelPrices).mockResolvedValue({
    source: "Models.dev", source_url: "https://models.dev/api.json", metadata_models: 3,
    matches: [match("gpt-5", 3), match("small", 0.15), match("gpt-6-terra", 2)], unmatched_models: ["mystery"],
  });
  vi.mocked(fetchCatalog).mockResolvedValue([
    { provider: "codex", model: "gpt-5" },
    { provider: "xai", model: "grok-4.7" },
    { provider: "openai", model: "grok-4.7" },
  ]);
  vi.mocked(importModels).mockResolvedValue({ created: [{ name: "grok-4.7" }], skipped: [] });
  vi.mocked(saveModelPrice).mockImplementation(async (price) => ({ ...price, priced: true }));
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
const dialog = () => container.querySelector<HTMLElement>('[role="dialog"]')!;

describe("Model prices page", () => {
  it("shows model id, provider qualifier, billing, base price, multiplier and charged price", async () => {
    await renderPage();
    const header = [...container.querySelectorAll(".model-table th")].map((cell) => cell.textContent);
    expect(header).toEqual([
      "models.colName", "models.colProvider", "models.colBilling", "models.colBasePrice",
      "models.colMultiplier", "models.colChargedPrice", "models.colActions",
    ]);
    const tableRows = container.querySelectorAll(".model-table tbody tr");
    expect(tableRows).toHaveLength(3);
    const cells = (index: number) => [...tableRows[index].querySelectorAll("td")].map((cell) => cell.textContent);
    expect(cells(0).slice(0, 2)).toEqual(["gpt-5", "codex"]);
    expect(cells(0)[4]).toBe("×1.2");
    expect(cells(0)[5]).toContain("$1.2");
    expect(cells(1)[1]).toBe("models.anyProvider");
    expect(tableRows[1].querySelector(".badge.warn")?.textContent).toBe("usage.unpriced");
    expect(cells(1)[5]).toBe("—");
    expect(cells(2)[4]).toBe("—");
    expect(container.querySelector(".model-table a")?.getAttribute("href")).toBe("/models/edit?name=gpt-5&provider=codex");
    expect(container.querySelectorAll(".model-card button.danger-outline")).toHaveLength(3);
  });

  it("lists models seen without a price and links each to a prefilled manual entry", async () => {
    await renderPage();
    const notice = container.querySelector(".unpriced-notice")!;
    expect(notice.textContent).toContain("models.unpricedNotice 2");
    const items = notice.querySelectorAll("li");
    expect(items[0].textContent).toContain("gpt-6-terra");
    expect(items[0].textContent).toContain("models.unpricedUsage 1,200 2026-10-16");
    expect(items[0].querySelector("a")?.getAttribute("href")).toBe("/models/new?name=gpt-6-terra");
    // Only a model whose provider-qualified rows missed its calls says so.
    expect(items[0].querySelector(".unpriced-qualified")).toBeNull();
    expect(items[1].querySelector(".unpriced-qualified")?.textContent).toBe("usage.unpricedQualified");
  });

  it("prices the unpriced models in one step, writing each new row whole with its multiplier", async () => {
    await renderPage();
    await act(async () => { button("models.priceUnpriced", container.querySelector(".unpriced-notice")!).click(); await tick(); await tick(); });
    expect(previewModelPrices).toHaveBeenCalledWith(["gpt-5", "small", "gpt-6-terra", "mystery"]);
    const items = dialog().querySelectorAll(".sync-item");
    expect(items).toHaveLength(1);
    expect(items[0].textContent).toContain("gpt-6-terra");
    expect(items[0].textContent).toContain("models.syncCreatesRow");
    expect(items[0].querySelector<HTMLInputElement>('input[type="checkbox"]')?.checked).toBe(true);
    const multiplier = [...items[0].querySelectorAll("label")].find((label) => label.textContent?.startsWith("models.createMultiplier"))!.querySelector("input")!;
    expect(multiplier.value).toBe("1");
    await act(async () => Simulate.change(multiplier, { target: { value: "1.5" } } as never));
    expect(dialog().textContent).toContain("models.pricingUnmatched mystery");
    await act(async () => { button("models.applyCount 1", dialog()).click(); await tick(); });
    expect(saveModelPrice).toHaveBeenCalledWith(expect.objectContaining({
      name: "gpt-6-terra", provider: "", billing_mode: "tokens", billing_multiplier: 1.5, input_price_per_million: 2, output_price_per_million: 8,
    }));
    expect(importModels).not.toHaveBeenCalled();
    expect(importModelPrices).not.toHaveBeenCalled();
    expect(dialog().textContent).toContain("models.syncCreated 1");
  });

  it("gives an unpriced model whose qualified rows missed its calls a row for any provider, spelled as its calls", async () => {
    vi.mocked(fetchModelPrices).mockResolvedValue({
      models: [{ name: "DeepSeek-V4-Pro", provider: "deepseek", billing_mode: "tokens", billing_multiplier: 1.2, input_price_per_million: 0.66, output_price_per_million: 1.98, priced: true }],
      unpriced: [{ name: "deepseek-v4-pro", calls: 1, last_seen: "2026-10-17", has_qualified_rows: true }],
    });
    vi.mocked(previewModelPrices).mockResolvedValue({
      source: "Models.dev", source_url: "https://models.dev/api.json", metadata_models: 1, matches: [match("deepseek-v4-pro", 0.5)], unmatched_models: [],
    });
    await renderPage();
    await act(async () => { button("models.priceUnpriced", container.querySelector(".unpriced-notice")!).click(); await tick(); await tick(); });
    expect(previewModelPrices).toHaveBeenCalledWith(["deepseek-v4-pro"]);
    const item = dialog().querySelector(".sync-item")!;
    expect(item.querySelector("strong")?.textContent).toBe("deepseek-v4-pro");
    expect(item.textContent).toContain("models.syncCreatesUnqualified deepseek");
    expect(item.textContent).toContain("models.currentPrice · deepseek");
    const multiplier = [...item.querySelectorAll("label")].find((label) => label.textContent?.startsWith("models.createMultiplier"))!.querySelector("input")!;
    expect(multiplier.value).toBe("1.2");
    await act(async () => { button("models.applyCount 1", dialog()).click(); await tick(); });
    expect(saveModelPrice).toHaveBeenCalledWith(expect.objectContaining({ name: "deepseek-v4-pro", provider: "", billing_multiplier: 1.2, input_price_per_million: 0.5, output_price_per_million: 2 }));
    expect(importModelPrices).toHaveBeenCalledWith({ dry_run: false, matches: [expect.objectContaining({ model: "deepseek-v4-pro", prompt_price_per_1m: 0.5 })] });
    expect(vi.mocked(saveModelPrice).mock.invocationCallOrder[0]).toBeLessThan(vi.mocked(importModelPrices).mock.invocationCallOrder[0]);
  });

  it("selects or clears every listed model at once", async () => {
    await renderPage();
    await act(async () => { button("models.syncPrices").click(); await tick(); await tick(); });
    const all = dialog().querySelector<HTMLInputElement>(".sync-select-all input")!;
    const boxes = () => [...dialog().querySelectorAll<HTMLInputElement>('.sync-item input[type="checkbox"]')];
    expect(all.checked).toBe(true);
    expect(dialog().textContent).toContain("models.selectAll 2 2");
    await act(async () => all.click());
    expect(boxes().every((box) => !box.checked)).toBe(true);
    expect(button("models.applyCount 0", dialog()).disabled).toBe(true);
    await act(async () => boxes()[1].click());
    expect(all.indeterminate).toBe(true);
    await act(async () => all.click());
    expect(boxes().every((box) => box.checked)).toBe(true);
    expect(all.indeterminate).toBe(false);
  });

  it("syncs by model id, preselecting unpriced or changed rows and leaving per-call rows alone", async () => {
    await renderPage();
    await act(async () => { button("models.syncPrices").click(); await tick(); await tick(); });
    expect(previewModelPrices).toHaveBeenCalledWith(["gpt-5", "small"]);
    const items = [...dialog().querySelectorAll(".sync-item")];
    expect(items.map((item) => item.querySelector("strong")?.textContent)).toEqual(["gpt-5", "small"]);
    expect(items[0].textContent).toContain("models.currentPrice · codex");
    expect(items.every((item) => item.querySelector<HTMLInputElement>('input[type="checkbox"]')?.checked)).toBe(true);
    expect(dialog().textContent).toContain("models.syncPerCallNote veo");
    const price = items[1].querySelector<HTMLInputElement>('input[type="number"]')!;
    await act(async () => Simulate.change(price, { target: { value: "0.2" } } as never));
    await act(async () => items[0].querySelector<HTMLInputElement>('input[type="checkbox"]')!.click());
    await act(async () => { button("models.applyCount 1", dialog()).click(); await tick(); });
    expect(importModels).not.toHaveBeenCalled();
    expect(importModelPrices).toHaveBeenCalledWith({
      dry_run: false,
      matches: [expect.objectContaining({ model: "small", prompt_price_per_1m: 0.2, completion_price_per_1m: 0.6 })],
    });
    expect(fetchModelPrices).toHaveBeenCalledTimes(3);
  });

  it("imports chosen catalog models as $0 rows, optionally limited to one provider, then offers price sync", async () => {
    await renderPage();
    await act(async () => { button("models.importFromCpa").click(); await tick(); await tick(); });
    const items = dialog().querySelectorAll(".import-item");
    expect(items).toHaveLength(2);
    expect(items[0].querySelector<HTMLInputElement>("input")!.disabled).toBe(true);
    expect(dialog().querySelectorAll<HTMLInputElement>('.import-item input[type="checkbox"]:checked')).toHaveLength(0);
    const provider = items[1].querySelector<HTMLSelectElement>("select")!;
    expect([...provider.options].map((option) => option.value)).toEqual(["", "xai", "openai"]);
    expect(provider.value).toBe("");
    await act(async () => items[1].querySelector<HTMLInputElement>("input")!.click());
    await act(async () => { button("models.importCount 1", dialog()).click(); await tick(); });
    expect(importModels).toHaveBeenLastCalledWith([{ name: "grok-4.7", provider: "" }]);
    expect(dialog().textContent).toContain("models.importDone");

    await act(async () => { button("models.syncImported 1", dialog()).click(); await tick(); await tick(); });
    expect(dialog().querySelector("h2")?.textContent).toBe("models.syncTitle");
  });

  it("limits an import to the chosen provider", async () => {
    await renderPage();
    await act(async () => { button("models.importFromCpa").click(); await tick(); await tick(); });
    const item = dialog().querySelectorAll(".import-item")[1];
    await act(async () => item.querySelector<HTMLInputElement>("input")!.click());
    await act(async () => Simulate.change(item.querySelector("select")!, { target: { value: "xai" } } as never));
    await act(async () => { button("models.importCount 1", dialog()).click(); await tick(); });
    expect(importModels).toHaveBeenCalledWith([{ name: "grok-4.7", provider: "xai" }]);
  });

  it("deletes one price row by id and provider only after confirmation", async () => {
    await renderPage();
    const cards = container.querySelectorAll(".model-card");
    await act(async () => cards[0].querySelector<HTMLButtonElement>("button.danger-outline")!.click());
    expect(dialog().textContent).toContain("models.deleteConfirm gpt-5 codex");
    expect(deleteModelPrice).not.toHaveBeenCalled();
    await act(async () => { button("models.confirmDelete", dialog()).click(); await tick(); });
    expect(deleteModelPrice).toHaveBeenCalledWith(rows[0]);
    expect(fetchModelPrices).toHaveBeenCalledTimes(2);
  });
});
