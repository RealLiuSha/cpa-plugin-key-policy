import { act } from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { createRoot } from "react-dom/client";

(globalThis as unknown as { IS_REACT_ACT_ENVIRONMENT?: boolean }).IS_REACT_ACT_ENVIRONMENT = true;

vi.mock("../api/audit", () => ({ fetchAuditEvents: vi.fn() }));
vi.mock("../i18n", () => ({ useT: () => (key: string) => key }));

import { fetchAuditEvents } from "../api/audit";
import Audit from "./Audit";

const tick = () => new Promise((resolve) => setTimeout(resolve, 0));

describe("Audit page", () => {
  let container: HTMLDivElement;
  let root: ReturnType<typeof createRoot>;
  beforeEach(() => {
    container = document.createElement("div");
    document.body.appendChild(container);
    vi.mocked(fetchAuditEvents).mockResolvedValue([{
      ts: "2026-08-08T00:00:00Z", actor: "management-api", action: "update_key", key_id: "k1",
      changes: { daily_limit_usd: { from: 1, to: 2 } },
    }]);
  });
  afterEach(() => {
    act(() => root.unmount());
    container.remove();
    vi.clearAllMocks();
  });
  it("renders events and applies a key filter", async () => {
    await act(async () => {
      root = createRoot(container);
      root.render(<Audit />);
      await tick();
      await tick();
    });
    expect(container.textContent).toContain("update_key");
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
    expect(fetchAuditEvents).toHaveBeenLastCalledWith("k1", 100);
  });
});
