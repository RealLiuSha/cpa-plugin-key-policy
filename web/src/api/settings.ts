import { apiClient, pluginPath } from "./client";
import type { SettingsResponse } from "../types";

export async function fetchSettings(): Promise<SettingsResponse> {
  const { data } = await apiClient().get<SettingsResponse>(pluginPath("/settings"));
  return { ...data, module_errors: data.module_errors ?? {}, hooks: data.hooks ?? [], config_warnings: data.config_warnings ?? [], files: data.files ?? [] };
}
