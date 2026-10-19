import { useEffect, useRef } from "react";

// Calls refresh every intervalMs while the page is visible, and right away
// when it becomes visible again; a hidden tab (or a panel tab showing another
// page) sends no requests. The first load is the caller's. refresh may change
// between renders without restarting the timer.
export function useVisiblePolling(refresh: () => void, intervalMs: number, enabled = true): void {
  const latest = useRef(refresh);
  latest.current = refresh;

  useEffect(() => {
    if (!enabled) return;
    let timer: number | undefined;
    const stop = () => {
      window.clearInterval(timer);
      timer = undefined;
    };
    const start = () => {
      stop();
      timer = window.setInterval(() => latest.current(), intervalMs);
    };
    const onVisibilityChange = () => {
      if (document.visibilityState === "visible") {
        latest.current();
        start();
      } else {
        stop();
      }
    };
    if (document.visibilityState === "visible") start();
    document.addEventListener("visibilitychange", onVisibilityChange);
    return () => {
      stop();
      document.removeEventListener("visibilitychange", onVisibilityChange);
    };
  }, [intervalMs, enabled]);
}
