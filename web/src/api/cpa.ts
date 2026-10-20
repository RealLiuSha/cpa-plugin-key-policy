import axios from "axios";
import { apiClient } from "./client";

// CPA's own management API, called with the same management key. Credential
// lists carry upstream API keys, proxy URLs with passwords and auth headers;
// every response is projected field by field right after it arrives, so
// those values never reach React state.

const MANAGEMENT = "/v0/management";

export type ConfigKind = "codex" | "claude" | "gemini" | "interactions" | "xai" | "meta" | "vertex" | "openai-compatibility";

const KEY_LISTS = ["codex", "claude", "gemini", "interactions", "xai", "meta", "vertex"] as const;

// One credential from CPA's config lists. authIndex is "" when CPA has not
// loaded the credential; position is its 1-based place in its CPA list, and
// keyPosition its place among the api-key-entries of an OpenAI-compatible
// provider that has several.
export interface CpaConfigCredential {
  kind: ConfigKind;
  provider: string;
  name: string;
  authIndex: string;
  baseUrl: string;
  models: string[];
  excludedModels: string[];
  priority?: number;
  weight?: number;
  prefix: string;
  disabled: boolean;
  position: number;
  keyPosition?: number;
}

export interface CpaCooldown {
  scope: string;
  model: string;
  reason: string;
  retryAt: string;
}

// One OAuth credential file from /auth-files with its live state.
export interface CpaAuthFile {
  id: string;
  authIndex: string;
  name: string;
  provider: string;
  label: string;
  status: string;
  statusMessage: string;
  disabled: boolean;
  unavailable: boolean;
  nextRetryAfter: string;
  cooldowns: CpaCooldown[];
  success: number;
  failed: number;
}

// A plugin CPA lists; the search page only asks whether one is installed.
export interface CpaPlugin {
  id: string;
}

type Json = Record<string, unknown>;

function record(value: unknown): Json {
  return value && typeof value === "object" && !Array.isArray(value) ? value as Json : {};
}

function list(value: unknown): unknown[] {
  return Array.isArray(value) ? value : [];
}

function text(value: unknown): string {
  return typeof value === "string" ? value.trim() : "";
}

function count(value: unknown): number {
  return typeof value === "number" && Number.isFinite(value) ? value : 0;
}

function optionalNumber(value: unknown): number | undefined {
  return typeof value === "number" && Number.isFinite(value) ? value : undefined;
}

function strings(value: unknown): string[] {
  return list(value).map(text).filter(Boolean);
}

// Model objects are read for their name only.
function modelNames(value: unknown): string[] {
  return list(value).map((model) => text(record(model).name)).filter(Boolean);
}

export function fromKeyList(kind: (typeof KEY_LISTS)[number], payload: unknown): CpaConfigCredential[] {
  return list(record(payload)[`${kind}-api-key`]).map((item, index) => {
    const entry = record(item);
    return {
      kind,
      // The provider CPA reports in usage for credentials of this list.
      provider: kind === "interactions" ? "gemini-interactions" : kind,
      name: "",
      authIndex: text(entry["auth-index"]),
      baseUrl: text(entry["base-url"]),
      models: modelNames(entry.models),
      excludedModels: strings(entry["excluded-models"]),
      priority: optionalNumber(entry.priority),
      weight: optionalNumber(entry.weight),
      prefix: text(entry.prefix),
      disabled: false,
      position: index + 1,
    };
  });
}

// An OpenAI-compatible provider is one credential per api-key-entries item,
// or a single keyless credential when it has none.
export function fromOpenAICompatibility(payload: unknown): CpaConfigCredential[] {
  return list(record(payload)["openai-compatibility"]).flatMap((item, index) => {
    const entry = record(item);
    const shared = {
      kind: "openai-compatibility" as const,
      provider: text(entry.name) || "openai-compatibility",
      name: text(entry.name),
      baseUrl: text(entry["base-url"]),
      models: modelNames(entry.models),
      excludedModels: [],
      priority: optionalNumber(entry.priority),
      prefix: text(entry.prefix),
      disabled: entry.disabled === true,
      position: index + 1,
    };
    const keys = list(entry["api-key-entries"]).map(record);
    if (keys.length === 0) return [{ ...shared, authIndex: text(entry["auth-index"]) }];
    return keys.map((key, keyIndex) => ({
      ...shared,
      authIndex: text(key["auth-index"]),
      weight: optionalNumber(key.weight),
      keyPosition: keys.length > 1 ? keyIndex + 1 : undefined,
    }));
  });
}

export function fromAuthFiles(payload: unknown): CpaAuthFile[] {
  return list(record(payload).files).map((item) => {
    const file = record(item);
    return {
      id: text(file.id),
      authIndex: text(file.auth_index),
      name: text(file.name),
      provider: (text(file.provider) || text(file.type)).toLowerCase(),
      label: text(file.label),
      status: text(file.status),
      statusMessage: text(file.status_message),
      disabled: file.disabled === true,
      unavailable: file.unavailable === true,
      nextRetryAfter: text(file.next_retry_after),
      cooldowns: list(file.cooldowns).map((value) => {
        const cooldown = record(value);
        return { scope: text(cooldown.scope), model: text(cooldown.model_key), reason: text(cooldown.reason), retryAt: text(cooldown.retry_at) };
      }),
      success: count(file.success),
      failed: count(file.failed),
    };
  });
}

export function fromPlugins(payload: unknown): CpaPlugin[] {
  return list(record(payload).plugins).map((item) => ({ id: text(record(item).id) }));
}

// An older CPA without one of the lists answers 404; that list is empty.
async function getOptional(path: string): Promise<unknown> {
  try {
    const { data } = await apiClient().get<unknown>(MANAGEMENT + path);
    return data;
  } catch (error) {
    if (axios.isAxiosError(error) && error.response?.status === 404) return {};
    throw error;
  }
}

// The models CPA routes to one credential, as its model registry has them:
// the names clients ask for, with excluded models dropped and a provider's
// default models filled in. name is an auth file's name or a credential ID.
export async function fetchCredentialModels(name: string): Promise<string[]> {
  const { data } = await apiClient().get<unknown>(MANAGEMENT + "/auth-files/models", { params: { name } });
  return list(record(data).models).map((model) => text(record(model).id)).filter(Boolean);
}

export interface CpaCredentials {
  config: CpaConfigCredential[];
  authFiles: CpaAuthFile[];
}

// The key lists come before the OpenAI-compatible providers: of two
// credentials sharing an auth_index, mergeChannels keeps the first.
export async function fetchCpaCredentials(): Promise<CpaCredentials> {
  const [compat, files, ...lists] = await Promise.all([
    getOptional("/openai-compatibility"),
    getOptional("/auth-files"),
    ...KEY_LISTS.map((kind) => getOptional(`/${kind}-api-key`)),
  ]);
  return {
    config: [...KEY_LISTS.flatMap((kind, index) => fromKeyList(kind, lists[index])), ...fromOpenAICompatibility(compat)],
    authFiles: fromAuthFiles(files),
  };
}

export async function fetchCpaPlugins(): Promise<CpaPlugin[]> {
  const { data } = await apiClient().get<unknown>(MANAGEMENT + "/plugins");
  return fromPlugins(data);
}

// The plugin's YAML block as CPA returns it. It can hold the plugin's own
// secrets, so callers pass it on and do not keep it.
export async function fetchPluginConfig(id: string): Promise<Record<string, unknown>> {
  const { data } = await apiClient().get<unknown>(`${MANAGEMENT}/plugins/${encodeURIComponent(id)}/config`);
  return record(data);
}

// CPA merges the fields into the plugin's YAML block, saves it and applies
// the configuration to every plugin.
export async function patchPluginConfig(id: string, fields: Record<string, unknown>): Promise<void> {
  await apiClient().patch(`${MANAGEMENT}/plugins/${encodeURIComponent(id)}/config`, fields);
}
