import { act } from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

(globalThis as unknown as { IS_REACT_ACT_ENVIRONMENT?: boolean }).IS_REACT_ACT_ENVIRONMENT = true;

import { createRoot } from "react-dom/client";
import { MemoryRouter, Route, Routes } from "react-router-dom";
import type { AliasMapping } from "../types";

vi.mock("../api/mappings", () => ({
  fetchAliases: vi.fn(),
  upsertAlias: vi.fn(),
  deleteAlias: vi.fn(),
  fetchClassifyRules: vi.fn(),
  upsertClassifyRule: vi.fn(),
  deleteClassifyRule: vi.fn(),
  reorderClassifyRules: vi.fn(),
  classifyPreview: vi.fn(),
  fetchCredentialDescriptors: vi.fn(),
  importAliasPrices: vi.fn(),
}));

vi.mock("../store/modelPrices", () => ({
  getPriceTable: vi.fn(async () => {
    const table = new Map([
      ["gpt-4o", {
        input_price_per_million: 2.5,
        output_price_per_million: 10,
        cache_read_price_per_million: 1.25,
      }],
    ]);
    return table;
  }),
  lookupPrice: (table: Map<string, { input_price_per_million: number; output_price_per_million: number; cache_read_price_per_million: number }> | null, model: string) => {
    if (!table || !model) return null;
    return table.get(model.toLowerCase()) ?? null;
  },
}));

vi.mock("../i18n", () => ({
  useT: () => (key: string, vars?: Record<string, string | number>) => {
    if (!vars) return key;
    return key + JSON.stringify(vars);
  },
}));

import {
  fetchAliases,
  deleteAlias,
  importAliasPrices,
} from "../api/mappings";
import Mapping, { AliasEditForm, isPassThroughAlias, isUnpricedAlias } from "./Mapping";

const tick = () => new Promise((r) => setTimeout(r, 0));
const flush = async () => {
  await act(async () => {
    await tick();
    await tick();
  });
};

const pricedAlias: AliasMapping = {
  alias: "gpt-4o",
  targets: [{ provider: "openai", target_model: "gpt-4o" }],
  dispatch: "round-robin",
  billing_mode: "tokens",
  input_price_per_million: 5,
  output_price_per_million: 30,
  cache_read_price_per_million: 1.25,
  ref_count: 2,
  ref_keys: ["k1", "k2"],
};

const unpricedOrphan: AliasMapping = {
  alias: "unused-model",
  targets: [{ provider: "openai", target_model: "unused-model" }],
  dispatch: "round-robin",
  billing_mode: "tokens",
  input_price_per_million: 0,
  output_price_per_million: 0,
  cache_read_price_per_million: 0,
  ref_count: 0,
  ref_keys: [],
};

const multiTarget: AliasMapping = {
  alias: "fast",
  targets: [
    { provider: "openai", target_model: "gpt-4o" },
    { provider: "anthropic", target_model: "claude" },
  ],
  dispatch: "priority",
  billing_mode: "tokens",
  input_price_per_million: 1,
  output_price_per_million: 2,
  cache_read_price_per_million: 0,
  ref_count: 1,
  ref_keys: ["k1"],
};

describe("isUnpricedAlias / isPassThroughAlias", () => {
  it("detects unpriced tokens aliases and pass-through 1:1", () => {
    expect(isUnpricedAlias(unpricedOrphan)).toBe(true);
    expect(isUnpricedAlias(pricedAlias)).toBe(false);
    expect(isUnpricedAlias({ ...pricedAlias, billing_mode: "per_call", per_call_usd: 0 })).toBe(false);
    expect(isPassThroughAlias(pricedAlias)).toBe(true);
    expect(isPassThroughAlias(multiTarget)).toBe(false);
    expect(isPassThroughAlias({ ...pricedAlias, alias: "Fast", targets: [{ provider: "openai", target_model: "fast" }] })).toBe(true);
  });
});

describe("AliasEditForm fetch / draft guard", () => {
  let container: HTMLDivElement;
  let root: ReturnType<typeof createRoot>;

  beforeEach(() => {
    container = document.createElement("div");
    document.body.appendChild(container);
    sessionStorage.clear();
    vi.mocked(fetchAliases).mockResolvedValue([pricedAlias, multiTarget, unpricedOrphan]);
  });

  afterEach(() => {
    act(() => root.unmount());
    container.remove();
    sessionStorage.clear();
    vi.clearAllMocks();
  });

  it("clean session edit loads server prices and targets via fetchAliases", async () => {
    // Delay fetch so we can assert the 0-price mount shell is NOT written to
    // sessionStorage before hydration.
    let resolveList: (v: AliasMapping[]) => void = () => {};
    vi.mocked(fetchAliases).mockImplementation(
      () => new Promise((res) => { resolveList = res; }),
    );

    await act(async () => {
      root = createRoot(container);
      root.render(
        <MemoryRouter initialEntries={["/mapping/alias/gpt-4o"]}>
          <Routes>
            <Route path="/mapping/alias/:aliasName" element={<AliasEditForm />} />
          </Routes>
        </MemoryRouter>,
      );
    });
    await flush();

    expect(fetchAliases).toHaveBeenCalled();
    // Pre-fetch: draft must not yet be a recoverable 0-shell for this alias.
    const preDraft = sessionStorage.getItem("cpa-key-policy:alias-form-draft");
    expect(preDraft).toBeNull();

    await act(async () => {
      resolveList([pricedAlias, multiTarget, unpricedOrphan]);
      await tick();
    });
    await flush();

    const input = container.querySelector<HTMLInputElement>('[data-testid="price-input"]');
    const output = container.querySelector<HTMLInputElement>('[data-testid="price-output"]');
    const cache = container.querySelector<HTMLInputElement>('[data-testid="price-cache"]');
    expect(input?.value).toBe("5");
    expect(output?.value).toBe("30");
    expect(cache?.value).toBe("1.25");
    const targets = container.querySelector('[data-testid="alias-targets"]');
    expect(targets?.textContent).toContain("gpt-4o");

    // Post-fetch draft may exist but must carry server prices, not zeros.
    const postRaw = sessionStorage.getItem("cpa-key-policy:alias-form-draft");
    expect(postRaw).not.toBeNull();
    const post = JSON.parse(postRaw!) as AliasMapping;
    expect(post.alias).toBe("gpt-4o");
    expect(post.input_price_per_million).toBe(5);
    expect(post.output_price_per_million).toBe(30);
  });

  it("same-name session draft without fromPicker still fetches (no skip guard)", async () => {
    // Simulate the old P0 bug condition: draft for same alias with 0 prices.
    sessionStorage.setItem(
      "cpa-key-policy:alias-form-draft",
      JSON.stringify({
        alias: "gpt-4o",
        targets: [],
        dispatch: "round-robin",
        billing_mode: "tokens",
        input_price_per_million: 0,
        output_price_per_million: 0,
        cache_read_price_per_million: 0,
      }),
    );
    await act(async () => {
      root = createRoot(container);
      root.render(
        <MemoryRouter initialEntries={["/mapping/alias/gpt-4o"]}>
          <Routes>
            <Route path="/mapping/alias/:aliasName" element={<AliasEditForm />} />
          </Routes>
        </MemoryRouter>,
      );
    });
    await flush();
    expect(fetchAliases).toHaveBeenCalled();
    const input = container.querySelector<HTMLInputElement>('[data-testid="price-input"]');
    expect(input?.value).toBe("5");
  });

  it("fromPicker return keeps user targets and does not require server overwrite", async () => {
    const draft: AliasMapping = {
      alias: "gpt-4o",
      targets: [{ provider: "openai", target_model: "old" }],
      dispatch: "round-robin",
      billing_mode: "tokens",
      input_price_per_million: 9,
      output_price_per_million: 9,
      cache_read_price_per_million: 9,
    };
    sessionStorage.setItem("cpa-key-policy:alias-form-draft", JSON.stringify(draft));
    // Mark is scoped to the alias name (not a global "1").
    sessionStorage.setItem("cpa-key-policy:alias-form-from-picker", "gpt-4o");

    await act(async () => {
      root = createRoot(container);
      root.render(
        <MemoryRouter
          initialEntries={[{
            pathname: "/mapping/alias/gpt-4o",
            state: {
              draftAlias: draft,
              pickedTargets: [
                { provider: "openai", target_model: "gpt-4o" },
                { provider: "anthropic", target_model: "claude" },
              ],
            },
          }]}
        >
          <Routes>
            <Route path="/mapping/alias/:aliasName" element={<AliasEditForm />} />
          </Routes>
        </MemoryRouter>,
      );
    });
    await flush();

    // Should not use server 5/30 because picker return preserves draft prices.
    const input = container.querySelector<HTMLInputElement>('[data-testid="price-input"]');
    expect(input?.value).toBe("9");
    const targets = container.querySelector('[data-testid="alias-targets"]');
    expect(targets?.textContent).toContain("claude");
    expect(targets?.textContent).toContain("gpt-4o");
  });

  it("stale fromPicker mark from abandoned new form does not leak into another alias edit", async () => {
    // Reproduce: new alias → pick targets → browser-back without leaveForm.
    // Draft + fromPicker("new") remain. Opening edit of gpt-4o must fetch
    // server data, not show my-half-built-alias.
    sessionStorage.setItem(
      "cpa-key-policy:alias-form-draft",
      JSON.stringify({
        alias: "my-half-built-alias",
        targets: [{ provider: "x", target_model: "y" }],
        dispatch: "round-robin",
        billing_mode: "tokens",
        input_price_per_million: 0,
        output_price_per_million: 0,
        cache_read_price_per_million: 0,
      }),
    );
    sessionStorage.setItem("cpa-key-policy:alias-form-from-picker", "new");

    await act(async () => {
      root = createRoot(container);
      root.render(
        <MemoryRouter initialEntries={["/mapping/alias/gpt-4o"]}>
          <Routes>
            <Route path="/mapping/alias/:aliasName" element={<AliasEditForm />} />
          </Routes>
        </MemoryRouter>,
      );
    });
    await flush();

    expect(fetchAliases).toHaveBeenCalled();
    const name = container.querySelector<HTMLInputElement>('[data-testid="alias-name"]');
    const input = container.querySelector<HTMLInputElement>('[data-testid="price-input"]');
    const output = container.querySelector<HTMLInputElement>('[data-testid="price-output"]');
    expect(name?.value).toBe("gpt-4o");
    expect(name?.value).not.toBe("my-half-built-alias");
    expect(input?.value).toBe("5");
    expect(output?.value).toBe("30");
    const targets = container.querySelector('[data-testid="alias-targets"]');
    expect(targets?.textContent).toContain("gpt-4o");
    expect(targets?.textContent).not.toContain("my-half-built-alias");
  });

  it("shows LiteLLM recommend only for single-target aliases", async () => {
    await act(async () => {
      root = createRoot(container);
      root.render(
        <MemoryRouter initialEntries={["/mapping/alias/gpt-4o"]}>
          <Routes>
            <Route path="/mapping/alias/:aliasName" element={<AliasEditForm />} />
          </Routes>
        </MemoryRouter>,
      );
    });
    await flush();
    expect(container.querySelector('[data-testid="recommend-price"]')).not.toBeNull();

    act(() => root.unmount());
    container.innerHTML = "";
    await act(async () => {
      root = createRoot(container);
      root.render(
        <MemoryRouter initialEntries={["/mapping/alias/fast"]}>
          <Routes>
            <Route path="/mapping/alias/:aliasName" element={<AliasEditForm />} />
          </Routes>
        </MemoryRouter>,
      );
    });
    await flush();
    expect(container.querySelector('[data-testid="recommend-price"]')).toBeNull();
  });
});

describe("Alias list badges / delete / pass-through fold / import UI", () => {
  let container: HTMLDivElement;
  let root: ReturnType<typeof createRoot>;

  beforeEach(() => {
    container = document.createElement("div");
    document.body.appendChild(container);
    sessionStorage.clear();
    vi.mocked(fetchAliases).mockResolvedValue([pricedAlias, multiTarget, unpricedOrphan]);
    vi.mocked(deleteAlias).mockResolvedValue(undefined);
    vi.mocked(importAliasPrices).mockImplementation(async (body) => ({
      applied: body.dry_run
        ? [{
            alias: "gpt-4o",
            old_input_price_per_million: 5,
            old_output_price_per_million: 30,
            old_cache_read_price_per_million: 1.25,
            new_input_price_per_million: 6,
            new_output_price_per_million: 31,
            new_cache_read_price_per_million: 1.5,
          }]
        : [{
            alias: "gpt-4o",
            old_input_price_per_million: 5,
            old_output_price_per_million: 30,
            old_cache_read_price_per_million: 1.25,
            new_input_price_per_million: 6,
            new_output_price_per_million: 31,
            new_cache_read_price_per_million: 1.5,
          }],
      unchanged: [],
      skipped: [{ model: "nope", reason: "no_match" }],
      affected_keys: ["k1", "k2"],
    }));
  });

  afterEach(() => {
    act(() => root.unmount());
    container.remove();
    vi.clearAllMocks();
  });

  it("renders ref counts, unpriced/orphan badges, disables delete when referenced", async () => {
    await act(async () => {
      root = createRoot(container);
      root.render(
        <MemoryRouter initialEntries={["/mapping"]}>
          <Routes>
            <Route path="/mapping" element={<Mapping />} />
          </Routes>
        </MemoryRouter>,
      );
    });
    await flush();

    // multiTarget is not pass-through → visible
    const multiCard = container.querySelector('[data-testid="alias-card-fast"]');
    expect(multiCard).not.toBeNull();
    expect(multiCard?.querySelector('[data-testid="delete-fast"]')?.hasAttribute("disabled")).toBe(true);

    // pass-through section collapsed by default
    const toggle = container.querySelector('[data-testid="passthrough-toggle"]');
    expect(toggle).not.toBeNull();
    expect(container.querySelector('[data-testid="alias-card-gpt-4o"]')).toBeNull();
    expect(container.querySelector('[data-testid="alias-card-unused-model"]')).toBeNull();

    await act(async () => {
      (toggle as HTMLButtonElement).click();
    });
    await flush();

    const priced = container.querySelector('[data-testid="alias-card-gpt-4o"]');
    const orphan = container.querySelector('[data-testid="alias-card-unused-model"]');
    expect(priced).not.toBeNull();
    expect(orphan).not.toBeNull();
    expect(orphan?.querySelector('[data-testid="badge-unpriced"]')).not.toBeNull();
    expect(orphan?.querySelector('[data-testid="badge-orphan"]')).not.toBeNull();
    expect(orphan?.querySelector('[data-testid="delete-unused-model"]')?.hasAttribute("disabled")).toBe(false);
    expect(priced?.querySelector('[data-testid="delete-gpt-4o"]')?.hasAttribute("disabled")).toBe(true);
  });

  it("import flow calls dry_run then apply with matches body", async () => {
    await act(async () => {
      root = createRoot(container);
      root.render(
        <MemoryRouter initialEntries={["/mapping"]}>
          <Routes>
            <Route path="/mapping" element={<Mapping />} />
          </Routes>
        </MemoryRouter>,
      );
    });
    await flush();

    const importBtn = Array.from(container.querySelectorAll("button")).find((b) =>
      (b.textContent || "").includes("mapping.importPrices"),
    );
    expect(importBtn).toBeTruthy();
    await act(async () => { importBtn!.click(); });
    await flush();

    const fixture = JSON.stringify({
      matches: [{
        model: "gpt-4o",
        prompt_price_per_1m: 6,
        completion_price_per_1m: 31,
        cache_read_price_per_1m: 1.5,
        cache_write_price_per_1m: 0,
      }],
    });
    const ta = container.querySelector<HTMLTextAreaElement>('[data-testid="import-json"]');
    expect(ta).not.toBeNull();
    await act(async () => {
      const nativeInputValueSetter = Object.getOwnPropertyDescriptor(HTMLTextAreaElement.prototype, "value")?.set;
      nativeInputValueSetter?.call(ta, fixture);
      ta!.dispatchEvent(new Event("input", { bubbles: true }));
      ta!.dispatchEvent(new Event("change", { bubbles: true }));
    });
    await flush();

    await act(async () => {
      container.querySelector<HTMLButtonElement>('[data-testid="import-preview"]')!.click();
    });
    await flush();

    expect(importAliasPrices).toHaveBeenCalledWith(expect.objectContaining({
      dry_run: true,
      matches: expect.arrayContaining([
        expect.objectContaining({ model: "gpt-4o", prompt_price_per_1m: 6 }),
      ]),
    }));
    expect(container.querySelector('[data-testid="import-result"]')).not.toBeNull();

    await act(async () => {
      container.querySelector<HTMLButtonElement>('[data-testid="import-apply"]')!.click();
    });
    await flush();

    expect(importAliasPrices).toHaveBeenCalledWith(expect.objectContaining({
      dry_run: false,
      matches: expect.arrayContaining([
        expect.objectContaining({ model: "gpt-4o" }),
      ]),
    }));
    const calls = vi.mocked(importAliasPrices).mock.calls.map((c) => c[0].dry_run);
    expect(calls[0]).toBe(true);
    expect(calls[1]).toBe(false);
  });
});
