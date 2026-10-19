import { translate } from "../i18n";

type ErrorBody = { error?: { code?: unknown; message?: unknown } | unknown; message?: unknown };

function responseBody(error: unknown): ErrorBody | undefined {
  if (typeof error !== "object" || error === null) return undefined;
  const data = (error as { response?: { data?: unknown } }).response?.data;
  return typeof data === "object" && data !== null ? data as ErrorBody : undefined;
}

// The plugin answers {"error": {"code", "message"}}; CPA's plugin endpoints
// answer {"error": "<code>", "message": "<text>"} and its other management
// routes {"error": "<text>"}. The plugin's two host-state codes are shown in
// the operator's language. upgrading also answers writes to an instance left
// read-only by a rolled-back replacement (app.go writeRefused): only its
// message tells the two apart, and retrying does not help there.
export function extractApiError(error: unknown, fallback: string): string {
  if (typeof error !== "object" || error === null) return fallback;
  const body = responseBody(error);
  const inner = body?.error;
  const text = (value: unknown) => typeof value === "string" && value.trim() ? value : undefined;
  const code = apiErrorCode(error);
  if (code === "upgrading") {
    const readOnly = text((inner as { message?: unknown }).message)?.includes("read-only");
    return translate(readOnly ? "settings.readOnly" : "quality.upgrading");
  }
  if (code === "module_unavailable") return translate("overview.moduleUnavailable");
  const responseMessage = typeof inner === "string" ? text(body?.message) ?? text(inner) : text((inner as { message?: unknown } | undefined)?.message);
  if (responseMessage) return responseMessage;
  return text((error as { message?: unknown }).message) ?? fallback;
}

// apiErrorCode is the plugin's error code, when its answer carried one.
export function apiErrorCode(error: unknown): string | undefined {
  const inner = responseBody(error)?.error;
  const code = typeof inner === "object" && inner !== null ? (inner as { code?: unknown }).code : undefined;
  return typeof code === "string" ? code : undefined;
}
