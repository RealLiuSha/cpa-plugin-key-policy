import { act } from "react";
import { AxiosError, AxiosHeaders, type AxiosResponse } from "axios";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { createRoot } from "react-dom/client";
import { MemoryRouter, Route, Routes, useLocation, useParams } from "react-router-dom";
import type { StatsReport, StatsTotals } from "../types";

(globalThis as unknown as { IS_REACT_ACT_ENVIRONMENT?: boolean }).IS_REACT_ACT_ENVIRONMENT = true;

vi.mock("../api/stats", async (importOriginal) => ({
  ...(await importOriginal<typeof import("../api/stats")>()),
  fetchStats: vi.fn(),
  exportStatsCSV: vi.fn(),
}));
vi.mock("../api/status", () => ({ fetchStatus: vi.fn() }));
const { translate } = vi.hoisted(() => ({ translate: (key: string, vars?: Record<string, string | number>) => key + (vars ? ` ${Object.values(vars).join(" ")}` : "") }));
vi.mock("../i18n", () => ({ useT: () => translate }));

import { exportStatsCSV, fetchStats } from "../api/stats";
import { fetchStatus } from "../api/status";
import Stats from "./Stats";
import { SEARCH_DEBOUNCE_MS } from "../search";

const tick = () => new Promise((resolve) => setTimeout(resolve, 0));

const zero: StatsTotals = { charged_usd: 0, calls: 0, unpriced_calls: 0, input_tokens: 0, cache_read_tokens: 0, cache_write_tokens: 0, output_tokens: 0 };

function report(days: number, overrides: Partial<StatsReport> = {}): StatsReport {
  return {
    timezone: "Asia/Shanghai",
    from: "2026-09-18",
    to: "2026-10-17",
    totals: { charged_usd: 123.456789, calls: 9400, unpriced_calls: 12, input_tokens: 3000, cache_read_tokens: 1000, cache_write_tokens: 0, output_tokens: 500 },
    days: Array.from({ length: days }, (_, index) => ({ ...zero, date: `2026-10-${String(index + 1).padStart(2, "0")}`, charged_usd: index === 0 ? 0.004 : index === 1 ? 40 : 0, calls: 1 })),
    models: [
      { ...zero, name: "gpt-6-sol", priced: true, charged_usd: 120, calls: 9000, input_tokens: 3000, cache_read_tokens: 1000 },
      { ...zero, name: "gpt-6-terra", priced: false, calls: 12, unpriced_calls: 12 },
    ],
    keys: [
      { ...zero, id: "ak-1", name: "Alice", deleted: false, charged_usd: 100, calls: 9000 },
      { ...zero, id: "ak-gone", name: "", deleted: true, charged_usd: 23, calls: 400 },
    ],
    keys_total: 2,
    keys_truncated: false,
    unpriced_models: [{ name: "gpt-6-terra", calls: 12, last_seen: "2026-10-16" }],
    ...overrides,
  };
}

function UsageMarker() {
  const state = useLocation().state as { keyListReturnTo?: string } | null;
  return <div data-testid="usage-page" data-return-to={state?.keyListReturnTo}>{useParams().id}</div>;
}

describe("Stats page", () => {
  let container: HTMLDivElement;
  let root: ReturnType<typeof createRoot>;

  beforeEach(() => {
    container = document.createElement("div");
    document.body.appendChild(container);
    vi.mocked(fetchStats).mockImplementation(async (days) => report(days));
    vi.mocked(fetchStatus).mockResolvedValue({
      enabled: true, state_file: "/data/state.json", key_count: 2,
      health: {
        last_flush_at: "2026-10-17T01:00:00Z", last_flush_error: "disk full", flush_failures: 3, skipped_failed: 0,
        ignored_unknown_key: 0, ignored_no_model: 0, ignored_empty_usage: 0, clamped_tokens: 0, invalid_usage: 0,
        unmetered_per_call: 2, unpriced_calls: 0,
      },
    });
  });

  afterEach(() => {
    vi.useRealTimers();
    act(() => root.unmount());
    container.remove();
    vi.clearAllMocks();
  });

  async function renderPage(path = "/stats") {
    await act(async () => {
      root = createRoot(container);
      root.render(
        <MemoryRouter initialEntries={[path]}>
          <Routes>
            <Route path="/stats" element={<Stats />} />
            <Route path="/keys/:id/usage" element={<UsageMarker />} />
          </Routes>
        </MemoryRouter>,
      );
      await tick();
      await tick();
    });
  }

  const button = (text: string) => [...container.querySelectorAll<HTMLButtonElement>("button")].find((item) => item.textContent === text)!;
  const section = (id: string) => container.querySelector(`[data-testid="${id}"]`)!;

  it("renders KPIs, the daily trend and the model and key tables of the default 30 days", async () => {
    await renderPage();
    expect(fetchStats).toHaveBeenCalledWith(30);
    expect(button("usage.rangeDays 30").getAttribute("aria-pressed")).toBe("true");
    const kpis = section("stats-kpis").textContent;
    for (const value of ["$123.46", "9,400", "3,000", "500", "25%", "12"]) expect(kpis).toContain(value);

    const bars = [...container.querySelectorAll<SVGRectElement>(".usage-chart-bar")];
    expect(bars).toHaveLength(30);
    expect(Number(bars[0].getAttribute("height"))).toBeGreaterThanOrEqual(2);
    expect(Number(bars[2].getAttribute("height"))).toBe(0);
    expect(section("stats-trend").textContent).toContain("stats.period 2026-09-18 2026-10-17 Asia/Shanghai");

    const models = [...section("stats-models").querySelectorAll("tbody tr")];
    expect(models[0].textContent).toContain("gpt-6-sol");
    expect(models[0].querySelector(".badge.warn")).toBeNull();
    expect(models[1].querySelector(".badge.warn")?.getAttribute("title")).toBe("usage.unpricedCalls 12");

    const notice = section("stats-unpriced");
    expect(notice.textContent).toContain("gpt-6-terra");
    expect(notice.querySelector("a")?.getAttribute("href")).toBe("/models");
    expect(section("stats-unpriced-qualified")).toBeNull();
    expect(section("stats-keys").querySelector('[role="note"]')).toBeNull();

    const [alice, gone] = [...section("stats-keys").querySelectorAll("tbody tr")];
    expect(gone.querySelector("a")).toBeNull();
    expect(gone.textContent).toContain("ak-gone");
    expect(gone.textContent).toContain("stats.deleted");
    const link = alice.querySelector("a")!;
    expect(link.textContent).toBe("Alice");
    await act(async () => { link.dispatchEvent(new MouseEvent("click", { bubbles: true, cancelable: true, button: 0 })); });
    expect(section("usage-page").textContent).toBe("ak-1");
    expect(section("usage-page").getAttribute("data-return-to")).toBe("/stats?days=30");
  });

  it("reads the range from the URL and sends a key's usage page back to the range on screen", async () => {
    await renderPage("/stats?days=90");
    expect(fetchStats).toHaveBeenCalledWith(90);
    expect(button("usage.rangeDays 90").getAttribute("aria-pressed")).toBe("true");
    await act(async () => { button("usage.rangeDays 7").click(); await tick(); });
    expect(fetchStats).toHaveBeenLastCalledWith(7);
    const link = section("stats-keys").querySelector("a")!;
    await act(async () => { link.dispatchEvent(new MouseEvent("click", { bubbles: true, cancelable: true, button: 0 })); });
    expect(section("usage-page").getAttribute("data-return-to")).toBe("/stats?days=7");
  });

  it("explains unpriced models whose provider-qualified rows missed their calls", async () => {
    vi.mocked(fetchStats).mockImplementation(async (days) => report(days, {
      unpriced_models: [
        { name: "gpt-6-terra", calls: 12, last_seen: "2026-10-16", has_qualified_rows: true },
        { name: "mystery", calls: 3, last_seen: "2026-10-01" },
      ],
    }));
    await renderPage();
    const qualified = section("stats-unpriced-qualified");
    expect(qualified.querySelector(".mono")?.textContent).toBe("gpt-6-terra");
    expect(qualified.textContent).toContain("usage.unpricedQualified");
  });

  it("reloads on a range switch and says when the key list is truncated", async () => {
    vi.mocked(fetchStats).mockImplementation(async (days) => report(days, days === 90 ? {
      keys: Array.from({ length: 200 }, (_, index) => ({ ...zero, id: `ak-${index}`, name: "", deleted: false, charged_usd: 1, calls: 1 })),
      keys_total: 223,
      keys_truncated: true,
    } : {}));
    await renderPage();
    await act(async () => { button("usage.rangeDays 90").click(); await tick(); });
    expect(fetchStats).toHaveBeenLastCalledWith(90);
    expect(button("usage.rangeDays 90").getAttribute("aria-pressed")).toBe("true");
    expect(container.querySelectorAll(".usage-chart-bar")).toHaveLength(90);
    expect(section("stats-keys").querySelector('[role="note"]')?.textContent).toBe("stats.keysTruncated 200 223");
    expect(section("stats-keys").querySelectorAll("tbody tr")).toHaveLength(200);
  });

  it("searches the loaded keys, keeps the search across a range switch and returns to it", async () => {
    await renderPage("/stats?days=7");
    const box = () => section("stats-keys").querySelector<HTMLInputElement>('input[type="search"]')!;
    const rows = () => [...section("stats-keys").querySelectorAll("tbody tr")];
    const type = async (value: string) => {
      vi.useFakeTimers();
      await act(async () => {
        Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, "value")!.set!.call(box(), value);
        box().dispatchEvent(new Event("input", { bubbles: true }));
      });
      await act(async () => { await vi.advanceTimersByTimeAsync(SEARCH_DEBOUNCE_MS); });
      vi.useRealTimers();
    };

    await type("ALI");
    expect(rows()).toHaveLength(1);
    expect(rows()[0].textContent).toContain("Alice");
    await act(async () => { button("usage.rangeDays 30").click(); await tick(); });
    expect(fetchStats).toHaveBeenLastCalledWith(30);
    expect(box().value).toBe("ALI");
    expect(rows()).toHaveLength(1);

    await type("nobody");
    expect(rows()).toHaveLength(1);
    expect(rows()[0].textContent).toContain("keys.searchNoMatch");
    await act(async () => { button("quota.clearFilters").click(); await tick(); });
    expect(box().value).toBe("");
    expect(rows()).toHaveLength(2);

    await type("gone");
    expect(rows()[0].textContent).toContain("ak-gone");
    await type("ali");
    const link = rows()[0].querySelector("a")!;
    await act(async () => { link.dispatchEvent(new MouseEvent("click", { bubbles: true, cancelable: true, button: 0 })); });
    expect(section("usage-page").getAttribute("data-return-to")).toBe("/stats?days=30&q=ali");
  });

  it("shows an empty state without chart or tables and keeps export off", async () => {
    vi.mocked(fetchStats).mockResolvedValue(report(7, { totals: zero, models: [], keys: [], keys_total: 0, unpriced_models: [] }));
    await renderPage();
    expect(section("stats-empty").textContent).toBe("stats.empty");
    expect(section("stats-trend")).toBeNull();
    expect(section("stats-models")).toBeNull();
    expect(section("stats-unpriced")).toBeNull();
    expect(button("stats.export").disabled).toBe(true);
  });

  it("shows a load failure with a working retry", async () => {
    vi.mocked(fetchStats).mockRejectedValueOnce(new Error("stats offline"));
    await renderPage();
    expect(container.querySelector('[role="alert"]')?.textContent).toContain("stats offline");
    await act(async () => { button("stats.retry").click(); await tick(); });
    expect(container.querySelector('[role="alert"]')).toBeNull();
    expect(section("stats-kpis")).not.toBeNull();
  });

  it("exports the loaded range as a file, disables the button meanwhile and explains a 413", async () => {
    // jsdom has no object URLs.
    const createObjectURL = vi.fn(() => "blob:stats");
    Object.assign(URL, { createObjectURL, revokeObjectURL: vi.fn() });
    const click = vi.spyOn(HTMLAnchorElement.prototype, "click").mockImplementation(() => {});
    let finish!: (value: { blob: Blob; filename: string }) => void;
    vi.mocked(exportStatsCSV).mockReturnValueOnce(new Promise((resolve) => { finish = resolve; }));
    await renderPage();

    await act(async () => { button("stats.export").click(); });
    expect(exportStatsCSV).toHaveBeenCalledWith("2026-09-18", "2026-10-17");
    expect(button("stats.exporting").disabled).toBe(true);
    const blob = new Blob(["date\n"], { type: "text/csv" });
    await act(async () => { finish({ blob, filename: "cpa-key-policy-usage-2026-09-18_2026-10-17.csv" }); await tick(); });
    expect(createObjectURL).toHaveBeenCalledWith(blob);
    expect(click).toHaveBeenCalledTimes(1);
    expect((click.mock.contexts[0] as HTMLAnchorElement).download).toBe("cpa-key-policy-usage-2026-09-18_2026-10-17.csv");
    expect(button("stats.export").disabled).toBe(false);

    const response = { status: 413, data: { error: { code: "range_too_large", message: "too many rows" } }, headers: {}, statusText: "", config: { headers: new AxiosHeaders() } } as AxiosResponse;
    vi.mocked(exportStatsCSV).mockRejectedValueOnce(new AxiosError("Request failed with status code 413", "ERR_BAD_REQUEST", undefined, undefined, response));
    await act(async () => { button("stats.export").click(); await tick(); });
    expect(container.querySelector('[role="alert"]')?.textContent).toBe("stats.exportTooLarge");
    expect(click).toHaveBeenCalledTimes(1);
    click.mockRestore();
  });

  it("collapses ledger health, flags a flush error and mutes zero counters", async () => {
    await renderPage();
    const health = section("stats-health") as HTMLDetailsElement;
    expect(health.open).toBe(false);
    expect(health.querySelector("summary .badge.danger")?.textContent).toBe("stats.health.flushError");
    expect(section("stats-flush-error").textContent).toContain("disk full");
    expect(health.querySelector('[data-field="flush_failures"]')?.className).toBe("");
    expect(health.querySelector('[data-field="unmetered_per_call"]')?.className).toBe("");
    expect(health.querySelector('[data-field="skipped_failed"]')?.className).toBe("zero");
  });
});
