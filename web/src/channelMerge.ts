import type { CpaAuthFile, CpaConfigCredential } from "./api/cpa";
import type { ChannelSummary, ChannelView } from "./types";

// A channel is one CPA credential, identified by its auth_index. CPA owns the
// credential (config entry or OAuth file); the plugin owns what it saw in
// usage and what the operator recorded. Both sides are joined here.

export interface ChannelSources {
  config: CpaConfigCredential[];
  authFiles: CpaAuthFile[];
  channels: ChannelView[];
  // false when CPA's credential lists could not be read: a channel the plugin
  // knows is then not reported as missing from CPA.
  cpaListed: boolean;
}

// config: a config API key; oauth: an auth file; plugin: only the plugin
// knows it (seen in usage or annotated, but not in CPA's lists).
export type ChannelOrigin = "config" | "oauth" | "plugin";

// configured: a config API key, whose live state CPA does not report.
// excluded: every model excluded ("*"), CPA's way to switch a key off.
// missing: not in CPA's lists any more; unknown: CPA's lists were not read.
export type ChannelState = "active" | "configured" | "disabled" | "excluded" | "error" | "unavailable" | "cooldown" | "missing" | "unknown";

export interface ChannelRow {
  // auth_index, or the CPA list position for a credential without one.
  key: string;
  authIndex: string;
  origin: ChannelOrigin;
  name: string;
  provider: string;
  baseUrl: string;
  vendor: string;
  state: ChannelState;
  config?: CpaConfigCredential;
  auth?: CpaAuthFile;
  view?: ChannelView;
  // A config credential whose auth_index an earlier row already has, named
  // by that row: CPA counts both under it.
  sharedWith?: string;
}

export interface ChannelGroup {
  vendor: string;
  rows: ChannelRow[];
}

export function hostOf(url: string): string {
  if (!url) return "";
  try {
    return new URL(url).host.toLowerCase();
  } catch {
    return "";
  }
}

function configState(config: CpaConfigCredential): ChannelState | null {
  if (config.disabled) return "disabled";
  if (config.excludedModels.includes("*")) return "excluded";
  return null;
}

function authState(auth: CpaAuthFile): ChannelState {
  if (auth.disabled || auth.status === "disabled") return "disabled";
  if (auth.status === "error") return "error";
  if (auth.unavailable) return "unavailable";
  if (auth.cooldowns.length > 0) return "cooldown";
  return "active";
}

function configLabel(config: CpaConfigCredential): string {
  const base = config.name || `${config.provider} #${config.position}`;
  return config.keyPosition ? `${base} #${config.keyPosition}` : base;
}

// CPA's key lists carry no group name, so an unnamed key is told apart by
// the models it serves before falling back to its auth_index.
function modelsLabel(models: string[]): string {
  if (models.length === 0) return "";
  const shown = models.slice(0, 2).join(", ");
  return models.length > 2 ? `${shown} +${models.length - 2}` : shown;
}

function row(key: string, origin: ChannelOrigin, state: ChannelState, parts: { config?: CpaConfigCredential; auth?: CpaAuthFile; view?: ChannelView }): ChannelRow {
  const { config, auth, view } = parts;
  const authIndex = config?.authIndex || auth?.authIndex || view?.auth_index || "";
  const provider = config?.provider || auth?.provider || view?.provider || "";
  const baseUrl = config?.baseUrl || view?.base_url || "";
  // Display name: the operator's name, then CPA's name or label, then the
  // models a config key serves, then the auth_index; a config key without
  // any of them is named by its list position.
  const configName = config ? (config.name ? configLabel(config) : "") : "";
  const name = view?.meta.display_name?.trim() || configName || auth?.label || auth?.name || (config ? modelsLabel(config.models) : "") || authIndex || (config ? configLabel(config) : key);
  const vendor = view?.meta.vendor?.trim() || hostOf(baseUrl) || provider;
  return { key, authIndex, origin, name, provider, baseUrl, vendor, state, config, auth, view };
}

// One row per CPA credential plus one per channel only the plugin knows.
// Credentials sharing an auth_index (an auth file mirroring a config key)
// become one row whose live state comes from the auth file. Config keys with
// the same base URL and key get the same auth_index from CPA; the later ones
// keep a row of their own without it.
export function mergeChannels({ config, authFiles, channels, cpaListed }: ChannelSources): ChannelRow[] {
  const views = new Map(channels.map((view) => [view.auth_index, view]));
  const files = new Map(authFiles.filter((file) => file.authIndex).map((file) => [file.authIndex, file]));
  const used = new Set<string>();
  const configNames = new Map<string, string>();
  const rows: ChannelRow[] = [];

  for (const entry of config) {
    if (!entry.authIndex || used.has(entry.authIndex)) {
      const key = `config:${entry.kind}:${entry.position}${entry.keyPosition ? `:${entry.keyPosition}` : ""}`;
      rows.push({ ...row(key, "config", configState(entry) ?? "configured", { config: { ...entry, authIndex: "" } }), sharedWith: configNames.get(entry.authIndex) });
      continue;
    }
    used.add(entry.authIndex);
    const auth = files.get(entry.authIndex);
    const state = configState(entry) ?? (auth ? authState(auth) : "configured");
    const item = row(entry.authIndex, "config", state, { config: entry, auth, view: views.get(entry.authIndex) });
    configNames.set(entry.authIndex, item.name);
    rows.push(item);
  }
  for (const file of authFiles) {
    if (file.authIndex && used.has(file.authIndex)) continue;
    const key = file.authIndex || `oauth:${file.id || file.name}`;
    if (file.authIndex) used.add(file.authIndex);
    rows.push(row(key, "oauth", authState(file), { auth: file, view: file.authIndex ? views.get(file.authIndex) : undefined }));
  }
  for (const view of channels) {
    if (!view.auth_index || used.has(view.auth_index)) continue;
    used.add(view.auth_index);
    rows.push(row(view.auth_index, "plugin", cpaListed ? "missing" : "unknown", { view }));
  }
  return rows;
}

// Groups by vendor; groups and their rows are in name order.
export function groupChannels(rows: ChannelRow[]): ChannelGroup[] {
  const groups = new Map<string, ChannelRow[]>();
  for (const item of rows) {
    const group = groups.get(item.vendor) ?? [];
    group.push(item);
    groups.set(item.vendor, group);
  }
  return [...groups]
    .map(([vendor, items]) => ({ vendor, rows: items.sort((a, b) => a.name.localeCompare(b.name) || a.key.localeCompare(b.key)) }))
    .sort((a, b) => a.vendor.localeCompare(b.vendor));
}

// Failed share of the window's attempts; null without attempts.
export function failureRate(summary?: Pick<ChannelSummary, "attempts" | "failures">): number | null {
  if (!summary || !(summary.attempts > 0)) return null;
  return summary.failures / summary.attempts;
}
