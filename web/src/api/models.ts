import axios from "axios";
import { apiClient, pluginPath } from "./client";
import type { CatalogModel } from "../types";

/** Prefix applied to custom credential groups in catalog entries / ModelTarget.group. */
export const CLASSIFY_GROUP_PREFIX = "classify:";

/** True when group is a custom classify group (classify:name). */
export function isClassifyGroup(group: string | undefined | null): boolean {
  return !!group && group.toLowerCase().startsWith(CLASSIFY_GROUP_PREFIX);
}

/**
 * Human label for a catalog/model-target group. Custom classify groups render as
 * "自定义 · name" (via picker.tier.classify); built-in tiers use picker.tier.*.
 */
export function formatTierLabel(
  t: (k: string, v?: Record<string, string | number>) => string,
  group: string,
): string {
  if (isClassifyGroup(group)) {
    const name = group.slice(CLASSIFY_GROUP_PREFIX.length);
    return t("picker.tier.classify", { name });
  }
  const key = "picker.tier." + group;
  const translated = t(key);
  return translated === key ? group : translated;
}

// CPA has no single "list providers+models" endpoint. We compose its current
// management DTOs and send auth-file credentials through the plugin catalog.

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

// Providers whose auth files carry a tier/plan identity claim. Only these get
// tier subgroups in the picker (codex via id_token plan_type, antigravity via
// tier_id). Everything else stays a flat per-provider group — there's no
// meaningful "tier" to split on, and the plugin Scheduler won't filter them.
const TIERED_PROVIDERS = new Set(["codex", "antigravity"]);

// "supported" is the synthetic group for a tiered-provider auth file whose
// identity claim is absent.
// It must NOT be confused with a real tier: a key pinned to "team" never lands
// on a "supported" file, and vice versa. The plugin Scheduler treats
// "supported"/"unknown" as the untiered bucket.
// Map the per-channel API-key management endpoints to the provider identity
// used in the catalog. The endpoint returns its key list under a top-level
// key named like the channel (e.g. { "gemini-api-key": [...] }); the provider
// group in the picker is the bare name ("gemini"). A non-empty list means the
// user has configured at least one API-key credential for that provider.
const API_KEY_CHANNELS: Record<string, string> = {
  "gemini-api-key": "gemini",
  "claude-api-key": "claude",
  "codex-api-key": "codex",
  "vertex-api-key": "vertex",
};

export interface RawEntry {
  provider: string;
  models: string[];
  group?: string;
}

// Collect (provider, [group], model) tuples from heterogeneous CPA responses.
// Within a single (provider, group) bucket, duplicate models are de-duplicated
// — same model supported by multiple same-tier auth files appears once. A model
// supported by BOTH "free" and "team" tiers appears as two separate rows so the
// user can authorize it under a specific tier (real isolation via Scheduler).
export function normalizeCatalog(entries: RawEntry[]): CatalogModel[] {
  const seen = new Set<string>();
  const out: CatalogModel[] = [];
  for (const e of entries) {
    const provider = e.provider.trim().toLowerCase();
    if (!provider) continue;
    const group = (e.group ?? "").trim().toLowerCase();
    for (const m of e.models) {
      const model = m.trim();
      if (!model) continue;
      const key = provider + "" + group + "" + model.toLowerCase();
      if (seen.has(key)) continue;
      seen.add(key);
      const row: CatalogModel = { provider, model };
      if (group) row.group = group;
      out.push(row);
    }
  }
  // Stable sort: provider, then group (empty group sorts first within provider),
  // then model (case-insensitive).
  out.sort((a, b) => {
    if (a.provider !== b.provider) return a.provider.localeCompare(b.provider);
    const ga = a.group ?? "";
    const gb = b.group ?? "";
    if (ga !== gb) return ga.localeCompare(gb);
    return a.model.toLowerCase().localeCompare(b.model.toLowerCase());
  });
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

// One auth-file row from /v0/management/auth-files. We only need the file name
// (to join against per-file models), the provider, and Codex's flat
// id_token.plan_type claim. Everything else is ignored.
export interface AuthFileMeta {
  name: string;
  // provider as reported by the auth-files LIST endpoint (e.g. "codex",
  // "antigravity", "claude"). The per-file /auth-files/models endpoint does NOT
  // echo a provider/channel — its models objects carry a per-model "type"
  // ("openai" for codex-backed models) which is NOT the auth provider. We must
  // carry the list-endpoint provider here so each file's models land under the
  // right provider group; otherwise the file name leaks in as the "provider".
  provider: string;
  planType: string;
}

export function fromAuthFiles(payload: unknown): AuthFileMeta[] {
  const root = objectValue(payload, "auth-files response");
  return arrayValue(root.files, "auth-files.files").map((item, index) => {
    const entry = objectValue(item, `auth-files.files[${index}]`);
    return {
      name: stringValue(entry.name, `auth-files.files[${index}].name`),
      provider: stringValue(entry.provider, `auth-files.files[${index}].provider`).toLowerCase(),
      planType: readPlanType(entry),
    };
  });
}

// Extract the tier/plan identity from an auth-files list entry. codex's
// ListAuthFiles flattens Codex claims directly onto id_token. Antigravity tier
// identity is not exposed by the current list DTO and therefore falls into the
// plugin's supported bucket.
// Returns "" when no recognizable claim is present (→ "supported" bucket).
// Exported for unit testing against real ListAuthFiles payloads.
export function readPlanType(entry: Record<string, unknown>): string {
  const idToken = entry["id_token"];
  if (idToken && typeof idToken === "object") {
    const tok = idToken as Record<string, unknown>;
    const plan = tok["plan_type"];
    if (typeof plan === "string" && plan.trim() !== "") {
      return plan.trim().toLowerCase();
    }
  }
  return "";
}

// Build a RawEntry from a per-file /auth-files/models response. The provider
// comes from the LIST endpoint (carried in `provider`), NOT the models
// payload — the models objects report a per-model "type" ("openai" for codex
// backed models) which is the upstream format, not the auth provider, and the
// response itself has no top-level channel/provider field.
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
  return { provider: responseChannel, models };
}

function fromPluginCatalog(payload: unknown): RawEntry[] {
  const root = objectValue(payload, "plugin catalog response");
  return arrayValue(root.entries, "plugin catalog.entries").map((item, index) => {
    const entry = objectValue(item, `plugin catalog.entries[${index}]`);
    const provider = stringValue(entry.provider, `plugin catalog.entries[${index}].provider`);
    const models = arrayValue(entry.models, `plugin catalog.entries[${index}].models`).map((model, modelIndex) =>
      stringValue(model, `plugin catalog.entries[${index}].models[${modelIndex}]`),
    );
    const group = entry.group === undefined
      ? undefined
      : stringValue(entry.group, `plugin catalog.entries[${index}].group`);
    return group ? { provider, group, models } : { provider, models };
  });
}

// Is the *-api-key list at `endpoint` non-empty? CPA's Get<Key> handlers return
// 200 with a top-level "<channel>-api-key": [...] array (nil/empty when none
// configured), NOT a 404, so we must inspect the body to know whether a
// credential is actually present. Returns the mapped provider name when at
// least one key exists, else "".
function apiKeyProviderIfConfigured(endpoint: string, payload: unknown): string {
  const provider = API_KEY_CHANNELS[endpoint];
  if (!provider) return "";
  const root = objectValue(payload, `${endpoint} response`);
  return arrayValue(root[endpoint], `${endpoint} response.${endpoint}`).length > 0 ? provider : "";
}

// Filter the collected raw entries down to those that should be visible in the
// picker. Bare (group-less) static entries — from model-definitions/<channel> —
// are dropped when EITHER:
//   1. They're a tiered provider (codex, antigravity) that the auth-files pass
//      already contributed tier subgroups for, so the bare row is a duplicate
//      with no backing auth file (the "codex · team" subgroup is the real one).
//   2. Their provider is neither configured (no credential) nor has a
//      currently-selected model — i.e. an unconfigured channel that should be
//      hidden from the picker.
// Entries carrying a `group` are auth-file sourced and already imply a
// configured credential, so they're kept unconditionally.
// Exported for unit testing.
export function filterByConfigured(
  entries: RawEntry[],
  configured: Set<string>,
  selected: Set<string>,
  tieredFromAuth: Set<string>,
): RawEntry[] {
  const out: RawEntry[] = [];
  for (const e of entries) {
    const provider = (e.provider ?? "").toLowerCase();
    if (e.group === undefined) {
      // Bare static entry (from model-definitions/<channel>).
      // Drop when it's a tiered provider already covered by auth-files (the
      // tier subgroups are the real, backed rows; this bare one is a dup with
      // no auth file behind it)...
      const dupOfTiered =
        TIERED_PROVIDERS.has(provider) && tieredFromAuth.has(provider);
      // ...or when the provider is neither configured nor has a selected model
      // (unconfigured channel should be hidden, but an edited key's rows stay
      // visible so the user can uncheck them).
      const unconfigured =
        !configured.has(provider) && !selected.has(provider);
      if (dupOfTiered || unconfigured) continue;
    }
    out.push(e);
  }
  return out;
}

// `selectedProviders` is the set of providers (lowercased) the caller already
// has model rules for (edit-mode prefill). Providers in this set stay visible
// even when unconfigured, so the user can see and uncheck their rows. New-key
// mode passes nothing (empty set) and only configured channels appear.
export async function fetchCatalog(
  selectedProviders?: Set<string>,
): Promise<CatalogModel[]> {
  const c = apiClient();
  const entries: RawEntry[] = [];
  // Tiered providers that the auth-files path contributed models for. Filled
  // during the auth-files pass; read in filterByConfigured to suppress bare
  // static entries for providers already covered with tier subgroups.
  const authFileTieredProviders = new Set<string>();
  // Providers the user has actually configured a credential for (an OAuth auth
  // file is present, or a non-empty *-api-key list). Bare static-definition
  // entries for providers NOT in this set are hidden unless a model under them
  // is already selected (see filterByConfigured).
  const configuredProviders = new Set<string>();
  const selected = new Set<string>();
  for (const p of selectedProviders ?? []) selected.add(p.toLowerCase());

  const compatResponse = await c.get("/v0/management/openai-compatibility");
  const compatEntries = fromOpenAICompat(compatResponse.data);
  for (const entry of compatEntries) {
    configuredProviders.add(entry.provider.toLowerCase());
  }
  entries.push(...compatEntries);

  // Per-channel API-key endpoints. These responses carry their key list under a
  // top-level "<channel>-api-key" array (NOT under "models"/"keys"), so
  // fromChannelKey yields no models here — we use them solely to detect whether
  // the user has configured an API-key credential, marking the mapped provider
  // as configured when the list is non-empty.
  for (const ch of Object.keys(API_KEY_CHANNELS)) {
    const response = await c.get("/v0/management/" + ch);
    const provider = apiKeyProviderIfConfigured(ch, response.data);
    if (provider) configuredProviders.add(provider);
  }

  // auth-files: fetch file list + per-file models, then POST to the plugin
  // /catalog so classify rules + built-in tiers are applied server-side
  // (custom groups come back as classify:<name>). Compat/API-key channels
  // stay flat and are merged separately above/below.
  const authFilesResponse = await c.get("/v0/management/auth-files");
  const metas = fromAuthFiles(authFilesResponse.data);
  for (const meta of metas) configuredProviders.add(meta.provider);

  const perFile = await Promise.all(
    metas.map(async (meta) => {
      const response = await c.get("/v0/management/auth-files/models", { params: { name: meta.name } });
      return { meta, entry: fromAuthFileModels(meta.provider, response.data) };
    }),
  );
  const credentials = perFile
    .filter(({ entry }) => entry.models.length > 0)
    .map(({ meta, entry }) => ({
      id: meta.name,
      provider: meta.provider,
      attributes: meta.planType ? { plan_type: meta.planType } : undefined,
      models: entry.models,
    }));

  if (credentials.length > 0) {
    const catalogResponse = await c.post(pluginPath("/catalog"), { credentials });
    for (const entry of fromPluginCatalog(catalogResponse.data)) {
      const provider = entry.provider.toLowerCase();
      entries.push(entry);
      if (entry.group || TIERED_PROVIDERS.has(provider)) {
        authFileTieredProviders.add(provider);
      }
    }
  }

  for (const ch of STATIC_CHANNELS) {
    try {
      const response = await c.get("/v0/management/model-definitions/" + ch);
      entries.push(fromModelDefinitions(ch, response.data));
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

  const filtered = filterByConfigured(
    entries,
    configuredProviders,
    selected,
    authFileTieredProviders,
  );
  return normalizeCatalog(filtered);
}

// A picker group: a provider, optionally split by tier (codex free / team /
// supported…). When group is undefined the picker renders a flat provider
// group. When group is set, the provider's models are shown under a tier label;
// the plugin Scheduler honors the chosen tier at runtime.
export interface CatalogGroup {
  provider: string;
  group?: string;
  models: string[];
}

// Build picker groups from the normalized catalog. Adjacent (provider, group)
// buckets collapse into one group with their models merged (normalizeCatalog
// already de-duplicated within a bucket, so the merge is just concatenation).
export function groupByCatalog(catalog: CatalogModel[]): CatalogGroup[] {
  const map = new Map<string, CatalogGroup>();
  for (const c of catalog) {
    const group = c.group ?? "";
    const key = c.provider + "\0" + group;
    let bucket = map.get(key);
    if (!bucket) {
      bucket = { provider: c.provider, models: [] };
      if (group) bucket.group = group;
      map.set(key, bucket);
    }
    bucket.models.push(c.model);
  }
  return Array.from(map.values()).sort((a, b) => {
    if (a.provider !== b.provider) return a.provider.localeCompare(b.provider);
    return (a.group ?? "").localeCompare(b.group ?? "");
  });
}
