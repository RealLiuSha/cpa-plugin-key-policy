import { formatCount, formatUSD } from "../format";
import { useT } from "../i18n";

export interface UsageChartPoint {
  date: string;
  usd: number;
  calls: number;
}

const WIDTH = 640;
const HEIGHT = 196;
const PLOT_TOP = 18;
const PLOT_BOTTOM = 160;
// A charged day never draws flatter than this, so a $0.004 day beside a $40
// day is still visible.
const MIN_BAR_HEIGHT = 2;

// The scale top: the value rounded up to 1, 2, 2.5 or 5 × 10^n so the tick
// labels stay short.
export function chartScaleMax(value: number): number {
  if (!(value > 0)) return 1;
  const magnitude = 10 ** Math.floor(Math.log10(value));
  return ([1, 2, 2.5, 5, 10].find((factor) => factor * magnitude >= value) ?? 10) * magnitude;
}

// Daily charged amounts as bars: dollar gridlines at the scale top and middle,
// about six dates along the bottom, and an optional limit line.
export default function UsageChart({ points, label, limit }: {
  points: UsageChartPoint[];
  label: string;
  limit?: { usd: number; label: string };
}) {
  const t = useT();
  const limitUSD = limit && limit.usd > 0 ? limit.usd : 0;
  const scaleMax = chartScaleMax(Math.max(limitUSD, ...points.map((point) => point.usd)));
  const y = (value: number) => PLOT_BOTTOM - (value / scaleMax) * (PLOT_BOTTOM - PLOT_TOP);
  const slot = WIDTH / Math.max(1, points.length);
  const labelEvery = points.length <= 10 ? 1 : Math.ceil(points.length / 6);
  return (
    <svg className="usage-chart" viewBox={`0 0 ${WIDTH} ${HEIGHT}`} role="img" aria-label={label}>
      {[scaleMax, scaleMax / 2].map((tick) => (
        <line key={tick} x1={0} x2={WIDTH} y1={y(tick)} y2={y(tick)} className="usage-chart-grid" />
      ))}
      <line x1={0} x2={WIDTH} y1={PLOT_BOTTOM} y2={PLOT_BOTTOM} className="usage-chart-axis" />
      {points.map((point, index) => {
        const height = point.usd > 0 ? Math.max(MIN_BAR_HEIGHT, PLOT_BOTTOM - y(point.usd)) : 0;
        return (
          <rect key={point.date} x={index * slot + slot * 0.16} y={PLOT_BOTTOM - height} width={Math.max(1, slot * 0.68)} height={height} className="usage-chart-bar">
            <title>{point.date}: {formatUSD(point.usd)} · {t("keys.mobile.callCount", { n: formatCount(point.calls) })}</title>
          </rect>
        );
      })}
      {limitUSD > 0 && (
        <line x1={0} x2={WIDTH} y1={y(limitUSD)} y2={y(limitUSD)} className="usage-chart-limit" data-testid="usage-chart-limit-line">
          <title>{limit!.label}</title>
        </line>
      )}
      {[scaleMax, scaleMax / 2].map((tick) => (
        <text key={tick} x={0} y={y(tick) - 4} className="usage-chart-label">{formatUSD(tick)}</text>
      ))}
      {points.map((point, index) => {
        const last = index === points.length - 1;
        if (index % labelEvery !== 0 && !last) return null;
        // The first centred label is nudged right so it is not clipped at x=0.
        return (
          <text key={point.date} x={last ? WIDTH : Math.max(22, index * slot + slot / 2)} y={HEIGHT - 18} textAnchor={last ? "end" : "middle"} className="usage-chart-label">
            {point.date.slice(5)}
          </text>
        );
      })}
    </svg>
  );
}
