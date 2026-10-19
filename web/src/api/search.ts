import { apiClient, pluginPath, PLUGIN_ID } from "./client";
import { patchPluginConfig } from "./cpa";
import { apiErrorCode } from "./error";
import type { SearchSettings, SearchState, SearchStatus } from "../types";

function settings(value: SearchSettings): SearchSettings {
  return { ...value, keep_models: value.keep_models ?? [], channels: value.channels ?? [] };
}

function status(value?: SearchStatus | null): SearchStatus {
  return { routed_total: 0, probe_running: false, probe_key_stored: false, ...(value ?? {}), channels: value?.channels ?? [] };
}

export async function fetchSearch(): Promise<SearchState> {
  const { data } = await apiClient().get<SearchState>(pluginPath("/search"));
  return { ...data, settings: settings(data.settings), status: status(data.status) };
}

// probe.api_key empty keeps the stored key; clearProbeKey removes it.
export async function saveSearch(next: SearchSettings, clearProbeKey: boolean): Promise<{ settings: SearchSettings; status: SearchStatus }> {
  const { data } = await apiClient().put<{ settings: SearchSettings; status: SearchStatus }>(pluginPath("/search"), { settings: next, clear_probe_key: clearProbeKey });
  return { settings: settings(data.settings), status: status(data.status) };
}

// config is alpha-search-model's YAML block; it may hold that plugin's probe
// key, so it goes straight from CPA to this plugin.
export async function importSearch(config: Record<string, unknown>): Promise<SearchSettings> {
  const { data } = await apiClient().post<{ settings: SearchSettings }>(pluginPath("/search/import"), { config });
  return settings(data.settings);
}

export const SEARCH_APPLY_TIMEOUT_MS = 15_000;
export const SEARCH_APPLY_POLL_MS = 1_000;

// The configuration was saved but the router registration did not follow in
// time; last is the last state read, if any.
export class SearchApplyTimeout extends Error {
  constructor(readonly last: SearchState | null) {
    super("search toggle not applied in time");
  }
}

const wait = (ms: number) => new Promise((resolve) => window.setTimeout(resolve, ms));

// Saves search_enabled through CPA, which applies the configuration to every
// plugin, then reads GET /search until the plugin's router registration
// matches. Reads fail while the plugin is being swapped, so only the deadline
// ends the wait, unless the search module could not open its file: that
// lasts, and the settings page shows why. onWait reports the seconds waited.
export async function setSearchEnabled(enabled: boolean, onWait?: (seconds: number) => void, isCancelled: () => boolean = () => false): Promise<SearchState> {
  await patchPluginConfig(PLUGIN_ID, { search_enabled: enabled });
  const started = Date.now();
  let last: SearchState | null = null;
  for (;;) {
    await wait(SEARCH_APPLY_POLL_MS);
    if (isCancelled()) throw new SearchApplyTimeout(last);
    try {
      last = await fetchSearch();
      if (last.declared === enabled) return last;
    } catch (reason) {
      if (apiErrorCode(reason) === "module_unavailable") throw reason;
      // The plugin is restarting with the new configuration.
    }
    const elapsed = Date.now() - started;
    if (elapsed >= SEARCH_APPLY_TIMEOUT_MS) throw new SearchApplyTimeout(last);
    onWait?.(Math.round(elapsed / 1000));
  }
}
