// Synchronize this plugin UI's theme with the official CPA management panel.
//
// The panel sets `data-theme` ("white" | "dark", or absent = light greige) on
// its OWN document.documentElement. Because the plugin is loaded as a
// same-origin iframe (see _panel_ref/src/features/plugins/PluginResourcePage.tsx),
// it is a SEPARATE document and does NOT inherit the parent's attribute or CSS
// variables. So we read the parent's applied theme and mirror it onto the
// iframe's own <html>, where styles.css's `[data-theme='white'|'dark']` blocks
// take over.
//
// Why read parent's `data-theme` (the DOM) rather than localStorage's
// `resolvedTheme`: the panel's store normalizes `white` → `light` in
// `resolvedTheme`, which contradicts the actual `data-theme="white"` on the DOM.
// The DOM attribute is the source of truth for what is visually applied,
// including the `auto` setting (which the panel resolves to white/dark on the
// DOM directly). Reading the DOM avoids the white/light mismatch.
//
// When NOT embedded (direct page open, self === top) there is no parent to
// follow; we fall back to light (the default :root) — standalone use keeps the
// existing look. We do not read localStorage in that case, so a standalone open
// never silently picks the panel's saved theme.

const THEME_ATTR = "data-theme";
const PANEL_THEME_STORAGE_KEY = "cli-proxy-theme";

export type AppliedTheme = "light" | "white" | "dark";

let observer: MutationObserver | null = null;
let storageHandler: ((e: StorageEvent) => void) | null = null;
let started = false;

function isEmbedded(): boolean {
  try {
    return window.self !== window.top;
  } catch {
    // Cross-origin access to window.top throws → not same-origin → not embedded
    // in the panel; treat as standalone.
    return false;
  }
}

// Resolve the panel's currently-applied theme from the parent document.
// Returns null when there is no readable same-origin parent (standalone, or
// cross-origin embed which we can't read).
function readParentTheme(): AppliedTheme | null {
  if (!isEmbedded()) return null;
  let parentEl: HTMLElement | null;
  try {
    parentEl = window.parent.document.documentElement;
  } catch {
    return null;
  }
  const raw = parentEl.getAttribute(THEME_ATTR);
  if (raw === "white" || raw === "dark") return raw;
  // Absent attribute (or any other value) = light greige, per panel applyTheme.
  return "light";
}

function applyThemeToSelf(theme: AppliedTheme): void {
  const el = document.documentElement;
  if (theme === "white" || theme === "dark") {
    el.setAttribute(THEME_ATTR, theme);
  } else {
    // light = remove attribute so :root (default light tokens) applies,
    // matching the panel's `applyTheme` semantics.
    el.removeAttribute(THEME_ATTR);
  }
}

// Compute current theme (from parent or light fallback) and apply it.
function sync(): void {
  applyThemeToSelf(readParentTheme() ?? "light");
}

// Start mirroring the parent panel's theme and keep it in sync.
// Idempotent: calling twice is a no-op.
export function initThemeSync(): void {
  if (started) return;
  started = true;

  // Apply immediately so the first paint matches the panel.
  sync();

  if (!isEmbedded()) return; // standalone: nothing to watch.

  // Watch the parent's <html> data-theme attribute. This catches:
  //  - user clicking a theme in the panel (setTheme → applyTheme writes DOM)
  //  - `auto` resolving to a different theme when the system preference changes
  let parentEl: HTMLElement;
  try {
    parentEl = window.parent.document.documentElement;
  } catch {
    return;
  }
  observer = new MutationObserver(() => sync());
  observer.observe(parentEl, { attributes: true, attributeFilter: [THEME_ATTR] });

  // Also listen for same-origin localStorage changes as a backstop — covers any
  // path that rewrites `cli-proxy-theme` and then re-applies the DOM.
  storageHandler = (e: StorageEvent) => {
    if (e.key === PANEL_THEME_STORAGE_KEY) sync();
  };
  window.addEventListener("storage", storageHandler);
}

// For tests: tear down listeners and reset state so a new init can run.
export function _teardownThemeSync(): void {
  observer?.disconnect();
  observer = null;
  if (storageHandler) {
    window.removeEventListener("storage", storageHandler);
    storageHandler = null;
  }
  started = false;
  document.documentElement.removeAttribute(THEME_ATTR);
}

// For tests: expose the resolver so cases can assert theme resolution without a live parent.
export function _resolveParentTheme(): AppliedTheme | null {
  return readParentTheme();
}
