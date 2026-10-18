import { act } from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { createRoot } from "react-dom/client";
import { Simulate } from "react-dom/test-utils";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import type { KeyPublic } from "../types";

(globalThis as unknown as { IS_REACT_ACT_ENVIRONMENT?: boolean }).IS_REACT_ACT_ENVIRONMENT = true;

vi.mock("../api/keys", () => ({
  listKeys: vi.fn(),
  patchKey: vi.fn(),
  rotateKey: vi.fn(),
  deleteKey: vi.fn(),
}));
const { translate } = vi.hoisted(() => ({ translate: (key: string) => key }));
vi.mock("../i18n", () => ({ useT: () => translate }));

import { listKeys, patchKey } from "../api/keys";
import KeyEdit from "./KeyEdit";

const key: KeyPublic = {
  id: "team-a",
  name: "Server name",
  enabled: true,
  key_preview: "cpa_te...am-a",
  rpm: 60,
  daily_limit_usd: 10,
  weekly_limit_usd: 20,
  monthly_limit_usd: 30,
  usage: { daily_usd: 1, weekly_usd: 2, monthly_usd: 3, daily_limit_usd: 10, weekly_limit_usd: 20, monthly_limit_usd: 30 },
};

const tick = () => new Promise((resolve) => setTimeout(resolve, 0));
let container: HTMLDivElement;
let root: ReturnType<typeof createRoot>;

beforeEach(() => {
  container = document.createElement("div");
  document.body.appendChild(container);
  vi.mocked(listKeys).mockResolvedValue([key]);
  vi.mocked(patchKey).mockResolvedValue(key);
});

afterEach(() => {
  act(() => root.unmount());
  container.remove();
  vi.clearAllMocks();
});

describe("KeyEdit", () => {
  it("patches the edited key fields and returns to the saved list view", async () => {
    await act(async () => {
      root = createRoot(container);
      root.render(
        <MemoryRouter initialEntries={[{ pathname: "/keys/team-a/edit", state: { keyListReturnTo: "/keys?q=team" } }]}>
          <Routes><Route path="/keys/:id/edit" element={<KeyEdit />} /><Route path="/keys" element={<div>key-list</div>} /></Routes>
        </MemoryRouter>,
      );
      await tick();
      await tick();
    });
    expect(container.querySelector<HTMLInputElement>('input[value="Server name"]')).not.toBeNull();
    expect(container.textContent).toContain("keyForm.currentUsage");
    const name = container.querySelector<HTMLInputElement>('input[value="Server name"]')!;
    await act(async () => Simulate.change(name, { target: { value: "Renamed" } } as never));
    await act(async () => {
      container.querySelector("form")!.dispatchEvent(new Event("submit", { bubbles: true, cancelable: true }));
      await tick();
    });
    expect(patchKey).toHaveBeenCalledWith({
      id: "team-a",
      name: "Renamed",
      enabled: true,
      rpm: 60,
      quota_mode: "cycle",
      daily_limit_usd: 10,
      weekly_limit_usd: 20,
      monthly_limit_usd: 30,
    });
    expect(container.textContent).toContain("key-list");
  });
});
