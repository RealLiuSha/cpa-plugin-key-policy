import { formatCount } from "../format";
import { useT } from "../i18n";

// Usage of a model without a price: billed $0, tokens and calls still recorded.
export default function UnpricedBadge({ calls }: { calls?: number }) {
  const t = useT();
  const title = calls === undefined ? t("usage.unpricedHint") : t("usage.unpricedCalls", { n: formatCount(calls) });
  return <span className="badge warn" title={title}>{t("usage.unpriced")}</span>;
}
