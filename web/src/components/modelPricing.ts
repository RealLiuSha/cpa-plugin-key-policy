import type { ModelDefinition } from "../types";

type Translate = (key: string, variables?: Record<string, string | number>) => string;

const clean = (value: number) => Number(value.toPrecision(12));

// A model without any price bills $0 until prices are synced or entered.
export function isUnpriced(model: ModelDefinition): boolean {
  if (model.billing_mode === "per_call") return !((model.per_call_usd ?? 0) > 0);
  return ![
    model.input_price_per_million,
    model.output_price_per_million,
    model.cache_read_price_per_million,
    model.cache_write_price_per_million,
  ].some((price) => (price ?? 0) > 0);
}

// What a key is charged per 1M tokens. Mirrors the ledger: a zero cache-read
// price and a missing cache-write price fall back to the input price, and the
// multiplier scales every token charge.
export function effectiveTokenPrices(model: ModelDefinition) {
  const multiplier = model.billing_multiplier ?? 1;
  const input = model.input_price_per_million ?? 0;
  return {
    input: clean(input * multiplier),
    output: clean((model.output_price_per_million ?? 0) * multiplier),
    cacheRead: clean((model.cache_read_price_per_million || input) * multiplier),
    cacheWrite: clean((model.cache_write_price_per_million ?? input) * multiplier),
  };
}

// The configured prices, before the multiplier.
export function basePriceSummary(model: ModelDefinition, translate: Translate): string {
  if (isUnpriced(model)) return translate("models.unpriced");
  if (model.billing_mode === "per_call") return translate("models.pricePerCallSummary", { price: clean(model.per_call_usd ?? 0) });
  const sameAsInput = translate("models.sameAsInput");
  return translate("models.priceTokenSummary", {
    input: "$" + clean(model.input_price_per_million ?? 0),
    output: "$" + clean(model.output_price_per_million ?? 0),
    cache: model.cache_read_price_per_million ? "$" + clean(model.cache_read_price_per_million) : sameAsInput,
    write: model.cache_write_price_per_million === undefined ? sameAsInput : "$" + clean(model.cache_write_price_per_million),
  });
}

// The prices a key actually pays, for places that show one line per model.
export function modelPriceSummary(model: ModelDefinition, translate: Translate): string {
  if (isUnpriced(model)) return translate("models.unpriced");
  if (model.billing_mode === "per_call") return translate("models.pricePerCallSummary", { price: clean(model.per_call_usd ?? 0) });
  const prices = effectiveTokenPrices(model);
  return translate("models.priceTokenSummary", {
    input: "$" + prices.input,
    output: "$" + prices.output,
    cache: "$" + prices.cacheRead,
    write: "$" + prices.cacheWrite,
  });
}
