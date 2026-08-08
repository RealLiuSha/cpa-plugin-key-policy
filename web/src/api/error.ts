export function extractApiError(error: unknown, fallback: string): string {
  if (typeof error !== "object" || error === null) return fallback;
  const value = error as {
    response?: { data?: { error?: { message?: unknown } } };
    message?: unknown;
  };
  const responseMessage = value.response?.data?.error?.message;
  if (typeof responseMessage === "string" && responseMessage.trim()) return responseMessage;
  if (typeof value.message === "string" && value.message.trim()) return value.message;
  return fallback;
}
