import { act } from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { createRoot } from "react-dom/client";
import { Simulate } from "react-dom/test-utils";
import type { CatalogModel, ModelDefinition } from "../types";
import { buildImportItems, groupCatalogCandidates } from "./ModelImportWizard";

(globalThis as unknown as { IS_REACT_ACT_ENVIRONMENT?: boolean }).IS_REACT_ACT_ENVIRONMENT = true;

vi.mock("../api/models", () => ({ fetchCatalog: vi.fn() }));
vi.mock("../api/modelDefinitions", () => ({
  fetchModelDefinitions: vi.fn(),
  importModels: vi.fn(),
  previewModelPrices: vi.fn(),
}));
const { translate } = vi.hoisted(() => ({ translate: (key: string) => key }));
vi.mock("../i18n", () => ({ useT: () => translate }));

import { fetchCatalog } from "../api/models";
import { fetchModelDefinitions, importModels, previewModelPrices } from "../api/modelDefinitions";
import ModelImportWizard from "./ModelImportWizard";

const tick = () => new Promise((resolve) => setTimeout(resolve, 0));

const catalog: CatalogModel[] = [
  { provider: "openai", model: "gpt-4o" },
  { provider: "codex", group: "team", model: "gpt-4o" },
  { provider: "claude", model: "claude-sonnet" },
];
const existing: ModelDefinition[] = [{
  name: "gpt-4o", targets: [{ provider: "openai", target_model: "gpt-4o" }],
  dispatch: "round-robin", billing_mode: "tokens", free: false, ref_count: 2, ref_keys: ["k1", "k2"],
}];

describe("groupCatalogCandidates", () => {
  it("keeps duplicate model IDs as separate candidates", () => {
    const groups = groupCatalogCandidates(catalog);
    expect(groups.get("gpt-4o")).toHaveLength(2);
    expect(groups.get("claude-sonnet")).toHaveLength(1);
  });
});

describe("ModelImportWizard", () => {
  let container: HTMLDivElement;
  let root: ReturnType<typeof createRoot>;

  beforeEach(() => {
    container = document.createElement("div");
    document.body.appendChild(container);
    vi.mocked(fetchCatalog).mockResolvedValue(catalog);
    vi.mocked(fetchModelDefinitions).mockResolvedValue(existing);
    vi.mocked(previewModelPrices).mockResolvedValue({
      source: "Models.dev", source_url: "https://models.dev/api.json", metadata_models: 1,
      matches: [{
        model: "claude-sonnet", matched_model: "claude-sonnet", match_type: "index_exact", source: "Models.dev",
        source_url: "https://models.dev/api.json", source_provider_id: "anthropic", source_provider_name: "Anthropic",
        prompt_price_per_1m: 3, completion_price_per_1m: 15, cache_read_price_per_1m: 0.3, cache_write_price_per_1m: 3.75,
      }],
      unmatched_models: ["gpt-4o"],
    });
    vi.mocked(importModels).mockResolvedValue({
      created: [{ name: "claude-sonnet", action: "create" }],
      updated: [], skipped: [], conflicts: [], missing_price: [], affected_keys: [],
    });
  });

  afterEach(() => {
    act(() => root.unmount());
    container.remove();
    vi.clearAllMocks();
  });

  it("requires a single target for duplicate IDs, skips existing by default, and applies only after preview", async () => {
    await act(async () => {
      root = createRoot(container);
      root.render(<ModelImportWizard onApplied={async () => undefined} />);
      await tick();
      await tick();
    });
    expect(container.querySelectorAll('input[type="radio"]')).toHaveLength(2);
    const overwrite = container.textContent ?? "";
    expect(overwrite).toContain("models.importOverwrite");
    const gptRow = container.querySelector('[data-testid="import-row-gpt-4o"]')!;
    expect(gptRow.querySelector<HTMLInputElement>('input[type="checkbox"]')?.checked).toBe(false);

    const apply = [...container.querySelectorAll<HTMLButtonElement>("button")].find((button) => button.textContent === "models.importApply")!;
    expect(apply.disabled).toBe(true);

    const previewPrices = [...container.querySelectorAll<HTMLButtonElement>("button")].find((button) => button.textContent === "models.pricingPreview")!;
    await act(async () => { previewPrices.click(); await tick(); });
    expect(previewModelPrices).toHaveBeenCalledWith(["claude-sonnet"]);

    const dryRun = [...container.querySelectorAll<HTMLButtonElement>("button")].find((button) => button.textContent === "models.importPreview")!;
    await act(async () => { dryRun.click(); await tick(); });
    expect(importModels).toHaveBeenNthCalledWith(1, expect.objectContaining({ dry_run: true }));
    const dispatch = container.querySelector<HTMLSelectElement>('[data-testid="import-row-claude-sonnet"] select')!;
    await act(async () => Simulate.change(dispatch, { target: { value: "priority" } } as never));
    expect(apply.disabled).toBe(true);
    await act(async () => { dryRun.click(); await tick(); });
    await act(async () => { apply.click(); await tick(); });
    expect(importModels).toHaveBeenNthCalledWith(3, expect.objectContaining({ dry_run: false }));
  });

  it("blocks apply when a selected non-free model is missing a price", async () => {
    vi.mocked(previewModelPrices).mockResolvedValue({
      source: "Models.dev", source_url: "https://models.dev/api.json", metadata_models: 0, matches: [], unmatched_models: ["claude-sonnet"],
    });
    await act(async () => {
      root = createRoot(container);
      root.render(<ModelImportWizard onApplied={async () => undefined} />);
      await tick();
      await tick();
    });
    const dryRun = [...container.querySelectorAll<HTMLButtonElement>("button")].find((button) => button.textContent === "models.importPreview")!;
    await act(async () => { dryRun.click(); await tick(); await tick(); });
    expect(importModels).not.toHaveBeenCalled();
    expect(container.textContent).toContain("models.importMissingPrice");
  });

  it("shows dry-run rows and blocks apply when the batch has conflicts", async () => {
    vi.mocked(importModels).mockResolvedValue({
      created: [], updated: [], skipped: [], missing_price: [], affected_keys: ["k1"],
      conflicts: [{ name: "claude-sonnet", action: "conflict", reason: "duplicate_batch_name", affected_keys: ["k1"] }],
    });
    await act(async () => {
      root = createRoot(container);
      root.render(<ModelImportWizard onApplied={async () => undefined} />);
      await tick();
      await tick();
    });
    const prices = [...container.querySelectorAll<HTMLButtonElement>("button")].find((button) => button.textContent === "models.pricingPreview")!;
    await act(async () => { prices.click(); await tick(); });
    const dryRun = [...container.querySelectorAll<HTMLButtonElement>("button")].find((button) => button.textContent === "models.importPreview")!;
    await act(async () => { dryRun.click(); await tick(); });
    const apply = [...container.querySelectorAll<HTMLButtonElement>("button")].find((button) => button.textContent === "models.importApply")!;
    expect(apply.disabled).toBe(true);
    expect(container.textContent).toContain("models.importResultRow");
  });
});

describe("buildImportItems", () => {
  it("emits one target per selected draft", () => {
    const items = buildImportItems([{
      modelId: "gpt-4o", selected: true, publicName: "gpt-4o", targetKey: "openai||gpt-4o",
      dispatch: "round-robin", overwrite: false, input: 1, output: 2, cacheRead: 0.1, cacheWrite: 0.3, missingPrice: false,
    }]);
    expect(items).toEqual([expect.objectContaining({
      name: "gpt-4o",
      targets: [{ provider: "openai", target_model: "gpt-4o" }],
    })]);
  });

  it("omits an unconfigured cache-write price", () => {
    const [item] = buildImportItems([{
      modelId: "gpt-mini", selected: true, publicName: "gpt-mini", targetKey: "openai||gpt-mini",
      dispatch: "round-robin", overwrite: false, input: 0.15, output: 0.6, cacheRead: 0, cacheWrite: undefined, missingPrice: false,
    }]);
    expect(item).not.toHaveProperty("cache_write_price_per_million");
  });
});
