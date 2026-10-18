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
vi.mock("./pages/KeyList", () => page("keys"));
vi.mock("./pages/KeyNew", () => page("new"));
vi.mock("./pages/KeyEdit", () => page("edit"));
vi.mock("./pages/KeyUsage", () => page("usage"));
vi.mock("./pages/Models", () => page("models"));
vi.mock("./pages/ModelForm", () => page("model-form"));
vi.mock("./pages/Audit", () => page("audit"));
vi.mock("./pages/Stats", () => page("stats"));

import App from "./App";
import { MobileTabBar } from "./components/MobileChrome";

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
});

const text = (selector: string) => [...container.querySelectorAll(selector)].map((node) => node.textContent);

describe("top navigation", () => {
  it.each([
    ["/stats", "stats", "header.stats"],
    ["/keys", "keys", "header.keyList"],
    ["/keys/new", "new", "header.newKey"],
    ["/keys/k1/usage", "usage", "header.keyList"],
    ["/models/edit?name=a%2Fb", "model-form", "header.models"],
    ["/audit", "audit", "header.audit"],
  ])("%s shows its page and highlights one link", async (path, shown, active) => {
    await act(async () => {
      root.render(<MemoryRouter initialEntries={[path]}><App /></MemoryRouter>);
    });
    expect(container.querySelector('[data-testid="page"]')?.textContent).toBe(shown);
    expect(text(".tn-link")).toEqual(["header.keyList", "header.newKey", "header.stats", "header.models", "header.audit"]);
    expect(text(".tn-link.active")).toEqual([active]);
  });
});

describe("mobile tab bar", () => {
  it("offers statistics, marks the active tab and navigates there", async () => {
    await act(async () => {
      root.render(<MemoryRouter initialEntries={["/keys/k1/usage"]}><MobileTabBar active="usage" showUsage usagePath="/keys/k1/usage" /><Location /></MemoryRouter>);
    });
    expect(text(".tabbar .tab")).toEqual(["#keys.mobile.tabKeys", "#keys.mobile.tabUsage", "$keys.mobile.tabStats", "+keys.mobile.tabNew"]);
    expect(text(".tabbar .tab.active")).toEqual(["#keys.mobile.tabUsage"]);
    const stats = [...container.querySelectorAll<HTMLButtonElement>(".tabbar .tab")].find((tab) => tab.textContent?.includes("tabStats"))!;
    await act(async () => { stats.click(); });
    expect(container.querySelector('[data-testid="location"]')?.textContent).toBe("/stats");

    await act(async () => {
      root.render(<MemoryRouter initialEntries={["/stats"]}><MobileTabBar active="stats" /></MemoryRouter>);
    });
    expect(text(".tabbar .tab")).toEqual(["#keys.mobile.tabKeys", "$keys.mobile.tabStats", "+keys.mobile.tabNew"]);
    expect(text(".tabbar .tab.active")).toEqual(["$keys.mobile.tabStats"]);
  });
});
