import { act } from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { createRoot } from "react-dom/client";
import { Simulate } from "react-dom/test-utils";
import { MemoryRouter, Route, Routes, useLocation } from "react-router-dom";

(globalThis as unknown as { IS_REACT_ACT_ENVIRONMENT?: boolean }).IS_REACT_ACT_ENVIRONMENT = true;

vi.mock("../api/modelDefinitions", () => ({
  fetchModelDefinitions: vi.fn(),
  upsertModelDefinition: vi.fn(),
}));
const { translate } = vi.hoisted(() => ({ translate: (key: string) => key }));
vi.mock("../i18n", () => ({ useT: () => translate }));

import { upsertModelDefinition } from "../api/modelDefinitions";
import ModelForm, { safeKeyReturnPath } from "./ModelForm";

const tick = () => new Promise((resolve) => setTimeout(resolve, 0));
let container: HTMLDivElement;
let root: ReturnType<typeof createRoot> | undefined;

function Destination() {
  const location = useLocation();
  return <pre data-testid="destination">{JSON.stringify({ path: location.pathname, state: location.state })}</pre>;
}

beforeEach(() => {
  container = document.createElement("div");
  document.body.appendChild(container);
  root = undefined;
  vi.mocked(upsertModelDefinition).mockImplementation(async (model) => model);
});

afterEach(() => {
  if (root) act(() => root?.unmount());
  container.remove();
  vi.clearAllMocks();
});

const draftKey = {
  id: "team-a", name: "Team A", enabled: true, rpm: 10, models: [],
  daily_limit_usd: 1, weekly_limit_usd: 2, monthly_limit_usd: 3,
};
const fromKeyForm = { returnTo: "/keys/new", draftKey, keyListReturnTo: "/keys?q=team&page=2" };

async function renderForm(state: unknown, back = "/keys/new") {
  await act(async () => {
    root = createRoot(container);
    root.render(
      <MemoryRouter initialEntries={[{ pathname: "/models/new", state }]}>
        <Routes>
          <Route path="/models/new" element={<ModelForm />} />
          <Route path={back} element={<Destination />} />
        </Routes>
      </MemoryRouter>,
    );
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

function destination() {
  return JSON.parse(container.querySelector('[data-testid="destination"]')!.textContent ?? "{}") as { path: string; state: unknown };
}

describe("ModelForm", () => {
  it("accepts only controlled key return paths", () => {
    expect(safeKeyReturnPath("/keys/new")).toBe("/keys/new");
    expect(safeKeyReturnPath("/keys/team-a/edit")).toBe("/keys/team-a/edit");
    expect(safeKeyReturnPath("https://example.invalid/keys/new")).toBeUndefined();
    expect(safeKeyReturnPath("/audit")).toBeUndefined();
  });

  it("saves an unpriced model with one upstream and returns to the key draft", async () => {
    await renderForm(fromKeyForm);
    await type(".upstream-row input", "xai", 0);
    await type(".upstream-row input", "grok-4.7", 1);
    await submit();
    expect(container.querySelector('[role="alert"]')?.textContent).toBe("models.required");
    await type(".card input", "grok-4.7", 0);
    expect(container.textContent).toContain("models.unpricedHint");
    await submit();
    expect(upsertModelDefinition).toHaveBeenCalledWith(expect.objectContaining({
      name: "grok-4.7", provider: "xai", target_model: "grok-4.7", billing_mode: "tokens", billing_multiplier: 1,
    }));
    expect(destination()).toEqual({ path: "/keys/new", state: { createdModel: "grok-4.7", draftKey, keyListReturnTo: "/keys?q=team&page=2" } });
  });

  it("cancel returns to the key form without creating a model", async () => {
    await renderForm(fromKeyForm);
    const cancel = [...container.querySelectorAll<HTMLButtonElement>("button")].find((button) => button.textContent === "keyForm.cancel")!;
    await act(async () => cancel.click());
    expect(upsertModelDefinition).not.toHaveBeenCalled();
    expect(destination().state).toEqual({ draftKey, keyListReturnTo: "/keys?q=team&page=2" });
  });

  it("applies the multiplier and shows what keys are charged", async () => {
    await renderForm(null, "/models");
    await type(".card input", "fast", 0);
    await type(".upstream-row input", "codex", 0);
    await type(".upstream-row input", "gpt-5", 1);
    await type(".field-row input", "2", 0);
    await type(".field-row input", "6", 1);
    const multiplier = container.querySelector<HTMLInputElement>(".model-multiplier input")!;
    await act(async () => Simulate.change(multiplier, { target: { value: "" } } as never));
    expect(multiplier.value).toBe("");
    await act(async () => Simulate.change(multiplier, { target: { value: "1.5" } } as never));
    expect(container.textContent).toContain("quota.multiplierExample");
    expect(container.textContent).toContain("models.chargedPrices");
    await submit();
    expect(upsertModelDefinition).toHaveBeenCalledWith(expect.objectContaining({
      name: "fast", input_price_per_million: 2, output_price_per_million: 6, billing_multiplier: 1.5,
    }));
    expect(destination().path).toBe("/models");
  });
});
