// List state travels with a detail/edit visit, without persisting user searches.
export function keyListReturnPath(state: unknown): string {
  if (state && typeof state === "object" && "keyListReturnTo" in state) {
    const path = state.keyListReturnTo;
    if (typeof path === "string" && /^\/keys(?:\?|$)/.test(path)) return path;
  }
  return "/keys";
}
