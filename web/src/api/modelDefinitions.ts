import { apiClient, pluginPath } from "./client";
import type {
  ModelDefinition,
  ModelImportItem,
  ModelImportResult,
  PriceImportRequest,
  PriceImportResult,
  PricingPreview,
} from "../types";

export async function fetchModelDefinitions(): Promise<ModelDefinition[]> {
  const { data } = await apiClient().get<{ models: ModelDefinition[] }>(pluginPath("/models"));
  return data.models ?? [];
}

export async function upsertModelDefinition(model: ModelDefinition): Promise<ModelDefinition> {
  const body: ModelDefinition = {
    name: model.name,
    targets: model.targets,
    dispatch: model.dispatch,
    billing_mode: model.billing_mode,
    free: model.free,
    billing_multiplier: model.billing_multiplier ?? 1,
    input_price_per_million: model.input_price_per_million,
    output_price_per_million: model.output_price_per_million,
    cache_read_price_per_million: model.cache_read_price_per_million,
    per_call_usd: model.per_call_usd,
    ...(model.cache_write_price_per_million === undefined ? {} : { cache_write_price_per_million: model.cache_write_price_per_million }),
  };
  const { data } = await apiClient().post<{ model: ModelDefinition }>(pluginPath("/models"), body);
  return data.model;
}

export async function deleteModelDefinition(name: string): Promise<void> {
  await apiClient().delete(pluginPath("/models"), { data: { name } });
}

export async function importModelPrices(body: PriceImportRequest): Promise<PriceImportResult> {
  const { data } = await apiClient().post<PriceImportResult>(pluginPath("/models/import-prices"), body);
  return data;
}

function asRecord(value: unknown, source: string): Record<string, unknown> {
  if (!value || typeof value !== "object" || Array.isArray(value)) {
    throw new Error(`${source} must be an object`);
  }
  return value as Record<string, unknown>;
}

function asString(value: unknown): string {
  return typeof value === "string" ? value : "";
}

function asNumber(value: unknown): number {
  return typeof value === "number" && Number.isFinite(value) ? value : 0;
}

function asOptionalNumber(value: unknown): number | undefined {
  return typeof value === "number" && Number.isFinite(value) ? value : undefined;
}

function asStringArray(value: unknown): string[] {
  return Array.isArray(value) ? value.filter((item): item is string => typeof item === "string") : [];
}

export async function previewModelPrices(models: string[]): Promise<PricingPreview> {
  const { data } = await apiClient().post<unknown>(pluginPath("/models/pricing-preview"), { models });
  const root = asRecord(data, "pricing preview");
  const matches = Array.isArray(root.matches) ? root.matches.map((item, index) => {
    const match = asRecord(item, `matches[${index}]`);
    return {
      model: asString(match.model),
      matched_model: asString(match.matched_model),
      match_type: asString(match.match_type),
      source: asString(match.source),
      source_url: asString(match.source_url),
      source_provider_id: asString(match.source_provider_id),
      source_provider_name: asString(match.source_provider_name),
      prompt_price_per_1m: asNumber(match.prompt_price_per_1m),
      completion_price_per_1m: asNumber(match.completion_price_per_1m),
      cache_read_price_per_1m: asNumber(match.cache_read_price_per_1m),
      cache_write_price_per_1m: asOptionalNumber(match.cache_write_price_per_1m),
    };
  }) : [];
  return {
    source: asString(root.source),
    source_url: asString(root.source_url),
    metadata_models: asNumber(root.metadata_models),
    matches,
    unmatched_models: asStringArray(root.unmatched_models),
  };
}

export async function importModels(body: { dry_run: boolean; items: ModelImportItem[] }): Promise<ModelImportResult> {
  const { data } = await apiClient().post<unknown>(pluginPath("/models/import"), body);
  const root = asRecord(data, "model import");
  const rows = (value: unknown, source: string): ModelImportResult["created"] => {
    if (!Array.isArray(value)) return [];
    return value.map((item, index) => {
      const row = asRecord(item, `${source}[${index}]`);
      return {
        name: asString(row.name),
        action: asString(row.action),
        reason: asString(row.reason) || undefined,
        affected_keys: asStringArray(row.affected_keys),
        duplicate: row.duplicate === true,
        missing_price: row.missing_price === true,
        price_conflict: row.price_conflict === true,
      };
    });
  };
  return {
    created: rows(root.created, "created"),
    updated: rows(root.updated, "updated"),
    skipped: rows(root.skipped, "skipped"),
    conflicts: rows(root.conflicts, "conflicts"),
    missing_price: rows(root.missing_price, "missing_price"),
    affected_keys: asStringArray(root.affected_keys),
  };
}
