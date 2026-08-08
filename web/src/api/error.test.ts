import { describe, expect, it } from "vitest";
import { extractApiError } from "./error";

describe("extractApiError", () => {
  it("prefers a non-empty management API error message", () => {
    expect(extractApiError({
      response: { data: { error: { message: "quota rejected" } } },
      message: "request failed",
    }, "fallback")).toBe("quota rejected");
  });

  it("falls back to a regular Error message", () => {
    expect(extractApiError(new Error("network failed"), "fallback")).toBe("network failed");
  });

  it("uses the fallback for empty or non-object errors", () => {
    expect(extractApiError({ message: "  " }, "fallback")).toBe("fallback");
    expect(extractApiError(null, "fallback")).toBe("fallback");
  });
});
