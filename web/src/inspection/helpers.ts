import type { InspectionCell, InspectionSettings, InspectionVerdict } from "../api/inspection";
import { servesModel, type ChannelModels } from "../channelModels";
import type { ChannelRow } from "../channelMerge";

export const INSPECTION_DAYS = [7, 14, 30] as const;

const TONES: Record<InspectionVerdict, string> = { ok: "ok", suspect: "warn", degraded: "danger", failed: "err", skipped: "" };
export const verdictTone = (verdict: InspectionVerdict) => TONES[verdict];

export const groupKey = (group: { model: string; effort: string }) => `${group.model}\u0000${group.effort}`;

// The questions a round asks: every group's channels, runs each.
export function questionsPerRound(settings: InspectionSettings): number {
  return settings.groups.reduce((sum, group) => sum + group.channels.length, 0) * settings.runs;
}

// Channels serving the group's model that it neither asks nor ignores.
export function newChannels(group: { model: string; channels: string[]; ignored: string[] }, rows: ChannelRow[], models: ChannelModels | null): ChannelRow[] {
  if (!models) return [];
  return rows.filter((row) => row.authIndex && servesModel(row.authIndex, group.model, models) && !group.channels.includes(row.authIndex) && !group.ignored.includes(row.authIndex));
}

// One group's results by day: cells[date][auth_index].
export function groupCells(days: { date: string; cells: InspectionCell[] }[], group: { model: string; effort: string }): Map<string, Map<string, InspectionCell>> {
  return new Map(days.map((day) => [day.date, new Map(day.cells.filter((cell) => cell.model === group.model && cell.effort === group.effort).map((cell) => [cell.auth_index, cell]))]));
}

// A time in the plugin's timezone, which may not be the browser's.
export function zonedTime(value: string | undefined, timezone: string, withDate = false): string {
  if (!value) return "";
  const date = new Date(value);
  if (Number.isNaN(date.getTime())) return "";
  const options: Intl.DateTimeFormatOptions = { hour: "2-digit", minute: "2-digit", hour12: false, ...(withDate ? { month: "2-digit", day: "2-digit" } : {}) };
  try {
    return date.toLocaleString([], { ...options, timeZone: timezone || undefined });
  } catch {
    return date.toLocaleString([], options);
  }
}
