import { chartScaleMax } from "./UsageChart";
import { useT } from "../i18n";
import type { ChannelHour } from "../types";

// Same frame and classes as UsageChart; this one counts attempts per hour and
// draws each hour's failed attempts at the base of its bar.
const WIDTH = 640;
const HEIGHT = 196;
const PLOT_TOP = 18;
const PLOT_BOTTOM = 160;
const MIN_BAR_HEIGHT = 2;

function hourLabel(iso: string): string {
  const date = new Date(iso);
  return Number.isNaN(date.getTime()) ? iso : `${String(date.getHours()).padStart(2, "0")}:00`;
}

const tick = (value: number) => (Number.isInteger(value) ? String(value) : value.toFixed(1));

export default function HourlyChart({ hours, label }: { hours: ChannelHour[]; label: string }) {
  const t = useT();
  const scaleMax = Math.max(2, chartScaleMax(Math.max(0, ...hours.map((hour) => hour.attempts))));
  const height = (value: number) => (value > 0 ? Math.max(MIN_BAR_HEIGHT, (value / scaleMax) * (PLOT_BOTTOM - PLOT_TOP)) : 0);
  const y = (value: number) => PLOT_BOTTOM - (value / scaleMax) * (PLOT_BOTTOM - PLOT_TOP);
  const slot = WIDTH / Math.max(1, hours.length);
  const labelEvery = hours.length <= 12 ? 2 : Math.ceil(hours.length / 6);
  return (
    <svg className="usage-chart" viewBox={`0 0 ${WIDTH} ${HEIGHT}`} role="img" aria-label={label}>
      {[scaleMax, scaleMax / 2].map((value) => (
        <line key={value} x1={0} x2={WIDTH} y1={y(value)} y2={y(value)} className="usage-chart-grid" />
      ))}
      <line x1={0} x2={WIDTH} y1={PLOT_BOTTOM} y2={PLOT_BOTTOM} className="usage-chart-axis" />
      {hours.map((hour, index) => {
        const x = index * slot + slot * 0.16;
        const width = Math.max(1, slot * 0.68);
        const total = height(hour.attempts);
        const failed = Math.min(total, height(hour.failures));
        return (
          <g key={hour.hour}>
            <title>{hourLabel(hour.hour)} · {t("channels.hourTitle", { attempts: hour.attempts, failures: hour.failures })}</title>
            <rect x={x} y={PLOT_BOTTOM - total} width={width} height={total} className="usage-chart-bar" />
            {failed > 0 && <rect x={x} y={PLOT_BOTTOM - failed} width={width} height={failed} className="usage-chart-bar failed" />}
          </g>
        );
      })}
      {[scaleMax, scaleMax / 2].map((value) => (
        <text key={value} x={0} y={y(value) - 4} className="usage-chart-label">{tick(value)}</text>
      ))}
      {hours.map((hour, index) => {
        const last = index === hours.length - 1;
        if (index % labelEvery !== 0 && !last) return null;
        return (
          <text key={hour.hour} x={last ? WIDTH : Math.max(22, index * slot + slot / 2)} y={HEIGHT - 18} textAnchor={last ? "end" : "middle"} className="usage-chart-label">
            {hourLabel(hour.hour)}
          </text>
        );
      })}
    </svg>
  );
}
