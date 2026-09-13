import type { ModelDefinition } from "../types";

type Translate = (key: string, variables?: Record<string, string | number>) => string;

// Match the ledger's missing cache-price fallback when showing effective prices.
export function modelPriceSummary(model: ModelDefinition, translate: Translate): string {
  if (model.free) return translate("models.free");
  if (model.billing_mode === "per_call") return translate("models.pricePerCallSummary", { price: model.per_call_usd ?? 0 });
  const multiplier = model.billing_multiplier ?? 1;
  const scaled = (price: number) => Number((price * multiplier).toPrecision(12));
  return translate("quota.multiplierSummary", { value: multiplier }) + " · " + translate("models.priceTokenSummary", {
    input: scaled(model.input_price_per_million ?? 0),
    output: scaled(model.output_price_per_million ?? 0),
    cache: scaled(model.cache_read_price_per_million || model.input_price_per_million || 0),
    write: scaled(model.cache_write_price_per_million ?? model.input_price_per_million ?? 0),
  });
}
