import { formatCount } from "../format";
import { useT } from "../i18n";

// Without a count the model has no price now: billed $0, tokens and calls still
// recorded. With a count the badge reports what the ledger already billed $0,
// not the price table: a price added later never re-bills those calls, so the
// badge stays until their days leave the retention window.
export default function UnpricedBadge({ calls }: { calls?: number }) {
  const t = useT();
  if (calls === undefined) return <span className="badge warn" title={t("usage.unpricedHint")}>{t("usage.unpriced")}</span>;
  const n = formatCount(calls);
  return <span className="badge warn" title={t("usage.unpricedCalls", { n })}>{t("usage.unpricedBadge", { n })}</span>;
}
