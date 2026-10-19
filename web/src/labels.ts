type Translate = (key: string, variables?: Record<string, string | number>) => string;

// The translation of key, or fallback when no locale has it: server values
// such as audit actions or rule types stay readable before they get a label.
export function labelOr(t: Translate, key: string, fallback: string): string {
  const label = t(key);
  return label === key ? fallback : label;
}
