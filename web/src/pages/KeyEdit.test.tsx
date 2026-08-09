import { act } from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { createRoot } from "react-dom/client";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import type { KeyPublic, ModelDefinition } from "../types";

(globalThis as unknown as { IS_REACT_ACT_ENVIRONMENT?: boolean }).IS_REACT_ACT_ENVIRONMENT = true;

vi.mock("../api/keys", () => ({
  listKeys: vi.fn(),
  patchKey: vi.fn(),
  rotateKey: vi.fn(),
  deleteKey: vi.fn(),
}));
vi.mock("../api/modelDefinitions", () => ({ fetchModelDefinitions: vi.fn() }));
const { translate } = vi.hoisted(() => ({ translate: (key: string) => key }));
vi.mock("../i18n", () => ({ useT: () => translate }));

import { fetchModelDefinitions } from "../api/modelDefinitions";
import { deleteKey, listKeys, patchKey, rotateKey } from "../api/keys";
import KeyEdit from "./KeyEdit";

const models: ModelDefinition[] = ["fast", "slow"].map((name) => ({
  name,
  targets: [{ provider: "codex", target_model: name }],
  dispatch: "round-robin",
  billing_mode: "tokens",
  free: true,
}));

const key: KeyPublic = {
  id: "team-a",
  name: "Server name",
  enabled: true,
  key_preview: "cpa_te...am-a",
  rpm: 60,
  models: [{ name: "fast", daily_limit_usd: 1 }],
  daily_limit_usd: 10,
  weekly_limit_usd: 20,
  monthly_limit_usd: 30,
  usage: { daily_usd: 1, weekly_usd: 2, daily_limit_usd: 10, weekly_limit_usd: 20 },
};

const tick = () => new Promise((resolve) => setTimeout(resolve, 0));
let container: HTMLDivElement;
let root: ReturnType<typeof createRoot>;

beforeEach(() => {
  container = document.createElement("div");
  document.body.appendChild(container);
  vi.mocked(listKeys).mockResolvedValue([key]);
  vi.mocked(fetchModelDefinitions).mockResolvedValue(models);
  vi.mocked(patchKey).mockResolvedValue(key);
  vi.mocked(rotateKey).mockResolvedValue({ key, plain_key: "cpa_secret", generated: true });
  vi.mocked(deleteKey).mockResolvedValue(undefined);
  vi.stubGlobal("confirm", vi.fn(() => true));
});

afterEach(() => {
  act(() => root.unmount());
  container.remove();
  vi.unstubAllGlobals();
  vi.clearAllMocks();
});

describe("KeyEdit v3 model return flow", () => {
  it("restores the key draft, auto-selects the created model and patches only references", async () => {
    const entry = {
      pathname: "/keys/team-a/edit",
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
          <Routes><Route path="/keys/:id/edit" element={<KeyEdit />} /><Route path="/keys" element={<div>key-list</div>} /></Routes>
        </MemoryRouter>,
      );
      await tick();
      await tick();
      await tick();
    });

    const checked = [...container.querySelectorAll<HTMLInputElement>(".model-definition-main input")].filter((input) => input.checked);
    expect(checked).toHaveLength(2);
    expect(container.querySelector<HTMLInputElement>('input[value="Draft name"]')).not.toBeNull();
    expect(container.textContent).toContain("keyForm.currentUsage");

    await act(async () => {
      container.querySelector("form")!.dispatchEvent(new Event("submit", { bubbles: true, cancelable: true }));
      await tick();
    });
    expect(patchKey).toHaveBeenCalledWith(expect.objectContaining({
      id: "team-a",
      name: "Draft name",
      rpm: 42,
      models: [{ name: "fast", daily_limit_usd: 4 }, { name: "slow", daily_limit_usd: 0 }],
      daily_limit_usd: 11,
      weekly_limit_usd: 22,
      monthly_limit_usd: 33,
    }));
    expect(JSON.stringify(vi.mocked(patchKey).mock.calls[0][0])).not.toContain("target_model");
    expect(container.textContent).toContain("key-list");
  });
});
