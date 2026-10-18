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
