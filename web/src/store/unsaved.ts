// Pages that hold edits not saved yet, each with the i18n key of the message
// the section links show before leaving it. Only in-page navigation asks:
// the browser's back button and the panel reloading this iframe do not.

const pages = new Map<string, string>();

// message null: the page has nothing unsaved (any more).
export function setUnsaved(page: string, message: string | null): void {
  if (message) pages.set(page, message);
  else pages.delete(page);
}

// The leave message of a page with unsaved edits, or null.
export function unsavedMessage(): string | null {
  return pages.values().next().value ?? null;
}
