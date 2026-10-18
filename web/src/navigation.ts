// The page a key's usage or edit view returns to travels with the visit, so
// the key list keeps its search and the stats page its range without
// persisting either. Only those two pages are accepted.
export function keyListReturnPath(state: unknown): string {
  if (state && typeof state === "object" && "keyListReturnTo" in state) {
    const path = state.keyListReturnTo;
    if (typeof path === "string" && /^\/(?:keys|stats)(?:\?|$)/.test(path)) return path;
  }
  return "/keys";
}

// A price row is identified by its model id and provider qualifier; both ride in
// the query so model ids containing "/" survive the hash route.
export function modelEditPath(row: { name: string; provider: string }): string {
  const params = new URLSearchParams({ name: row.name });
  if (row.provider) params.set("provider", row.provider);
  return `/models/edit?${params}`;
}

export function modelNewPath(name = ""): string {
  return name ? `/models/new?${new URLSearchParams({ name })}` : "/models/new";
}
