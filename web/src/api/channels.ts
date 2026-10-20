import { apiClient, pluginPath } from "./client";
import { fetchCpaCredentials, type CpaConfigCredential, type CpaCredentials } from "./cpa";
import type { ChannelDetail, ChannelMeta, ChannelPatch, ChannelSummary, ChannelView } from "../types";

const EMPTY_SUMMARY: ChannelSummary = {
  attempts: 0, failures: 0, success_rate: 0, latency_p50_ms: 0, latency_p95_ms: 0, ttft_p50_ms: 0, ttft_p95_ms: 0,
  input_tokens: 0, output_tokens: 0, list_cost_usd: 0,
};

function summary(value?: Partial<ChannelSummary> | null): ChannelSummary {
  return { ...EMPTY_SUMMARY, ...(value ?? {}) };
}

function view(value: ChannelView): ChannelView {
  return {
    ...value,
    meta: { ...(value.meta ?? {}), auth_index: value.auth_index },
    last_24h: summary(value.last_24h),
    today: summary(value.today),
    rules: value.rules ?? 0,
  };
}

// The plugin's channels, and the group names and credential IDs of CPA's
// config keys by auth_index (CPA's own key lists leave both out).
export async function fetchChannels(): Promise<{ channels: ChannelView[]; groups: Record<string, string>; ids: Record<string, string> }> {
  const { data } = await apiClient().get<{ channels: ChannelView[] | null; groups?: Record<string, string> | null; ids?: Record<string, string> | null }>(pluginPath("/channels"));
  return { channels: (data.channels ?? []).map(view), groups: data.groups ?? {}, ids: data.ids ?? {} };
}

// Names the config keys CPA lists without a name by their config group; keys
// sharing a group are told apart by their place in it.
export function nameByGroup(config: CpaConfigCredential[], groups: Record<string, string>): CpaConfigCredential[] {
  const named = config.map((entry) => (entry.name || !groups[entry.authIndex] ? entry : { ...entry, name: groups[entry.authIndex] }));
  const total = new Map<string, number>();
  for (const entry of named) {
    if (entry.name && !entry.keyPosition) total.set(`${entry.kind}:${entry.name}`, (total.get(`${entry.kind}:${entry.name}`) ?? 0) + 1);
  }
  const seen = new Map<string, number>();
  return named.map((entry) => {
    const group = `${entry.kind}:${entry.name}`;
    if (entry.name === "" || entry.keyPosition || (total.get(group) ?? 0) < 2) return entry;
    const position = (seen.get(group) ?? 0) + 1;
    seen.set(group, position);
    return { ...entry, keyPosition: position };
  });
}

export async function fetchChannelDetail(authIndex: string): Promise<ChannelDetail> {
  const { data } = await apiClient().get<{ channel: ChannelDetail }>(pluginPath("/channels/detail"), { params: { auth_index: authIndex } });
  const channel = data.channel;
  return {
    ...channel,
    ...view(channel),
    last_7d: summary(channel.last_7d),
    hours: channel.hours ?? [],
    recent_errors: channel.recent_errors ?? [],
  };
}

export async function patchChannel(patch: ChannelPatch): Promise<ChannelMeta> {
  const { data } = await apiClient().patch<{ meta: ChannelMeta }>(pluginPath("/channels"), patch);
  return data.meta;
}

// Forgets a channel CPA's config no longer has: its statistics, notes and
// adapter rules. The plugin refuses one still in the config (channel_in_cpa).
export async function deleteChannel(authIndex: string): Promise<void> {
  await apiClient().delete(pluginPath("/channels"), { params: { auth_index: authIndex } });
}

// Everything the channel list joins: CPA's credential lists and the plugin's
// channels. CPA's lists failing leaves the plugin's data on screen.
export interface ChannelData {
  channels: ChannelView[];
  cpa: CpaCredentials | null;
  cpaError: unknown;
  // Config keys' credential IDs by auth_index, to look their models up.
  ids: Record<string, string>;
}

export async function fetchChannelData(): Promise<ChannelData> {
  const [plugin, cpa] = await Promise.all([fetchChannels(), fetchCpaCredentials().then((value) => ({ value, error: null }), (error: unknown) => ({ value: null, error }))]);
  const credentials = cpa.value ? { ...cpa.value, config: nameByGroup(cpa.value.config, plugin.groups) } : null;
  return { channels: plugin.channels, cpa: credentials, cpaError: cpa.error, ids: plugin.ids };
}
