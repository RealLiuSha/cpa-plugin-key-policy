import { act } from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { createRoot } from "react-dom/client";
import { Simulate } from "react-dom/test-utils";
import type { KeyPublic } from "../types";

(globalThis as unknown as { IS_REACT_ACT_ENVIRONMENT?: boolean }).IS_REACT_ACT_ENVIRONMENT = true;

const { translate } = vi.hoisted(() => ({ translate: (key: string, vars?: Record<string, string | number>) => key + (vars ? ` ${Object.values(vars).join(" ")}` : "") }));
vi.mock("../i18n", () => ({ useT: () => translate }));

import KeyForm, { keyWriteRequestFromForm, type KeyFormValues } from "./KeyForm";

const initial: KeyPublic = {
  id: "team-a",
  name: "Team A",
  enabled: true,
  key_preview: "cpa_te...am-a",
  rpm: 60,
  daily_limit_usd: 10,
  weekly_limit_usd: 0,
  monthly_limit_usd: 100,
  usage: { daily_usd: 1, weekly_usd: 2.5, monthly_usd: 0.0042, daily_limit_usd: 10, weekly_limit_usd: 0, monthly_limit_usd: 100 },
};

const tick = () => new Promise((resolve) => setTimeout(resolve, 0));
let container: HTMLDivElement;
let root: ReturnType<typeof createRoot> | undefined;

beforeEach(() => {
  container = document.createElement("div");
  document.body.appendChild(container);
  root = undefined;
});

afterEach(() => {
  if (root) act(() => root?.unmount());
  container.remove();
  vi.clearAllMocks();
});

async function flush() {
  await act(async () => { await tick(); await tick(); });
}

function collectStyleRules(rules: CSSRuleList): CSSStyleRule[] {
  return [...rules].flatMap((rule) => {
    if ("selectorText" in rule && "style" in rule) return [rule as CSSStyleRule];
    if ("cssRules" in rule) return collectStyleRules((rule as CSSMediaRule).cssRules);
    return [];
  });
}

function scanColumnRuleConflicts(css: string): { columnRuleCount: number; conflictingSelectors: string[] } {
  const style = document.createElement("style");
  style.textContent = css;
  document.head.appendChild(style);
  const fixture = document.createElement("div");
  fixture.innerHTML = '<div class="form-grid"><label class="check-row"><input type="checkbox">enabled</label></div><div class="field-row"><label class="check-row"><input type="checkbox">free</label></div>';
  document.body.appendChild(fixture);
  const checkRows = [...fixture.querySelectorAll(".check-row")];
  const columnRules = (style.sheet ? collectStyleRules(style.sheet.cssRules) : []).filter((rule) =>
    rule.style.getPropertyValue("flex-direction") === "column",
  );
  const conflictingSelectors = columnRules.flatMap((rule) =>
    rule.selectorText.split(",").map((selector) => selector.trim()).filter((selector) =>
      checkRows.some((row) => row.matches(selector)),
    ),
  );
  fixture.remove();
  style.remove();
  return { columnRuleCount: columnRules.length, conflictingSelectors };
}

async function renderForm(onSubmit: (values: KeyFormValues) => Promise<void> = async () => {}) {
  await act(async () => {
    root = createRoot(container);
    root.render(<KeyForm initial={initial} showCurrentUsage submitLabel="save" onCancel={() => {}} onSubmit={onSubmit} />);
  });
  await flush();
}

async function submit() {
  await act(async () => {
    container.querySelector("form")!.dispatchEvent(new Event("submit", { bubbles: true, cancelable: true }));
    await tick();
  });
}

describe("KeyForm", () => {
  it("keeps the shared card layout, field hints and current usage wired", async () => {
    await renderForm();
    expect(container.querySelector("form.key-form")?.classList.contains("card")).toBe(true);
    expect(container.querySelector<HTMLInputElement>('input[placeholder="keyForm.idPlaceholder"]')).not.toBeNull();
    expect(container.querySelector<HTMLInputElement>('input[placeholder="keyForm.namePlaceholder"]')).not.toBeNull();
    expect(container.textContent).toContain("keyForm.statusLabel");
    expect(container.textContent).toContain("keyForm.currentUsage $1.00");
    expect(container.textContent).toContain("keyForm.currentUsage $2.50");
    expect(container.textContent).toContain("keyForm.currentUsage $0.0042");
  });

  it("offers no model grants or /v1/models switch and marks a 0 limit as unlimited", async () => {
    await renderForm();
    expect(container.querySelectorAll('input[type="checkbox"]')).toHaveLength(1);
    expect(container.textContent).not.toContain("models");
    const unlimited = [...container.querySelectorAll(".field-label")].filter((label) => label.querySelector(".badge")?.textContent === "keyForm.unlimited");
    expect(unlimited.map((label) => label.firstChild?.textContent)).toEqual(["keyForm.weeklyLimitLabel"]);
    const daily = container.querySelectorAll<HTMLInputElement>('.form-grid input[type="number"]')[1];
    await act(async () => Simulate.change(daily, { target: { value: "0" } } as never));
    expect(container.querySelectorAll(".field-label .badge")).toHaveLength(2);
  });

  it("keeps checkbox rows outside every column-flow rule, including nested media rules", async () => {
    const { readFileSync } = await import("node:fs");
    const { resolve } = await import("node:path");
    const css = readFileSync(resolve(__dirname, "../styles.css"), "utf8");
    const current = scanColumnRuleConflicts(css);
    expect(current.columnRuleCount).toBeGreaterThan(0);
    expect(current.conflictingSelectors).toEqual([]);

    const withoutExclusion = scanColumnRuleConflicts(css.replaceAll("> label:not(.check-row)", "> label"));
    expect(withoutExclusion.conflictingSelectors).toEqual(expect.arrayContaining([".form-grid > label", ".field-row > label"]));

    const nestedMediaConflict = scanColumnRuleConflicts(`${css}\n@media (max-width: 640px) { .form-grid > label { flex-direction: column; } }`);
    expect(nestedMediaConflict.conflictingSelectors).toContain(".form-grid > label");
  });

  it("builds one complete request from the key fields only", () => {
    const request = keyWriteRequestFromForm({
      id: "team-a",
      name: " ",
      enabled: true,
      rpm: 60,
      daily_limit_usd: 10,
      weekly_limit_usd: 20,
      monthly_limit_usd: 30,
      quota_mode: "cycle",
      initial_balance_usd: 0,
    });
    expect(request).toEqual({
      id: "team-a",
      name: undefined,
      enabled: true,
      rpm: 60,
      quota_mode: "cycle",
      daily_limit_usd: 10,
      weekly_limit_usd: 20,
      monthly_limit_usd: 30,
    });
  });

  it("submits the edited values and generates an ID when none is given", async () => {
    let submitted: KeyFormValues | null = null;
    await act(async () => {
      root = createRoot(container);
      root.render(<KeyForm submitLabel="save" onCancel={() => {}} onSubmit={async (values) => { submitted = values; }} />);
    });
    const rpm = container.querySelectorAll<HTMLInputElement>('.form-grid input[type="number"]')[0];
    await act(async () => Simulate.change(rpm, { target: { value: "30" } } as never));
    await submit();
    expect(submitted).toMatchObject({ rpm: 30, enabled: true, daily_limit_usd: 0, weekly_limit_usd: 0, monthly_limit_usd: 0 });
    expect(submitted!.id).toMatch(/^key-[0-9a-f]{8}$/);
  });

  it("sends a balance key without period limits", () => {
    const request = keyWriteRequestFromForm({
      id: "prepaid", name: "Prepaid", enabled: true, rpm: 0, daily_limit_usd: 10, weekly_limit_usd: 20, monthly_limit_usd: 30,
      quota_mode: "balance", initial_balance_usd: 25,
    });
    expect(request).toEqual({ id: "prepaid", name: "Prepaid", enabled: true, rpm: 0, quota_mode: "balance" });
  });

  it("switches a new key to a prepaid balance with a starting amount instead of period limits", async () => {
    const onSubmit = vi.fn<(values: KeyFormValues) => Promise<void>>().mockResolvedValue();
    await act(async () => {
      root = createRoot(container);
      root.render(<KeyForm submitLabel="create" onSubmit={onSubmit} onCancel={vi.fn()} />);
    });
    const modeButton = (mode: string) => [...container.querySelectorAll<HTMLButtonElement>('[role="radio"]')].find((item) => item.textContent === `balance.mode.${mode}`)!;
    expect(container.textContent).toContain("keyForm.dailyLimitLabel");
    await act(async () => { modeButton("balance").click(); });
    expect(modeButton("balance").getAttribute("aria-checked")).toBe("true");
    expect(container.textContent).not.toContain("keyForm.dailyLimitLabel");
    expect(container.textContent).toContain("balance.modeHint.balance");
    const startingBalance = [...container.querySelectorAll<HTMLInputElement>('input[type="number"]')].find((input) => input.closest("label")?.textContent?.includes("balance.initialLabel"))!;
    await act(async () => Simulate.change(startingBalance, { target: { value: "25" } } as never));
    await act(async () => {
      container.querySelector("form")!.dispatchEvent(new Event("submit", { bubbles: true, cancelable: true }));
      await tick();
    });
    expect(onSubmit).toHaveBeenCalledWith(expect.objectContaining({ quota_mode: "balance", initial_balance_usd: 25 }));
  });

  it("warns before an edit clears period limits or parks a balance", async () => {
    const render = async (key: KeyPublic) => {
      if (root) act(() => root?.unmount());
      root = createRoot(container);
      await act(async () => { root!.render(<KeyForm initial={key} idReadOnly submitLabel="save" onSubmit={vi.fn()} onCancel={vi.fn()} />); });
    };
    const modeButton = (mode: string) => [...container.querySelectorAll<HTMLButtonElement>('[role="radio"]')].find((item) => item.textContent === `balance.mode.${mode}`)!;
    await render(initial);
    expect(container.textContent).not.toContain("balance.switchClearsLimits");
    await act(async () => { modeButton("balance").click(); });
    const notice = container.querySelector('[role="alert"]')?.textContent ?? "";
    expect(notice).toContain("balance.switchClearsLimits");
    expect(notice).toContain("balance.switchEmptyBalance");
    expect(container.textContent).not.toContain("balance.currentOnEdit");

    const prepaid: KeyPublic = { ...initial, daily_limit_usd: 0, monthly_limit_usd: 0, quota_mode: "balance", usage: { ...initial.usage, quota_mode: "balance", balance_usd: 12.5, net_credited_usd: 20 } };
    await render(prepaid);
    expect(container.textContent).toContain("balance.currentOnEdit $12.50");
    await act(async () => { modeButton("cycle").click(); });
    expect(container.textContent).toContain("balance.switchKeepsBalance $12.50");
    expect(container.textContent).toContain("keyForm.dailyLimitLabel");
  });

  it("blocks a negative RPM or limit at the form boundary", async () => {
    const onSubmit = vi.fn();
    await renderForm(onSubmit);
    const weekly = container.querySelectorAll<HTMLInputElement>('.form-grid input[type="number"]')[2];
    await act(async () => Simulate.change(weekly, { target: { value: "-1" } } as never));
    await submit();
    expect(onSubmit).not.toHaveBeenCalled();
    expect(container.textContent).toContain("keyForm.limitInvalid");
  });
});
