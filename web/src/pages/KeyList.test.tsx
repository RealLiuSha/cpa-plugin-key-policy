import { act } from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { Simulate } from "react-dom/test-utils";

(globalThis as unknown as { IS_REACT_ACT_ENVIRONMENT?: boolean }).IS_REACT_ACT_ENVIRONMENT = true;

import { createRoot } from "react-dom/client";
import { MemoryRouter } from "react-router-dom";
import type { KeyPublic } from "../types";

vi.mock("../api/keys", () => ({
  listKeys: vi.fn(),
  deleteKey: vi.fn(),
  rotateKey: vi.fn(),
  resetRPM: vi.fn(),
  resetUsage: vi.fn(),
}));

const { translate } = vi.hoisted(() => ({
  translate: (key: string, vars?: Record<string, string | number>) => {
    if (key === "keys.lastUpdated") return `${key} ${vars?.time ?? "—"}`;
    if (key === "usage.windowHelp") return `${key} ${vars?.timezone ?? ""}`;
    return key;
  },
}));

vi.mock("../i18n", () => ({
  useT: () => translate,
}));

import KeyList, {
  KEY_LIST_PAGE_SIZE,
  filterKeys,
  formatResetCountdown,
  isAnyLimitHit,
  isLimitHit,
  paginateKeys,
  sortKeys,
} from "./KeyList";
import { deleteKey, listKeys, resetRPM, resetUsage, rotateKey } from "../api/keys";

const baseUsage = {
  daily_usd: 1,
  weekly_usd: 2,
  monthly_usd: 3,
  daily_limit_usd: 10,
  weekly_limit_usd: 50,
  monthly_limit_usd: 100,
  daily_reset_at: "2099-08-09T00:00:00+08:00",
  timezone: "Asia/Shanghai",
};

const key: KeyPublic = {
  id: "team-a",
  name: "Team A",
  enabled: true,
  key_preview: "cpa_te...am-a",
  rpm: 60,
  models: [
    { alias: "gpt-4o", provider: "openai", target_model: "gpt-4o" },
    { alias: "claude", provider: "anthropic", target_model: "claude-3" },
  ],
  daily_limit_usd: 10,
  weekly_limit_usd: 50,
  usage: {
    daily_usd: 8,
    weekly_usd: 20,
    monthly_usd: 30,
    daily_limit_usd: 10,
    weekly_limit_usd: 50,
    monthly_limit_usd: 100,
    daily_reset_at: "2099-08-09T00:00:00+08:00",
    timezone: "Asia/Shanghai",
  },
};

function makeKey(id: string, overrides: Partial<KeyPublic> = {}): KeyPublic {
  return {
    id,
    name: overrides.name ?? id,
    enabled: true,
    key_preview: overrides.key_preview ?? `cpa_${id.slice(0, 4)}...`,
    rpm: 0,
    models: overrides.models ?? [],
    daily_limit_usd: 0,
    weekly_limit_usd: 0,
    usage: baseUsage,
    ...overrides,
  };
}

const tick = () => new Promise((resolve) => setTimeout(resolve, 0));

describe("isLimitHit / isAnyLimitHit", () => {
  it("treats limit 0 as unlimited (never hit)", () => {
    expect(isLimitHit(100, 0)).toBe(false);
    expect(isLimitHit(0, 0)).toBe(false);
  });

  it("hits when used >= positive limit", () => {
    expect(isLimitHit(10, 10)).toBe(true);
    expect(isLimitHit(11, 10)).toBe(true);
    expect(isLimitHit(9.99, 10)).toBe(false);
  });

  it("any-hit is true when daily, weekly, or monthly is at limit", () => {
    expect(isAnyLimitHit({
      daily_usd: 10, weekly_usd: 1, daily_limit_usd: 10, weekly_limit_usd: 50,
    })).toBe(true);
    expect(isAnyLimitHit({
      daily_usd: 1, weekly_usd: 50, daily_limit_usd: 10, weekly_limit_usd: 50,
    })).toBe(true);
    expect(isAnyLimitHit({
      daily_usd: 1, weekly_usd: 2, daily_limit_usd: 10, weekly_limit_usd: 50,
    })).toBe(false);
    expect(isAnyLimitHit({
      daily_usd: 1, weekly_usd: 2, monthly_usd: 30,
      daily_limit_usd: 10, weekly_limit_usd: 50, monthly_limit_usd: 30,
    })).toBe(true);
  });
});

describe("sortKeys / reset countdown", () => {
  const a = makeKey("a", { updated_at: "2026-08-01T00:00:00Z", usage: { ...baseUsage, daily_usd: 2 } });
  const b = makeKey("b", { updated_at: "2026-08-02T00:00:00Z", usage: { ...baseUsage, daily_usd: 8, daily_limit_usd: 10 } });
  it("sorts after filtering by daily, ratio, and activity", () => {
    expect(sortKeys([a, b], "daily").map((item) => item.id)).toEqual(["b", "a"]);
    expect(sortKeys([a, b], "ratio").map((item) => item.id)).toEqual(["b", "a"]);
    expect(sortKeys([a, b], "active").map((item) => item.id)).toEqual(["b", "a"]);
  });
  it("formats the next natural-day boundary countdown", () => {
    expect(formatResetCountdown("2026-08-08T01:02:03Z", Date.parse("2026-08-08T00:00:00Z"))).toBe("01:02:03");
  });
});

describe("KeyList table and more menu", () => {
  let container: HTMLDivElement;
  let root: ReturnType<typeof createRoot>;

  beforeEach(() => {
    container = document.createElement("div");
    document.body.appendChild(container);
    (listKeys as ReturnType<typeof vi.fn>).mockResolvedValue([key]);
    (rotateKey as ReturnType<typeof vi.fn>).mockResolvedValue({
      plain_key: "cpa_new_plain_secret",
      key,
    });
    (deleteKey as ReturnType<typeof vi.fn>).mockResolvedValue(undefined);
    (resetRPM as ReturnType<typeof vi.fn>).mockResolvedValue(undefined);
    (resetUsage as ReturnType<typeof vi.fn>).mockResolvedValue(undefined);
    vi.stubGlobal("confirm", vi.fn(() => true));
  });

  afterEach(() => {
    act(() => root.unmount());
    container.remove();
    vi.unstubAllGlobals();
    vi.clearAllMocks();
  });

  const renderList = async () => {
    await act(async () => {
      root = createRoot(container);
      root.render(
        <MemoryRouter>
          <KeyList />
        </MemoryRouter>,
      );
      await tick();
    });
  };

  const menuButton = (label: string) =>
    Array.from(container.querySelectorAll<HTMLButtonElement>(".more-menu button")).find(
      (button) => button.textContent === label,
    );

  const openMoreMenu = async () => {
    const details = container.querySelector<HTMLDetailsElement>(".key-list-table .more-menu");
    if (!details) throw new Error("more menu not found");
    await act(async () => {
      details.open = true;
      details.dispatchEvent(new Event("toggle"));
      await tick();
    });
    return details;
  };

  it("renders a desktop table with required columns and cell content", async () => {
    await renderList();

    const table = container.querySelector(".key-list-table table");
    expect(table).not.toBeNull();

    const headers = Array.from(table!.querySelectorAll("thead th")).map((th) => th.textContent);
    expect(headers).toEqual([
      "keys.colIdName",
      "keys.colStatus",
      "keys.colPreview",
      "keys.colRpm",
      "keys.colUsage · Asia/Shanghai",
      "keys.colModels",
      "keys.colAliases",
      "keys.colActions",
    ]);

    const row = table!.querySelector("tbody tr");
    expect(row).not.toBeNull();
    expect(row!.textContent).toContain("team-a");
    expect(row!.textContent).toContain("Team A");
    expect(row!.textContent).toContain("keys.enabled");
    expect(row!.textContent).toContain("cpa_te...am-a");
    expect(row!.textContent).toContain("60");
    expect(row!.textContent).toContain("gpt-4o");
    expect(row!.textContent).toContain("claude");
    expect(row!.textContent).toContain("usage.last7Days");
    expect(row!.textContent).toContain("usage.nextReset");
    expect(table!.querySelectorAll("thead th")[4].getAttribute("title")).toBe("usage.windowHelp Asia/Shanghai");
    expect(container.querySelector('[data-testid="last-updated-desktop"]')?.textContent).not.toContain("—");
  });

  it("marks daily and/or weekly usage red when either limit is hit", async () => {
    const dailyHit = makeKey("daily-hit", {
      usage: {
        daily_usd: 10,
        weekly_usd: 5,
        daily_limit_usd: 10,
        weekly_limit_usd: 50,
      },
    });
    const weeklyHit = makeKey("weekly-hit", {
      usage: {
        daily_usd: 1,
        weekly_usd: 50,
        daily_limit_usd: 10,
        weekly_limit_usd: 50,
      },
    });
    const ok = makeKey("ok-key", {
      usage: {
        daily_usd: 1,
        weekly_usd: 2,
        daily_limit_usd: 10,
        weekly_limit_usd: 50,
      },
    });
    (listKeys as ReturnType<typeof vi.fn>).mockResolvedValue([dailyHit, weeklyHit, ok]);
    await renderList();

    const dailyLine = container.querySelector('[data-testid="usage-daily-daily-hit"]');
    const weeklyLineOnDailyHit = container.querySelector('[data-testid="usage-weekly-daily-hit"]');
    expect(dailyLine?.classList.contains("over")).toBe(true);
    expect(weeklyLineOnDailyHit?.classList.contains("over")).toBe(false);
    expect(container.querySelector('[data-testid="usage-cell-daily-hit"]')?.classList.contains("usage-over")).toBe(true);

    const weeklyLine = container.querySelector('[data-testid="usage-weekly-weekly-hit"]');
    const dailyLineOnWeeklyHit = container.querySelector('[data-testid="usage-daily-weekly-hit"]');
    expect(weeklyLine?.classList.contains("over")).toBe(true);
    expect(dailyLineOnWeeklyHit?.classList.contains("over")).toBe(false);
    expect(container.querySelector('[data-testid="usage-cell-weekly-hit"]')?.classList.contains("usage-over")).toBe(true);

    expect(container.querySelector('[data-testid="usage-daily-ok-key"]')?.classList.contains("over")).toBe(false);
    expect(container.querySelector('[data-testid="usage-weekly-ok-key"]')?.classList.contains("over")).toBe(false);
    expect(container.querySelector('[data-testid="usage-cell-ok-key"]')?.classList.contains("usage-over")).toBe(false);

    // Mobile cards: over class when either limit hit.
    expect(container.querySelector('[data-testid="keycard-daily-hit"]')?.classList.contains("over")).toBe(true);
    expect(container.querySelector('[data-testid="keycard-weekly-hit"]')?.classList.contains("over")).toBe(true);
    expect(container.querySelector('[data-testid="keycard-ok-key"]')?.classList.contains("over")).toBe(false);
  });

  it("visually distinguishes disabled, quota-blocked, and soft-warning states", async () => {
    const disabled = makeKey("disabled", { enabled: false });
    const limited = makeKey("limited", { usage: { ...baseUsage, daily_usd: 10, daily_limit_usd: 10 } });
    const warning = makeKey("warning", { usage: { ...baseUsage, soft_limit_hit: true } });
    (listKeys as ReturnType<typeof vi.fn>).mockResolvedValue([disabled, limited, warning]);
    await renderList();
    expect(container.querySelector('[data-testid="key-state-disabled-disabled"]')).not.toBeNull();
    expect(container.querySelector('[data-testid="key-state-limited-limited"]')).not.toBeNull();
    expect(container.querySelector('[data-testid="key-state-warning-warning"]')).not.toBeNull();
    expect(container.querySelector('[data-testid="keycard-disabled"]')?.getAttribute("data-state")).toBe("disabled");
    expect(container.querySelector('[data-testid="keycard-limited"]')?.getAttribute("data-state")).toBe("limited");
    expect(container.querySelector('[data-testid="keycard-warning"]')?.getAttribute("data-state")).toBe("warning");
    const { readFileSync } = await import("node:fs");
    const { resolve } = await import("node:path");
    const css = readFileSync(resolve(__dirname, "../styles.css"), "utf8");
    expect(css).toMatch(/\.tag\.state-disabled\s*\{[^}]*color:\s*var\(--muted\)/s);
    expect(css).toMatch(/\.tag\.state-limited\s*\{[^}]*color:\s*var\(--danger\)/s);
    expect(css).toMatch(/\.tag\.state-warning,[\s\S]*?color:\s*#9a6700/s);
    expect(css).toMatch(/\.keycard\.disabled \.kc-dot\s*\{[^}]*var\(--muted\)/s);
    expect(css).toMatch(/\.keycard\.over \.kc-dot\s*\{[^}]*var\(--danger\)/s);
  });

  it("offers a mobile refresh and shows the successful refresh time", async () => {
    await renderList();
    const mobileRefresh = container.querySelector<HTMLButtonElement>(".key-list-mobile-refresh button");
    expect(mobileRefresh).not.toBeNull();
    expect(container.querySelector('[data-testid="last-updated-mobile"]')?.textContent).not.toContain("—");
    await act(async () => {
      mobileRefresh!.click();
      await tick();
    });
    expect(listKeys).toHaveBeenCalledTimes(2);
  });

  it("shows rolling-window wording and countdown for an unlimited mobile key", async () => {
    (listKeys as ReturnType<typeof vi.fn>).mockResolvedValue([makeKey("unlimited", {
      usage: { ...baseUsage, daily_limit_usd: 0, weekly_limit_usd: 0, monthly_limit_usd: 0 },
    })]);
    await renderList();
    const card = container.querySelector('[data-testid="keycard-unlimited"]');
    expect(card?.textContent).toContain("usage.last7Days");
    expect(card?.textContent).toContain("usage.nextReset");
  });

  it("exposes edit and detail links with correct routes", async () => {
    await renderList();

    const edit = container.querySelector<HTMLAnchorElement>(
      '.key-list-table a[href="/keys/team-a/edit"]',
    );
    const detail = container.querySelector<HTMLAnchorElement>(
      '.key-list-table a[href="/keys/team-a/usage"]',
    );
    expect(edit).not.toBeNull();
    expect(edit?.textContent).toBe("keys.edit");
    expect(detail).not.toBeNull();
    expect(detail?.textContent).toBe("keys.detail");
  });

  it("lists more-menu items in fixed order including danger delete", async () => {
    await renderList();

    const details = container.querySelector<HTMLDetailsElement>(".key-list-table .more-menu");
    expect(details).not.toBeNull();
    expect(details?.querySelector("summary")?.textContent).toContain("keys.more");

    const labels = Array.from(details!.querySelectorAll<HTMLButtonElement>("button")).map(
      (b) => b.textContent,
    );
    expect(labels).toEqual([
      "keys.resetDaily",
      "keys.resetWeekly",
      "keys.resetMonthly",
      "keys.resetRpm",
      "keys.resetKey",
      "keys.delete",
    ]);
    const deleteBtn = menuButton("keys.delete");
    expect(deleteBtn?.classList.contains("danger")).toBe(true);
  });

  it("dispatches daily, weekly, monthly, and RPM resets with correct confirm/API paths", async () => {
    await renderList();

    await act(async () => {
      menuButton("keys.resetDaily")?.dispatchEvent(new MouseEvent("click", { bubbles: true }));
      await tick();
    });
    expect(confirm).toHaveBeenCalledWith("keys.resetDailyConfirm");
    expect(resetUsage).toHaveBeenCalledWith("team-a", "daily");

    await act(async () => {
      menuButton("keys.resetWeekly")?.dispatchEvent(new MouseEvent("click", { bubbles: true }));
      await tick();
    });
    expect(confirm).toHaveBeenCalledWith("keys.resetWeeklyConfirm");
    expect(resetUsage).toHaveBeenCalledWith("team-a", "weekly");

    await act(async () => {
      menuButton("keys.resetMonthly")?.dispatchEvent(new MouseEvent("click", { bubbles: true }));
      await tick();
    });
    expect(confirm).toHaveBeenCalledWith("keys.resetMonthlyConfirm");
    expect(resetUsage).toHaveBeenCalledWith("team-a", "monthly");

    await act(async () => {
      menuButton("keys.resetRpm")?.dispatchEvent(new MouseEvent("click", { bubbles: true }));
      await tick();
    });
    expect(resetRPM).toHaveBeenCalledWith("team-a");
    // RPM has no confirm; only usage-window resets confirmed above.
    expect(confirm).toHaveBeenCalledTimes(3);
  });

  it("rotates with confirm, API call, and one-time plain key modal", async () => {
    await renderList();

    await act(async () => {
      menuButton("keys.resetKey")?.dispatchEvent(new MouseEvent("click", { bubbles: true }));
      await tick();
    });

    expect(confirm).toHaveBeenCalledWith("keys.rotateConfirm");
    expect(rotateKey).toHaveBeenCalledWith("team-a");
    expect(container.textContent).toContain("cpa_new_plain_secret");
    expect(container.querySelector(".modal")).not.toBeNull();
  });

  it("deletes with confirm and deleteKey API", async () => {
    await renderList();

    await act(async () => {
      menuButton("keys.delete")?.dispatchEvent(new MouseEvent("click", { bubbles: true }));
      await tick();
    });

    expect(confirm).toHaveBeenCalledWith("keys.deleteConfirm");
    expect(deleteKey).toHaveBeenCalledWith("team-a");
  });

  it("does not call API when confirmation is cancelled", async () => {
    vi.mocked(confirm).mockReturnValue(false);
    await renderList();

    await act(async () => {
      menuButton("keys.resetWeekly")?.dispatchEvent(new MouseEvent("click", { bubbles: true }));
      await tick();
    });
    expect(resetUsage).not.toHaveBeenCalled();

    await act(async () => {
      menuButton("keys.resetKey")?.dispatchEvent(new MouseEvent("click", { bubbles: true }));
      await tick();
    });
    expect(rotateKey).not.toHaveBeenCalled();

    await act(async () => {
      menuButton("keys.delete")?.dispatchEvent(new MouseEvent("click", { bubbles: true }));
      await tick();
    });
    expect(deleteKey).not.toHaveBeenCalled();
  });

  it("closes the more menu on outside pointer press and Escape", async () => {
    await renderList();

    const details = await openMoreMenu();
    expect(details.open).toBe(true);

    await act(async () => {
      document.body.dispatchEvent(new Event("pointerdown", { bubbles: true }));
      await tick();
    });
    expect(details.open).toBe(false);

    await openMoreMenu();
    await act(async () => {
      document.dispatchEvent(new KeyboardEvent("keydown", { key: "Escape", bubbles: true }));
      await tick();
    });
    expect(details.open).toBe(false);
    expect(document.activeElement).toBe(details.querySelector("summary"));
  });

  it("does not navigate when the table row body is clicked", async () => {
    await renderList();

    const row = container.querySelector(".key-list-table tbody tr");
    expect(row).not.toBeNull();
    // No onClick / role=link / data-nav on the row — only explicit action links.
    expect(row!.getAttribute("onclick")).toBeNull();
    expect(row!.getAttribute("role")).not.toBe("link");
    const rowLinks = row!.querySelectorAll("a");
    // Only the action column links, not a whole-row wrapper.
    expect(rowLinks.length).toBe(2);
    expect(Array.from(rowLinks).every((a) => a.closest(".key-actions"))).toBe(true);
  });

  it("mobile cards expose edit/detail/more without swipe revoke or whole-card nav", async () => {
    await renderList();

    const stack = container.querySelector(".key-list-cards.mobile-only");
    expect(stack).not.toBeNull();
    const card = stack!.querySelector(".keycard");
    expect(card).not.toBeNull();
    expect(card!.querySelector(".kc-revoke")).toBeNull();
    expect(card!.querySelector(".kc-actions .more-menu")).not.toBeNull();

    const edit = card!.querySelector('a[href="/keys/team-a/edit"]');
    const detail = card!.querySelector('a[href="/keys/team-a/usage"]');
    expect(edit).not.toBeNull();
    expect(detail).not.toBeNull();

    // Card itself is not a navigation control.
    expect(card!.getAttribute("role")).not.toBe("link");
    expect(card!.getAttribute("onclick")).toBeNull();
  });

  it("styles keep mobile card stack as flex+gap so spacing survives .mobile-only block", async () => {
    // Structural guard: .mobile-only { display:block !important } would zero out
    // flex gap unless .key-list-cards.mobile-only reasserts flex with !important.
    const { readFileSync } = await import("node:fs");
    const { resolve } = await import("node:path");
    const css = readFileSync(resolve(__dirname, "../styles.css"), "utf8");
    expect(css).toMatch(/\.key-list-cards\.mobile-only\s*\{[^}]*display:\s*flex\s*!important/s);
    expect(css).toMatch(/\.key-list-cards\.mobile-only\s*\{[^}]*gap:\s*10px/s);
    expect(css).toMatch(/\.key-list-cards\s*\{[^}]*display:\s*flex/s);
    expect(css).not.toMatch(
      /@media\s*\(max-width:\s*640px\)[\s\S]*?\.key-list-cards\s*\{\s*display:\s*block/,
    );
  });

  it("does not create ancestor stacking contexts that trap the fixed more-menu", async () => {
    const { readFileSync } = await import("node:fs");
    const { resolve } = await import("node:path");
    const css = readFileSync(resolve(__dirname, "../styles.css"), "utf8");
    // Regression: z-index on .kc-actions or .keycard.menu-open re-traps fixed
    // panels under tabbar (z-index 100). Assert neither exists as a property.
    const actionsBlock = css.match(/\.keycard \.kc-actions\s*\{[^}]*\}/s)?.[0] ?? "";
    expect(actionsBlock).not.toMatch(/(?<![\w-])z-index\s*:/);
    expect(css).not.toMatch(/\.keycard\.menu-open\s*\{[^}]*z-index\s*:/);
    // Whole-card/row opacity would dim fixed menus and create stacking contexts.
    expect(css).not.toMatch(/\.keycard\.disabled\s*\{[^}]*opacity\s*:/);
    expect(css).not.toMatch(/tr\.row-disabled\s*>\s*td\s*\{[^}]*opacity\s*:/);
    expect(css).toMatch(
      /tr\.row-disabled\s*>\s*td:not\(\.key-actions-cell\)\s*\{[^}]*opacity\s*:/,
    );
    // Narrow desktop: table scrolls horizontally instead of collapsing columns.
    expect(css).toMatch(/\.key-list-table\.card\.table-wrap\s*\{[^}]*overflow-x:\s*auto/s);
    expect(css).toMatch(/\.key-list-table table\s*\{[^}]*min-width:\s*960px/s);
  });

  it("positions the open more-menu panel with fixed so it escapes overflow/stacking", async () => {
    await renderList();
    const details = container.querySelector<HTMLDetailsElement>(".key-list-table .more-menu");
    expect(details).not.toBeNull();
    await act(async () => {
      details!.open = true;
      details!.dispatchEvent(new Event("toggle"));
      await tick();
      await tick();
    });
    const panel = details!.querySelector<HTMLElement>(".more-menu-options");
    expect(panel).not.toBeNull();
    expect(panel!.style.position).toBe("fixed");
    expect(panel!.style.zIndex).toBe("200");
  });

  it("filters and paginates the list client-side (default page size 10)", async () => {
    const many = Array.from({ length: 12 }, (_, i) =>
      makeKey(`key-${String(i + 1).padStart(2, "0")}`, {
        name: i === 0 ? "Alpha Team" : `Key ${i + 1}`,
        models: i === 1 ? [{ alias: "special-model", provider: "p", target_model: "m" }] : [],
      }),
    );
    (listKeys as ReturnType<typeof vi.fn>).mockResolvedValue(many);
    await renderList();

    // Default page: first 10 of 12.
    expect(KEY_LIST_PAGE_SIZE).toBe(10);
    expect(container.querySelectorAll(".key-list-table tbody tr")).toHaveLength(10);
    expect(container.textContent).toContain("key-01");
    expect(container.textContent).not.toContain("key-11");

    const pager = container.querySelector(".key-list-pager");
    expect(pager).not.toBeNull();
    expect(pager!.textContent).toContain("keys.pageInfo");

    const next = Array.from(pager!.querySelectorAll("button")).find(
      (b) => b.textContent === "keys.nextPage",
    );
    expect(next).not.toBeNull();
    await act(async () => {
      next!.dispatchEvent(new MouseEvent("click", { bubbles: true }));
      await tick();
    });
    expect(container.querySelectorAll(".key-list-table tbody tr")).toHaveLength(2);
    expect(container.textContent).toContain("key-11");
    expect(container.textContent).toContain("key-12");
    expect(container.textContent).not.toContain("key-01");

    const search = container.querySelector<HTMLInputElement>(".key-list-search");
    expect(search).not.toBeNull();
    await act(async () => {
      Simulate.change(search!, { target: { value: "Alpha" } } as never);
      await tick();
    });
    expect(container.querySelectorAll(".key-list-table tbody tr")).toHaveLength(1);
    expect(container.textContent).toContain("key-01");
    expect(container.textContent).toContain("Alpha Team");
    // Search collapses to one page — pager hidden.
    expect(container.querySelector(".key-list-pager")).toBeNull();

    await act(async () => {
      Simulate.change(search!, { target: { value: "special-model" } } as never);
      await tick();
    });
    expect(container.querySelectorAll(".key-list-table tbody tr")).toHaveLength(1);
    expect(container.textContent).toContain("key-02");

    await act(async () => {
      Simulate.change(search!, { target: { value: "no-such-key-zzzz" } } as never);
      await tick();
    });
    expect(container.textContent).toContain("keys.searchNoMatch");
    expect(container.querySelector(".key-list-table")).toBeNull();
  });
});

describe("filterKeys and paginateKeys", () => {
  const sample = [
    makeKey("alpha", { name: "Alpha", models: [{ alias: "gpt-4o", provider: "o", target_model: "g" }] }),
    makeKey("beta", { name: "Beta", key_preview: "cpa_be...ta" }),
    makeKey("gamma", { name: "Other" }),
  ];

  it("matches id, name, preview, and model alias", () => {
    expect(filterKeys(sample, "ALP").map((k) => k.id)).toEqual(["alpha"]);
    expect(filterKeys(sample, "beta").map((k) => k.id)).toEqual(["beta"]);
    expect(filterKeys(sample, "cpa_be").map((k) => k.id)).toEqual(["beta"]);
    expect(filterKeys(sample, "gpt-4o").map((k) => k.id)).toEqual(["alpha"]);
    expect(filterKeys(sample, "  ").map((k) => k.id)).toEqual(["alpha", "beta", "gamma"]);
  });

  it("slices pages with the default page size of 10", () => {
    const items = Array.from({ length: 25 }, (_, i) => i);
    expect(paginateKeys(items, 0)).toEqual(items.slice(0, 10));
    expect(paginateKeys(items, 1)).toEqual(items.slice(10, 20));
    expect(paginateKeys(items, 2)).toEqual(items.slice(20, 25));
    expect(KEY_LIST_PAGE_SIZE).toBe(10);
  });
});
