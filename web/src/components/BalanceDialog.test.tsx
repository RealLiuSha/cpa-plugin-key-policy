import { act } from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { createRoot, type Root } from "react-dom/client";
import { MemoryRouter } from "react-router-dom";
import type { KeyUsageResponse, UsageSummary } from "../types";

(globalThis as unknown as { IS_REACT_ACT_ENVIRONMENT?: boolean }).IS_REACT_ACT_ENVIRONMENT = true;

vi.mock("../api/keys", () => ({ fetchKeyUsage: vi.fn(), adjustBalance: vi.fn() }));
const { translate } = vi.hoisted(() => ({ translate: (key: string, vars?: Record<string, string | number>) => key + (vars ? ` ${Object.values(vars).join(" ")}` : "") }));
vi.mock("../i18n", () => ({ useT: () => translate }));

import { adjustBalance, fetchKeyUsage } from "../api/keys";
import BalanceDialog from "./BalanceDialog";

const tick = () => new Promise((resolve) => setTimeout(resolve, 0));
const setNativeValue = Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, "value")!.set!;
let container: HTMLDivElement;
let root: Root;

function usageResponse(usage: Partial<UsageSummary>): KeyUsageResponse {
  return {
    key_id: "prepaid", key_name: "Prepaid user", daily_limit_usd: 0, weekly_limit_usd: 0, monthly_limit_usd: 0, models: [],
    usage: { daily_usd: 0, weekly_usd: 0, monthly_usd: 0, daily_limit_usd: 0, weekly_limit_usd: 0, monthly_limit_usd: 0, ...usage },
  };
}
const balanceUsage = usageResponse({ quota_mode: "balance", balance_usd: 5, net_credited_usd: 10, status: "normal" });

async function renderDialog(onComplete = vi.fn()) {
  await act(async () => {
    root.render(<MemoryRouter><BalanceDialog keyId="prepaid" onClose={vi.fn()} onComplete={onComplete} /></MemoryRouter>);
    await tick();
  });
  return onComplete;
}
async function type(input: HTMLInputElement, value: string) {
  await act(async () => { setNativeValue.call(input, value); input.dispatchEvent(new Event("input", { bubbles: true })); });
}
function buttonStartingWith(label: string): HTMLButtonElement {
  const found = [...container.querySelectorAll<HTMLButtonElement>("button")].find((item) => item.textContent?.startsWith(label));
  if (!found) throw new Error(`Missing button ${label}`);
  return found;
}
async function click(element: Element) { await act(async () => { element.dispatchEvent(new MouseEvent("click", { bubbles: true })); await tick(); }); }
const amountInput = () => container.querySelector<HTMLInputElement>('.balance-amount input')!;

beforeEach(() => {
  container = document.createElement("div");
  document.body.appendChild(container);
  root = createRoot(container);
  vi.mocked(fetchKeyUsage).mockResolvedValue(balanceUsage);
});
afterEach(async () => {
  await act(async () => root.unmount());
  container.remove();
  vi.clearAllMocks();
});

describe("BalanceDialog", () => {
  it("credits once even when the first response is lost: the retry reuses the request id", async () => {
    const lost = Object.assign(new Error("Network Error"), { isAxiosError: true, response: undefined });
    vi.mocked(adjustBalance).mockRejectedValueOnce(lost).mockResolvedValueOnce({
      id: "prepaid", amount_usd: 20, balance_before_usd: 5, balance_usd: 25, net_credited_usd: 30, duplicate: true,
    });
    const onComplete = await renderDialog();
    await type(amountInput(), "20");
    expect(container.querySelector(".quota-reset-preview")?.textContent).toContain("$25.00");
    await click(buttonStartingWith("balance.confirmCredit"));

    expect(container.textContent).toContain("balance.uncertain");
    expect(amountInput().disabled).toBe(true);
    await click(buttonStartingWith("balance.retry"));

    expect(adjustBalance).toHaveBeenCalledTimes(2);
    const [first, second] = vi.mocked(adjustBalance).mock.calls;
    expect(first[0]).toBe("prepaid");
    expect(first[1]).toBe(20);
    expect(second[1]).toBe(20);
    expect(second[3]).toBe(first[3]);
    expect(container.textContent).toContain("balance.duplicate");
    expect(container.textContent).toContain("balance.afterThen $25.00");
    expect(onComplete).toHaveBeenCalledTimes(1);
  });

  it("deducts with a negative amount and warns before the balance drops to zero or below", async () => {
    vi.mocked(adjustBalance).mockResolvedValue({ id: "prepaid", amount_usd: -6, balance_before_usd: 5, balance_usd: -1, net_credited_usd: 4, duplicate: false });
    await renderDialog();
    await click(buttonStartingWith("balance.debit"));
    await type(amountInput(), "6");
    expect(container.querySelector(".balance-preview-negative")?.textContent).toContain("-$1.00");
    expect(container.textContent).toContain("balance.afterNotPositive");
    await click(buttonStartingWith("balance.confirmDebit"));
    expect(adjustBalance).toHaveBeenCalledWith("prepaid", -6, "", expect.any(String));
    expect(container.textContent).toContain("balance.debited $6.00");
  });

  it("keeps the form editable after a rejection the server answered", async () => {
    const conflict = Object.assign(new Error("Conflict"), { isAxiosError: true, response: { status: 409, data: { error: { code: "request_id_conflict", message: "conflict" } } } });
    vi.mocked(adjustBalance).mockRejectedValue(conflict);
    await renderDialog();
    await type(amountInput(), "3");
    await click(buttonStartingWith("balance.confirmCredit"));
    expect(container.textContent).toContain("balance.requestConflict");
    expect(amountInput().disabled).toBe(false);
  });

  it("sends a cycle key to its edit page instead of adjusting", async () => {
    vi.mocked(fetchKeyUsage).mockResolvedValue(usageResponse({ quota_mode: "cycle" }));
    await renderDialog();
    expect(container.textContent).toContain("balance.notBalanceMode");
    expect(container.querySelector('a[href="/keys/prepaid/edit"]')).not.toBeNull();
    expect([...container.querySelectorAll("button")].some((item) => item.textContent?.startsWith("balance.confirm"))).toBe(false);
  });
});
