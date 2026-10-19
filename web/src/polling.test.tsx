import { act } from "react";
import { createRoot, type Root } from "react-dom/client";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { useVisiblePolling } from "./polling";

(globalThis as unknown as { IS_REACT_ACT_ENVIRONMENT?: boolean }).IS_REACT_ACT_ENVIRONMENT = true;

let visibility: DocumentVisibilityState = "visible";
Object.defineProperty(document, "visibilityState", { configurable: true, get: () => visibility });

function setVisibility(next: DocumentVisibilityState) {
  visibility = next;
  act(() => { document.dispatchEvent(new Event("visibilitychange")); });
}

function Poller({ refresh, enabled = true }: { refresh: () => void; enabled?: boolean }) {
  useVisiblePolling(refresh, 30_000, enabled);
  return null;
}

let container: HTMLDivElement;
let root: Root;
beforeEach(() => {
  vi.useFakeTimers();
  visibility = "visible";
  container = document.createElement("div");
  root = createRoot(container);
});
afterEach(() => {
  act(() => root.unmount());
  vi.useRealTimers();
});

describe("useVisiblePolling", () => {
  it("polls while visible, pauses while hidden and refreshes at once on return", () => {
    const refresh = vi.fn();
    act(() => root.render(<Poller refresh={refresh} />));
    expect(refresh).not.toHaveBeenCalled();
    act(() => { vi.advanceTimersByTime(30_000); });
    expect(refresh).toHaveBeenCalledTimes(1);

    setVisibility("hidden");
    act(() => { vi.advanceTimersByTime(120_000); });
    expect(refresh).toHaveBeenCalledTimes(1);

    setVisibility("visible");
    expect(refresh).toHaveBeenCalledTimes(2);
    act(() => { vi.advanceTimersByTime(29_999); });
    expect(refresh).toHaveBeenCalledTimes(2);
    act(() => { vi.advanceTimersByTime(1); });
    expect(refresh).toHaveBeenCalledTimes(3);
  });

  it("uses the latest callback, stops when disabled or unmounted", () => {
    const first = vi.fn();
    const second = vi.fn();
    act(() => root.render(<Poller refresh={first} />));
    act(() => root.render(<Poller refresh={second} />));
    act(() => { vi.advanceTimersByTime(30_000); });
    expect(first).not.toHaveBeenCalled();
    expect(second).toHaveBeenCalledTimes(1);

    act(() => root.render(<Poller refresh={second} enabled={false} />));
    act(() => { vi.advanceTimersByTime(60_000); });
    setVisibility("hidden");
    setVisibility("visible");
    expect(second).toHaveBeenCalledTimes(1);

    act(() => root.render(<Poller refresh={second} />));
    act(() => root.unmount());
    root = createRoot(container);
    act(() => { vi.advanceTimersByTime(60_000); });
    expect(second).toHaveBeenCalledTimes(1);
  });
});
