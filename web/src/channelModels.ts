import { useEffect, useRef, useState } from "react";
import { fetchCredentialModels } from "./api/cpa";
import type { ChannelRow } from "./channelMerge";

// Which models each channel serves, by auth_index, as CPA's model registry
// has them. It takes one request per credential, so lookups run a few at a
// time and are kept for a few minutes: the channel lists refresh every 30
// seconds, a credential's models rarely change.

export type ChannelModels = ReadonlyMap<string, readonly string[]>;

const MODELS_TTL_MS = 5 * 60_000;
const PARALLEL_LOOKUPS = 6;
const lookups = new Map<string, { at: number; models: Promise<string[] | null> }>();

// CPA finds an auth file by its name or ID and a config key by its
// credential ID, which only the plugin knows.
function lookupName(row: ChannelRow, ids: Record<string, string>): string {
  return row.auth?.id || row.auth?.name || ids[row.authIndex] || "";
}

// A failed lookup is null and is not kept, so the next refresh asks again.
function lookup(name: string): Promise<string[] | null> {
  const cached = lookups.get(name);
  if (cached && Date.now() - cached.at < MODELS_TTL_MS) return cached.models;
  const models = fetchCredentialModels(name).catch(() => {
    lookups.delete(name);
    return null;
  });
  lookups.set(name, { at: Date.now(), models });
  return models;
}

// Without an answer from CPA a config key falls back to the models it
// declares; a key that declares none serves its provider's defaults, which
// only CPA knows.
async function loadModels(rows: ChannelRow[], ids: Record<string, string>): Promise<Map<string, string[]>> {
  const out = new Map<string, string[]>();
  const queue = rows.filter((row) => row.authIndex);
  const work = async () => {
    for (let row = queue.shift(); row; row = queue.shift()) {
      const name = lookupName(row, ids);
      const models = name ? await lookup(name) : null;
      out.set(row.authIndex, models ?? row.config?.models ?? []);
    }
  };
  await Promise.all(Array.from({ length: PARALLEL_LOOKUPS }, work));
  return out;
}

// The models of rows, or null until the first lookup ends.
export function useChannelModels(rows: ChannelRow[], ids: Record<string, string>): ChannelModels | null {
  const [models, setModels] = useState<ChannelModels | null>(null);
  const latest = useRef({ rows, ids });
  latest.current = { rows, ids };
  // Refreshed rows of the same credentials ask nothing again.
  const signature = rows.map((row) => `${row.authIndex}=${lookupName(row, ids)}`).join(",");
  useEffect(() => {
    let live = true;
    void loadModels(latest.current.rows, latest.current.ids).then((next) => { if (live) setModels(next); });
    return () => { live = false; };
  }, [signature]);
  return models;
}

// A channel's type: where CPA keeps the credential and its provider, with
// every OpenAI-compatible provider as one type. "" for a channel CPA does
// not list.
export function channelType(row: ChannelRow): string {
  if (row.config) return `ai_providers:${row.config.kind === "openai-compatibility" ? "openai-compatibility" : row.provider}`;
  if (row.auth) return `auth_files:${row.provider}`;
  return "";
}

export const ALL = "all";

export function channelTypes(rows: ChannelRow[]): string[] {
  return [...new Set(rows.map(channelType).filter(Boolean))].sort();
}

// The models the channels serve, each with how many of them serve it.
export function modelCounts(authIndexes: string[], models: ChannelModels): { model: string; count: number }[] {
  const counts = new Map<string, number>();
  for (const authIndex of authIndexes) {
    for (const model of models.get(authIndex) ?? []) counts.set(model, (counts.get(model) ?? 0) + 1);
  }
  return [...counts].map(([model, count]) => ({ model, count })).sort((a, b) => a.model.localeCompare(b.model));
}

// A model's name as the plugin's statistics key it: lower case, without a
// reasoning suffix such as "(high)" (see modelKey in internal/channel).
export function modelKey(name: string): string {
  let key = name.trim();
  const open = key.lastIndexOf("(");
  if (key.endsWith(")") && open > 0) key = key.slice(0, open).trim();
  return key.toLowerCase();
}

export function servesModel(authIndex: string, model: string, models: ChannelModels | null): boolean {
  if (model === ALL) return true;
  const key = modelKey(model);
  return Boolean(models?.get(authIndex)?.some((name) => modelKey(name) === key));
}

// The name CPA lists for a model a link gave in another spelling, such as
// the statistics' lower case; the name itself when no channel lists it.
export function canonicalModel(models: ChannelModels | null, model: string): string {
  if (model === ALL || !models) return model;
  const key = modelKey(model);
  for (const names of models.values()) {
    const found = names.find((name) => modelKey(name) === key);
    if (found) return found;
  }
  return model;
}

export function filterChannels(rows: ChannelRow[], type: string, model: string, models: ChannelModels | null): ChannelRow[] {
  return rows.filter((row) => (type === ALL || channelType(row) === type) && servesModel(row.authIndex, model, models));
}
