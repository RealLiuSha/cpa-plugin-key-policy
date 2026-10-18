import { act } from "react";
import { createRoot, type Root } from "react-dom/client";
import { Simulate } from "react-dom/test-utils";
import { MemoryRouter, useLocation, useNavigate } from "react-router-dom";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import type { KeyPublic, QuotaWindow } from "../types";

(globalThis as unknown as { IS_REACT_ACT_ENVIRONMENT?: boolean }).IS_REACT_ACT_ENVIRONMENT = true;
vi.mock("../api/keys", () => ({ listKeys: vi.fn(), fetchKeyUsage: vi.fn(), deleteKey: vi.fn(), rotateKey: vi.fn(), resetRPM: vi.fn(), resetUsage: vi.fn(), adjustBalance: vi.fn() }));
const { translate } = vi.hoisted(() => ({ translate: (key: string, vars?: Record<string, string | number>) => key + (vars ? ` ${Object.values(vars).join(" ")}` : "") }));
vi.mock("../i18n", () => ({ useT: () => translate }));
import KeyList, { filterKeys, paginateKeys, sortKeys, KEY_LIST_PAGE_SIZE } from "./KeyList";
import { SEARCH_DEBOUNCE_MS } from "../search";
import { deleteKey, fetchKeyUsage, listKeys, resetRPM, resetUsage, rotateKey } from "../api/keys";

const windows: QuotaWindow[] = ["daily", "weekly", "monthly"];
function makeKey(id: string, overrides: Partial<KeyPublic> = {}): KeyPublic {
  return {
    id, name: `User ${id}`, enabled: true, key_preview: `cpa_${id}...last`, rpm: 60,
    daily_limit_usd: 10, weekly_limit_usd: 50, monthly_limit_usd: 100,
    usage: {
      daily_usd: 1, weekly_usd: 20, monthly_usd: 30, daily_limit_usd: 10, weekly_limit_usd: 50, monthly_limit_usd: 100,
      status: "normal", timezone: "Asia/Shanghai",
      cycles: windows.map((window, i) => ({ window, started_at: "2026-09-12T00:00:00+08:00", resets_at: ["2026-09-13", "2026-09-19", "2026-10-12"][i] + "T00:00:00+08:00", reset_kind: "initial", used_usd: [1, 20, 30][i], limit_usd: [10, 50, 100][i], reset_after_manual_at: ["2026-09-13", "2026-09-19", "2026-10-12"][i] + "T00:00:00+08:00" })),
    },
    ...overrides,
  };
}
const key = makeKey("team-a");
let container: HTMLDivElement;
let root: Root;
const tick = () => new Promise((resolve) => setTimeout(resolve, 0));
// Shows the router location next to the list and can navigate like a back
// button or a link from another page would.
function LocationProbe() {
  const location = useLocation();
  const navigate = useNavigate();
  return <><output data-testid="location">{location.search}</output><button type="button" onClick={() => navigate("/keys?q=team-a")}>probe-navigate</button></>;
}
async function renderList(path = "/keys") { await act(async () => { root.render(<MemoryRouter initialEntries={[path]}><KeyList /><LocationProbe /></MemoryRouter>); await tick(); }); }
const currentSearch = () => new URLSearchParams(container.querySelector('[data-testid="location"]')!.textContent ?? "");
const searchBox = () => container.querySelector<HTMLInputElement>('input[type="search"]')!;
const setNativeValue = Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, "value")!.set!;
async function typeInto(input: HTMLInputElement, value: string) {
  await act(async () => { setNativeValue.call(input, value); input.dispatchEvent(new Event("input", { bubbles: true })); });
}
async function compose(input: HTMLInputElement, type: "compositionstart" | "compositionend", data = "") {
  await act(async () => { input.dispatchEvent(new CompositionEvent(type, { bubbles: true, data })); });
}
async function advance(ms: number) { await act(async () => { await vi.advanceTimersByTimeAsync(ms); }); }
function button(label: string, scope: ParentNode = container): HTMLButtonElement {
  const found = [...scope.querySelectorAll<HTMLButtonElement>("button")].find((item) => item.textContent === label);
  if (!found) throw new Error(`Missing button ${label}`);
  return found;
}
async function click(element: Element) { await act(async () => { element.dispatchEvent(new MouseEvent("click", { bubbles: true })); await tick(); }); }
async function openReset() { await click(button("quota.resetTitle")); return container.querySelector<HTMLElement>('[role="dialog"]')!; }

beforeEach(() => {
  vi.clearAllMocks();
  vi.mocked(listKeys).mockResolvedValue([key]);
  vi.mocked(fetchKeyUsage).mockResolvedValue({ key_id: key.id, key_name: key.name, daily_limit_usd: 10, weekly_limit_usd: 50, monthly_limit_usd: 100, models: [], usage: key.usage });
  vi.mocked(resetUsage).mockResolvedValue({ id: key.id, window: "weekly", next_accounting_boundary_at: "2026-09-19T00:00:00+08:00" });
  vi.mocked(rotateKey).mockResolvedValue({ key, plain_key: "cpa_new_plain", generated: true });
  vi.mocked(resetRPM).mockResolvedValue(undefined);
  vi.mocked(deleteKey).mockResolvedValue(undefined);
  container = document.createElement("div"); document.body.appendChild(container); root = createRoot(container);
});
afterEach(async () => { vi.useRealTimers(); await act(async () => root.unmount()); container.remove(); vi.restoreAllMocks(); });

describe("Key list operations", () => {
  it("renders user identity, remaining balances and separate reset dates in one responsive row", async () => {
    await renderList();
    const row = container.querySelector("article")!;
    expect(container.querySelectorAll("article")).toHaveLength(1);
    expect(row.querySelector(".key-person-name")?.textContent).toBe("User team-a");
    expect(row.textContent).toContain("cpa_team-a...last");
    expect(row.querySelector('[data-testid="quota-weekly"]')?.textContent).toContain("30.00");
    expect(row.querySelector('[data-testid="quota-monthly"]')?.textContent).toContain("70.00");
    expect(row.querySelectorAll(".quota-date")).toHaveLength(3);
    expect(row.textContent).toContain("quota.status.normal");
    expect(container.textContent).toContain("keys.lastUpdated");
  });
  it("uses server states for row styling and lists exhausted keys first", async () => {
    const states = ["disabled", "limited", "warning"] as const;
    vi.mocked(listKeys).mockResolvedValue(states.map((status) => makeKey(status, { usage: { ...key.usage, status } })));
    await renderList();
    for (const state of states) expect(container.querySelector(`article.state-${state}`)).not.toBeNull();
    expect(container.querySelector("article")?.className).toContain("state-limited");
  });
  it("shows prepaid keys with their balance, a balance action and attention order by spent share", async () => {
    const prepaid = (id: string, balance: number, credited: number, status: "normal" | "warning" | "limited") => makeKey(id, {
      quota_mode: "balance", daily_limit_usd: 0, weekly_limit_usd: 0, monthly_limit_usd: 0,
      usage: { ...key.usage, quota_mode: "balance", balance_usd: balance, net_credited_usd: credited, status, blocked_reason: status === "limited" ? "balance_exhausted" : undefined, cycles: key.usage.cycles!.map((cycle) => ({ ...cycle, limit_usd: 0 })) },
    });
    vi.mocked(listKeys).mockResolvedValue([prepaid("healthy", 40, 50, "normal"), key, prepaid("overdrawn", -0.15, 20, "limited"), prepaid("low", 2, 20, "warning")]);
    await renderList();
    const rows = [...container.querySelectorAll("article")];
    // Same status: the higher spent share first (team-a's 30-day quota is 40% used, healthy 20%).
    expect(rows.map((row) => row.getAttribute("aria-label"))).toEqual(["User overdrawn", "User low", "User team-a", "User healthy"]);
    const overdrawn = rows[0];
    expect(overdrawn.querySelector('[data-testid="balance-usage"]')?.textContent).toContain("-$0.15");
    expect(overdrawn.querySelector(".quota-exhausted")).not.toBeNull();
    expect(overdrawn.textContent).toContain("balance.badge");
    expect(overdrawn.textContent).toContain("quota.reason.balance_exhausted");
    expect(() => button("balance.adjust", overdrawn)).not.toThrow();
    expect(() => button("quota.resetTitle", overdrawn)).toThrow();
    expect(rows[2].querySelector('[data-testid="balance-usage"]')).toBeNull();

    // A new router: MemoryRouter only reads its initial entries once.
    await act(async () => root.unmount());
    root = createRoot(container);
    await renderList("/keys?mode=balance");
    expect(container.querySelectorAll("article")).toHaveLength(3);
    expect(container.querySelector(".key-filter-tabs")?.textContent).toContain("quota.filter.limited1");
  });
  it("shows unlimited consumption without an exhausted meter", async () => {
    vi.mocked(listKeys).mockResolvedValue([makeKey("unlimited", { usage: { ...key.usage, cycles: key.usage.cycles!.map((cycle) => ({ ...cycle, limit_usd: 0 })) } })]);
    await renderList();
    expect(container.querySelector("article")?.textContent).toContain("usage.unlimited");
    expect(container.querySelector('[role="meter"]')).toBeNull();
    expect(container.querySelector(".quota-exhausted")).toBeNull();
  });
  it("retains search and status filters in navigation and paginates the matching results", async () => {
    const data = Array.from({ length: 45 }, (_, i) => makeKey(`group-${String(i).padStart(2, "0")}`));
    vi.mocked(listKeys).mockResolvedValue(data);
    await renderList("/keys?q=group&sort=name&page=1");
    expect(container.querySelectorAll("article")).toHaveLength(KEY_LIST_PAGE_SIZE);
    expect(container.querySelector("article")?.textContent).toContain("group-20");
    expect(searchBox().value).toBe("group");
    vi.useFakeTimers();
    await typeInto(searchBox(), "group-44");
    await advance(SEARCH_DEBOUNCE_MS - 1);
    expect(currentSearch().get("q")).toBe("group");
    await advance(1);
    expect(currentSearch().get("q")).toBe("group-44");
    expect(currentSearch().get("sort")).toBe("name");
    expect(currentSearch().has("page")).toBe(false);
    expect(container.querySelectorAll("article")).toHaveLength(1);
    expect(container.querySelector("article")?.textContent).toContain("group-44");
  });
  it("exposes edit and detail links without making the whole row a navigation target", async () => {
    await renderList();
    expect(container.querySelector('a[href="/keys/team-a/edit"]')).not.toBeNull();
    expect(container.querySelectorAll('a[href="/keys/team-a/usage"]')).toHaveLength(2);
    expect(container.querySelector("article")?.getAttribute("role")).not.toBe("link");
  });
  it("previews each reset period and dispatches only the selected quota", async () => {
    await renderList();
    for (const window of windows) {
      const dialog = await openReset();
      expect(fetchKeyUsage).toHaveBeenCalledWith("team-a");
      await act(async () => { Simulate.change(dialog.querySelector<HTMLInputElement>(`input[value="${window}"]`)!); });
      expect(dialog.textContent).toContain("quota.resetHelp");
      expect(dialog.querySelector(".quota-reset-preview")?.textContent).toContain("0.00");
      await click(button("quota.confirmReset", dialog));
      expect(resetUsage).toHaveBeenLastCalledWith("team-a", window, {
        started_at: key.usage.cycles!.find((cycle) => cycle.window === window)!.started_at,
        reset_after_manual_at: key.usage.cycles!.find((cycle) => cycle.window === window)!.reset_after_manual_at,
      });
      expect(dialog.textContent).toContain("quota.resetDone");
      await click(button("quota.done", dialog));
    }
  });
  it("cancels reset without changing data and retains the dialog after a failed reset", async () => {
    await renderList();
    let dialog = await openReset();
    await click(button("keyForm.cancel", dialog));
    expect(resetUsage).not.toHaveBeenCalled();
    vi.mocked(resetUsage).mockRejectedValueOnce(new Error("write failed"));
    dialog = await openReset();
    await click(button("quota.confirmReset", dialog));
    expect(dialog.querySelector('[role="alert"]')?.textContent).toContain("write failed");
    expect(button("quota.confirmReset", dialog).disabled).toBe(false);
    await click(button("quota.confirmReset", dialog));
    expect(dialog.textContent).toContain("quota.resetDone");
  });
  it("resets RPM independently", async () => {
    await renderList(); await click(button("keys.resetRpm"));
    expect(resetRPM).toHaveBeenCalledWith("team-a"); expect(resetUsage).not.toHaveBeenCalled();
  });
  it("refreshes a stale reset preview and waits for a second confirmation", async () => {
    const refreshed = {
      key_id: key.id, key_name: key.name, daily_limit_usd: 10, weekly_limit_usd: 50, monthly_limit_usd: 100, models: [],
      usage: { ...key.usage, cycles: key.usage.cycles!.map((cycle) => ({ ...cycle, started_at: "2026-09-13T10:00:00+08:00", reset_after_manual_at: "2026-10-13T00:00:00+08:00" })) },
    };
    await renderList();
    const dialog = await openReset();
    await act(async () => { Simulate.change(dialog.querySelector<HTMLInputElement>('input[value="monthly"]')!); });
    vi.mocked(fetchKeyUsage).mockResolvedValueOnce(refreshed);
    vi.mocked(resetUsage).mockRejectedValueOnce({ isAxiosError: true, response: { status: 409 } });
    await click(button("quota.confirmReset", dialog));
    expect(resetUsage).toHaveBeenCalledTimes(1);
    expect(dialog.textContent).toContain("quota.previewChanged");
    expect(dialog.querySelector<HTMLInputElement>('input[value="monthly"]')?.checked).toBe(true);
    await click(button("quota.confirmReset", dialog));
    expect(resetUsage).toHaveBeenLastCalledWith("team-a", "monthly", {
      started_at: "2026-09-13T10:00:00+08:00", reset_after_manual_at: "2026-10-13T00:00:00+08:00",
    });
    expect(dialog.textContent).toContain("quota.resetDone");
  });
  it("requires confirmation to replace a Key and displays its new secret once", async () => {
    await renderList(); await click(button("keys.resetKey"));
    expect(rotateKey).not.toHaveBeenCalled();
    const dialog = container.querySelector<HTMLElement>('[role="dialog"]')!;
    await click(button("keys.resetKey", dialog));
    expect(rotateKey).toHaveBeenCalledWith("team-a");
    expect(container.textContent).toContain("cpa_new_plain");
  });
  it("requires confirmation to delete, and cancelling never dispatches the delete", async () => {
    await renderList(); await click(button("keys.delete"));
    let dialog = container.querySelector<HTMLElement>('[role="dialog"]')!;
    await click(button("keyForm.cancel", dialog)); expect(deleteKey).not.toHaveBeenCalled();
    await click(button("keys.delete")); dialog = container.querySelector<HTMLElement>('[role="dialog"]')!;
    await click(button("keys.delete", dialog)); expect(deleteKey).toHaveBeenCalledWith("team-a");
  });
  it("keeps the more menu fixed and closes it on Escape", async () => {
    await renderList();
    const menu = container.querySelector<HTMLDetailsElement>(".more-menu")!;
    await act(async () => { menu.open = true; Simulate.toggle(menu); await tick(); });
    expect(menu.querySelector<HTMLElement>(".more-menu-options")?.style.position).toBe("fixed");
    await act(async () => { document.dispatchEvent(new KeyboardEvent("keydown", { key: "Escape", bubbles: true })); });
    expect(menu.open).toBe(false);
  });
  it("preserves visible rows when refresh fails and recovers through refresh", async () => {
    await renderList(); vi.mocked(listKeys).mockRejectedValueOnce(new Error("network unavailable"));
    await click(button("keys.refresh"));
    expect(container.querySelectorAll("article")).toHaveLength(1);
    expect(container.querySelector('[role="alert"]')?.textContent).toContain("network unavailable");
    await click(button("keys.refresh")); expect(container.querySelector('[role="alert"]')).toBeNull();
  });
});

describe("key search input", () => {
  it("never writes the URL or resets the page during an IME composition and commits it at once", async () => {
    vi.mocked(listKeys).mockResolvedValue(Array.from({ length: 45 }, (_, i) => makeKey(`group-${String(i).padStart(2, "0")}`)));
    await renderList("/keys?page=1");
    vi.useFakeTimers();
    const input = searchBox();
    await compose(input, "compositionstart");
    await typeInto(input, "g");
    await typeInto(input, "gr");
    await advance(SEARCH_DEBOUNCE_MS * 4);
    expect(input.value).toBe("gr");
    expect(currentSearch().has("q")).toBe(false);
    expect(currentSearch().get("page")).toBe("1");
    expect(container.querySelectorAll("article")).toHaveLength(KEY_LIST_PAGE_SIZE);

    await typeInto(input, "group-07");
    await compose(input, "compositionend", "group-07");
    expect(currentSearch().get("q")).toBe("group-07");
    expect(currentSearch().has("page")).toBe(false);
    expect(container.querySelectorAll("article")).toHaveLength(1);
    // A trailing input event after compositionend (Safari order) changes nothing.
    await typeInto(input, "group-07");
    await advance(SEARCH_DEBOUNCE_MS);
    expect(currentSearch().get("q")).toBe("group-07");
  });

  it("keeps a typed draft through the debounce and follows outside URL changes", async () => {
    await renderList("/keys?q=nobody");
    expect(searchBox().value).toBe("nobody");
    expect(container.querySelectorAll("article")).toHaveLength(0);
    await click(button("quota.clearFilters"));
    expect(searchBox().value).toBe("");
    expect(currentSearch().toString()).toBe("");

    vi.useFakeTimers();
    await typeInto(searchBox(), "team");
    // A keystroke landing between a commit and its re-render must survive it.
    await act(async () => {
      await vi.advanceTimersByTimeAsync(SEARCH_DEBOUNCE_MS);
      setNativeValue.call(searchBox(), "team-");
      searchBox().dispatchEvent(new Event("input", { bubbles: true }));
    });
    expect(currentSearch().get("q")).toBe("team");
    expect(searchBox().value).toBe("team-");
    await advance(SEARCH_DEBOUNCE_MS);
    expect(currentSearch().get("q")).toBe("team-");

    await typeInto(searchBox(), "team-x");
    await act(async () => { button("probe-navigate").click(); });
    expect(searchBox().value).toBe("team-a");
    await advance(SEARCH_DEBOUNCE_MS);
    expect(currentSearch().get("q")).toBe("team-a");
    expect(container.querySelectorAll("article")).toHaveLength(1);
  });
});

describe("key search, sorting and pagination", () => {
  it("matches identity and key preview, and filters using server state", () => {
    const data = [key, makeKey("other", { usage: { ...key.usage, status: "limited" } }), makeKey("busy", { usage: { ...key.usage, status: "warning" } })];
    for (const query of ["team-a", "User team-a", "CPA_TEAM-A"]) expect(filterKeys(data, query)).toEqual([key]);
    expect(filterKeys(data, "", "limited").map((item) => item.id)).toEqual(["other"]);
    expect(filterKeys(data, "", "warning").map((item) => item.id)).toEqual(["busy"]);
    expect(paginateKeys(Array.from({ length: 45 }, (_, i) => i), 2)).toEqual([40, 41, 42, 43, 44]);
  });
  it("sorts by the selected quota ratio or actual reset date without mutating input", () => {
    const other = makeKey("other", { usage: { ...key.usage, cycles: key.usage.cycles!.map((cycle) => ({ ...cycle, used_usd: cycle.limit_usd * .9, resets_at: "2026-09-12T00:00:00+08:00" })) } });
    const input = [key, other];
    expect(sortKeys(input, "ratio")[0].id).toBe("other");
    expect(sortKeys(input, "reset")[0].id).toBe("other");
    expect(input[0].id).toBe(key.id);
  });
});
