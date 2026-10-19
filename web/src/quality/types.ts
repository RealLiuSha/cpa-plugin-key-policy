// 萝卜's detection routes, ported from candy-eval. GET /quality lists every
// CPA credential with its test records (oldest first) and the test running
// on it; records keep candy-eval's field names. Lists carry summaries, and
// GET /quality/record returns a whole record.

export const QUALITY_TESTS = ["candy", "fingerprint", "modeltrace", "pelican"] as const;
export type QualityTest = (typeof QUALITY_TESTS)[number];

// none sends no reasoning effort and leaves it to CPA.
export const EFFORTS = ["none", "minimal", "low", "medium", "high", "xhigh"] as const;
export const DEFAULT_MODEL = "gpt-6.1-sol";
export const DEFAULT_EFFORT = "low";

// Candy, ModelTrace and pelican requests each start a new session with the
// full Codex CLI context, about this many input tokens.
export const CODEX_CONTEXT_TOKENS = 14_000;

// Token counts are null when the upstream reported none.
export interface Usage {
  input_tokens?: number | null;
  output_tokens?: number | null;
  reasoning_tokens?: number | null;
  duration_ms?: number;
}

// ok: the answer has a 21 not joined to other digits. The answer comes with
// the record route; summaries leave it empty.
export interface CandyResult extends Usage {
  time: string;
  model: string;
  effort: string;
  ok: boolean;
  answer?: string;
  error?: string;
  skipped?: boolean;
}

export interface FingerprintCell {
  cell: string;
  jsd: number;
  valid_a: number;
  valid_b: number;
}

export interface FingerprintComparison {
  model: string;
  mean_jsd: number | null;
  p_value: number | null;
  verdict: string;
  self_jsd?: number | null;
  cells?: FingerprintCell[] | null;
}

// Summaries carry the status and nearest model; warnings and comparisons
// come with the record route.
export interface FingerprintAttribution {
  status: string;
  message?: string;
  nearest?: string;
  alpha?: number;
  self_jsd?: number | null;
  reference_p_value?: number | null;
  warnings?: string[] | null;
  comparisons?: FingerprintComparison[] | null;
}

// status: completed, failed, cancelled or skipped.
export interface FingerprintResult extends Usage {
  id: string;
  time: string;
  model: string;
  mode: string;
  concurrency?: number;
  effort?: string;
  status: string;
  total: number;
  done: number;
  valid: number;
  errors: number;
  error?: string;
  attribution?: FingerprintAttribution | null;
}

export interface TraceCandidate {
  model: string;
  display_name?: string;
  family?: string;
  family_name?: string;
  probability: number;
}

export interface TraceFamily {
  family: string;
  display_name: string;
  probability: number;
}

// index counts the challenges from 0.
export interface TraceDiagnostic {
  index: number;
  parsed_numbers: number;
  minimum_numbers: number;
  accepted: boolean;
}

// Summaries keep the top-level fields; results, family_probabilities and
// diagnostics come with the record route.
export interface TraceAttribution {
  prediction: string;
  probability: number;
  used_outputs: number;
  family_prediction_name: string;
  family_probability: number;
  results?: TraceCandidate[] | null;
  family_probabilities?: TraceFamily[] | null;
  diagnostics?: TraceDiagnostic[] | null;
}

export interface TraceSample {
  prompt: string;
  expected_count: number;
  attempts?: number;
  text?: string;
  error?: string;
  parsed_numbers: number;
  accepted: boolean;
}

// status: completed, partial, failed, cancelled or skipped. Samples come
// with the record route.
export interface TraceResult extends Usage {
  bank_revision?: string;
  id: string;
  time: string;
  model: string;
  concurrency?: number;
  status: string;
  error?: string;
  samples?: TraceSample[] | null;
  attribution?: TraceAttribution | null;
}

// html comes with the record route.
export interface PelicanResult extends Usage {
  id: string;
  time: string;
  model: string;
  effort: string;
  skipped?: boolean;
  error?: string;
  html?: string;
}

export type CollectionPhase = "collecting" | "comparing" | "cancelling";

export interface CandyProgress {
  done: number;
  total: number;
}

export interface FingerprintProgress {
  model: string;
  mode: string;
  done: number;
  total: number;
  valid: number;
  errors: number;
  phase: CollectionPhase;
  concurrency: number;
}

export interface TraceProgress {
  model: string;
  phase: CollectionPhase;
  done: number;
  concurrency: number;
}

export interface PelicanProgress {
  model?: string;
  phase?: CollectionPhase;
}

export type CredentialSource = "auth_files" | "ai_providers";

// A credential from CPA: an auth file, or an API key from CPA's config,
// named by a masked preview and set apart by its group (the config group or
// OpenAI-compatible provider name) and base URL. unavailable means cooling
// down until next_retry_after.
export interface QualityCredential {
  id: string;
  auth_index?: string;
  provider: string;
  source: CredentialSource;
  name: string;
  group?: string;
  base_url?: string;
  email?: string;
  plan_type?: string;
  priority?: number;
  disabled: boolean;
  unavailable?: boolean;
  status_message?: string;
  next_retry_after?: string;
  running?: CandyProgress | null;
  results: CandyResult[];
  fingerprint_running?: FingerprintProgress | null;
  fingerprints: FingerprintResult[];
  modeltrace_running?: TraceProgress | null;
  modeltraces: TraceResult[];
  pelican_running?: PelicanProgress | null;
  pelicans: PelicanResult[];
}

export interface FingerprintMode {
  id: string;
  name: string;
  cells: number;
  samples_per_cell: number;
}

export interface QualityConfig {
  candy: { max_runs: number; prompt: string; answer: string };
  fingerprint: { modes: FingerprintMode[]; default_concurrency: number; max_concurrency: number };
  modeltrace: { requests: number; default_concurrency: number; max_concurrency: number; bank_revision: string };
  pelican: { prompt: string };
}

export interface QualityState {
  auths: QualityCredential[];
  // Non-empty while records cannot be saved, e.g. a damaged file that
  // saving would overwrite.
  storage_error: string;
  // Non-empty when CPA's config could not be read: its API keys are then
  // missing from auths, auth files are still listed.
  config_error: string;
  config: QualityConfig;
}

export type QualityRunRequest =
  | { test: "candy"; auth_ids: string[]; model: string; effort: string; runs: number }
  | { test: "fingerprint"; auth_ids: string[]; model: string; mode: string; concurrency: number }
  | { test: "modeltrace"; auth_ids: string[]; model: string; concurrency: number }
  | { test: "pelican"; auth_ids: string[]; model: string; effort: string };

// busy: credentials skipped because a test was already running on them.
export interface QualityRunResult {
  started: number;
  busy: number;
}

export interface QualityRecords {
  candy: CandyResult;
  fingerprint: FingerprintResult;
  modeltrace: TraceResult;
  pelican: PelicanResult;
}

// The record a detail dialog loads. A candy record has no id: it is named by
// its time, exactly as the list has it.
export interface DetailTarget {
  test: QualityTest;
  credential: QualityCredential;
  id: string;
}
