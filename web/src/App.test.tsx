import { act } from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { createRoot } from "react-dom/client";
import { MemoryRouter, useLocation } from "react-router-dom";

(globalThis as unknown as { IS_REACT_ACT_ENVIRONMENT?: boolean }).IS_REACT_ACT_ENVIRONMENT = true;

vi.mock("./store/session", () => ({
  isAuthed: () => true,
  subscribe: () => () => {},
  getSession: () => ({ baseUrl: "http://127.0.0.1:8317", secretKey: "test" }),
  clearSession: vi.fn(),
  bootstrapFromPanel: vi.fn(async () => false),
}));
vi.mock("./i18n", () => ({ useT: () => (key: string) => key }));
const { page } = vi.hoisted(() => ({ page: (name: string) => ({ default: () => <div data-testid="page">{name}</div> }) }));
vi.mock("./pages/Login", () => page("login"));
vi.mock("./pages/Overview", () => page("overview"));
vi.mock("./pages/KeyList", () => page("keys"));
vi.mock("./pages/KeyNew", () => page("new"));
vi.mock("./pages/KeyEdit", () => page("edit"));
vi.mock("./pages/KeyUsage", () => page("usage"));
vi.mock("./pages/Channels", () => page("channels"));
vi.mock("./pages/Models", () => page("models"));
vi.mock("./pages/ModelForm", () => page("model-form"));
vi.mock("./pages/Stats", () => page("stats"));
vi.mock("./pages/Search", () => page("search"));
vi.mock("./pages/Quality", () => page("quality"));
vi.mock("./pages/Inspection", () => page("inspection"));
vi.mock("./pages/Audit", () => page("audit"));
vi.mock("./pages/Settings", () => page("settings"));

import App from "./App";
import { setUnsaved } from "./store/unsaved";

function Location() {
  return <div data-testid="location">{useLocation().pathname}</div>;
}

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
  setUnsaved("channels", null);
});

const text = (selector: string) => [...document.querySelectorAll(selector)].map((node) => node.textContent);
const shown = () => container.querySelector('[data-testid="page"]')?.textContent;
const SECTIONS = ["overview", "keys", "channels", "models", "stats", "search", "quality", "inspection", "audit", "settings"];

async function renderAt(path: string) {
  await act(async () => {
    root.render(<MemoryRouter initialEntries={[path]}><App /><Location /></MemoryRouter>);
  });
}

describe("top navigation", () => {
  it.each([
    ["/", "overview", "header.overview"],
    ["/overview", "overview", "header.overview"],
    ["/keys", "keys", "header.keys"],
    ["/keys/new", "new", "header.keys"],
    ["/keys/k1/usage", "usage", "header.keys"],
    ["/channels?auth_index=47c2d6c61505edc3", "channels", "header.channels"],
    ["/models/edit?name=a%2Fb", "model-form", "header.models"],
    ["/stats", "stats", "header.stats"],
    ["/search", "search", "header.search"],
    ["/quality", "quality", "header.quality"],
    ["/inspection", "inspection", "header.inspection"],
    ["/audit", "audit", "header.audit"],
    ["/settings", "settings", "header.settings"],
  ])("%s shows its page and highlights one link", async (path, page, active) => {
    await renderAt(path);
    expect(shown()).toBe(page);
    expect(text(".tn-link")).toEqual(SECTIONS.map((id) => `header.${id}`));
    expect(text(".tn-link.active")).toEqual([active]);
  });
});

describe("mobile tab bar", () => {
  it("keeps four sections and a more sheet for the rest", async () => {
    await renderAt("/stats");
    expect(text(".tabbar .tab")).toEqual(["◎header.overview", "#header.keys", "⇄header.channels", "$header.stats", "⋯header.more"]);
    expect(text(".tabbar .tab.active")).toEqual(["$header.stats"]);

    const more = [...container.querySelectorAll<HTMLButtonElement>(".tabbar .tab")].at(-1)!;
    await act(async () => { more.click(); });
    expect(text(".more-sheet-item")).toEqual(["¤header.models", "⌕header.search", "✓header.quality", "◷header.inspection", "≡header.audit", "⚙header.settings"]);
    const search = [...document.querySelectorAll<HTMLButtonElement>(".more-sheet-item")].find((item) => item.textContent?.includes("header.search"))!;
    await act(async () => { search.click(); });
    expect(container.querySelector('[data-testid="location"]')?.textContent).toBe("/search");
    expect(document.querySelector(".more-sheet")).toBeNull();
    // A section from the sheet names itself on the more tab.
    expect(text(".tabbar .tab.active")).toEqual(["⋯header.search"]);

    const keys = [...container.querySelectorAll<HTMLButtonElement>(".tabbar .tab")].find((tab) => tab.textContent?.includes("header.keys"))!;
    await act(async () => { keys.click(); });
    expect(shown()).toBe("keys");
    expect(text(".tabbar .tab.active")).toEqual(["#header.keys"]);
  });
});

describe("leaving unsaved edits", () => {
  const location = () => container.querySelector('[data-testid="location"]')?.textContent;
  const dialogButton = (label: string) => [...document.querySelectorAll<HTMLButtonElement>('[role="dialog"] button')].find((item) => item.textContent === label)!;

  it("asks before a section link drops them, from the top nav and the tab bar", async () => {
    await renderAt("/channels");
    setUnsaved("channels", "channels.leaveMessage");

    const stats = [...container.querySelectorAll<HTMLAnchorElement>(".tn-link")].find((link) => link.textContent === "header.stats")!;
    await act(async () => { stats.click(); });
    expect(location()).toBe("/channels");
    expect(document.querySelector('[role="dialog"]')?.textContent).toContain("channels.leaveMessage");
    await act(async () => { dialogButton("channels.leaveStay").click(); });
    expect(document.querySelector('[role="dialog"]')).toBeNull();
    expect(location()).toBe("/channels");

    const keys = [...container.querySelectorAll<HTMLButtonElement>(".tabbar .tab")].find((tab) => tab.textContent?.includes("header.keys"))!;
    await act(async () => { keys.click(); });
    expect(location()).toBe("/channels");
    await act(async () => { dialogButton("channels.leaveDiscard").click(); });
    expect(location()).toBe("/keys");
  });

  it("leaves a modified click, such as a new tab, to the browser", async () => {
    await renderAt("/channels");
    setUnsaved("channels", "channels.leaveMessage");
    const stats = [...container.querySelectorAll<HTMLAnchorElement>(".tn-link")].find((link) => link.textContent === "header.stats")!;
    let prevented: boolean | undefined;
    // Runs after React's handlers; jsdom cannot follow the link itself.
    const settle = (event: Event) => { prevented = event.defaultPrevented; event.preventDefault(); };
    document.addEventListener("click", settle);
    try {
      await act(async () => { stats.dispatchEvent(new MouseEvent("click", { bubbles: true, cancelable: true, ctrlKey: true })); });
    } finally {
      document.removeEventListener("click", settle);
    }
    expect(prevented).toBe(false);
    expect(document.querySelector('[role="dialog"]')).toBeNull();
    expect(location()).toBe("/channels");
  });

  it("lets the links go when nothing is unsaved", async () => {
    await renderAt("/channels");
    const stats = [...container.querySelectorAll<HTMLAnchorElement>(".tn-link")].find((link) => link.textContent === "header.stats")!;
    await act(async () => { stats.click(); });
    expect(location()).toBe("/stats");
    expect(document.querySelector('[role="dialog"]')).toBeNull();
  });
});
