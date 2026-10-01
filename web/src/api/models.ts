import axios from "axios";
import { apiClient } from "./client";
import type { CatalogModel } from "../types";

// CPA has no single "list providers+models" endpoint. The catalog is composed
// from its management DTOs: OpenAI-compatibility providers, auth files with
// their per-file models, and the static model definitions of configured
// channels.

const STATIC_CHANNELS = [
  "claude",
  "gemini",
  "vertex",
  "aistudio",
  "codex",
  "kimi",
  "antigravity",
  "xai",
] as const;

// Map the per-channel API-key management endpoints to the provider identity
// used in the catalog. The endpoint returns its key list under a top-level
// key named like the channel (e.g. { "gemini-api-key": [...] }). A non-empty
// list means the user has configured at least one API-key credential.
const API_KEY_CHANNELS: Record<string, string> = {
  "gemini-api-key": "gemini",
  "claude-api-key": "claude",
  "codex-api-key": "codex",
  "vertex-api-key": "vertex",
};

export interface RawEntry {
  provider: string;
  models: string[];
  // Static model definitions describe what a channel can serve, not what a
  // credential is configured for; they are filtered by configured providers.
  static?: boolean;
}

// One row per (provider, model), lowercased provider, de-duplicated
// case-insensitively and sorted by provider then model.
export function normalizeCatalog(entries: RawEntry[]): CatalogModel[] {
  const seen = new Set<string>();
  const out: CatalogModel[] = [];
  for (const entry of entries) {
    const provider = entry.provider.trim().toLowerCase();
    if (!provider) continue;
    for (const raw of entry.models) {
      const model = raw.trim();
      if (!model) continue;
      const key = provider + "\u0000" + model.toLowerCase();
      if (seen.has(key)) continue;
      seen.add(key);
      out.push({ provider, model });
    }
  }
  out.sort((a, b) => a.provider.localeCompare(b.provider) || a.model.toLowerCase().localeCompare(b.model.toLowerCase()));
  return out;
}

function objectValue(value: unknown, source: string): Record<string, unknown> {
  if (!value || typeof value !== "object" || Array.isArray(value)) {
    throw new Error(`${source} must be an object`);
  }
  return value as Record<string, unknown>;
}

function arrayValue(value: unknown, source: string): unknown[] {
  if (value === null) return [];
  if (!Array.isArray(value)) throw new Error(`${source} must be an array`);
  return value;
}

function stringValue(value: unknown, source: string): string {
  if (typeof value !== "string" || value.trim() === "") {
    throw new Error(`${source} must be a non-empty string`);
  }
  return value.trim();
}

export function fromOpenAICompat(payload: unknown): RawEntry[] {
  const root = objectValue(payload, "openai-compatibility response");
  return arrayValue(root["openai-compatibility"], "openai-compatibility").map((item, index) => {
    const entry = objectValue(item, `openai-compatibility[${index}]`);
    const provider = stringValue(entry.name, `openai-compatibility[${index}].name`);
    const models = arrayValue(entry.models, `openai-compatibility[${index}].models`).map((model, modelIndex) => {
      const definition = objectValue(model, `openai-compatibility[${index}].models[${modelIndex}]`);
      return stringValue(definition.name, `openai-compatibility[${index}].models[${modelIndex}].name`);
    });
    return { provider, models };
  });
}

// One auth-file row from /v0/management/auth-files: the file name joins its
// per-file models, and the provider comes from this list endpoint because the
// per-file models payload only reports each model's upstream format.
export interface AuthFileMeta {
  name: string;
  provider: string;
}

export function fromAuthFiles(payload: unknown): AuthFileMeta[] {
  const root = objectValue(payload, "auth-files response");
  return arrayValue(root.files, "auth-files.files").map((item, index) => {
    const entry = objectValue(item, `auth-files.files[${index}]`);
    return {
      name: stringValue(entry.name, `auth-files.files[${index}].name`),
      provider: stringValue(entry.provider, `auth-files.files[${index}].provider`).toLowerCase(),
    };
  });
}

export function fromAuthFileModels(provider: string, payload: unknown): RawEntry {
  const root = objectValue(payload, "auth-file models response");
  const models = arrayValue(root.models, "auth-file models.models").map((model, index) => {
    const entry = objectValue(model, `auth-file models.models[${index}]`);
    return stringValue(entry.id, `auth-file models.models[${index}].id`);
  });
  return { provider, models };
}

export function fromModelDefinitions(channel: string, payload: unknown): RawEntry {
  const root = objectValue(payload, "model-definitions response");
  const responseChannel = stringValue(root.channel, "model-definitions.channel").toLowerCase();
  if (responseChannel !== channel.toLowerCase()) {
    throw new Error(`model-definitions channel mismatch: got ${responseChannel}, want ${channel}`);
  }
  const models = arrayValue(root.models, "model-definitions.models").map((model, index) => {
    const entry = objectValue(model, `model-definitions.models[${index}]`);
    return stringValue(entry.id, `model-definitions.models[${index}].id`);
  });
  return { provider: responseChannel, models, static: true };
}

// CPA's Get<Key> handlers return 200 with a "<channel>-api-key" array that is
// empty when nothing is configured, so the body decides whether the channel
// counts as configured.
function apiKeyProviderIfConfigured(endpoint: string, payload: unknown): string {
  const provider = API_KEY_CHANNELS[endpoint];
  if (!provider) return "";
  const root = objectValue(payload, `${endpoint} response`);
  return arrayValue(root[endpoint], `${endpoint} response.${endpoint}`).length > 0 ? provider : "";
}

// Static channel definitions are shown only for providers the user has a
// credential for, or that an existing model already uses.
export function filterByConfigured(entries: RawEntry[], configured: Set<string>, inUse: Set<string>): RawEntry[] {
  return entries.filter((entry) => {
    if (!entry.static) return true;
    const provider = entry.provider.toLowerCase();
    return configured.has(provider) || inUse.has(provider);
  });
}

// inUseProviders keeps static definitions of providers that existing models
// already route to visible even if their credential was removed.
export async function fetchCatalog(inUseProviders?: Set<string>): Promise<CatalogModel[]> {
  const c = apiClient();
  const entries: RawEntry[] = [];
  const configured = new Set<string>();
  const inUse = new Set<string>();
  for (const provider of inUseProviders ?? []) inUse.add(provider.toLowerCase());

  const compatResponse = await c.get("/v0/management/openai-compatibility");
  const compatEntries = fromOpenAICompat(compatResponse.data);
  for (const entry of compatEntries) configured.add(entry.provider.toLowerCase());
  entries.push(...compatEntries);

  for (const channel of Object.keys(API_KEY_CHANNELS)) {
    const response = await c.get("/v0/management/" + channel);
    const provider = apiKeyProviderIfConfigured(channel, response.data);
    if (provider) configured.add(provider);
  }

  const authFilesResponse = await c.get("/v0/management/auth-files");
  const metas = fromAuthFiles(authFilesResponse.data);
  for (const meta of metas) configured.add(meta.provider);
  const perFile = await Promise.all(
    metas.map(async (meta) => {
      const response = await c.get("/v0/management/auth-files/models", { params: { name: meta.name } });
      return fromAuthFileModels(meta.provider, response.data);
    }),
  );
  entries.push(...perFile);

  for (const channel of STATIC_CHANNELS) {
    try {
      const response = await c.get("/v0/management/model-definitions/" + channel);
      entries.push(fromModelDefinitions(channel, response.data));
    } catch (error) {
      if (
        axios.isAxiosError(error) &&
        error.response?.status === 400 &&
        objectValue(error.response.data, "model-definitions error").error === "unknown channel"
      ) {
        continue;
      }
      throw error;
    }
  }
  return normalizeCatalog(filterByConfigured(entries, configured, inUse));
}
