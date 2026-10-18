import { act } from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { createRoot } from "react-dom/client";
import { Simulate } from "react-dom/test-utils";
import { MemoryRouter, Route, Routes, useLocation, useParams } from "react-router-dom";

(globalThis as unknown as { IS_REACT_ACT_ENVIRONMENT?: boolean }).IS_REACT_ACT_ENVIRONMENT = true;

vi.mock("../api/keys", () => ({ createKey: vi.fn(), adjustBalance: vi.fn() }));
const { translate } = vi.hoisted(() => ({ translate: (key: string) => key }));
vi.mock("../i18n", () => ({ useT: () => translate }));

import { adjustBalance, createKey } from "../api/keys";
import KeyNew from "./KeyNew";

const tick = () => new Promise((resolve) => setTimeout(resolve, 0));
let container: HTMLDivElement;
let root: ReturnType<typeof createRoot>;

beforeEach(() => {
  container = document.createElement("div");
  document.body.appendChild(container);
  vi.mocked(createKey).mockResolvedValue({
    key: {
      id: "team-a", name: "Team A", enabled: true, key_preview: "cpa_...", rpm: 0,
      daily_limit_usd: 0, weekly_limit_usd: 0, monthly_limit_usd: 0,
      usage: { daily_usd: 0, weekly_usd: 0, monthly_usd: 0, daily_limit_usd: 0, weekly_limit_usd: 0, monthly_limit_usd: 0 },
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

describe("KeyNew", () => {
  it("creates a key from the form fields and shows its secret once", async () => {
    await act(async () => {
      root = createRoot(container);
      root.render(<MemoryRouter initialEntries={["/keys/new"]}><Routes><Route path="/keys/new" element={<KeyNew />} /><Route path="/keys" element={<div>key-list</div>} /></Routes></MemoryRouter>);
      await tick();
    });
    expect(container.textContent).not.toContain("keyForm.currentUsage");
    const [id, name] = container.querySelectorAll<HTMLInputElement>(".form-grid input:not([type])");
    await act(async () => Simulate.change(id, { target: { value: "team-a" } } as never));
    await act(async () => Simulate.change(name, { target: { value: "Team A" } } as never));
    await act(async () => {
      container.querySelector("form")!.dispatchEvent(new Event("submit", { bubbles: true, cancelable: true }));
      await tick();
    });
    expect(createKey).toHaveBeenCalledWith({
      id: "team-a", name: "Team A", enabled: true, rpm: 0, quota_mode: "cycle", daily_limit_usd: 0, weekly_limit_usd: 0, monthly_limit_usd: 0,
    });
    expect(container.textContent).toContain("cpa_secret");
    const saved = [...container.querySelectorAll<HTMLButtonElement>("button")].find((button) => button.textContent === "plainModal.saved")!;
    await act(async () => saved.click());
    expect(container.textContent).toContain("key-list");
  });

  async function createBalanceKey() {
    await act(async () => {
      root = createRoot(container);
      root.render(<MemoryRouter initialEntries={["/keys/new"]}><Routes><Route path="/keys/new" element={<KeyNew />} /><Route path="/keys" element={<div>key-list</div>} /><Route path="/keys/:id/usage" element={<UsageProbe />} /></Routes></MemoryRouter>);
      await tick();
    });
    const [id] = container.querySelectorAll<HTMLInputElement>(".form-grid input:not([type])");
    await act(async () => Simulate.change(id, { target: { value: "team-a" } } as never));
    const prepaid = [...container.querySelectorAll<HTMLButtonElement>('[role="radio"]')].find((item) => item.textContent === "balance.mode.balance")!;
    await act(async () => { prepaid.click(); });
    const startingBalance = [...container.querySelectorAll<HTMLInputElement>('input[type="number"]')].find((input) => input.closest("label")?.textContent?.includes("balance.initialLabel"))!;
    await act(async () => Simulate.change(startingBalance, { target: { value: "50" } } as never));
    await act(async () => {
      container.querySelector("form")!.dispatchEvent(new Event("submit", { bubbles: true, cancelable: true }));
      await tick();
    });
  }
  const closeSecret = async () => {
    const saved = [...container.querySelectorAll<HTMLButtonElement>("button")].find((button) => button.textContent === "plainModal.saved")!;
    await act(async () => saved.click());
  };

  it("writes the starting balance of a new balance key right after creating it", async () => {
    vi.mocked(adjustBalance).mockResolvedValue({ id: "team-a", amount_usd: 50, balance_before_usd: 0, balance_usd: 50, net_credited_usd: 50, duplicate: false });
    await createBalanceKey();
    expect(createKey).toHaveBeenCalledWith(expect.objectContaining({ id: "team-a", quota_mode: "balance" }));
    expect(vi.mocked(createKey).mock.calls[0][0]).not.toHaveProperty("daily_limit_usd");
    expect(adjustBalance).toHaveBeenCalledWith("team-a", 50, "balance.initialNote", expect.any(String));
    await closeSecret();
    expect(container.textContent).toContain("key-list");
  });

  it("still shows the secret once and then leads to the usage page when the starting balance fails", async () => {
    vi.mocked(adjustBalance).mockRejectedValue(new Error("unavailable"));
    await createBalanceKey();
    expect(container.textContent).toContain("cpa_secret");
    await closeSecret();
    expect(container.textContent).toContain("usage:team-a:balance.initialFailed");
  });
});

function UsageProbe() {
  const location = useLocation();
  const params = useParams();
  return <div>{`usage:${params.id}:${(location.state as { notice?: string } | null)?.notice ?? ""}`}</div>;
}
