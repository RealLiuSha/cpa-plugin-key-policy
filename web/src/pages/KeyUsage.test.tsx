import { act } from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { createRoot } from "react-dom/client";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import type { KeyHistoryResponse } from "../types";

(globalThis as unknown as { IS_REACT_ACT_ENVIRONMENT?: boolean }).IS_REACT_ACT_ENVIRONMENT = true;

vi.mock("../api/keys", () => ({
  fetchKeyUsage: vi.fn(),
  fetchKeyHistory: vi.fn(),
}));
const { translate } = vi.hoisted(() => ({ translate: (key: string, vars?: Record<string, string | number>) => key + (vars ? ` ${Object.values(vars).join(" ")}` : "") }));
vi.mock("../i18n", () => ({ useT: () => translate }));

import { fetchKeyHistory, fetchKeyUsage } from "../api/keys";
import KeyUsage, { sumHistoryByModel } from "./KeyUsage";

const tick = () => new Promise((resolve) => setTimeout(resolve, 0));

function history(days: number): KeyHistoryResponse {
  return {
    key_id: "k1",
    timezone: "Asia/Shanghai",
    days: Array.from({ length: days }, (_, index) => ({
      date: `2026-07-${String(index + 1).padStart(2, "0")}`,
      total_usd: 0.5,
      call_count: 3,
      by_model: {
        fast: { total_usd: 0.4, call_count: 2, input_tokens: 30, cache_read_tokens: 10 },
        "gpt-6-terra": { call_count: 1, unpriced_calls: 1, input_tokens: 5 },
      },
    })),
  };
}

describe("sumHistoryByModel", () => {
  it("adds every bucket field across days and puts the most expensive model first", () => {
    const totals = sumHistoryByModel([
      { date: "2026-07-01", by_model: { cheap: { total_usd: 0.001, call_count: 1, unpriced_calls: 1 }, fast: { total_usd: 1, input_tokens: 10 } } },
      { date: "2026-07-02" },
      { date: "2026-07-03", by_model: { fast: { total_usd: 2, input_tokens: 5, cache_write_usd: 0.5 } } },
    ]);
    expect(totals.map((row) => row.name)).toEqual(["fast", "cheap"]);
    expect(totals[0].usage).toMatchObject({ total_usd: 3, input_tokens: 15, cache_write_usd: 0.5, call_count: 0 });
    expect(totals[1].usage).toMatchObject({ total_usd: 0.001, call_count: 1, unpriced_calls: 1 });
  });
});

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
      usage: { daily_usd: 1, weekly_usd: 4, monthly_usd: 9, daily_limit_usd: 5, weekly_limit_usd: 20, monthly_limit_usd: 50 },
      models: [
        {
          name: "fast", billing_mode: "tokens", billing_multiplier: 1, priced: true,
          daily: { total_usd: 1, cache_read_tokens: 10, cache_cost_usd: 0.01, cache_write_tokens: 4, cache_write_usd: 0.02 },
          weekly: { total_usd: 4, cache_read_tokens: 20, cache_write_tokens: 8, cache_write_usd: 0.04 },
          monthly: { total_usd: 9, cache_read_tokens: 30, cache_write_tokens: 12, cache_write_usd: 0.06 },
        },
        {
          name: "gpt-6-terra", billing_mode: "tokens", billing_multiplier: 1, priced: false,
          daily: { total_usd: 0, call_count: 2, unpriced_calls: 2 },
          weekly: { total_usd: 0, call_count: 2, unpriced_calls: 2 },
          monthly: { total_usd: 0, call_count: 2, unpriced_calls: 2 },
        },
      ],
    });
    vi.mocked(fetchKeyHistory).mockImplementation(async (_id, days = 30) => history(days));
  });

  afterEach(() => {
    act(() => root.unmount());
    container.remove();
    vi.clearAllMocks();
  });

  async function renderPage(state?: unknown) {
    await act(async () => {
      root = createRoot(container);
      root.render(
        <MemoryRouter initialEntries={[{ pathname: "/keys/k1/usage", state }]}>
          <Routes><Route path="/keys/:id/usage" element={<KeyUsage />} /></Routes>
        </MemoryRouter>,
      );
      await tick();
      await tick();
    });
  }

  const rangeButton = (days: number) => [...container.querySelectorAll<HTMLButtonElement>(".usage-range-switch button")]
    .find((button) => button.textContent === `usage.rangeDays ${days}`)!;

  it("renders a 30-day chart with limit line and all three quota windows", async () => {
    await renderPage();
    expect(fetchKeyHistory).toHaveBeenCalledWith("k1", 30);
    expect(container.querySelector('[data-testid="usage-history-chart"]')).not.toBeNull();
    expect(container.querySelector('[data-testid="usage-chart-limit-line"]')).not.toBeNull();
    expect(container.querySelectorAll(".usage-chart-bar")).toHaveLength(30);
    const tabs = Array.from(container.querySelectorAll('[role="tab"]')).map((tab) => tab.textContent);
    expect(tabs).toEqual(["keyUsage.tabDaily", "keyUsage.tabWeekly", "keyUsage.tabMonthly"]);
    const monthly = Array.from(container.querySelectorAll<HTMLButtonElement>('[role="tab"]')).find((button) => button.textContent === "keyUsage.tabMonthly");
    await act(async () => { monthly!.click(); });
    expect(container.textContent).toContain("$9.00");
    expect(container.textContent).toContain("keyUsage.colCacheRead");
    expect(container.textContent).toContain("keyUsage.colCacheWrite");
  });

  it("goes back to the key list or stats page it was opened from and nowhere else", async () => {
    const backHref = () => [...container.querySelectorAll("a")].find((link) => link.textContent === "keyUsage.back")?.getAttribute("href");
    await renderPage({ keyListReturnTo: "/stats?days=90" });
    expect(backHref()).toBe("/stats?days=90");
    act(() => root.unmount());
    await renderPage({ keyListReturnTo: "/models" });
    expect(backHref()).toBe("/keys");
  });

  it("marks unpriced models in the period table and the mobile ranking", async () => {
    await renderPage();
    const rows = [...container.querySelectorAll<HTMLTableRowElement>(".table-wrap:not(.usage-range-models .table-wrap) tbody tr")];
    const terra = rows.find((row) => row.textContent?.includes("gpt-6-terra"))!;
    const fast = rows.find((row) => row.textContent?.includes("fast"))!;
    expect(terra.querySelector(".badge.warn")?.textContent).toBe("usage.unpriced");
    expect(fast.querySelector(".badge.warn")).toBeNull();
    const mobile = [...container.querySelectorAll(".mobile-only .br-row")].filter((row) => row.textContent?.includes("gpt-6-terra"));
    expect(mobile.every((row) => row.querySelector(".badge.warn"))).toBe(true);
  });

  it("switches the history range with pressed-state buttons and totals the range by model", async () => {
    await renderPage();
    expect(rangeButton(30).getAttribute("aria-pressed")).toBe("true");
    expect(rangeButton(7).getAttribute("aria-pressed")).toBe("false");
    const range = () => container.querySelector('[data-testid="usage-range-models"]')!;
    expect(range().textContent).toContain("keyUsage.rangeModelsTitle 30");
    expect(range().querySelector("tbody tr")?.textContent).toContain("$12.00");

    await act(async () => { rangeButton(90).click(); await tick(); });
    expect(fetchKeyHistory).toHaveBeenLastCalledWith("k1", 90);
    expect(rangeButton(90).getAttribute("aria-pressed")).toBe("true");
    expect(container.querySelectorAll(".usage-chart-bar")).toHaveLength(90);
    const rows = range().querySelectorAll("tbody tr");
    expect(rows[0].textContent).toContain("fast");
    expect(rows[0].textContent).toContain("$36.00");
    expect(rows[0].textContent).toContain("2,700");
    expect(rows[0].textContent).toContain("25%");
    expect(rows[1].textContent).toContain("gpt-6-terra");
    expect(rows[1].querySelector(".badge.warn")?.getAttribute("title")).toBe("usage.unpricedCalls 90");

    await act(async () => { rangeButton(7).click(); await tick(); });
    expect(range().textContent).toContain("keyUsage.rangeModelsTitle 7");
    expect(container.querySelectorAll(".usage-chart-bar")).toHaveLength(7);
  });

  it("keeps the page usable when only the history request fails", async () => {
    vi.mocked(fetchKeyHistory).mockRejectedValueOnce(new Error("history offline"));
    await renderPage();
    expect(container.querySelector('[data-testid="usage-history-chart"] [role="alert"]')?.textContent).toBe("history offline");
    expect(container.textContent).toContain("gpt-6-terra");
    await act(async () => { rangeButton(7).click(); await tick(); });
    expect(container.querySelector('[data-testid="usage-history-chart"] [role="alert"]')).toBeNull();
    expect(container.querySelectorAll(".usage-chart-bar")).toHaveLength(7);
  });
});
