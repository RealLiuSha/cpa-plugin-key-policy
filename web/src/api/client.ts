import axios, { AxiosInstance } from "axios";
import { translate } from "../i18n";
import { getSession, clearSession } from "../store/session";

// Build a fresh client per request using the current in-memory session.
// We avoid a singleton so that login/logout switches take effect immediately.
export function apiClient(): AxiosInstance {
  const s = getSession();
  if (!s || !s.baseUrl) {
    throw new Error("not authenticated");
  }
  const instance = axios.create({
    baseURL: s.baseUrl,
    headers: {
      Authorization: "Bearer " + s.secretKey,
      "Content-Type": "application/json",
    },
    // Validate status: treat 401/403 as auth failure -> force re-login.
    validateStatus: (code) => code >= 200 && code < 300,
  });

  instance.interceptors.response.use(
    (r) => r,
    (err) => {
      if (err?.response?.status === 401 || err?.response?.status === 403) {
        clearSession();
      }
      // CPA answers an empty 404 on every route of a plugin it is not serving:
      // removed after a failed configuration, fused, between the instances of
      // a hot replacement, or a route this version lacks.
      if (err?.response?.status === 404 && !err.response.data && String(err.config?.url ?? "").startsWith(PLUGIN_BASE)) {
        err.message = translate("session.pluginUnavailable");
      }
      return Promise.reject(err);
    },
  );

  return instance;
}

export const PLUGIN_ID = "cpa-key-policy";
const PLUGIN_BASE = "/v0/management/plugins/" + PLUGIN_ID;

export function pluginPath(suffix: string): string {
  return PLUGIN_BASE + suffix;
}

