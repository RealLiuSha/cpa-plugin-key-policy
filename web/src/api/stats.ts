import axios from "axios";
import { apiClient, pluginPath } from "./client";
import type { StatsReport } from "../types";

export async function fetchStats(days: number): Promise<StatsReport> {
  const { data } = await apiClient().get<StatsReport>(pluginPath("/stats"), { params: { days } });
  return {
    ...data,
    days: data.days ?? [],
    models: data.models ?? [],
    keys: data.keys ?? [],
    unpriced_models: data.unpriced_models ?? [],
  };
}

export interface StatsExport {
  blob: Blob;
  filename: string;
}

// The CSV of every (day, key, model) bucket in [from, to]. The file name
// repeats the server's Content-Disposition, which an embedding panel on
// another origin may not expose to scripts.
export async function exportStatsCSV(from: string, to: string): Promise<StatsExport> {
  try {
    const { data } = await apiClient().get<Blob>(pluginPath("/stats/export"), { params: { from, to }, responseType: "blob" });
    return { blob: data, filename: `cpa-key-policy-usage-${from}_${to}.csv` };
  } catch (error) {
    // A blob request receives its JSON error body as a Blob as well.
    if (axios.isAxiosError(error) && error.response?.data instanceof Blob) {
      try {
        error.response.data = JSON.parse(await error.response.data.text());
      } catch {
        // Not JSON: the caller falls back to its generic message.
      }
    }
    throw error;
  }
}

// 413 range_too_large: more than 92 days or 200000 CSV rows.
export function isRangeTooLarge(error: unknown): boolean {
  return axios.isAxiosError(error) && error.response?.status === 413;
}
