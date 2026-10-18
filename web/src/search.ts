import { useCallback, useEffect, useRef, useState, type ChangeEvent, type CompositionEvent } from "react";
import type { useSearchParams } from "react-router-dom";

export const SEARCH_DEBOUNCE_MS = 250;

type SetSearchParams = ReturnType<typeof useSearchParams>[1];
export type SearchDraft = ReturnType<typeof useSearchDraft>;

// A search box keeps its own text so an IME composition (pinyin, kana, …)
// never reaches the URL's q half-typed: nothing is committed between
// compositionstart and compositionend, the finished text is committed at once,
// and plain typing is committed after a short pause. Committing resets the page
// and keeps every other parameter.
export function useSearchDraft(query: string, setParams: SetSearchParams) {
  const [value, setValue] = useState(query);
  const composing = useRef(false);
  const timer = useRef<number | undefined>(undefined);
  const committed = useRef(query);
  // A pending timer must commit against the latest params, not the ones of the
  // render that scheduled it, or it would undo a sort/status change.
  const setParamsRef = useRef(setParams);
  setParamsRef.current = setParams;

  const cancel = () => window.clearTimeout(timer.current);
  const commit = useCallback((text: string) => {
    window.clearTimeout(timer.current);
    committed.current = text;
    setParamsRef.current((current) => {
      if ((current.get("q") ?? "") === text) return current;
      const next = new URLSearchParams(current);
      if (text) next.set("q", text); else next.delete("q");
      next.delete("page");
      return next;
    }, { replace: true });
  }, []);

  // Navigation (back, cleared filters) changes q without the box; adopt it,
  // except for the value this box just committed itself.
  useEffect(() => {
    if (composing.current || query === committed.current) return;
    cancel();
    committed.current = query;
    setValue(query);
  }, [query]);
  useEffect(() => cancel, []);

  return {
    value,
    onChange: (event: ChangeEvent<HTMLInputElement>) => {
      const text = event.target.value;
      setValue(text);
      if (composing.current) return;
      cancel();
      timer.current = window.setTimeout(() => commit(text), SEARCH_DEBOUNCE_MS);
    },
    onCompositionStart: () => {
      composing.current = true;
      cancel();
    },
    onCompositionEnd: (event: CompositionEvent<HTMLInputElement>) => {
      composing.current = false;
      commit(event.currentTarget.value);
    },
    reset: () => {
      cancel();
      committed.current = "";
      setValue("");
    },
  };
}
