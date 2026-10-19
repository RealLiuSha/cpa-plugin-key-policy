import axios from "axios";
import { apiClient, pluginPath } from "./client";
import type { AdapterRule, AdapterRuleType } from "../types";

export interface AdaptersResponse {
  rules: AdapterRule[];
  types: AdapterRuleType[];
}

export async function fetchAdapters(): Promise<AdaptersResponse> {
  const { data } = await apiClient().get<Partial<AdaptersResponse>>(pluginPath("/adapters"));
  return { rules: data.rules ?? [], types: data.types ?? [] };
}

// An empty id creates the rule; otherwise it replaces the rule with that id.
export async function saveAdapter(rule: AdapterRule): Promise<AdapterRule> {
  const { data } = await apiClient().post<{ rule: AdapterRule }>(pluginPath("/adapters"), rule);
  return data.rule;
}

export async function deleteAdapter(id: string): Promise<void> {
  await apiClient().delete(pluginPath("/adapters"), { params: { id } });
}

export interface AdapterPreview {
  changed: boolean;
  body: unknown;
}

// Runs the rule, enabled or not, on a sample JSON body.
export async function previewAdapter(rule: AdapterRule, body: unknown): Promise<AdapterPreview> {
  const { data } = await apiClient().post<AdapterPreview>(pluginPath("/adapters/preview"), { rule, body });
  return data;
}

// The upstream's answer to one real request: the saved rule applied to the
// body, sent through CPA pinned to the rule's channel. body is the response
// text, cut at 4096 characters when truncated.
export interface AdapterTryResult {
  status_code: number;
  latency_ms: number;
  auth_id?: string;
  body: string;
  truncated: boolean;
}

// The plugin cannot bound the upstream call, so the page stops waiting after
// TRY_TIMEOUT_MS. Closing the request (the timeout or signal) makes CPA cancel
// the upstream call, which runs in the management request's context.
export const TRY_TIMEOUT_MS = 300_000;

export async function tryAdapter(ruleId: string, body: unknown, signal?: AbortSignal): Promise<AdapterTryResult> {
  const { data } = await apiClient().post<AdapterTryResult>(pluginPath("/adapters/try"), { rule_id: ruleId, body }, { timeout: TRY_TIMEOUT_MS, signal });
  return data;
}

// axios gave up waiting: ECONNABORTED, or ETIMEDOUT with clarifyTimeoutError.
export function isTryTimeout(error: unknown): boolean {
  return axios.isAxiosError(error) && (error.code === "ECONNABORTED" || error.code === "ETIMEDOUT");
}

// 409 channel_not_seen: the plugin has not seen traffic on the channel yet,
// so it cannot pin the request to that credential.
export function isChannelNotSeen(error: unknown): boolean {
  if (!error || typeof error !== "object") return false;
  const response = (error as { response?: { status?: number; data?: { error?: { code?: unknown } } } }).response;
  return response?.data?.error?.code === "channel_not_seen";
}
