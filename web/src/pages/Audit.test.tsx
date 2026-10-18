import { act } from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { createRoot } from "react-dom/client";
import { MemoryRouter } from "react-router-dom";
import { _resetLocale, translate } from "../i18n";
import type { AuditEvent } from "../types";

(globalThis as unknown as { IS_REACT_ACT_ENVIRONMENT?: boolean }).IS_REACT_ACT_ENVIRONMENT = true;

vi.mock("../api/audit", () => ({ fetchAuditEvents: vi.fn() }));

import { fetchAuditEvents } from "../api/audit";
import Audit, { AUDIT_ACTIONS, auditChanges, auditSubject } from "./Audit";

const tick = () => new Promise((resolve) => setTimeout(resolve, 0));
const event = (action: string, changes: AuditEvent["changes"], keyID?: string): AuditEvent => ({
  ts: "2026-10-17T00:00:00Z", actor: "management-api", action, ...(keyID ? { key_id: keyID } : {}), changes,
});

beforeEach(() => _resetLocale("en"));
afterEach(() => _resetLocale("zh-CN"));

describe("Audit page", () => {
  let container: HTMLDivElement;
  let root: ReturnType<typeof createRoot>;
  beforeEach(() => {
    container = document.createElement("div");
    document.body.appendChild(container);
    vi.mocked(fetchAuditEvents).mockResolvedValue([
      event("update_key", { daily_limit_usd: { from: 1, to: 2 } }, "k1"),
      event("future_action", { rpm: { from: 1, to: 2 } }, "k2"),
    ]);
  });
  afterEach(() => {
    act(() => root.unmount());
    container.remove();
    vi.clearAllMocks();
  });
  it("renders localized events, falls back to the raw action and applies a key filter", async () => {
    await act(async () => {
      root = createRoot(container);
      root.render(<MemoryRouter><Audit /></MemoryRouter>);
      await tick();
      await tick();
    });
    const heads = [...container.querySelectorAll(".audit-head strong")].map((head) => head.textContent);
    expect(heads).toEqual(["Key updated", "future_action"]);
    expect(container.querySelector(".audit-changes")?.textContent).toBe("Daily quota：$1 → $2");
    const input = container.querySelector<HTMLInputElement>("input")!;
    await act(async () => {
      input.dispatchEvent(new Event("focus", { bubbles: true }));
      const descriptor = Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, "value")!;
      descriptor.set!.call(input, "k1");
      input.dispatchEvent(new Event("input", { bubbles: true }));
    });
    await act(async () => {
      container.querySelector("form")!.dispatchEvent(new Event("submit", { bubbles: true, cancelable: true }));
      await tick();
    });
    expect(fetchAuditEvents).toHaveBeenLastCalledWith({ keyId: "k1", action: "", limit: 100 });
  });

  async function renderAt(path: string) {
    await act(async () => {
      root = createRoot(container);
      root.render(<MemoryRouter initialEntries={[path]}><Audit /></MemoryRouter>);
      await tick();
      await tick();
    });
  }
  const buttonNamed = (label: string) => [...container.querySelectorAll("button")].find((item) => item.textContent === label);

  it("asks the server for one action, loads more of it and stops at the cap", async () => {
    vi.mocked(fetchAuditEvents).mockImplementation(async ({ limit = 100 } = {}) =>
      Array.from({ length: limit }, (_, index) => event("adjust_balance", { amount_usd: { from: null, to: 1 } }, `k${index}`)));
    await renderAt("/audit?key_id=k1");
    expect(fetchAuditEvents).toHaveBeenLastCalledWith({ keyId: "k1", action: "", limit: 100 });
    const select = container.querySelector<HTMLSelectElement>("select")!;
    expect([...select.options].map((option) => option.value)).toEqual(["", ...AUDIT_ACTIONS]);

    await act(async () => {
      select.value = "adjust_balance";
      select.dispatchEvent(new Event("change", { bubbles: true }));
      await tick();
    });
    expect(fetchAuditEvents).toHaveBeenLastCalledWith({ keyId: "k1", action: "adjust_balance", limit: 100 });
    expect(container.querySelectorAll(".audit-item")).toHaveLength(100);
    await act(async () => { buttonNamed("Load more")!.click(); await tick(); });
    expect(fetchAuditEvents).toHaveBeenLastCalledWith({ keyId: "k1", action: "adjust_balance", limit: 500 });
    await act(async () => { buttonNamed("Load more")!.click(); await tick(); });
    expect(fetchAuditEvents).toHaveBeenLastCalledWith({ keyId: "k1", action: "adjust_balance", limit: 1000 });
    expect(buttonNamed("Load more")).toBeUndefined();
    expect(container.querySelector('[role="note"]')?.textContent).toContain("1000");
  });

  it("reads the filters from the URL and offers to clear filters that match nothing", async () => {
    vi.mocked(fetchAuditEvents).mockResolvedValue([]);
    await renderAt("/audit?key_id=ghost&action=delete_key");
    expect(fetchAuditEvents).toHaveBeenLastCalledWith({ keyId: "ghost", action: "delete_key", limit: 100 });
    expect(container.querySelector<HTMLInputElement>("input")!.value).toBe("ghost");
    expect(container.querySelector<HTMLSelectElement>("select")!.value).toBe("delete_key");
    expect(container.textContent).toContain("No audit events match these filters.");
    await act(async () => { buttonNamed("Clear filters")!.click(); await tick(); });
    expect(fetchAuditEvents).toHaveBeenLastCalledWith({ keyId: "", action: "", limit: 100 });
    expect(container.querySelector<HTMLInputElement>("input")!.value).toBe("");
    expect(container.textContent).toContain("No audit events");
    expect(container.textContent).not.toContain("match these filters");
  });
});

describe("audit change formatting", () => {
  it("labels every recorded action in all four locales", () => {
    const actions = [
      "create_key", "update_key", "rotate_key", "delete_key", "reset_usage", "create_model", "update_model",
      "delete_model", "import_create_model", "import_update_prices", "migrate_state", "adjust_balance",
    ];
    for (const locale of ["zh-CN", "zh-TW", "en", "ru"] as const) {
      _resetLocale(locale);
      for (const action of actions) expect(translate(`audit.action.${action}`), `${locale} ${action}`).not.toBe(`audit.action.${action}`);
      for (const field of ["name", "enabled", "rpm", "provider", "daily_limit_usd", "per_call_usd", "amount_usd", "balance_usd", "note", "quota_mode"]) {
        expect(translate(`audit.field.${field}`), `${locale} ${field}`).not.toBe(`audit.field.${field}`);
      }
    }
  });

  it("names key fields, hides unchanged ones and reads limits of 0 as unlimited", () => {
    const update = event("update_key", {
      name: { from: "", to: "Alice" },
      enabled: { from: true, to: false },
      rpm: { from: 0, to: 60 },
      daily_limit_usd: { from: 0, to: 5 },
      weekly_limit_usd: { from: 10, to: 10 },
    }, "k1");
    expect(auditChanges(update, translate)).toEqual(["Name：Alice", "Status：enabled → disabled", "RPM：unlimited → 60", "Daily quota：unlimited → $5"]);
    expect(auditChanges(event("update_key", { name: { from: "Alice", to: "" } }, "k1"), translate)).toEqual(["Name：Alice → —"]);
    const created = event("create_key", { enabled: { from: false, to: true }, model_daily_limits: { from: {}, to: { fast: 0, slow: 5 } } }, "k1");
    expect(auditChanges(created, translate)).toEqual(["Status：enabled", "Model daily quotas：fast unlimited, slow $5"]);
    const removed = event("delete_key", { daily_usd: { from: 1.2, to: null }, weekly_usd: { from: 0.004, to: null } }, "k1");
    expect(auditChanges(removed, translate)).toEqual(["Daily used：$1.20", "7-day used：$0.004"]);
  });

  it("shows a balance adjustment as a signed amount with the balance before and after", () => {
    const credit = event("adjust_balance", { amount_usd: { from: null, to: 20 }, balance_usd: { from: -0.15, to: 19.85 }, note: { from: null, to: "order 7" } }, "k1");
    expect(auditChanges(credit, translate)).toEqual(["Amount：+$20.00", "Balance：-$0.15 → $19.85", "Note：order 7"]);
    const debit = event("adjust_balance", { amount_usd: { from: null, to: -5 }, balance_usd: { from: 20, to: 15 } }, "k1");
    expect(auditChanges(debit, translate)).toEqual(["Amount：−$5.00", "Balance：$20.00 → $15.00"]);
    const switched = event("update_key", { quota_mode: { from: "cycle", to: "balance" }, daily_limit_usd: { from: 10, to: 0 } }, "k1");
    expect(auditChanges(switched, translate)).toEqual(["Quota type：Period quotas → Prepaid balance", "Daily quota：$10 → unlimited"]);
  });

  it("identifies the price row and shows price changes verbatim", () => {
    const update = event("update_model", { model: { from: "fast", to: "fast" }, billing_multiplier: { from: 1, to: 1.2 }, upstream: { from: "xai/a", to: "xai/a" } });
    expect(auditSubject(update)).toBe("fast");
    expect(auditChanges(update, translate)).toEqual(["Multiplier：×1 → ×1.2"]);
    const synced = event("import_update_prices", {
      model: { from: "gpt-5", to: "gpt-5" },
      provider: { from: "codex", to: "codex" },
      input_price_per_million: { from: 1, to: 1.25 },
      cache_write_price_per_million: { from: null, to: 0.125 },
    });
    expect(auditSubject(synced)).toBe("gpt-5 (codex)");
    expect(auditChanges(synced, translate)).toEqual(["Input price：$1 → $1.25", "Cache-write price：same as input → $0.125"]);
    const deleted = event("delete_model", { model: { from: "gpt-5", to: "" }, provider: { from: "codex", to: "" } });
    expect(auditSubject(deleted)).toBe("gpt-5");
    expect(auditChanges(deleted, translate)).toEqual(["Provider qualifier：codex"]);
  });
});
