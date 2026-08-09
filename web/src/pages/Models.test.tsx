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
}));
const { translate } = vi.hoisted(() => ({ translate: (key: string) => key }));
vi.mock("../i18n", () => ({ useT: () => translate }));

import { deleteModelDefinition, fetchModelDefinitions, importModelPrices } from "../api/modelDefinitions";
import Models from "./Models";

const models: ModelDefinition[] = [
  {
    name: "fast",
    targets: [{ provider: "codex", group: "team", target_model: "gpt-5" }],
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
  vi.stubGlobal("confirm", vi.fn(() => true));
});

afterEach(() => {
  act(() => root.unmount());
  container.remove();
  vi.unstubAllGlobals();
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
    const cards = container.querySelectorAll(".model-card");
    expect(cards).toHaveLength(2);
    expect(cards[0].querySelector<HTMLButtonElement>("button.danger")?.disabled).toBe(true);
    expect(cards[1].querySelector<HTMLButtonElement>("button.danger")?.disabled).toBe(false);
  });

  it("previews and applies price imports, then refreshes model definitions", async () => {
    await renderPage();
    const textarea = container.querySelector<HTMLTextAreaElement>("textarea")!;
    await act(async () => Simulate.change(textarea, { target: { value: JSON.stringify({ matches: [{ model: "gpt-5", prompt_price_per_1m: 3 }] }) } } as never));
    const buttons = [...container.querySelectorAll<HTMLButtonElement>(".model-import button")];
    await act(async () => { buttons[0].click(); await tick(); });
    expect(importModelPrices).toHaveBeenNthCalledWith(1, { dry_run: true, matches: [{ model: "gpt-5", prompt_price_per_1m: 3 }] });
    await act(async () => { buttons[1].click(); await tick(); });
    expect(importModelPrices).toHaveBeenNthCalledWith(2, { dry_run: false, matches: [{ model: "gpt-5", prompt_price_per_1m: 3 }] });
    expect(fetchModelDefinitions).toHaveBeenCalledTimes(2);
  });

  it("deletes an unreferenced model only after confirmation", async () => {
    await renderPage();
    const cards = container.querySelectorAll(".model-card");
    const deleteButton = cards[1].querySelector<HTMLButtonElement>("button.danger")!;
    await act(async () => { deleteButton.click(); await tick(); });
    expect(confirm).toHaveBeenCalledWith("models.deleteConfirm");
    expect(deleteModelDefinition).toHaveBeenCalledWith("free-model");
    expect(fetchModelDefinitions).toHaveBeenCalledTimes(2);
  });
});
