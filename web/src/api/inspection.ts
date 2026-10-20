import { apiClient, pluginPath } from "./client";

// The daily inspection: at each time of day (the plugin's timezone) every
// group's channels are asked the candy question runs times with the group's
// model and effort. A round spends the channels' quota like a candy test.

export interface InspectionGroup {
  model: string;
  effort: string;
  channels: string[];
  // Channels serving the model the operator left out.
  ignored: string[];
}

export interface InspectionSettings {
  enabled: boolean;
  times: string[];
  runs: number;
  groups: InspectionGroup[];
}

// ok: at least two thirds right; suspect: at least one third; failed: no
// answer at all; skipped: the channel was missing, disabled or cooling.
export type InspectionVerdict = "ok" | "suspect" | "degraded" | "failed" | "skipped";

export interface InspectionRun {
  round: string;
  started: string;
  scheduled: boolean;
  answered: number;
  correct: number;
  failed: number;
  skipped?: string;
  error?: string;
  wrong_answers: number;
}

export interface InspectionCell {
  auth_index: string;
  model: string;
  effort: string;
  answered: number;
  correct: number;
  failed: number;
  verdict: InspectionVerdict;
  rounds: InspectionRun[];
}

export interface InspectionDay {
  date: string;
  cells: InspectionCell[];
}

export interface InspectionState {
  settings: InspectionSettings;
  timezone: string;
  running?: { slot?: string; started: string; done: number; total: number } | null;
  next_at?: string;
  storage_error?: string;
  // Newest first, today included.
  days: InspectionDay[];
}

export interface InspectionAnswers {
  answered: number;
  correct: number;
  failed: number;
  error?: string;
  wrong?: string[] | null;
}

// Go encodes empty lists as null.
function settings(value?: Partial<InspectionSettings> | null): InspectionSettings {
  return {
    enabled: value?.enabled ?? false,
    times: value?.times ?? [],
    runs: value?.runs ?? 3,
    groups: (value?.groups ?? []).map((group) => ({ ...group, channels: group.channels ?? [], ignored: group.ignored ?? [] })),
  };
}

export async function fetchInspection(days: number): Promise<InspectionState> {
  const { data } = await apiClient().get<Partial<InspectionState>>(pluginPath("/quality/inspection"), { params: { days } });
  return {
    settings: settings(data.settings),
    timezone: data.timezone ?? "",
    running: data.running ?? null,
    next_at: data.next_at,
    storage_error: data.storage_error ?? "",
    days: (data.days ?? []).map((day) => ({ ...day, cells: (day.cells ?? []).map((cell) => ({ ...cell, rounds: cell.rounds ?? [] })) })),
  };
}

export async function saveInspection(next: InspectionSettings): Promise<InspectionSettings> {
  const { data } = await apiClient().put<{ settings?: Partial<InspectionSettings> }>(pluginPath("/quality/inspection"), next);
  return settings(data.settings);
}

// Answers 409 while a round is under way.
export async function startInspection(): Promise<void> {
  await apiClient().post(pluginPath("/quality/inspection"));
}

export async function fetchInspectionAnswers(round: string, cell: InspectionCell): Promise<InspectionAnswers> {
  const { data } = await apiClient().get<InspectionAnswers>(pluginPath("/quality/inspection/answers"), {
    params: { round, auth_index: cell.auth_index, model: cell.model, effort: cell.effort },
  });
  return data;
}
