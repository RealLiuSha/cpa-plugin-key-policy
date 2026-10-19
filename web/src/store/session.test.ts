import { describe, it, expect, beforeEach, afterEach, vi } from "vitest";
vi.mock("./panelAuth", () => ({ readPanelAuth: vi.fn(() => null) }));
import { readPanelAuth } from "./panelAuth";
import {
  setSession,
  clearSession,
  getSession,
  isAuthed,
  subscribe,
  bootstrapFromPanel,
  verifySession,
} from "./session";
import { translate } from "../i18n";

beforeEach(() => clearSession());
afterEach(() => vi.unstubAllGlobals());

describe("session storage", () => {
  it("starts unauthenticated", () => {
    expect(isAuthed()).toBe(false);
    expect(getSession()).toBeNull();
  });

  it("stores base url and key in memory", () => {
    setSession("http://localhost:8317/", "secret-xyz");
    const s = getSession();
    expect(s).not.toBeNull();
    expect(s!.baseUrl).toBe("http://localhost:8317");
    expect(s!.secretKey).toBe("secret-xyz");
    expect(isAuthed()).toBe(true);
  });

  it("adds http:// scheme when missing", () => {
    setSession("127.0.0.1:8317", "k");
    expect(getSession()!.baseUrl).toBe("http://127.0.0.1:8317");
  });

  it("preserves https://", () => {
    setSession("https://cpa.example.com/", "k");
    expect(getSession()!.baseUrl).toBe("https://cpa.example.com");
  });

  it("trims trailing slashes", () => {
    setSession("http://h:8317///", "k");
    expect(getSession()!.baseUrl).toBe("http://h:8317");
  });

  it("clears on logout", () => {
    setSession("http://h", "k");
    clearSession();
    expect(isAuthed()).toBe(false);
    expect(getSession()).toBeNull();
  });

  it("notifies subscribers on set and clear", () => {
    let calls = 0;
    const unsub = subscribe(() => calls++);
    setSession("http://h", "k");
    clearSession();
    expect(calls).toBeGreaterThanOrEqual(2);
    unsub();
  });

  it("is not authed when key empty", () => {
    setSession("http://h", "");
    expect(isAuthed()).toBe(false);
  });
});

describe("bootstrap from the panel", () => {
  beforeEach(() => vi.mocked(readPanelAuth).mockReturnValue({ apiBase: "http://h:8317", managementKey: "k" }));

  // CPA's empty 404 comes while it is not serving the plugin, e.g. between
  // the instances of a hot replacement: the key is kept for the pages.
  it("keeps the session when CPA is not serving the plugin", async () => {
    vi.stubGlobal("fetch", vi.fn(async () => new Response(null, { status: 404 })));
    await expect(bootstrapFromPanel()).resolves.toBe(true);
    expect(getSession()).toMatchObject({ baseUrl: "http://h:8317", secretKey: "k" });
    await expect(verifySession(fetch)).rejects.toThrow(translate("session.pluginUnavailable"));
  });

  it("clears the session when the management key is rejected", async () => {
    vi.stubGlobal("fetch", vi.fn(async () => new Response(JSON.stringify({ error: "invalid management key" }), { status: 401 })));
    await expect(bootstrapFromPanel()).resolves.toBe(false);
    expect(getSession()).toBeNull();
  });
});
