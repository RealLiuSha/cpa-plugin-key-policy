import { apiClient, pluginPath } from "./client";
import type { AuditEvent } from "../types";

export async function fetchAuditEvents(keyId = "", limit = 100): Promise<AuditEvent[]> {
  const { data } = await apiClient().get<{ events: AuditEvent[] }>(pluginPath("/audit"), {
    params: { key_id: keyId || undefined, limit },
  });
  return data.events ?? [];
}
