import { apiClient, pluginPath } from "./client";
import type {
  KeyPublic,
  KeyWriteRequest,
  CreateKeyResponse,
  RotateKeyResponse,
  KeyUsageResponse,
  KeyHistoryResponse,
} from "../types";

export async function listKeys(): Promise<KeyPublic[]> {
  const c = apiClient();
  const { data } = await c.get<{ keys: KeyPublic[] }>(pluginPath("/keys"));
  return data.keys ?? [];
}

export async function createKey(
  req: KeyWriteRequest,
): Promise<CreateKeyResponse> {
  const c = apiClient();
  const { data } = await c.post<CreateKeyResponse>(pluginPath("/keys"), req);
  return data;
}

export async function patchKey(
  req: KeyWriteRequest,
): Promise<KeyPublic> {
  const c = apiClient();
  const { data } = await c.patch<{ key: KeyPublic }>(pluginPath("/keys"), req);
  return data.key;
}

export async function deleteKey(id: string): Promise<void> {
  const c = apiClient();
  await c.delete(pluginPath("/keys"), { params: { id } });
}

export async function rotateKey(id: string): Promise<RotateKeyResponse> {
  const c = apiClient();
  const { data } = await c.post<RotateKeyResponse>(
    pluginPath("/keys/rotate"),
    { id },
  );
  return data;
}

export async function resetRPM(id: string): Promise<void> {
  const c = apiClient();
  await c.post(pluginPath("/keys/reset-rpm"), { id });
}

export type UsageResetWindow = "daily" | "weekly" | "monthly";

export interface UsageResetResult {
  id: string;
  window: UsageResetWindow;
  next_accounting_boundary_at: string;
}

export interface UsageResetExpectation {
  started_at: string;
  reset_after_manual_at: string;
}

export async function resetUsage(id: string, window: UsageResetWindow, expected?: UsageResetExpectation): Promise<UsageResetResult> {
  const c = apiClient();
  const { data } = await c.post<UsageResetResult>(pluginPath("/keys/reset-usage"), { id, window, ...(expected ? { expected } : {}) });
  return data;
}

// fetchKeyUsage returns the per-model usage breakdown for one key.
export async function fetchKeyUsage(id: string): Promise<KeyUsageResponse> {
  const c = apiClient();
  const { data } = await c.get<KeyUsageResponse>(pluginPath("/keys/usage"), {
    params: { id },
  });
  return data;
}

export async function fetchKeyHistory(id: string, days = 30): Promise<KeyHistoryResponse> {
  const c = apiClient();
  const { data } = await c.get<KeyHistoryResponse>(pluginPath("/keys/history"), {
    params: { id, days },
  });
  return data;
}
