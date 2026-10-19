import axios from "axios";
import { apiClient, pluginPath } from "./client";
import type { QualityConfig, QualityCredential, QualityRecords, QualityRunRequest, QualityRunResult, QualityState, QualityTest } from "../quality/types";

// 萝卜's detection routes. Test requests use the credentials' quota: they go
// upstream through CPA and count as channel attempts, but no downstream key
// is billed. Go encodes an empty record list as null.

const DEFAULT_CONFIG: QualityConfig = {
  candy: { max_runs: 10, prompt: "", answer: "21" },
  fingerprint: { modes: [], default_concurrency: 2, max_concurrency: 6 },
  modeltrace: { requests: 3, default_concurrency: 3, max_concurrency: 3, bank_revision: "" },
  pelican: { prompt: "" },
};

function config(value?: Partial<QualityConfig> | null): QualityConfig {
  return {
    candy: { ...DEFAULT_CONFIG.candy, ...value?.candy },
    fingerprint: { ...DEFAULT_CONFIG.fingerprint, ...value?.fingerprint, modes: value?.fingerprint?.modes ?? [] },
    modeltrace: { ...DEFAULT_CONFIG.modeltrace, ...value?.modeltrace },
    pelican: { ...DEFAULT_CONFIG.pelican, ...value?.pelican },
  };
}

function credential(value: QualityCredential): QualityCredential {
  return { ...value, results: value.results ?? [], fingerprints: value.fingerprints ?? [], modeltraces: value.modeltraces ?? [], pelicans: value.pelicans ?? [] };
}

export async function fetchQuality(): Promise<QualityState> {
  const { data } = await apiClient().get<Partial<QualityState>>(pluginPath("/quality"));
  return { auths: (data.auths ?? []).map(credential), storage_error: data.storage_error ?? "", config_error: data.config_error ?? "", config: config(data.config) };
}

// The plugin answers 503 upgrading while it is being hot-replaced.
export async function runQuality(body: QualityRunRequest): Promise<QualityRunResult> {
  const { data } = await apiClient().post<Partial<QualityRunResult>>(pluginPath("/quality/run"), body);
  return { started: data.started ?? 0, busy: data.busy ?? 0 };
}

// Requests in flight are closed, which cancels them upstream; the test
// leaves a cancelled record.
export async function cancelQuality(test: QualityTest, authIds: string[]): Promise<number> {
  const { data } = await apiClient().post<{ cancelled?: number }>(pluginPath("/quality/cancel"), { test, auth_ids: authIds });
  return data.cancelled ?? 0;
}

export async function fetchQualityRecord<T extends QualityTest>(test: T, authId: string, id: string): Promise<QualityRecords[T]> {
  const { data } = await apiClient().get<QualityRecords[T]>(pluginPath("/quality/record"), { params: { test, auth_id: authId, id } });
  return data;
}

// A record that was cleared answers 404 with an error body; CPA's empty 404
// means the plugin itself is not being served.
export function isRecordGone(error: unknown): boolean {
  return axios.isAxiosError(error) && error.response?.status === 404 && Boolean(error.response.data);
}

export async function clearQualityResults(test: QualityTest | "all"): Promise<void> {
  await apiClient().delete(pluginPath("/quality/results"), { params: { test } });
}
