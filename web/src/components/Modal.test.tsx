import { act, useState } from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { createRoot } from "react-dom/client";
import Modal, { ConfirmDialog } from "./Modal";

(globalThis as unknown as { IS_REACT_ACT_ENVIRONMENT?: boolean }).IS_REACT_ACT_ENVIRONMENT = true;

let container: HTMLDivElement;
let root: ReturnType<typeof createRoot>;

beforeEach(() => {
  container = document.createElement("div");
  document.body.appendChild(container);
  root = createRoot(container);
});

afterEach(() => {
  act(() => root.unmount());
  container.remove();
});

describe("Modal", () => {
  it("announces itself, closes on Escape and restores focus", async () => {
    function Harness() {
      const [open, setOpen] = useState(false);
      return <><button onClick={() => setOpen(true)}>open</button>{open && <Modal title="Dialog title" closeLabel="close" onClose={() => setOpen(false)}><button>last action</button></Modal>}</>;
    }
    await act(async () => root.render(<Harness />));
    const opener = container.querySelector<HTMLButtonElement>("button")!;
    opener.focus();
    await act(async () => opener.click());

    const dialog = container.querySelector<HTMLElement>('[role="dialog"]')!;
    expect(dialog.getAttribute("aria-modal")).toBe("true");
    expect(document.activeElement).toBe(dialog);
    const close = container.querySelector<HTMLButtonElement>(".modal-close")!;
    const lastAction = [...dialog.querySelectorAll<HTMLButtonElement>("button")].at(-1)!;
    await act(async () => document.dispatchEvent(new KeyboardEvent("keydown", { key: "Tab", shiftKey: true })));
    expect(document.activeElement).toBe(lastAction);
    await act(async () => document.dispatchEvent(new KeyboardEvent("keydown", { key: "Tab" })));
    expect(document.activeElement).toBe(close);
    await act(async () => document.dispatchEvent(new KeyboardEvent("keydown", { key: "Tab", shiftKey: true })));
    expect(document.activeElement).toBe(lastAction);
    await act(async () => document.dispatchEvent(new KeyboardEvent("keydown", { key: "Escape" })));
    expect(container.querySelector('[role="dialog"]')).toBeNull();
    expect(document.activeElement).toBe(opener);
  });

  it("cannot dismiss a busy confirmation", async () => {
    const onCancel = vi.fn();
    await act(async () => root.render(
      <ConfirmDialog title="Delete" message="Continue?" cancelLabel="Cancel" confirmLabel="Delete" busy onCancel={onCancel} onConfirm={() => {}} />,
    ));
    await act(async () => document.dispatchEvent(new KeyboardEvent("keydown", { key: "Escape" })));
    const overlay = container.querySelector<HTMLElement>(".modal-overlay")!;
    await act(async () => overlay.dispatchEvent(new MouseEvent("mousedown", { bubbles: true })));
    expect(onCancel).not.toHaveBeenCalled();
    expect(container.querySelector<HTMLButtonElement>(".modal-close")?.disabled).toBe(true);
  });
});
