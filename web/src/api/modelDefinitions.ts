import { apiClient, pluginPath } from "./client";
import type { ModelDefinition, PriceImportRequest, PriceImportResult } from "../types";

export async function fetchModelDefinitions(): Promise<ModelDefinition[]> {
  const { data } = await apiClient().get<{ models: ModelDefinition[] }>(pluginPath("/models"));
  return data.models ?? [];
}

export async function upsertModelDefinition(model: ModelDefinition): Promise<ModelDefinition> {
  const { data } = await apiClient().post<{ model: ModelDefinition }>(pluginPath("/models"), model);
  return data.model;
}

export async function deleteModelDefinition(name: string): Promise<void> {
  await apiClient().delete(pluginPath("/models"), { data: { name } });
}

export async function importModelPrices(body: PriceImportRequest): Promise<PriceImportResult> {
  const { data } = await apiClient().post<PriceImportResult>(pluginPath("/models/import-prices"), body);
  return data;
}
