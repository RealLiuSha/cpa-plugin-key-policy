// In-memory session only. Never persisted to localStorage.
// Rationale: the secret-key is CPA's total management credential; we don't want
// it lying around in storage. Refreshing/closing the tab resets to the login page.

import { translate } from "../i18n";
import { readPanelAuth } from "./panelAuth";
import type { StatusResponse } from "../types";

interface Session {
  baseUrl: string;
  secretKey: string;
}

let current: Session | null = null;

const listeners = new Set<() => void>();

function normalizeBase(url: string): string {
  let u = url.trim();
  if (u === "") return "";
  u = u.replace(/\/+$/, "");
  if (!/^https?:\/\//i.test(u)) u = "http://" + u;
  return u;
}

export function setSession(baseUrl: string, secretKey: string): Session {
  const session: Session = {
    baseUrl: normalizeBase(baseUrl),
    secretKey: secretKey.trim(),
  };
  current = session;
  emit();
  return session;
}

export function clearSession(): void {
  current = null;
  emit();
}

export function getSession(): Session | null {
  return current;
}

export function isAuthed(): boolean {
  return current !== null && current.secretKey !== "" && current.baseUrl !== "";
}

export function subscribe(fn: () => void): () => void {
  listeners.add(fn);
  return () => listeners.delete(fn);
}

function emit(): void {
  for (const fn of listeners) fn();
}

// CPA answered its empty 404: it is not serving the plugin's routes right
// now, e.g. between the instances of a hot replacement. It says nothing
// about the management key.
export class PluginUnavailableError extends Error {
  constructor() {
    super(translate("session.pluginUnavailable"));
  }
}

// Attempt to restore a session from the official panel's saved management key
// (only available when this UI is loaded as a same-origin iframe inside the
// panel AND the user checked "remember password" there). On success the
// session is set and true is returned. While CPA is not serving the plugin
// the session is kept, so the pages show that and their retry; a rejected
// key comes back as 401/403 there and clears it. On any other failure the
// session is cleared and false is returned so the caller falls back to the
// login page.
export async function bootstrapFromPanel(): Promise<boolean> {
  const auth = readPanelAuth();
  if (!auth) return false;
  setSession(auth.apiBase, auth.managementKey);
  try {
    await verifySession(fetch);
    return true;
  } catch (error) {
    if (error instanceof PluginUnavailableError) return true;
    clearSession();
    return false;
  }
}

// Probe helper: confirm the key works by hitting the plugin status route.
// Returns the session on success; throws on non-2xx.
export async function verifySession(
  fetchImpl: typeof fetch,
): Promise<Session> {
  const s = current;
  if (!s) throw new Error("no session");
  const res = await fetchImpl(s.baseUrl + "/v0/management/plugins/cpa-key-policy/status", {
    headers: { Authorization: "Bearer " + s.secretKey },
  });
  if (res.status === 404 && (await res.text()) === "") throw new PluginUnavailableError();
  if (!res.ok) {
    throw new Error("management key rejected (" + res.status + ")");
  }
  await res.json() as StatusResponse;
  return s;
}
