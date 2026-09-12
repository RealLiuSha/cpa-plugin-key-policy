export interface ModelTarget {
  provider: string;
  target_model: string;
  group?: string;
}

export interface ModelDefinition {
	 billing_multiplier?: number;
  name: string;
  targets: ModelTarget[];
  dispatch: "round-robin" | "priority";
  billing_mode: "tokens" | "per_call";
  free: boolean;
  input_price_per_million?: number;
  output_price_per_million?: number;
  cache_read_price_per_million?: number;
  cache_write_price_per_million?: number;
  per_call_usd?: number;
  ref_count?: number;
  ref_keys?: string[];
}

export interface KeyModelRef {
  name: string;
  daily_limit_usd?: number;
}

export interface UsageSummary {
	cycles?: QuotaCycle[];
	status?: KeyStatus;
	blocked_reason?: string;
	limited_models?: string[];
  daily_usd: number;
  weekly_usd: number;
  monthly_usd?: number;
  daily_limit_usd: number;
  weekly_limit_usd: number;
  monthly_limit_usd?: number;
  next_accounting_boundary_at?: string;
  daily_cache_cost_usd?: number;
  weekly_cache_cost_usd?: number;
  monthly_cache_cost_usd?: number;
  daily_cache_read_tokens?: number;
  weekly_cache_read_tokens?: number;
  monthly_cache_read_tokens?: number;
  daily_cache_write_usd?: number;
  weekly_cache_write_usd?: number;
  monthly_cache_write_usd?: number;
  daily_cache_write_tokens?: number;
  weekly_cache_write_tokens?: number;
  monthly_cache_write_tokens?: number;
  daily_input_tokens?: number;
  weekly_input_tokens?: number;
  monthly_input_tokens?: number;
  daily_call_count?: number;
  weekly_call_count?: number;
  monthly_call_count?: number;
  soft_limit_hit?: boolean;
  timezone?: string;
  limits_changed_at?: string;
}

export type KeyStatus = "normal" | "warning" | "limited" | "partial" | "disabled";
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
  models: KeyModelRef[];
  daily_limit_usd: number;
  weekly_limit_usd: number;
  monthly_limit_usd?: number;
  allow_models_endpoint?: boolean;
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
  models?: KeyModelRef[];
  daily_limit_usd?: number;
  weekly_limit_usd?: number;
  monthly_limit_usd?: number;
  allow_models_endpoint?: boolean;
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

export interface UsageWindow {
  total_usd: number;
  window_start?: string;
  cache_read_tokens?: number;
  cache_cost_usd?: number;
  cache_write_tokens?: number;
  cache_write_usd?: number;
  input_tokens?: number;
  output_tokens?: number;
  call_count?: number;
}

export interface ModelUsageEntry {
  name: string;
  billing_mode?: "tokens" | "per_call";
  free: boolean;
  per_call_usd?: number;
  in_config: boolean;
  daily: UsageWindow;
  weekly: UsageWindow;
  monthly?: UsageWindow;
}

export interface KeyUsageResponse {
	usage?: UsageSummary;
  key_id: string;
  key_name: string;
  daily_limit_usd: number;
  weekly_limit_usd: number;
  monthly_limit_usd?: number;
  models: ModelUsageEntry[];
}

export interface CatalogModel {
  provider: string;
  group?: string;
  model: string;
}

export interface StatusResponse {
  enabled: boolean;
  state_file: string;
  dataset_id?: string;
  key_count: number;
  model_count?: number;
  rpm_usage?: Record<string, unknown>;
  usage?: Record<string, UsageSummary>;
}

export interface ClassifyRule {
  name: string;
  field: string;
  pattern: string;
  group: string;
  enabled: boolean;
}

export interface UsageBucket {
  total_usd?: number;
  call_count?: number;
  cache_read_tokens?: number;
  cache_cost_usd?: number;
  cache_write_tokens?: number;
  cache_write_usd?: number;
  input_tokens?: number;
  output_tokens?: number;
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
  affected_keys: string[];
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

export interface ModelImportItem {
  name: string;
  targets: ModelTarget[];
  dispatch?: "round-robin" | "priority";
  free?: boolean;
  overwrite?: boolean;
  input_price_per_million?: number;
  output_price_per_million?: number;
  cache_read_price_per_million?: number;
  cache_write_price_per_million?: number;
  missing_price?: boolean;
  price_conflict?: boolean;
}

export interface ModelImportRow {
  name: string;
  action: string;
  reason?: string;
  affected_keys?: string[];
  duplicate?: boolean;
  missing_price?: boolean;
  price_conflict?: boolean;
}

export interface ModelImportResult {
  created: ModelImportRow[];
  updated: ModelImportRow[];
  skipped: ModelImportRow[];
  conflicts: ModelImportRow[];
  missing_price: ModelImportRow[];
  affected_keys: string[];
}

export interface CredentialDescriptor {
  id: string;
  provider: string;
  attributes?: Record<string, string>;
}

export interface ClassifyPreviewResponse {
  groups: Record<string, string[]>;
  group_counts: Record<string, number>;
}
