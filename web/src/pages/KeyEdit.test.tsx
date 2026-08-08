import { act } from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

(globalThis as unknown as { IS_REACT_ACT_ENVIRONMENT?: boolean }).IS_REACT_ACT_ENVIRONMENT = true;

import { createRoot } from "react-dom/client";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import type { AliasMapping, KeyPublic } from "../types";

vi.mock("../api/keys", () => ({
  listKeys: vi.fn(),
  patchKey: vi.fn(),
  rotateKey: vi.fn(),
  resetRPM: vi.fn(),
  resetUsage: vi.fn(),
  deleteKey: vi.fn(),
}));

vi.mock("../api/mappings", () => ({
  fetchAliases: vi.fn(),
}));

vi.mock("../api/models", () => ({
  formatTierLabel: (_t: (k: string) => string, group: string) => group,
  fetchCatalog: vi.fn(async () => []),
  groupByCatalog: () => [],
}));

vi.mock("../components/ModelPicker", () => ({
  default: () => <div data-testid="model-picker" />,
}));

const { translate } = vi.hoisted(() => ({
  translate: (key: string, vars?: Record<string, string | number>) => {
    if (!vars) return key;
    if (key === "keyForm.unpricedAfterSave") return `${vars.n} unpriced aliases`;
    let s = key;
    for (const [k, v] of Object.entries(vars)) {
      s = s.replace(new RegExp(`\\{\\{${k}\\}\\}`, "g"), String(v));
    }
    return s;
  },
}));

vi.mock("../i18n", () => ({
  useT: () => translate,
}));

import { listKeys, patchKey, resetRPM, resetUsage } from "../api/keys";
import { fetchAliases } from "../api/mappings";
import KeyEdit from "./KeyEdit";

const key: KeyPublic = {
  id: "team-a",
  name: "Team A",
  enabled: true,
  key_preview: "cpa_te...am-a",
  rpm: 60,
  models: [{ alias: "priced", provider: "openai", target_model: "priced" }],
  daily_limit_usd: 10,
  weekly_limit_usd: 50,
  usage: {
    daily_usd: 8,
    weekly_usd: 20,
    daily_limit_usd: 10,
    weekly_limit_usd: 50,
  },
};

const globalAliases: AliasMapping[] = [
  {
    alias: "priced",
    targets: [{ provider: "openai", target_model: "priced" }],
    dispatch: "round-robin",
    billing_mode: "tokens",
    input_price_per_million: 5,
    output_price_per_million: 30,
    cache_read_price_per_million: 1,
  },
  {
    alias: "unpriced",
    targets: [{ provider: "openai", target_model: "unpriced" }],
    dispatch: "round-robin",
    billing_mode: "tokens",
    input_price_per_million: 0,
    output_price_per_million: 0,
    cache_read_price_per_million: 0,
  },
];

const tick = () => new Promise((resolve) => setTimeout(resolve, 0));
const flush = async () => {
  await act(async () => {
    await tick();
    await tick();
  });
};

describe("KeyEdit reset more-menu", () => {
  let container: HTMLDivElement;
  let root: ReturnType<typeof createRoot>;

  beforeEach(() => {
    container = document.createElement("div");
    document.body.appendChild(container);
    vi.mocked(listKeys).mockResolvedValue([key]);
    vi.mocked(fetchAliases).mockResolvedValue(globalAliases);
    vi.stubGlobal("confirm", vi.fn(() => true));
  });

  afterEach(() => {
    act(() => root.unmount());
    container.remove();
    vi.unstubAllGlobals();
    vi.clearAllMocks();
  });

  it("exposes the same daily, weekly, monthly, and RPM reset menu on desktop and mobile", async () => {
    await act(async () => {
      root = createRoot(container);
      root.render(
        <MemoryRouter initialEntries={["/keys/team-a/edit"]}>
          <Routes>
            <Route path="/keys/:id/edit" element={<KeyEdit />} />
          </Routes>
        </MemoryRouter>,
      );
      await tick();
    });
    await flush();

    const desktopDetails = container.querySelector<HTMLDetailsElement>(".fp-head .more-menu");
    expect(desktopDetails).not.toBeNull();
    expect(desktopDetails?.querySelector("summary")?.textContent).toContain("keys.reset");

    const mobileDetails = container.querySelector<HTMLDetailsElement>(".mobile-key-reset .more-menu");
    expect(mobileDetails).not.toBeNull();
    expect(mobileDetails?.querySelector("summary")?.textContent).toContain("keys.reset");
    const buttons = Array.from(mobileDetails?.querySelectorAll<HTMLButtonElement>("button") ?? []);
    expect(buttons.map((button) => button.textContent)).toEqual([
      "keys.resetDaily",
      "keys.resetWeekly",
      "keys.resetMonthly",
      "keys.resetRpm",
    ]);
    // Edit page stays reset-only — no rotate/delete in the more menu.
    expect(buttons.some((b) => b.textContent === "keys.resetKey")).toBe(false);
    expect(buttons.some((b) => b.textContent === "keys.delete")).toBe(false);

    await act(async () => {
      buttons[0]?.dispatchEvent(new MouseEvent("click", { bubbles: true }));
      await tick();
    });
    expect(confirm).toHaveBeenCalledWith("keys.resetDailyConfirm");
    expect(resetUsage).toHaveBeenCalledWith("team-a", "daily");

    await act(async () => {
      buttons[2]?.dispatchEvent(new MouseEvent("click", { bubbles: true }));
      await tick();
    });
    expect(confirm).toHaveBeenCalledWith("keys.resetMonthlyConfirm");
    expect(resetUsage).toHaveBeenCalledWith("team-a", "monthly");

    await act(async () => {
      buttons[3]?.dispatchEvent(new MouseEvent("click", { bubbles: true }));
      await tick();
    });
    expect(resetRPM).toHaveBeenCalledWith("team-a");
  });
});

describe("KeyEdit save path — unpriced after-save notice (real KeyForm)", () => {
  let container: HTMLDivElement;
  let root: ReturnType<typeof createRoot>;

  beforeEach(() => {
    container = document.createElement("div");
    document.body.appendChild(container);
    vi.mocked(listKeys).mockResolvedValue([key]);
    vi.mocked(fetchAliases).mockResolvedValue(globalAliases);
    vi.mocked(patchKey).mockResolvedValue(key);
    vi.stubGlobal("confirm", vi.fn(() => true));
  });

  afterEach(() => {
    act(() => root.unmount());
    container.remove();
    vi.unstubAllGlobals();
    vi.clearAllMocks();
  });

  it("after patch with a newly added unpriced alias stays mounted and shows parent notice (does not nav away)", async () => {
    await act(async () => {
      root = createRoot(container);
      root.render(
        <MemoryRouter initialEntries={["/keys/team-a/edit"]}>
          <Routes>
            <Route path="/keys/:id/edit" element={<KeyEdit />} />
            <Route path="/keys" element={<div data-testid="keys-list">keys list</div>} />
            <Route path="/mapping" element={<div data-testid="mapping-page">mapping</div>} />
          </Routes>
        </MemoryRouter>,
      );
    });
    await flush();

    // Add unpriced alias via global alias chip (real KeyForm path).
    const chip = Array.from(container.querySelectorAll("button")).find((b) =>
      (b.textContent || "").trim().startsWith("unpriced"),
    );
    expect(chip).toBeTruthy();
    await act(async () => { chip!.click(); });
    await flush();

    const form = container.querySelector("form.key-form, form");
    expect(form).not.toBeNull();
    await act(async () => {
      form!.dispatchEvent(new Event("submit", { bubbles: true, cancelable: true }));
    });
    await flush();

    expect(patchKey).toHaveBeenCalled();
    // Must NOT have navigated to keys list — notice is on the edit page.
    expect(container.querySelector('[data-testid="keys-list"]')).toBeNull();
    // Parent owns the banner: exactly one notice, not KeyForm + parent.
    const notices = container.querySelectorAll('[data-testid="unpriced-after-save"]');
    expect(notices.length).toBe(1);
    expect(notices[0]?.textContent).toMatch(/1/);
    expect(notices[0]?.querySelector('a[href="/mapping"]')).not.toBeNull();
  });

  it("after patch with no new unpriced aliases navigates to keys list", async () => {
    await act(async () => {
      root = createRoot(container);
      root.render(
        <MemoryRouter initialEntries={["/keys/team-a/edit"]}>
          <Routes>
            <Route path="/keys/:id/edit" element={<KeyEdit />} />
            <Route path="/keys" element={<div data-testid="keys-list">keys list</div>} />
          </Routes>
        </MemoryRouter>,
      );
    });
    await flush();

    const form = container.querySelector("form");
    expect(form).not.toBeNull();
    await act(async () => {
      form!.dispatchEvent(new Event("submit", { bubbles: true, cancelable: true }));
    });
    await flush();

    expect(patchKey).toHaveBeenCalled();
    expect(container.querySelector('[data-testid="keys-list"]')).not.toBeNull();
    expect(container.querySelector('[data-testid="unpriced-after-save"]')).toBeNull();
  });
});
