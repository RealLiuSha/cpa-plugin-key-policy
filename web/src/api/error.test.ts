import { afterEach, describe, expect, it } from "vitest";
import { _resetLocale, translate } from "../i18n";
import { extractApiError } from "./error";

afterEach(() => _resetLocale());

describe("extractApiError", () => {
  it("prefers a non-empty management API error message", () => {
    expect(extractApiError({
      response: { data: { error: { message: "quota rejected" } } },
      message: "request failed",
    }, "fallback")).toBe("quota rejected");
  });

  it("reads CPA's own string error bodies", () => {
    expect(extractApiError({ response: { data: { error: "invalid config" } }, message: "Request failed" }, "fallback")).toBe("invalid config");
    // CPA's plugin endpoints put a code in error and the text in message.
    expect(extractApiError({ response: { data: { error: "invalid_config", message: "yaml: line 3: bad indent" } }, message: "Request failed" }, "fallback")).toBe("yaml: line 3: bad indent");
  });

  it("shows the plugin's hot-replacement and module errors in the operator's language", () => {
    _resetLocale("zh-CN");
    const upgrading = { response: { data: { error: { code: "upgrading", message: "a newer plugin instance is taking over; retry in a moment" } } } };
    expect(extractApiError(upgrading, "fallback")).toBe("萝卜正在热替换，请稍后重试。");
    const unavailable = { response: { data: { error: { code: "module_unavailable", message: "search is unavailable; see the settings page" } } } };
    expect(extractApiError(unavailable, "fallback")).toBe("数据文件打不开，详见设置页");
    // The same code after a rolled-back replacement: retrying does not help.
    const readOnly = { response: { data: { error: { code: "upgrading", message: "this plugin instance is read-only: it could not take the data directory back after a rolled-back replacement; see the settings page" } } } };
    expect(extractApiError(readOnly, "fallback")).toBe(translate("settings.readOnly"));
    expect(extractApiError(readOnly, "fallback")).not.toBe(translate("quality.upgrading"));
  });

  it("falls back to a regular Error message", () => {
    expect(extractApiError(new Error("network failed"), "fallback")).toBe("network failed");
  });

  it("uses the fallback for empty or non-object errors", () => {
    expect(extractApiError({ message: "  " }, "fallback")).toBe("fallback");
    expect(extractApiError(null, "fallback")).toBe("fallback");
  });
});
