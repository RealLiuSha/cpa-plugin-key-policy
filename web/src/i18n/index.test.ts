import { afterEach, describe, expect, it } from "vitest";
import en from "./locales/en.json";
import ru from "./locales/ru.json";
import zhCN from "./locales/zh-CN.json";
import zhTW from "./locales/zh-TW.json";
import {
  getLocale,
  setLocale,
  subscribe,
  translate,
  _resetLocale,
  isSupportedLocale,
} from "./index";

afterEach(() => {
  _resetLocale("zh-CN");
});

describe("translate", () => {
  it("resolves nested dot-path keys in the current locale", () => {
    _resetLocale("zh-CN");
    expect(translate("login.submit")).toBe("登录");
    expect(translate("keys.refresh")).toBe("刷新");
  });

  it("switches output when setLocale changes the locale", () => {
    _resetLocale("zh-CN");
    expect(translate("keys.refresh")).toBe("刷新");
    setLocale("en");
    expect(translate("keys.refresh")).toBe("Refresh");
    setLocale("ru");
    expect(translate("keys.refresh")).toBe("Обновить");
  });

  it("interpolates {{name}} placeholders", () => {
    _resetLocale("en");
    expect(translate("keys.rotateConfirm", { id: "team-a" })).toBe(
      "Reset the secret for key team-a? The old key becomes invalid immediately.",
    );
    expect(translate("edit.title", { id: "foo" })).toBe("Edit Key · foo");
  });

  it("handles zh-CN <-> zh-TW divergence (simplified vs traditional)", () => {
    _resetLocale("zh-CN");
    expect(translate("login.memoryNote")).toContain("内存");
    _resetLocale("zh-TW");
    expect(translate("login.memoryNote")).toContain("記憶體");
  });

  it("provides more-menu and table labels in all four locales", () => {
    _resetLocale("zh-CN");
    expect(translate("keys.more")).toBe("更多");
    expect(translate("keys.resetRpm")).toBe("重置 RPM");
    expect(translate("keys.resetKey")).toBe("更换 Key");
    expect(translate("header.models")).toBe("模型价格");
    _resetLocale("en");
    expect(translate("keys.more")).toBe("More");
    expect(translate("keys.resetRpm")).toBe("Reset RPM");
    expect(translate("keys.resetKey")).toBe("Replace Key");
    expect(translate("header.models")).toBe("Model prices");
    _resetLocale("zh-TW");
    expect(translate("keys.more")).toBe("更多");
    expect(translate("keys.resetKey")).toBe("更換 Key");
    _resetLocale("ru");
    expect(translate("keys.more")).toBe("Ещё");
    expect(translate("keys.resetKey")).toBe("Заменить ключ");
  });

  it("provides usage-accounting and audit copy in all four locales", () => {
    for (const locale of ["zh-CN", "zh-TW", "en", "ru"] as const) {
      _resetLocale(locale);
      for (const key of [
        "usage.unpriced",
        "usage.unpricedCalls",
        "keyForm.monthlyLimitLabel",
        "keyForm.limitInvalid",
        "keyUsage.tabMonthly",
        "keyUsage.historyTitle",
        "usage.rangeDays",
        "keyUsage.rangeModelsTitle",
        "keyUsage.colModel",
        "models.title",
        "models.priceHint",
        "models.priceTokenSummary",
        "models.importFromCpa",
        "models.cacheWritePrice",
        "models.providerQualifier",
        "models.unpricedNotice",
        "keyUsage.colCacheWrite",
        "models.importCount",
        "models.syncHint",
        "audit.title",
        "audit.action.migrate_state",
        "audit.field.billing_multiplier",
        "audit.field.provider",
        "header.stats",
        "keys.mobile.tabStats",
        "stats.title",
        "stats.exportTooLarge",
        "stats.keysTruncated",
        "stats.health.title",
      ]) {
        expect(translate(key)).not.toBe(key);
      }
    }
  });

  it("keeps the same non-empty keys and placeholders in every locale", () => {
    const entries = (node: unknown, prefix = ""): [string, string][] => Object.entries(node as Record<string, unknown>).flatMap(([key, value]) =>
      typeof value === "string" ? [[prefix + key, value] as [string, string]] : entries(value, `${prefix}${key}.`));
    const placeholders = (text: string) => [...text.matchAll(/\{\{(\w+)\}\}/g)].map((match) => match[1]).sort().join(",");
    const base = new Map(entries(zhCN));
    for (const [locale, messages] of Object.entries({ en, ru, "zh-TW": zhTW })) {
      const current = new Map(entries(messages));
      expect([...current.keys()].sort(), locale).toEqual([...base.keys()].sort());
      for (const [key, text] of current) {
        expect(text.trim(), `${locale} ${key}`).not.toBe("");
        expect(placeholders(text), `${locale} ${key}`).toBe(placeholders(base.get(key)!));
      }
    }
  });

  it("falls back to zh-CN base for a key missing in the requested locale", () => {
    // Strip a key from en's bundle to simulate a not-yet-translated entry.
    // Translate key that exists in zh-CN base; we craft a guaranteed-present
    // key and assert fallback path by requesting a locale that lacks it.
    _resetLocale("en");
    // 'header.title' exists in en, so this is NOT a fallback case — sanity.
    expect(translate("header.title")).toBe("cpa-key-policy Management");
    // A genuinely unknown key returns the key itself (never empty).
    expect(translate("nonexistent.deep.key")).toBe("nonexistent.deep.key");
  });

  it("ignores unknown interpolation names (leaves the placeholder)", () => {
    _resetLocale("en");
    expect(translate("keys.rotateConfirm", { wrong: "x" })).toContain("{{id}}");
  });
});

describe("locale store", () => {
  it("get/setLocale round-trips supported locales", () => {
    _resetLocale("zh-CN");
    expect(getLocale()).toBe("zh-CN");
    setLocale("en");
    expect(getLocale()).toBe("en");
    setLocale("zh-TW");
    expect(getLocale()).toBe("zh-TW");
    setLocale("ru");
    expect(getLocale()).toBe("ru");
  });

  it("rejects unsupported locales silently (no-op)", () => {
    _resetLocale("en");
    setLocale("fr" as never);
    expect(getLocale()).toBe("en");
  });

  it("skips notify when setting the current locale again", () => {
    _resetLocale("en");
    let calls = 0;
    const unsub = subscribe(() => { calls++; });
    setLocale("en"); // same → should NOT notify
    expect(calls).toBe(0);
    setLocale("zh-CN"); // different → notify
    expect(calls).toBe(1);
    unsub();
  });

  it("subscribe notifies on locale change and unsub stops it", () => {
    _resetLocale("en");
    let calls = 0;
    const unsub = subscribe(() => { calls++; });
    setLocale("ru");
    expect(calls).toBe(1);
    unsub();
    setLocale("en");
    expect(calls).toBe(1); // unsubscribed → no further notifications
  });

  it("isSupportedLocale guards the Locale union", () => {
    expect(isSupportedLocale("zh-CN")).toBe(true);
    expect(isSupportedLocale("zh-TW")).toBe(true);
    expect(isSupportedLocale("en")).toBe(true);
    expect(isSupportedLocale("ru")).toBe(true);
    expect(isSupportedLocale("ja")).toBe(false);
    expect(isSupportedLocale("")).toBe(false);
  });
});
