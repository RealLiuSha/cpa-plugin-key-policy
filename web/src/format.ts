const usd2 = new Intl.NumberFormat("en-US", { style: "currency", currency: "USD", minimumFractionDigits: 2, maximumFractionDigits: 2 });
const usdSubCent = new Intl.NumberFormat("en-US", { style: "currency", currency: "USD", minimumFractionDigits: 2, maximumFractionDigits: 4 });

// Charged amounts. A sub-cent charge keeps up to four decimals so $0.0042 is
// not shown as $0.00; configured unit prices are shown verbatim elsewhere.
export function formatUSD(value: number): string {
  const amount = Number.isFinite(value) ? value : 0;
  const magnitude = Math.abs(amount);
  return (magnitude > 0 && magnitude < 0.01 ? usdSubCent : usd2).format(amount);
}

export function formatCount(value: number): string {
  if (!Number.isFinite(value) || value <= 0) return "0";
  return Math.round(value).toLocaleString("en-US");
}

// Share of input served from cache; "—" when there was no input at all.
export function formatHitRate(cacheReadTokens: number, inputTokens: number): string {
  const total = cacheReadTokens + inputTokens;
  if (total <= 0) return "—";
  return Math.round((cacheReadTokens / total) * 100) + "%";
}

// Go encodes an unset time as year 1; that and anything unparsable is null.
export function timeValue(value?: string | null): number | null {
  if (!value) return null;
  const ms = Date.parse(value);
  return Number.isFinite(ms) && new Date(ms).getUTCFullYear() > 1970 ? ms : null;
}

export function formatDateTime(value?: string | null): string {
  const ms = timeValue(value);
  return ms === null ? "" : new Date(ms).toLocaleString();
}

// A share in [0, 1] as a percentage: one decimal below 10%, none above.
export function formatPercent(ratio: number): string {
  if (!Number.isFinite(ratio)) return "—";
  const percent = ratio * 100;
  return (percent > 0 && percent < 10 ? percent.toFixed(1).replace(/\.0$/, "") : Math.round(percent).toString()) + "%";
}

// Milliseconds as "850 ms" or "2.3 s"; 0 means not measured.
export function formatMillis(ms: number): string {
  if (!(ms > 0)) return "—";
  if (ms < 1000) return `${Math.round(ms)} ms`;
  return `${(ms / 1000).toFixed(ms < 10_000 ? 1 : 0)} s`;
}

// Microseconds as "40 µs", "1.2 ms" or "1.5 s".
export function formatMicros(us: number): string {
  if (!Number.isFinite(us) || us < 0) return "—";
  if (us < 1000) return `${Math.round(us)} µs`;
  if (us < 1_000_000) return `${(us / 1000).toFixed(us < 10_000 ? 1 : 0)} ms`;
  return `${(us / 1_000_000).toFixed(1)} s`;
}

export function formatBytes(bytes: number): string {
  if (!(bytes > 0)) return "0 B";
  if (bytes < 1024) return `${bytes} B`;
  if (bytes < 1024 * 1024) return `${(bytes / 1024).toFixed(1)} KB`;
  return `${(bytes / 1024 / 1024).toFixed(1)} MB`;
}
