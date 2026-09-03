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
import { safeModelFormReturnPath } from "./ModelPick";

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
  id: "team-a",
  name: "Team A",
  enabled: true,
  rpm: 10,
  models: [],
  daily_limit_usd: 1,
  weekly_limit_usd: 2,
  monthly_limit_usd: 3,
};

const draftModel = {
  name: "fast",
  targets: [{ provider: "codex", target_model: "gpt-5" }],
  dispatch: "round-robin" as const,
  billing_mode: "tokens" as const,
  free: true,
  returnTo: "/keys/new",
  draftKey,
};

describe("ModelForm return flow", () => {
  it("accepts only controlled key and model-form return paths", () => {
    expect(safeKeyReturnPath("/keys/new")).toBe("/keys/new");
    expect(safeKeyReturnPath("/keys/team-a/edit")).toBe("/keys/team-a/edit");
    expect(safeKeyReturnPath("https://example.invalid/keys/new")).toBeUndefined();
    expect(safeKeyReturnPath("/audit")).toBeUndefined();
    expect(safeModelFormReturnPath("/models/new")).toBe("/models/new");
    expect(safeModelFormReturnPath("/models/fast/edit")).toBe("/models/fast/edit");
    expect(safeModelFormReturnPath("//example.invalid/models/new")).toBe("/models/new");
  });

  it("returns the complete key draft and auto-selection signal after save", async () => {
    await act(async () => {
      root = createRoot(container);
      root.render(
        <MemoryRouter initialEntries={[{ pathname: "/models/new", state: { draftModel } }]}>
          <Routes>
            <Route path="/models/new" element={<ModelForm />} />
            <Route path="/keys/new" element={<Destination />} />
          </Routes>
        </MemoryRouter>,
      );
    });
    await act(async () => {
      container.querySelector("form")!.dispatchEvent(new Event("submit", { bubbles: true, cancelable: true }));
      await tick();
    });
    expect(upsertModelDefinition).toHaveBeenCalledWith(expect.objectContaining({ name: "fast", free: true }));
    const destination = JSON.parse(container.querySelector('[data-testid="destination"]')!.textContent ?? "{}") as { path: string; state: { createdModel: string; draftKey: typeof draftKey } };
    expect(destination.path).toBe("/keys/new");
    expect(destination.state).toEqual({ createdModel: "fast", draftKey });
  });

  it("cancel returns to the key form without creating a model", async () => {
    await act(async () => {
      root = createRoot(container);
      root.render(
        <MemoryRouter initialEntries={[{ pathname: "/models/new", state: { draftModel } }]}>
          <Routes>
            <Route path="/models/new" element={<ModelForm />} />
            <Route path="/keys/new" element={<Destination />} />
          </Routes>
        </MemoryRouter>,
      );
    });
    const cancel = [...container.querySelectorAll<HTMLButtonElement>("button")].find((button) => button.textContent === "keyForm.cancel")!;
    await act(async () => cancel.click());
    expect(upsertModelDefinition).not.toHaveBeenCalled();
    const destination = JSON.parse(container.querySelector('[data-testid="destination"]')!.textContent ?? "{}") as { state: { draftKey: typeof draftKey } };
    expect(destination.state).toEqual({ draftKey });
  });

  it("submits a custom multi-target model with the selected dispatch and global prices", async () => {
    const multiTargetDraft = {
      name: "asd",
      targets: [
        { provider: "codex", target_model: "gpt" },
        { provider: "xai", group: "plus", target_model: "grok" },
      ],
      dispatch: "round-robin" as const,
      billing_mode: "tokens" as const,
      free: false,
      input_price_per_million: 1,
      output_price_per_million: 2,
      cache_read_price_per_million: 0.5,
      cache_write_price_per_million: 1.5,
      per_call_usd: 0,
    };
    await act(async () => {
      root = createRoot(container);
      root.render(
        <MemoryRouter initialEntries={[{ pathname: "/models/new", state: { draftModel: multiTargetDraft } }]}>
          <Routes>
            <Route path="/models/new" element={<ModelForm />} />
            <Route path="/models" element={<Destination />} />
          </Routes>
        </MemoryRouter>,
      );
    });
    const dispatch = container.querySelector<HTMLSelectElement>(".field-row select")!;
    await act(async () => Simulate.change(dispatch, { target: { value: "priority" } } as never));
    expect(container.textContent).toContain("models.formHint");
    expect(container.textContent).toContain("models.priceHint");
    expect(container.textContent).toContain("codex / gpt");
    expect(container.textContent).toContain("xai · plus / grok");
    await act(async () => {
      container.querySelector("form")!.dispatchEvent(new Event("submit", { bubbles: true, cancelable: true }));
      await tick();
    });
    expect(upsertModelDefinition).toHaveBeenCalledWith({
      ...multiTargetDraft,
      dispatch: "priority",
    });
  });
});
