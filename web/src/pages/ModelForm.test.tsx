import { act } from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { createRoot } from "react-dom/client";
import { Simulate } from "react-dom/test-utils";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import type { ModelPrice } from "../types";

(globalThis as unknown as { IS_REACT_ACT_ENVIRONMENT?: boolean }).IS_REACT_ACT_ENVIRONMENT = true;

vi.mock("../api/modelPrices", () => ({
  fetchModelPrices: vi.fn(),
  saveModelPrice: vi.fn(),
}));
const { translate } = vi.hoisted(() => ({ translate: (key: string, vars?: Record<string, string | number>) => key + (vars ? ` ${Object.values(vars).join(" ")}` : "") }));
vi.mock("../i18n", () => ({ useT: () => translate }));

import { fetchModelPrices, saveModelPrice } from "../api/modelPrices";
import { modelEditPath, modelNewPath } from "../navigation";
import ModelForm from "./ModelForm";

const tick = () => new Promise((resolve) => setTimeout(resolve, 0));
let container: HTMLDivElement;
let root: ReturnType<typeof createRoot> | undefined;

beforeEach(() => {
  container = document.createElement("div");
  document.body.appendChild(container);
  root = undefined;
  vi.mocked(saveModelPrice).mockImplementation(async (price: ModelPrice) => ({ ...price, priced: true }));
  vi.mocked(fetchModelPrices).mockResolvedValue({
    models: [
      { name: "anthropic/claude-sonnet", provider: "", billing_mode: "tokens", billing_multiplier: 1, input_price_per_million: 3, output_price_per_million: 15, priced: true },
      { name: "anthropic/claude-sonnet", provider: "openrouter", billing_mode: "tokens", billing_multiplier: 2, input_price_per_million: 4, output_price_per_million: 16, priced: true },
    ],
    unpriced: [],
  });
});

afterEach(() => {
  if (root) act(() => root?.unmount());
  container.remove();
  vi.clearAllMocks();
});

async function renderAt(path: string) {
  await act(async () => {
    root = createRoot(container);
    root.render(
      <MemoryRouter initialEntries={[path]}>
        <Routes>
          <Route path="/models" element={<div data-testid="models-page" />} />
          <Route path="/models/new" element={<ModelForm key="new" mode="new" />} />
          <Route path="/models/edit" element={<ModelForm key="edit" mode="edit" />} />
        </Routes>
      </MemoryRouter>,
    );
    await tick();
    await tick();
  });
}

async function type(selector: string, value: string, index = 0) {
  const input = container.querySelectorAll<HTMLInputElement>(selector)[index];
  await act(async () => Simulate.change(input, { target: { value } } as never));
}

async function submit() {
  await act(async () => {
    container.querySelector("form")!.dispatchEvent(new Event("submit", { bubbles: true, cancelable: true }));
    await tick();
  });
}

const identityInputs = () => container.querySelectorAll<HTMLInputElement>("section.card")[0].querySelectorAll("input");

describe("ModelForm", () => {
  it("builds edit and new routes that keep model ids with slashes intact", () => {
    expect(modelEditPath({ name: "anthropic/claude-sonnet", provider: "" })).toBe("/models/edit?name=anthropic%2Fclaude-sonnet");
    expect(modelEditPath({ name: "gpt-5", provider: "codex" })).toBe("/models/edit?name=gpt-5&provider=codex");
    expect(modelNewPath()).toBe("/models/new");
    expect(modelNewPath("gpt-6-terra")).toBe("/models/new?name=gpt-6-terra");
  });

  it("requires a model id and saves an unpriced row for any provider", async () => {
    await renderAt("/models/new");
    await submit();
    expect(container.querySelector('[role="alert"]')?.textContent).toBe("models.required");
    await type("section.card input", "grok-4.7", 0);
    expect(container.textContent).toContain("models.unpricedHint");
    await submit();
    expect(saveModelPrice).toHaveBeenCalledWith(expect.objectContaining({
      name: "grok-4.7", provider: "", billing_mode: "tokens", billing_multiplier: 1, input_price_per_million: 0,
    }));
    expect(container.querySelector('[data-testid="models-page"]')).not.toBeNull();
  });

  it("prefills the model id of an unpriced model and saves a provider-qualified price with its multiplier", async () => {
    await renderAt(modelNewPath("gpt-6-terra"));
    expect(identityInputs()[0].value).toBe("gpt-6-terra");
    await type("section.card input", " codex ", 1);
    await type(".field-row input", "2", 0);
    await type(".field-row input", "6", 1);
    const multiplier = container.querySelector<HTMLInputElement>(".model-multiplier input")!;
    await act(async () => Simulate.change(multiplier, { target: { value: "51" } } as never));
    expect(container.textContent).not.toContain("quota.multiplierExample");
    await submit();
    expect(container.querySelector('[role="alert"]')?.textContent).toBe("quota.invalidMultiplier");
    await act(async () => Simulate.change(multiplier, { target: { value: "1.5" } } as never));
    expect(container.textContent).toContain("quota.multiplierExample $8.00 $12.00");
    expect(container.textContent).toContain("models.chargedPrices");
    await submit();
    expect(saveModelPrice).toHaveBeenCalledWith(expect.objectContaining({
      name: "gpt-6-terra", provider: "codex", input_price_per_million: 2, output_price_per_million: 6, billing_multiplier: 1.5,
    }));
  });

  it("says when a new entry is an existing row, which saving updates under its own spelling", async () => {
    await renderAt("/models/new");
    await type("section.card input", "ANTHROPIC/Claude-Sonnet", 0);
    expect(container.querySelector('[role="note"]')?.textContent).toBe("models.existingRow anthropic/claude-sonnet models.anyProvider");
    await type("section.card input", "openai-compatible-OpenRouter", 1);
    expect(container.querySelector('[role="note"]')?.textContent).toBe("models.existingRow anthropic/claude-sonnet openrouter");
    await type("section.card input", "codex", 1);
    expect(container.querySelector('[role="note"]')).toBeNull();
  });

  it("says where per-call billing applies only while per-call is chosen", async () => {
    await renderAt("/models/new");
    const [tokens, perCall] = container.querySelectorAll<HTMLInputElement>('input[name="billing_mode"]');
    expect(container.querySelector(".per-call-hint")).toBeNull();
    await act(async () => perCall.click());
    expect(container.querySelector(".per-call-hint")?.textContent).toBe("models.perCallHint");
    await act(async () => tokens.click());
    expect(container.querySelector(".per-call-hint")).toBeNull();
  });

  it("edits the row named by model id and provider and keeps its identity read-only", async () => {
    await renderAt(modelEditPath({ name: "anthropic/claude-sonnet", provider: "openrouter" }));
    const [name, provider] = identityInputs();
    expect(name.value).toBe("anthropic/claude-sonnet");
    expect(provider.value).toBe("openrouter");
    expect(name.disabled && provider.disabled).toBe(true);
    expect(container.querySelector<HTMLInputElement>(".model-multiplier input")?.value).toBe("2");
    await type(".field-row input", "5", 0);
    await submit();
    expect(saveModelPrice).toHaveBeenCalledWith(expect.objectContaining({
      name: "anthropic/claude-sonnet", provider: "openrouter", input_price_per_million: 5, output_price_per_million: 16, billing_multiplier: 2,
    }));
  });

  it("reports a missing row and cancels without saving", async () => {
    await renderAt(modelEditPath({ name: "anthropic/claude-sonnet", provider: "vertex" }));
    expect(container.querySelector('[role="alert"]')?.textContent).toBe("models.notFound");
    const cancel = [...container.querySelectorAll<HTMLButtonElement>("button")].find((button) => button.textContent === "keyForm.cancel")!;
    await act(async () => cancel.click());
    expect(saveModelPrice).not.toHaveBeenCalled();
    expect(container.querySelector('[data-testid="models-page"]')).not.toBeNull();
  });
});
