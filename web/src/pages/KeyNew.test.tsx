import { act } from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { createRoot } from "react-dom/client";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import type { ModelDefinition } from "../types";

(globalThis as unknown as { IS_REACT_ACT_ENVIRONMENT?: boolean }).IS_REACT_ACT_ENVIRONMENT = true;

vi.mock("../api/keys", () => ({ createKey: vi.fn() }));
vi.mock("../api/modelDefinitions", () => ({ fetchModelDefinitions: vi.fn() }));
const { translate } = vi.hoisted(() => ({ translate: (key: string) => key }));
vi.mock("../i18n", () => ({ useT: () => translate }));

import { createKey } from "../api/keys";
import { fetchModelDefinitions } from "../api/modelDefinitions";
import KeyNew from "./KeyNew";

const models: ModelDefinition[] = ["fast", "slow"].map((name) => ({
  name,
  targets: [{ provider: "codex", target_model: name }],
  dispatch: "round-robin",
  billing_mode: "tokens",
  free: true,
}));
const tick = () => new Promise((resolve) => setTimeout(resolve, 0));
let container: HTMLDivElement;
let root: ReturnType<typeof createRoot>;

beforeEach(() => {
  container = document.createElement("div");
  document.body.appendChild(container);
  vi.mocked(fetchModelDefinitions).mockResolvedValue(models);
  vi.mocked(createKey).mockResolvedValue({
    key: {
      id: "team-a", name: "Draft name", enabled: true, key_preview: "cpa_...", rpm: 42,
      models: [{ name: "fast" }, { name: "slow" }], daily_limit_usd: 11, weekly_limit_usd: 22, monthly_limit_usd: 33,
      usage: { daily_usd: 0, weekly_usd: 0, daily_limit_usd: 11, weekly_limit_usd: 22 },
    },
    plain_key: "cpa_secret",
    generated: true,
  });
});

afterEach(() => {
  act(() => root.unmount());
  container.remove();
  vi.clearAllMocks();
});

describe("KeyNew v3 model return flow", () => {
  it("restores the draft, auto-selects the created model and creates only model references", async () => {
    const entry = {
      pathname: "/keys/new",
      state: {
        createdModel: "slow",
        draftKey: {
          id: "team-a",
          name: "Draft name",
          enabled: true,
          rpm: 42,
          models: [{ name: "fast", daily_limit_usd: 4 }],
          daily_limit_usd: 11,
          weekly_limit_usd: 22,
          monthly_limit_usd: 33,
        },
      },
    };
    await act(async () => {
      root = createRoot(container);
      root.render(
        <MemoryRouter initialEntries={[entry]}>
          <Routes><Route path="/keys/new" element={<KeyNew />} /></Routes>
        </MemoryRouter>,
      );
      await tick();
      await tick();
    });
    const checked = [...container.querySelectorAll<HTMLInputElement>(".model-definition-main input")].filter((input) => input.checked);
    expect(checked).toHaveLength(2);
    expect(container.querySelector<HTMLInputElement>('input[value="Draft name"]')).not.toBeNull();
    expect(container.textContent).not.toContain("keyForm.currentUsage");

    await act(async () => {
      container.querySelector("form")!.dispatchEvent(new Event("submit", { bubbles: true, cancelable: true }));
      await tick();
    });
    expect(createKey).toHaveBeenCalledWith(expect.objectContaining({
      id: "team-a",
      name: "Draft name",
      rpm: 42,
      models: [{ name: "fast", daily_limit_usd: 4 }, { name: "slow", daily_limit_usd: 0 }],
      daily_limit_usd: 11,
      weekly_limit_usd: 22,
      monthly_limit_usd: 33,
    }));
    expect(JSON.stringify(vi.mocked(createKey).mock.calls[0][0])).not.toContain("target_model");
  });
});
