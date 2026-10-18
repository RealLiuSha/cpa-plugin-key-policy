import { apiClient, pluginPath } from "./client";
import type { StatusResponse } from "../types";

export async function fetchStatus(): Promise<StatusResponse> {
  const { data } = await apiClient().get<StatusResponse>(pluginPath("/status"));
  return data;
}
