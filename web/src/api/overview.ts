import { apiClient, pluginPath } from "./client";
import type { OverviewResponse } from "../types";

export async function fetchOverview(): Promise<OverviewResponse> {
  const { data } = await apiClient().get<OverviewResponse>(pluginPath("/overview"));
  return {
    ...data,
    keys: { ...data.keys, attention: data.keys?.attention ?? [] },
    channels: { ...data.channels, attention: data.channels?.attention ?? [] },
    warnings: data.warnings ?? [],
    recent_changes: data.recent_changes ?? [],
  };
}
