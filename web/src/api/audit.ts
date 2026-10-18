import { apiClient, pluginPath } from "./client";
import type { AuditEvent } from "../types";

export interface AuditQuery {
  keyId?: string;
  action?: string;
  limit?: number;
}

// The server filters by key and action before it applies the limit.
export async function fetchAuditEvents({ keyId = "", action = "", limit = 100 }: AuditQuery = {}): Promise<AuditEvent[]> {
  const { data } = await apiClient().get<{ events: AuditEvent[] }>(pluginPath("/audit"), {
    params: { key_id: keyId || undefined, action: action || undefined, limit },
  });
  return data.events ?? [];
}
