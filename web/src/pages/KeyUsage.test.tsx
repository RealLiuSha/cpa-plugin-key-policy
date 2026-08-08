import { act } from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { createRoot } from "react-dom/client";
import { MemoryRouter, Route, Routes } from "react-router-dom";

(globalThis as unknown as { IS_REACT_ACT_ENVIRONMENT?: boolean }).IS_REACT_ACT_ENVIRONMENT = true;

vi.mock("../api/keys", () => ({
  fetchKeyUsage: vi.fn(),
  fetchKeyHistory: vi.fn(),
}));
vi.mock("../i18n", () => ({ useT: () => (key: string) => key }));

import { fetchKeyHistory, fetchKeyUsage } from "../api/keys";
import KeyUsage from "./KeyUsage";

const tick = () => new Promise((resolve) => setTimeout(resolve, 0));

describe("KeyUsage history and window controls", () => {
  let container: HTMLDivElement;
  let root: ReturnType<typeof createRoot>;

  beforeEach(() => {
    container = document.createElement("div");
    document.body.appendChild(container);
    vi.mocked(fetchKeyUsage).mockResolvedValue({
      key_id: "k1",
      key_name: "K1",
      daily_limit_usd: 5,
      weekly_limit_usd: 20,
      monthly_limit_usd: 50,
      aliases: [{
        alias: "fast", in_config: true,
        daily: { total_usd: 1 },
        weekly: { total_usd: 4 },
        monthly: { total_usd: 9 },
      }],
    });
    vi.mocked(fetchKeyHistory).mockResolvedValue({
      key_id: "k1",
      timezone: "Asia/Shanghai",
      days: Array.from({ length: 30 }, (_, index) => ({ date: `2026-07-${String(index + 1).padStart(2, "0")}`, total_usd: index / 10 })),
    });
  });

  afterEach(() => {
    act(() => root.unmount());
    container.remove();
    vi.clearAllMocks();
  });

  it("renders a 30-day chart with limit line and all three windows", async () => {
    await act(async () => {
      root = createRoot(container);
      root.render(
        <MemoryRouter initialEntries={["/keys/k1/usage"]}>
          <Routes><Route path="/keys/:id/usage" element={<KeyUsage />} /></Routes>
        </MemoryRouter>,
      );
      await tick();
      await tick();
    });
    expect(fetchKeyHistory).toHaveBeenCalledWith("k1", 30);
    expect(container.querySelector('[data-testid="usage-history-chart"]')).not.toBeNull();
    expect(container.querySelector('[data-testid="usage-history-limit-line"]')).not.toBeNull();
    const tabs = Array.from(container.querySelectorAll('[role="tab"]')).map((tab) => tab.textContent);
    expect(tabs).toEqual(["keyUsage.tabDaily", "keyUsage.tabWeekly", "keyUsage.tabMonthly"]);
    const monthly = Array.from(container.querySelectorAll<HTMLButtonElement>('[role="tab"]')).find((button) => button.textContent === "keyUsage.tabMonthly");
    await act(async () => { monthly!.click(); });
    expect(container.textContent).toContain("$9.00");
  });
});
