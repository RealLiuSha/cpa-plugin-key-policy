export type BillingMode = "tokens" | "per_call";

// One price row. `name` is the upstream model id seen in CPA usage records;
// an empty `provider` prices that model on any provider. All-zero prices are
// valid and bill $0 as unpriced.
export interface ModelPrice {
  name: string;
  provider: string;
  billing_mode: BillingMode;
  billing_multiplier?: number;
  input_price_per_million?: number;
  output_price_per_million?: number;
  cache_read_price_per_million?: number;
  cache_write_price_per_million?: number;
  per_call_usd?: number;
}

export interface ModelPriceRow extends ModelPrice {
  priced: boolean;
}

// A model with calls in the window that matched no price row. Models with a
// row for any provider are left out; has_qualified_rows means rows exist but
// all are provider-qualified, so calls from other providers missed them.
export interface UnpricedModel {
  name: string;
  calls: number;
  last_seen: string;
  has_qualified_rows?: boolean;
}

export interface ModelPricesResponse {
  models: ModelPriceRow[];
  unpriced: UnpricedModel[];
}

// cycle: daily / 7-day / 30-day quotas; balance: a prepaid balance that
// management adjustments add to and billed calls deduct from.
export type QuotaMode = "cycle" | "balance";

export interface UsageSummary {
  quota_mode?: QuotaMode;
  // Present for balance keys only; can be negative after calls that were in
  // flight when the balance ran out.
  balance_usd?: number;
  net_credited_usd?: number;
  cycles?: QuotaCycle[];
  status?: KeyStatus;
  blocked_reason?: string;
  daily_usd: number;
  weekly_usd: number;
  monthly_usd: number;
  daily_limit_usd: number;
  weekly_limit_usd: number;
  monthly_limit_usd: number;
  next_accounting_boundary_at?: string;
  soft_limit_hit?: boolean;
  timezone?: string;
}

export type KeyStatus = "normal" | "warning" | "limited" | "disabled";
export type QuotaWindow = "daily" | "weekly" | "monthly";
export interface QuotaCycle {
  window: QuotaWindow;
  started_at: string;
  resets_at: string;
  reset_kind: "initial" | "migration" | "manual" | "automatic";
  used_usd: number;
  limit_usd: number;
  reset_after_manual_at: string;
}

export interface KeyPublic {
  id: string;
  name: string;
  enabled: boolean;
  key_preview: string;
  rpm: number;
  daily_limit_usd: number;
  weekly_limit_usd: number;
  monthly_limit_usd: number;
  quota_mode?: QuotaMode;
  usage: UsageSummary;
  created_at?: string;
  updated_at?: string;
}

export interface KeyWriteRequest {
  id: string;
  name?: string;
  enabled?: boolean;
  key?: string;
  rpm?: number;
  daily_limit_usd?: number;
  weekly_limit_usd?: number;
  monthly_limit_usd?: number;
  quota_mode?: QuotaMode;
}

export interface BalanceAdjustment {
  id: string;
  amount_usd: number;
  balance_before_usd: number;
  balance_usd: number;
  net_credited_usd: number;
  // true when the request id was already applied: the amounts are the first result.
  duplicate: boolean;
}

export interface CreateKeyResponse {
  key: KeyPublic;
  plain_key: string;
  generated: boolean;
}

export interface RotateKeyResponse {
  key: KeyPublic;
  plain_key: string;
  generated: boolean;
}

// Ledger counters as the management API reports them; zero values are omitted.
export interface UsageBucket {
  total_usd?: number;
  call_count?: number;
  unpriced_calls?: number;
  input_tokens?: number;
  output_tokens?: number;
  cache_read_tokens?: number;
  cache_cost_usd?: number;
  cache_write_tokens?: number;
  cache_write_usd?: number;
}

export interface UsageWindow extends UsageBucket {
  total_usd: number;
  window_start?: string;
}

export interface ModelUsageEntry {
  name: string;
  billing_mode: BillingMode;
  billing_multiplier: number;
  priced: boolean;
  daily: UsageWindow;
  weekly: UsageWindow;
  monthly: UsageWindow;
}

export interface KeyUsageResponse {
  key_id: string;
  key_name: string;
  daily_limit_usd: number;
  weekly_limit_usd: number;
  monthly_limit_usd: number;
  models: ModelUsageEntry[];
  usage: UsageSummary;
}

export interface UsageHistoryDay extends UsageBucket {
  date: string;
  by_model?: Record<string, UsageBucket>;
}

export interface KeyHistoryResponse {
  key_id: string;
  timezone: string;
  days: UsageHistoryDay[];
}

export interface CatalogModel {
  provider: string;
  model: string;
}

// In-process ledger counters since the plugin started; last_flush_at is ""
// until the first successful write.
export interface LedgerHealth {
  last_flush_at: string;
  last_flush_error: string;
  flush_failures: number;
  skipped_failed: number;
  ignored_unknown_key: number;
  ignored_no_model: number;
  ignored_empty_usage: number;
  clamped_tokens: number;
  invalid_usage: number;
  unmetered_per_call: number;
  unpriced_calls: number;
}

export interface StatusResponse {
  enabled: boolean;
  state_file: string;
  dataset_id?: string;
  key_count: number;
  model_count?: number;
  rpm_usage?: Record<string, number>;
  health?: LedgerHealth;
}

// GET /stats counters; every field is always present.
export interface StatsTotals {
  charged_usd: number;
  calls: number;
  unpriced_calls: number;
  input_tokens: number;
  cache_read_tokens: number;
  cache_write_tokens: number;
  output_tokens: number;
}

export interface StatsDay extends StatsTotals {
  date: string;
}

export interface StatsModel extends StatsTotals {
  name: string;
  priced: boolean;
}

// A deleted key keeps its history until retention drops it; its name is gone.
export interface StatsKey extends StatsTotals {
  id: string;
  name: string;
  deleted: boolean;
}

// from/to are inclusive natural days in `timezone`; days has one entry per
// day, models and keys are sorted by charged_usd descending, keys capped at 200.
export interface StatsReport {
  timezone: string;
  from: string;
  to: string;
  totals: StatsTotals;
  days: StatsDay[];
  models: StatsModel[];
  keys: StatsKey[];
  keys_total: number;
  keys_truncated: boolean;
  unpriced_models: UnpricedModel[];
}

export interface AuditChange {
  from: unknown;
  to: unknown;
}

// target names what a change outside the key store applied to, such as a
// channel's auth_index or an adapter rule id.
export interface AuditEvent {
  ts: string;
  actor: string;
  action: string;
  key_id?: string;
  target?: string;
  changes?: Record<string, AuditChange>;
}

export interface PriceImportMatch {
  model: string;
  prompt_price_per_1m?: number;
  completion_price_per_1m?: number;
  cache_read_price_per_1m?: number;
  cache_write_price_per_1m?: number;
}

export interface PriceImportApplied {
  model: string;
  old_input_price_per_million: number;
  old_output_price_per_million: number;
  old_cache_read_price_per_million: number;
  old_cache_write_price_per_million?: number;
  new_input_price_per_million: number;
  new_output_price_per_million: number;
  new_cache_read_price_per_million: number;
  new_cache_write_price_per_million?: number;
  note?: string;
}

export interface PriceImportUnchanged { model: string }

export interface PriceImportSkipped {
  match_model?: string;
  model?: string;
  reason: string;
}

export interface PriceImportResult {
  applied: PriceImportApplied[];
  unchanged: PriceImportUnchanged[];
  skipped: PriceImportSkipped[];
}

export interface PriceImportRequest {
  dry_run: boolean;
  matches: PriceImportMatch[];
}

export interface PricingPreviewMatch {
  model: string;
  matched_model: string;
  match_type: string;
  source: string;
  source_url: string;
  source_provider_id: string;
  source_provider_name: string;
  prompt_price_per_1m: number;
  completion_price_per_1m: number;
  cache_read_price_per_1m: number;
  cache_write_price_per_1m?: number;
}

export interface PricingPreview {
  source: string;
  source_url: string;
  metadata_models: number;
  matches: PricingPreviewMatch[];
  unmatched_models: string[];
}

// Import creates a $0 price row; an empty provider prices the model on any provider.
export interface ModelImportItem {
  name: string;
  provider: string;
}

export interface ModelImportRow {
  name: string;
  reason?: "exists" | "duplicate" | string;
}

export interface ModelImportResult {
  created: ModelImportRow[];
  skipped: ModelImportRow[];
}

// ===== Channels: the plugin's own data about each CPA credential =====

// One window of a channel's upstream attempts, built from CPA usage records.
// Latencies are in milliseconds; costs are at list price and, when the
// channel has a cost ratio, at upstream price.
export interface ChannelSummary {
  attempts: number;
  failures: number;
  success_rate: number;
  status_codes?: Record<string, number>;
  latency_p50_ms: number;
  latency_p95_ms: number;
  ttft_p50_ms: number;
  ttft_p95_ms: number;
  input_tokens: number;
  output_tokens: number;
  list_cost_usd: number;
  last_at?: string;
}

// What the operator records about a channel.
export interface ChannelMeta {
  auth_index: string;
  display_name?: string;
  vendor?: string;
  note?: string;
  updated_at?: string;
}

// auth_id is CPA's stable credential id from the usage records.
export interface ChannelView {
  auth_index: string;
  auth_id?: string;
  provider?: string;
  base_url?: string;
  meta: ChannelMeta;
  last_24h: ChannelSummary;
  today: ChannelSummary;
  rules?: number;
}

export interface ChannelHour {
  hour: string;
  attempts: number;
  failures: number;
  latency_p50_ms: number;
  list_cost_usd: number;
}

export interface ChannelError {
  at: string;
  status_code: number;
  model: string;
  snippet?: string;
}

export interface ChannelDetail extends ChannelView {
  last_7d: ChannelSummary;
  hours: ChannelHour[];
  recent_errors: ChannelError[];
}

// PATCH /channels changes only the fields it carries.
export interface ChannelPatch {
  auth_index: string;
  display_name?: string;
  vendor?: string;
  note?: string;
}

// ===== Adapter rules: per-channel request rewrites =====

// value is the JSON value set_field writes; paths empty means the type's
// default paths.
export interface AdapterRule {
  id: string;
  channel: string;
  type: string;
  paths?: string[];
  field?: string;
  value?: unknown;
  overwrite?: boolean;
  enabled: boolean;
  note?: string;
  updated_at?: string;
  hits?: { count: number; last_at?: string };
}

export interface AdapterRuleType {
  type: string;
  default_paths: string[];
  needs_field: boolean;
  needs_value: boolean;
}

// ===== Search routing =====

export interface SearchChannel {
  name: string;
  // Selection model template; "{model}" is replaced by the requested model.
  model: string;
  force_unhealthy: boolean;
}

// api_key is write-only: GET never returns it and an empty value on PUT keeps
// the stored key.
export interface SearchProbe {
  interval_seconds: number;
  timeout_seconds: number;
  model: string;
  url: string;
  user_agent: string;
  idle_seconds: number;
  api_key?: string;
}

export interface SearchSettings {
  target_model: string;
  keep_models: string[];
  channels: SearchChannel[];
  probe: SearchProbe;
}

export interface SearchChannelStatus {
  name: string;
  model: string;
  healthy: boolean;
  forced: boolean;
  routed: number;
  last_routed_at?: string;
  last_probe_at?: string;
  last_probe_error?: string;
}

export interface SearchStatus {
  channels: SearchChannelStatus[];
  routed_total: number;
  last_search_at?: string;
  probe_running: boolean;
  probe_key_stored: boolean;
}

// enabled is search_enabled in the plugin's YAML; declared is whether the
// running plugin has registered its router with CPA. They differ while a
// configuration change is still being applied.
export interface SearchState {
  enabled: boolean;
  declared: boolean;
  settings: SearchSettings;
  status: SearchStatus;
}

// ===== Overview and settings =====

// A module that could not open its data file is missing here; the settings
// answer's module_errors says why.
export interface ModuleStates {
  search: { enabled: boolean; declared: boolean };
  adapters?: { rules: number; enabled_rules: number };
  channels?: { tracked: number };
}

// requests counts billed calls of plugin keys; attempts and failures count
// every upstream attempt in the channel statistics, CPA's own keys and
// retries included.
export interface OverviewToday {
  requests: number;
  attempts: number;
  failures: number;
  billed_usd: number;
  list_cost_usd: number;
}

export interface OverviewKeyAttention {
  id: string;
  name: string;
  status: KeyStatus;
  blocked_reason?: string;
}

// failure_rate: value is the 24-hour failure rate (0–1). degraded and
// suspect are the verdicts of the last 24 hours of inspection rounds, with
// the model asked and the answers.
export type ChannelAttentionReason = "failure_rate" | "degraded" | "suspect";

export interface OverviewChannelAttention {
  auth_index: string;
  display_name?: string;
  reason: ChannelAttentionReason;
  value?: number;
  model?: string;
  effort?: string;
  answered?: number;
  correct?: number;
}

export interface OverviewResponse {
  today: OverviewToday;
  keys: { total: number; enabled: number; limited: number; warning: number; attention: OverviewKeyAttention[] };
  channels: { total: number; attention: OverviewChannelAttention[] };
  modules: ModuleStates;
  warnings: string[];
  recent_changes: AuditEvent[];
}

export interface HookTiming {
  method: string;
  count: number;
  p50_us: number;
  p99_us: number;
  max_us: number;
}

export interface DataFile {
  name: string;
  path: string;
  size: number;
  modified?: string;
  missing?: boolean;
}

export interface SettingsResponse {
  version: string;
  plugin_id: string;
  display_name: string;
  modules: ModuleStates;
  module_errors: Record<string, string>;
  hooks: HookTiming[];
  config_warnings: string[];
  files: DataFile[];
  ui: { source: string; sha256: string };
  ownership: { owner: boolean; handoff: string };
  health?: LedgerHealth;
}
