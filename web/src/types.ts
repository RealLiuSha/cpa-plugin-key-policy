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

export interface AuditEvent {
  ts: string;
  actor: string;
  action: string;
  key_id?: string;
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
