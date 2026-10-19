import type { ChannelRow } from "../channelMerge";
import { formatCount } from "../format";
import { labelOr } from "../labels";
import { DEFAULT_EFFORT, DEFAULT_MODEL, type CandyResult, type FingerprintAttribution, type FingerprintResult, type QualityConfig, type QualityCredential, type QualityRunRequest, type QualityTest, type TraceResult } from "./types";

type Translate = (key: string, variables?: Record<string, string | number>) => string;

export type Tone = "ok" | "warn" | "danger" | "";

// The test running on a credential; a credential runs one test at a time.
export function runningTest(credential: QualityCredential): QualityTest | null {
  if (credential.running) return "candy";
  if (credential.fingerprint_running) return "fingerprint";
  if (credential.modeltrace_running) return "modeltrace";
  if (credential.pelican_running) return "pelican";
  return null;
}

// Whether the running test is stopping: it sends no more requests and
// closes the ones in flight.
export function stopping(credential: QualityCredential, test: QualityTest): boolean {
  const progress = test === "fingerprint" ? credential.fingerprint_running : test === "modeltrace" ? credential.modeltrace_running : test === "pelican" ? credential.pelican_running : null;
  return progress?.phase === "cancelling";
}

// Disabled and cooling credentials are listed but not tested.
export const testable = (credential: QualityCredential) => !credential.disabled && !credential.unavailable;
export const selectable = (credential: QualityCredential) => testable(credential) && runningTest(credential) === null;

export function recordCount(credential: QualityCredential, test: QualityTest): number {
  switch (test) {
    case "candy": return credential.results.length;
    case "fingerprint": return credential.fingerprints.length;
    case "modeltrace": return credential.modeltraces.length;
    case "pelican": return credential.pelicans.length;
  }
}

export const lastOf = <T,>(records: T[]): T | undefined => records[records.length - 1];

// CPA names each OpenAI-compatible provider after its config entry; they
// share one type.
export function providerType(credential: QualityCredential): string {
  const provider = credential.provider;
  return provider === "openai-compatibility" || provider.startsWith("openai-compatible-") ? "openai-compatibility" : provider;
}

// Most channels are API keys of CPA's config, so the list starts unfiltered.
export const ALL_CREDENTIALS = "all";
export const DEFAULT_CREDENTIAL_TYPE = ALL_CREDENTIALS;
export const credentialType = (credential: QualityCredential) => `${credential.source}:${providerType(credential)}`;
export const ofType = (credential: QualityCredential, type: string) => type === ALL_CREDENTIALS || credentialType(credential) === type;

// The types to filter by: those listed, plus the current one while no
// credential has it.
export function credentialTypes(credentials: QualityCredential[], current: string): string[] {
  return [...new Set([current, ...credentials.map(credentialType)])].filter((type) => type !== ALL_CREDENTIALS).sort();
}

// A type from the URL, or the default when it names no credential source.
export function parseCredentialType(value: string | null): string {
  return value && (value === ALL_CREDENTIALS || /^(auth_files|ai_providers):./.test(value)) ? value : DEFAULT_CREDENTIAL_TYPE;
}

// API keys are named by their masked preview, auth files by their account.
export const credentialLabel = (credential: QualityCredential) => (credential.source === "ai_providers" ? credential.name : credential.email || credential.name) || credential.id;

// The operator's channel name comes first, as on the channel page; then an
// API key's group, the channel page's name, and the email or masked key.
export function displayName(credential: QualityCredential, channels: Map<string, ChannelRow>): string {
  const row = credential.auth_index ? channels.get(credential.auth_index) : undefined;
  return row?.view?.meta.display_name?.trim() || credential.group || row?.name || credentialLabel(credential);
}

// CPA keeps the upstream error body, often JSON; only its message is shown.
export function statusText(text?: string): string {
  if (!text) return "";
  const message = (value: unknown) => (typeof value === "string" && value.trim() ? value : undefined);
  try {
    const data: unknown = JSON.parse(text);
    if (data && typeof data === "object") {
      const body = data as { error?: unknown; message?: unknown };
      const inner = body.error && typeof body.error === "object" ? message((body.error as { message?: unknown }).message) : message(body.error);
      const found = inner ?? message(body.message);
      if (found) return found;
    }
  } catch {
    // Plain text.
  }
  return text;
}

export const modelLabel = (model: string, effort?: string) => (effort ? `${model}(${effort})` : model);

// The failures the plugin records itself, in Simplified Chinese as
// internal/quality writes them (runs.go, host.go, model.go,
// modeltrace_algorithm.go); error is a nested failure. Other text, such as an
// upstream's error body, is shown as recorded.
const RECORD_ERRORS: [RegExp, string][] = [
  [/^已取消$/, "cancelled"],
  [/^回答中没有 HTML 代码$/, "noHtml"],
  [/^HTML 超过 (?<kb>\d+) KB，未保存$/, "htmlTooLarge"],
  [/^连续 (?<count>\d+) 次请求失败，已停止采集：(?<error>[\s\S]*)$/, "failedInARow"],
  [/^回答过长，本次未计入$/, "answerTooLong"],
  [/^有效数字不足$/, "tooFewNumbers"],
  [/^未采集到有效数字序列$/, "noSequence"],
  [/^模型 (?<minutes>\d+) 分钟内未完成回答，已中断请求$/, "timeout"],
  [/^模型响应未完成（(?<status>[^）]*)）：(?<error>[\s\S]*)$/, "incomplete"],
  [/^模型调用了工具，没有直接回答$/, "toolCall"],
  [/^模型未返回文本$/, "noText"],
  [/^解析模型响应失败：(?<error>[\s\S]*)$/, "parseFailed"],
  [/^模型返回错误：(?<error>[\s\S]*)$/, "modelError"],
  [/^模型响应流在完成前中断$/, "streamEnded"],
  [/^模型响应流格式无效$/, "streamInvalid"],
  [/^模型响应流中断：(?<error>[\s\S]*)$/, "streamBroken"],
  [/^模型响应超过 (?<mb>\d+) MB，已中断$/, "streamTooLarge"],
  [/^模型请求异常：(?<error>[\s\S]*)$/, "requestFailed"],
];

export function recordError(t: Translate, text?: string): string {
  if (!text) return "";
  for (const [pattern, key] of RECORD_ERRORS) {
    const match = pattern.exec(text);
    if (!match) continue;
    const variables: Record<string, string> = { ...match.groups };
    if (variables.error !== undefined) variables.error = recordError(t, variables.error);
    return t(`quality.recordError.${key}`, variables);
  }
  return text;
}

export type CandyKind = "ok" | "bad" | "err" | "skip";
export const candyKind = (record: CandyResult): CandyKind => (record.skipped ? "skip" : record.error ? "err" : record.ok ? "ok" : "bad");
export const inScope = (record: CandyResult, model: string, effort: string) => record.model === model && (record.effort || "none") === effort;

// Correct answers among the graded runs of model at effort; failed and
// skipped runs are not graded.
export function candyRate(records: CandyResult[], model: string, effort: string): { correct: number; graded: number } {
  const graded = records.filter((record) => inScope(record, model, effort) && !record.error && !record.skipped);
  return { correct: graded.filter((record) => record.ok).length, graded: graded.length };
}

// A collection that did not complete reports its own status, else its
// attribution does.
export function fingerprintStatus(record: FingerprintResult): string {
  return record.status && record.status !== "completed" ? record.status : record.attribution?.status ?? "";
}

export const nearestModel = (record: FingerprintResult) => record.attribution?.nearest || "";

// A split-half distance above this makes fingerprint_algorithm.go warn that
// the samples, the run's own or a baseline's, disagree with themselves.
export const FINGERPRINT_UNSTABLE_JSD = 0.25;

// The attribution's warnings, rebuilt from its numbers in the operator's
// language; like the plugin, none before some baseline could be compared.
export function fingerprintWarnings(t: Translate, attribution?: FingerprintAttribution | null): string[] {
  const comparisons = attribution?.comparisons ?? [];
  if (!attribution || !comparisons.some((item) => typeof item.mean_jsd === "number")) return [];
  const unstable = (value?: number | null) => typeof value === "number" && value > FINGERPRINT_UNSTABLE_JSD;
  return [
    ...(unstable(attribution.self_jsd) ? [t("quality.fingerprint.warningSelf")] : []),
    ...comparisons.filter((item) => unstable(item.self_jsd)).map((item) => t("quality.fingerprint.warningBaseline", { model: item.model })),
  ];
}

// A collection mode by its translated name, else the name the plugin gives.
export const fingerprintModeName = (t: Translate, config: QualityConfig, mode: string) =>
  labelOr(t, `quality.fingerprint.mode.${mode}`, config.fingerprint.modes.find((item) => item.id === mode)?.name || mode || "—");

const FINGERPRINT_TONES: Record<string, Tone> = { consistent: "ok", substitution: "warn", unstable: "warn", different: "danger", failed: "danger" };
export const fingerprintTone = (status: string): Tone => FINGERPRINT_TONES[status] ?? "";

// Models are compared without their provider prefix.
const modelID = (model: string) => model.trim().toLowerCase().split("/").pop() ?? "";

// Whether the prediction names the tested model; null without a verdict.
export function traceMatches(record: TraceResult): boolean | null {
  if (!record.attribution || (record.status !== "completed" && record.status !== "partial")) return null;
  return modelID(record.model) === modelID(record.attribution.prediction);
}

export function probability(value?: number | null): string {
  return typeof value === "number" && value >= 0 && value <= 1 ? `${(value * 100).toFixed(1)}%` : "—";
}

// Fingerprint distances and p-values.
export const statistic = (value?: number | null) => (typeof value === "number" && Number.isFinite(value) ? value.toFixed(4) : "—");

export const tokenCount = (value?: number | null) => (typeof value === "number" ? formatCount(value) : "—");

// Each test's settings as typed, so a half-typed number is not rewritten.
export interface QualitySettings {
  candy: { model: string; effort: string; runs: string };
  fingerprint: { model: string; mode: string; concurrency: string };
  modeltrace: { model: string; concurrency: string };
  pelican: { model: string; effort: string };
}

export function defaultSettings(config: QualityConfig): QualitySettings {
  const modes = config.fingerprint.modes;
  return {
    candy: { model: DEFAULT_MODEL, effort: DEFAULT_EFFORT, runs: "1" },
    fingerprint: { model: DEFAULT_MODEL, mode: modes.some((mode) => mode.id === "quick") ? "quick" : modes[0]?.id ?? "", concurrency: String(config.fingerprint.default_concurrency) },
    modeltrace: { model: DEFAULT_MODEL, concurrency: String(config.modeltrace.default_concurrency) },
    pelican: { model: DEFAULT_MODEL, effort: DEFAULT_EFFORT },
  };
}

export function wholeNumber(value: string, min: number, max: number): number | null {
  const number = Number(value);
  return value.trim() !== "" && Number.isInteger(number) && number >= min && number <= max ? number : null;
}

// Requests one credential is sent: fingerprint ones are short, the others
// carry the Codex context. null while the settings are incomplete.
export function requestsPerCredential(test: QualityTest, settings: QualitySettings, config: QualityConfig): number | null {
  switch (test) {
    case "candy": return wholeNumber(settings.candy.runs, 1, config.candy.max_runs);
    case "fingerprint": {
      const mode = config.fingerprint.modes.find((item) => item.id === settings.fingerprint.mode);
      return mode ? mode.cells * mode.samples_per_cell : null;
    }
    case "modeltrace": return config.modeltrace.requests;
    case "pelican": return 1;
  }
}

export interface PlannedRun {
  body: QualityRunRequest;
  perCredential: number;
}

// The run request for ids, or why the settings cannot be sent. Collection
// tests take a model without a reasoning-effort suffix.
export function planRun(test: QualityTest, settings: QualitySettings, config: QualityConfig, ids: string[], t: Translate): PlannedRun | { error: string } {
  const model = settings[test].model.trim();
  if (!model) return { error: t("quality.settings.modelRequired") };
  if ((test === "fingerprint" || test === "modeltrace") && /[()]/.test(model)) return { error: t("quality.settings.suffixInvalid") };
  const perCredential = requestsPerCredential(test, settings, config);
  switch (test) {
    case "candy":
      if (perCredential === null) return { error: t("quality.settings.runsInvalid", { max: config.candy.max_runs }) };
      return { body: { test, auth_ids: ids, model, effort: settings.candy.effort, runs: perCredential }, perCredential };
    case "fingerprint": {
      if (perCredential === null) return { error: t("quality.settings.modeRequired") };
      const concurrency = wholeNumber(settings.fingerprint.concurrency, 1, config.fingerprint.max_concurrency);
      if (concurrency === null) return { error: t("quality.settings.concurrencyInvalid", { max: config.fingerprint.max_concurrency }) };
      return { body: { test, auth_ids: ids, model, mode: settings.fingerprint.mode, concurrency }, perCredential };
    }
    case "modeltrace": {
      const concurrency = wholeNumber(settings.modeltrace.concurrency, 1, config.modeltrace.max_concurrency);
      if (concurrency === null) return { error: t("quality.settings.concurrencyInvalid", { max: config.modeltrace.max_concurrency }) };
      return { body: { test, auth_ids: ids, model, concurrency }, perCredential: perCredential ?? config.modeltrace.requests };
    }
    case "pelican":
      return { body: { test, auth_ids: ids, model, effort: settings.pelican.effort }, perCredential: perCredential ?? 1 };
  }
}

// Price rows suggest models; image models are left out, and so are effort
// suffixes for the collection tests.
export function modelSuggestions(names: string[], plain: boolean): string[] {
  const unique = new Set(names.map((name) => name.trim()).filter((name) => name && !/image/i.test(name) && !(plain && /[()]/.test(name))));
  unique.delete(DEFAULT_MODEL);
  return [DEFAULT_MODEL, ...[...unique].sort()];
}
