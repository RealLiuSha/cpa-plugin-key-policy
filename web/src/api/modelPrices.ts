import { apiClient, pluginPath } from "./client";
import type {
  ModelImportItem,
  ModelImportResult,
  ModelPrice,
  ModelPriceRow,
  ModelPricesResponse,
  PriceImportRequest,
  PriceImportResult,
  PricingPreview,
} from "../types";

export async function fetchModelPrices(): Promise<ModelPricesResponse> {
  const { data } = await apiClient().get<Partial<ModelPricesResponse>>(pluginPath("/models"));
  return { models: data.models ?? [], unpriced: data.unpriced ?? [] };
}

export async function saveModelPrice(price: ModelPrice): Promise<ModelPriceRow> {
  const body = {
    name: price.name,
    provider: price.provider,
    billing_mode: price.billing_mode,
    billing_multiplier: price.billing_multiplier ?? 1,
    input_price_per_million: price.input_price_per_million ?? 0,
    output_price_per_million: price.output_price_per_million ?? 0,
    cache_read_price_per_million: price.cache_read_price_per_million ?? 0,
    per_call_usd: price.per_call_usd ?? 0,
    ...(price.cache_write_price_per_million === undefined ? {} : { cache_write_price_per_million: price.cache_write_price_per_million }),
  };
  const { data } = await apiClient().post<{ model: ModelPriceRow }>(pluginPath("/models"), body);
  return data.model;
}

export async function deleteModelPrice(price: Pick<ModelPrice, "name" | "provider">): Promise<void> {
  await apiClient().delete(pluginPath("/models"), { data: { name: price.name, provider: price.provider } });
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

// Import creates $0 price rows and never overwrites an existing row.
export async function importModels(items: ModelImportItem[]): Promise<ModelImportResult> {
  const { data } = await apiClient().post<unknown>(pluginPath("/models/import"), { items });
  const root = asRecord(data, "model import");
  const rows = (value: unknown, source: string): ModelImportResult["created"] => {
    if (!Array.isArray(value)) return [];
    return value.map((item, index) => {
      const row = asRecord(item, `${source}[${index}]`);
      return { name: asString(row.name), reason: asString(row.reason) || undefined };
    });
  };
  return { created: rows(root.created, "created"), skipped: rows(root.skipped, "skipped") };
}
