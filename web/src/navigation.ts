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

export type NavSection = "overview" | "keys" | "channels" | "models" | "stats" | "search" | "quality" | "inspection" | "audit" | "settings";

export interface NavItem {
  id: NavSection;
  path: string;
  icon: string;
  // Primary items have their own slot in the mobile tab bar; the rest share
  // its "more" sheet.
  primary: boolean;
}

// The panel menu in desktop order; labels are header.<id>.
export const NAV_ITEMS: readonly NavItem[] = [
  { id: "overview", path: "/overview", icon: "◎", primary: true },
  { id: "keys", path: "/keys", icon: "#", primary: true },
  { id: "channels", path: "/channels", icon: "⇄", primary: true },
  { id: "models", path: "/models", icon: "¤", primary: false },
  { id: "stats", path: "/stats", icon: "$", primary: true },
  { id: "search", path: "/search", icon: "⌕", primary: false },
  { id: "quality", path: "/quality", icon: "✓", primary: false },
  { id: "inspection", path: "/inspection", icon: "◷", primary: false },
  { id: "audit", path: "/audit", icon: "≡", primary: false },
  { id: "settings", path: "/settings", icon: "⚙", primary: false },
];

// The section a path belongs to: /keys/k1/usage is under keys.
export function navSection(pathname: string): NavSection | null {
  return NAV_ITEMS.find((item) => pathname === item.path || pathname.startsWith(item.path + "/"))?.id ?? null;
}

// A channel's detail opens from the URL, so other pages can link to it.
export function channelPath(authIndex: string): string {
  return `/channels?${new URLSearchParams({ auth_index: authIndex })}`;
}
