import { act } from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { createRoot } from "react-dom/client";
import { Simulate } from "react-dom/test-utils";
import { MemoryRouter, Route, Routes, useLocation } from "react-router-dom";
import type { KeyPublic, ModelDefinition } from "../types";

(globalThis as unknown as { IS_REACT_ACT_ENVIRONMENT?: boolean }).IS_REACT_ACT_ENVIRONMENT = true;

vi.mock("../api/modelDefinitions", () => ({ fetchModelDefinitions: vi.fn() }));
const { translate } = vi.hoisted(() => ({ translate: (key: string) => key }));
vi.mock("../i18n", () => ({ useT: () => translate }));

import { fetchModelDefinitions } from "../api/modelDefinitions";
import KeyForm, { keyWriteRequestFromForm, type KeyFormValues } from "./KeyForm";

const definitions: ModelDefinition[] = [
  {
    name: "fast",
    targets: [{ provider: "codex", target_model: "gpt-5" }],
    dispatch: "round-robin",
    billing_mode: "tokens",
    free: false,
    input_price_per_million: 1,
    output_price_per_million: 2,
    cache_read_price_per_million: 0.2,
  },
  {
    name: "community",
    targets: [{ provider: "openai", target_model: "small" }],
    dispatch: "priority",
    billing_mode: "tokens",
    free: true,
  },
];

const initial: KeyPublic = {
  id: "team-a",
  name: "Team A",
  enabled: true,
  key_preview: "cpa_te...am-a",
  rpm: 60,
  models: [{ name: "fast", daily_limit_usd: 2 }],
  daily_limit_usd: 10,
  weekly_limit_usd: 50,
  monthly_limit_usd: 100,
  usage: { daily_usd: 1, weekly_usd: 2, daily_limit_usd: 10, weekly_limit_usd: 50 },
};

const tick = () => new Promise((resolve) => setTimeout(resolve, 0));
let container: HTMLDivElement;
let root: ReturnType<typeof createRoot> | undefined;

beforeEach(() => {
  container = document.createElement("div");
  document.body.appendChild(container);
  root = undefined;
  vi.mocked(fetchModelDefinitions).mockResolvedValue(definitions);
});

afterEach(() => {
  if (root) act(() => root?.unmount());
  container.remove();
  vi.clearAllMocks();
});

async function flush() {
  await act(async () => { await tick(); await tick(); });
}

describe("KeyForm v3 model references", () => {
  it("builds one complete request without target or price fields", () => {
    const request = keyWriteRequestFromForm({
      id: "team-a",
      name: " ",
      enabled: true,
      rpm: 60,
      models: [{ name: "fast", daily_limit_usd: 3 }],
      daily_limit_usd: 10,
      weekly_limit_usd: 20,
      monthly_limit_usd: 30,
      allow_models_endpoint: true,
    });
    expect(request).toEqual({
      id: "team-a",
      name: undefined,
      enabled: true,
      rpm: 60,
      models: [{ name: "fast", daily_limit_usd: 3 }],
      daily_limit_usd: 10,
      weekly_limit_usd: 20,
      monthly_limit_usd: 30,
      allow_models_endpoint: true,
    });
    expect(JSON.stringify(request)).not.toContain("target_model");
    expect(JSON.stringify(request)).not.toContain("price_per_million");
  });

  it("shows model prices read-only and submits selected names with per-model daily limits", async () => {
    let submitted: KeyFormValues | null = null;
    await act(async () => {
      root = createRoot(container);
      root.render(<MemoryRouter><KeyForm initial={initial} submitLabel="save" onCancel={() => {}} onSubmit={async (values) => { submitted = values; }} /></MemoryRouter>);
    });
    await flush();

    expect(container.textContent).toContain("models.priceTokenSummary");
    expect(container.textContent).toContain("models.free");
    const modelChecks = container.querySelectorAll<HTMLInputElement>(".model-definition-main input");
    expect(modelChecks).toHaveLength(2);
    expect(modelChecks[0].checked).toBe(true);
    expect(modelChecks[1].checked).toBe(false);

    const fastLimit = container.querySelector<HTMLInputElement>(".model-limit-field input");
    expect(fastLimit?.value).toBe("2");
    await act(async () => {
      Simulate.change(fastLimit!, { target: { value: "3.5" } } as never);
      modelChecks[1].click();
    });
    await act(async () => {
      container.querySelector("form")!.dispatchEvent(new Event("submit", { bubbles: true, cancelable: true }));
      await tick();
    });

    expect(submitted).not.toBeNull();
    expect(submitted!.models).toEqual([
      { name: "fast", daily_limit_usd: 3.5 },
      { name: "community", daily_limit_usd: 0 },
    ]);
    expect(JSON.stringify(submitted!.models)).not.toContain("provider");
  });

  it("blocks negative total or model limits at the form boundary", async () => {
    const onSubmit = vi.fn();
    await act(async () => {
      root = createRoot(container);
      root.render(<MemoryRouter><KeyForm initial={initial} submitLabel="save" onCancel={() => {}} onSubmit={onSubmit} /></MemoryRouter>);
    });
    await flush();
    const fastLimit = container.querySelector<HTMLInputElement>(".model-limit-field input");
    await act(async () => Simulate.change(fastLimit!, { target: { value: "-1" } } as never));
    await act(async () => container.querySelector("form")!.dispatchEvent(new Event("submit", { bubbles: true, cancelable: true })));
    expect(onSubmit).not.toHaveBeenCalled();
    expect(container.textContent).toContain("keyForm.modelLimitInvalid");
  });

  it("opens model creation with a safe return path and the complete key draft", async () => {
    function Destination() {
      const location = useLocation();
      return <pre data-testid="destination">{JSON.stringify(location.state)}</pre>;
    }
    await act(async () => {
      root = createRoot(container);
      root.render(
        <MemoryRouter initialEntries={["/keys/team-a/edit"]}>
          <Routes>
            <Route path="/keys/team-a/edit" element={<KeyForm initial={initial} returnPath="/keys/team-a/edit" submitLabel="save" onCancel={() => {}} onSubmit={async () => {}} />} />
            <Route path="/models/new" element={<Destination />} />
          </Routes>
        </MemoryRouter>,
      );
    });
    await flush();
    const create = Array.from(container.querySelectorAll<HTMLButtonElement>("button")).find((button) => button.textContent === "keyForm.newModel");
    await act(async () => create!.click());
    const state = JSON.parse(container.querySelector('[data-testid="destination"]')!.textContent ?? "{}") as { returnTo: string; draftKey: KeyFormValues };
    expect(state.returnTo).toBe("/keys/team-a/edit");
    expect(state.draftKey).toMatchObject({ id: "team-a", models: [{ name: "fast", daily_limit_usd: 2 }] });
  });
});
