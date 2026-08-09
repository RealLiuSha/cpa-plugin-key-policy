import { afterEach, beforeEach, describe, expect, it } from "vitest";
import {
  _resetKeyCache,
  obfuscateData,
  deobfuscateData,
  readPanelAuth,
  type PanelAuth,
} from "./panelAuth";

const STORAGE_KEY = "cli-proxy-auth";

// Build a panel-style persisted auth blob: zustand persist envelope
// {state, version} serialized to JSON, then obfuscated with the shared algo.
function storePanelAuth(auth: PanelAuth): void {
  const envelope = JSON.stringify({
    state: {
      apiBase: auth.apiBase,
      managementKey: auth.managementKey,
      rememberPassword: true,
    },
    version: 0,
  });
  localStorage.setItem(STORAGE_KEY, obfuscateData(envelope));
}

describe("panelAuth obfuscation", () => {
  it("round-trips through obfuscate/deobfuscate", () => {
    const original = JSON.stringify({ state: { apiBase: "http://h:1", managementKey: "k" } });
    expect(deobfuscateData(obfuscateData(original))).toBe(original);
  });
});

describe("readPanelAuth", () => {
  const realSelf = window.self;
  const realTop = window.top;

  beforeEach(() => {
    localStorage.clear();
    _resetKeyCache();
  });

  afterEach(() => {
    localStorage.clear();
    _resetKeyCache();
    // Restore embedded state.
    Object.defineProperty(window, "self", { value: realSelf, configurable: true });
    Object.defineProperty(window, "top", { value: realTop, configurable: true });
  });

  function setEmbedded(embedded: boolean) {
    // window.self !== window.top → embedded
    Object.defineProperty(window, "self", { value: window, configurable: true });
    Object.defineProperty(window, "top", {
      value: embedded ? ({} as Window) : window,
      configurable: true,
    });
  }

  it("returns null when not embedded (direct page open)", () => {
    setEmbedded(false);
    storePanelAuth({ apiBase: "http://127.0.0.1:8317", managementKey: "secret-xyz" });
    expect(readPanelAuth()).toBeNull();
  });

  it("reads apiBase + managementKey when embedded and remember-password was set", () => {
    setEmbedded(true);
    storePanelAuth({ apiBase: "http://127.0.0.1:8317", managementKey: "secret-xyz" });
    expect(readPanelAuth()).toEqual({
      apiBase: "http://127.0.0.1:8317",
      managementKey: "secret-xyz",
    });
  });

  it("returns null when managementKey is empty (remember password unchecked)", () => {
    setEmbedded(true);
    const envelope = JSON.stringify({
      state: { apiBase: "http://h:1", managementKey: "", rememberPassword: false },
      version: 0,
    });
    localStorage.setItem(STORAGE_KEY, obfuscateData(envelope));
    expect(readPanelAuth()).toBeNull();
  });

  it("returns null when nothing is stored", () => {
    setEmbedded(true);
    expect(readPanelAuth()).toBeNull();
  });

  it("returns null when the stored value is garbage", () => {
    setEmbedded(true);
    localStorage.setItem(STORAGE_KEY, "not-valid-json-or-obfuscated");
    expect(readPanelAuth()).toBeNull();
  });

  it("rejects plaintext and non-envelope storage shapes", () => {
    setEmbedded(true);
    const state = { apiBase: "http://h:1", managementKey: "secret-xyz" };

    localStorage.setItem(STORAGE_KEY, JSON.stringify({ state, version: 0 }));
    expect(readPanelAuth()).toBeNull();

    localStorage.setItem(STORAGE_KEY, obfuscateData(JSON.stringify(state)));
    expect(readPanelAuth()).toBeNull();
  });

  it("rejects an unsupported persist envelope version", () => {
    setEmbedded(true);
    localStorage.setItem(STORAGE_KEY, obfuscateData(JSON.stringify({
      state: { apiBase: "http://h:1", managementKey: "secret-xyz" },
      version: 1,
    })));
    expect(readPanelAuth()).toBeNull();
  });
});
