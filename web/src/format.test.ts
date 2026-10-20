import { describe, expect, it } from "vitest";
import { formatCount, formatHitRate, formatUSD } from "./format";

describe("formatUSD", () => {
  it("shows two decimals for zero and amounts of a cent or more", () => {
    expect(formatUSD(0)).toBe("$0.00");
    expect(formatUSD(0.01)).toBe("$0.01");
    expect(formatUSD(1.5)).toBe("$1.50");
    expect(formatUSD(1234.567)).toBe("$1,234.57");
    expect(formatUSD(-2)).toBe("-$2.00");
  });

  it("keeps up to four decimals for a non-zero sub-cent amount", () => {
    expect(formatUSD(0.0042)).toBe("$0.0042");
    expect(formatUSD(0.004)).toBe("$0.004");
    expect(formatUSD(0.00999)).toBe("$0.01");
    expect(formatUSD(-0.0042)).toBe("-$0.0042");
  });

  it("treats non-finite input as zero", () => {
    expect(formatUSD(Number.NaN)).toBe("$0.00");
  });
});

describe("count and hit-rate formatting", () => {
  it("groups integers and shows 0 for empty counters", () => {
    expect(formatCount(1234567)).toBe("1,234,567");
    expect(formatCount(0)).toBe("0");
  });

  it("reports the cached share of input and avoids 0/0", () => {
    expect(formatHitRate(25, 75)).toBe("25%");
    expect(formatHitRate(0, 0)).toBe("—");
  });
});
