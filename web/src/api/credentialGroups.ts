import { apiClient, pluginPath } from "./client";
import type { ClassifyPreviewResponse, ClassifyRule, CredentialDescriptor } from "../types";
import { readPlanType } from "./models";

export async function fetchClassifyRules(): Promise<ClassifyRule[]> {
  const { data } = await apiClient().get<{ rules: ClassifyRule[] }>(pluginPath("/classify-rules"));
  return data.rules ?? [];
}

export async function upsertClassifyRule(rule: ClassifyRule): Promise<ClassifyRule> {
  const { data } = await apiClient().post<{ rule: ClassifyRule }>(pluginPath("/classify-rules"), rule);
  return data.rule;
}

export async function deleteClassifyRule(name: string): Promise<void> {
  await apiClient().delete(pluginPath("/classify-rules"), { data: { name } });
}

export async function reorderClassifyRules(names: string[]): Promise<void> {
  await apiClient().post(pluginPath("/classify-rules/reorder"), { names });
}

export async function classifyPreview(
  descriptors: CredentialDescriptor[],
  rules?: ClassifyRule[],
): Promise<ClassifyPreviewResponse> {
  const body: Record<string, unknown> = { descriptors };
  if (rules && rules.length > 0) body.rules = rules;
  const { data } = await apiClient().post<ClassifyPreviewResponse>(pluginPath("/classify-preview"), body);
  return data;
}

// Build the preview descriptors from CPA's auth-file list. The shared
// plan-type reader keeps catalog grouping and credential grouping consistent.
export async function fetchCredentialDescriptors(): Promise<CredentialDescriptor[]> {
  const { data } = await apiClient().get<unknown>("/v0/management/auth-files");
  const root = data as Record<string, unknown> | null;
  const list = root?.["files"] ?? root?.["auth-files"];
  if (!Array.isArray(list)) return [];

  const descriptors: CredentialDescriptor[] = [];
  for (const item of list) {
    const value = (item ?? {}) as Record<string, unknown>;
    const id = ((value["id"] as string) ?? (value["name"] as string) ?? "").trim();
    if (!id) continue;
    const provider = ((value["provider"] as string) ?? (value["type"] as string) ?? "").trim().toLowerCase();
    const attributes: Record<string, string> = {};
    const planType = readPlanType(value);
    if (planType) attributes["plan_type"] = planType;
    const tier = value["tier"];
    if (typeof tier === "string" && tier.trim()) attributes["tier"] = tier.trim().toLowerCase();
    descriptors.push({ id, provider, attributes });
  }
  return descriptors;
}
