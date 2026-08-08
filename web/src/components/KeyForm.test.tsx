import { act } from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

(globalThis as unknown as { IS_REACT_ACT_ENVIRONMENT?: boolean }).IS_REACT_ACT_ENVIRONMENT = true;

import { createRoot } from "react-dom/client";
import { Simulate } from "react-dom/test-utils";
import { MemoryRouter } from "react-router-dom";
import type { AliasMapping, KeyPublic, ModelRule } from "../types";

vi.mock("../api/mappings", () => ({
  fetchAliases: vi.fn(),
}));

vi.mock("../api/models", () => ({
  formatTierLabel: (_t: (k: string) => string, group: string) => group,
  fetchCatalog: vi.fn(async () => []),
  groupByCatalog: () => [],
}));

vi.mock("./ModelPicker", () => ({
  default: () => <div data-testid="model-picker" />,
}));

vi.mock("../i18n", () => ({
  useT: () => (key: string, vars?: Record<string, string | number>) => {
    if (!vars) return key;
    // Mirror real templates for keys that only carry {{n}} in locale files.
    let s = key;
    if (key === "keyForm.unpricedAfterSave") s = "{{n}} alias(es) still unpriced";
    if (key === "keyForm.currentUsage") s = "Current usage {{amount}}";
    if (key === "keyForm.limitBelowUsageConfirm") s = "Blocking limits:\n{{details}}";
    for (const [k, v] of Object.entries(vars)) {
      s = s.replace(new RegExp(`\\{\\{${k}\\}\\}`, "g"), String(v));
    }
    return s;
  },
}));

import { fetchAliases } from "../api/mappings";
import KeyForm, { isUnpricedAlias, keyWriteRequestFromForm, modelsWithoutPrices } from "./KeyForm";

const tick = () => new Promise((r) => setTimeout(r, 0));
const flush = async () => {
  await act(async () => {
    await tick();
    await tick();
  });
};

const globalAliases: AliasMapping[] = [
  {
    alias: "priced",
    targets: [{ provider: "openai", target_model: "priced" }],
    dispatch: "round-robin",
    billing_mode: "tokens",
    input_price_per_million: 5,
    output_price_per_million: 30,
    cache_read_price_per_million: 1,
  },
  {
    alias: "unpriced",
    targets: [{ provider: "openai", target_model: "unpriced" }],
    dispatch: "round-robin",
    billing_mode: "tokens",
    input_price_per_million: 0,
    output_price_per_million: 0,
    cache_read_price_per_million: 0,
  },
];

const initial: KeyPublic = {
  id: "k1",
  name: "K1",
  enabled: true,
  key_preview: "cpa_xx",
  rpm: 10,
  models: [
    { alias: "priced", provider: "openai", target_model: "priced" },
    { alias: "unpriced", provider: "openai", target_model: "unpriced" },
  ],
  aliases: [
    { alias: "priced", daily_limit_usd: 2, input_price_per_million: 4 },
    { alias: "unpriced", daily_limit_usd: 0 },
  ],
  daily_limit_usd: 0,
  weekly_limit_usd: 0,
  usage: { daily_usd: 0, weekly_usd: 0, daily_limit_usd: 0, weekly_limit_usd: 0 },
};

describe("modelsWithoutPrices / isUnpricedAlias", () => {
  it("strips price fields from models payload", () => {
    const models: ModelRule[] = [{
      alias: "a",
      provider: "p",
      target_model: "m",
      group: "team",
      input_price_per_million: 1,
      output_price_per_million: 2,
      cache_read_price_per_million: 3,
      billing_mode: "tokens",
      per_call_usd: 0.5,
    }];
    const out = modelsWithoutPrices(models);
    expect(out).toEqual([{ alias: "a", provider: "p", target_model: "m", group: "team" }]);
    expect(out[0]).not.toHaveProperty("input_price_per_million");
    expect(out[0]).not.toHaveProperty("billing_mode");
    expect(out[0]).not.toHaveProperty("per_call_usd");
  });

  it("detects unpriced tokens aliases", () => {
    expect(isUnpricedAlias(globalAliases[1])).toBe(true);
    expect(isUnpricedAlias(globalAliases[0])).toBe(false);
  });

  it("builds one complete API request so page wrappers cannot drop new limit fields", () => {
    expect(keyWriteRequestFromForm({
      id: "k1",
      name: "",
      enabled: true,
      rpm: 10,
      models: initial.models,
      aliases: initial.aliases ?? [],
      daily_limit_usd: 10,
      weekly_limit_usd: 20,
      monthly_limit_usd: 30,
      allow_models_endpoint: true,
    })).toMatchObject({
      id: "k1",
      name: undefined,
      aliases: initial.aliases,
      monthly_limit_usd: 30,
      allow_models_endpoint: true,
    });
  });
});

describe("KeyForm de-price + unpriced warnings", () => {
  let container: HTMLDivElement;
  let root: ReturnType<typeof createRoot>;

  beforeEach(() => {
    container = document.createElement("div");
    document.body.appendChild(container);
    vi.mocked(fetchAliases).mockResolvedValue(globalAliases);
  });

  afterEach(() => {
    act(() => root.unmount());
    container.remove();
    vi.clearAllMocks();
  });

  it("has no price inputs and submits models without price keys", async () => {
    let submitted: { models: ModelRule[]; newUnpricedCount: number } | null = null;
    const onSubmit = async (v: { models: ModelRule[] }, meta: { newUnpricedCount: number }) => {
      submitted = { models: v.models, newUnpricedCount: meta.newUnpricedCount };
    };
    await act(async () => {
      root = createRoot(container);
      root.render(
        <MemoryRouter>
          <KeyForm
            initial={initial}
            pickPath="/keys/k1/edit/models"
            submitLabel="Save"
            onCancel={() => {}}
            onSubmit={onSubmit}
          />
        </MemoryRouter>,
      );
    });
    await flush();

    // No legacy price table / recommend controls.
    expect(container.textContent).not.toContain("keyForm.priceLabel");
    expect(container.textContent).not.toContain("keyForm.colInput");
    expect(container.textContent).not.toContain("keyForm.recommend");
    expect(container.textContent).not.toContain("keyForm.billingTokens");
    expect(container.querySelector('[data-testid="recommend-price"]')).toBeNull();

    // Unpriced chip warning + mapping link present.
    expect(container.textContent).toContain("keyForm.unpricedBadge");
    const link = container.querySelector('a[href="/mapping/alias/unpriced"]');
    expect(link).not.toBeNull();

    const form = container.querySelector("form");
    expect(form).not.toBeNull();
    await act(async () => {
      form!.dispatchEvent(new Event("submit", { bubbles: true, cancelable: true }));
    });
    await flush();

    expect(submitted).not.toBeNull();
    expect(submitted!.models.length).toBe(2);
    for (const m of submitted!.models) {
      expect(m).not.toHaveProperty("input_price_per_million");
      expect(m).not.toHaveProperty("output_price_per_million");
      expect(m).not.toHaveProperty("cache_read_price_per_million");
      expect(m).not.toHaveProperty("billing_mode");
      expect(m).not.toHaveProperty("per_call_usd");
      expect(m).toHaveProperty("alias");
      expect(m).toHaveProperty("provider");
      expect(m).toHaveProperty("target_model");
    }
  });

  it("after save with newly added unpriced alias passes meta and does NOT self-draw after-save banner", async () => {
    let gotMeta: { newUnpricedCount: number } | null = null;
    const onSubmit = async (_v: { models: ModelRule[] }, meta: { newUnpricedCount: number }) => {
      gotMeta = meta;
      // Parent owns the banner (KeyEdit/KeyNew); KeyForm must not duplicate it.
    };
    // Start with only priced model; add unpriced via chip.
    const onlyPriced: KeyPublic = {
      ...initial,
      models: [{ alias: "priced", provider: "openai", target_model: "priced" }],
    };
    await act(async () => {
      root = createRoot(container);
      root.render(
        <MemoryRouter>
          <KeyForm
            initial={onlyPriced}
            pickPath="/keys/k1/edit/models"
            submitLabel="Save"
            onCancel={() => {}}
            onSubmit={onSubmit}
          />
        </MemoryRouter>,
      );
    });
    await flush();

    // Click the unpriced global alias chip to add it.
    const chip = Array.from(container.querySelectorAll("button")).find((b) =>
      (b.textContent || "").includes("unpriced"),
    );
    expect(chip).toBeTruthy();
    await act(async () => { chip!.click(); });
    await flush();

    const form = container.querySelector("form");
    await act(async () => {
      form!.dispatchEvent(new Event("submit", { bubbles: true, cancelable: true }));
    });
    await flush();

    expect(gotMeta).toEqual({ newUnpricedCount: 1 });
    // KeyForm itself must not render the after-save banner.
    expect(container.querySelectorAll('[data-testid="unpriced-after-save"]').length).toBe(0);
  });

  it("waits for aliases to load before submitting so newUnpricedCount is not inflated", async () => {
    let resolveAliases: (v: AliasMapping[]) => void = () => {};
    vi.mocked(fetchAliases).mockImplementation(
      () => new Promise((res) => { resolveAliases = res; }),
    );

    let submittedMeta: { newUnpricedCount: number } | null = null;
    const onlyPriced: KeyPublic = {
      ...initial,
      models: [{ alias: "priced", provider: "openai", target_model: "priced" }],
    };

    await act(async () => {
      root = createRoot(container);
      root.render(
        <MemoryRouter>
          <KeyForm
            initial={onlyPriced}
            pickPath="/keys/k1/edit/models"
            submitLabel="Save"
            onCancel={() => {}}
            onSubmit={async (_v, meta) => { submittedMeta = meta; }}
          />
        </MemoryRouter>,
      );
    });
    await flush();

    // Submit while aliases still inflight — must not call onSubmit yet.
    const form = container.querySelector("form");
    await act(async () => {
      form!.dispatchEvent(new Event("submit", { bubbles: true, cancelable: true }));
    });
    await flush();
    expect(submittedMeta).toBeNull();
    expect(container.querySelector('[data-testid="keyform-submit"]')?.hasAttribute("disabled")).toBe(true);

    // Resolve with priced + unpriced; select unpriced then submit.
    await act(async () => {
      resolveAliases(globalAliases);
      await tick();
    });
    await flush();

    const chip = Array.from(container.querySelectorAll("button")).find((b) =>
      (b.textContent || "").includes("unpriced"),
    );
    expect(chip).toBeTruthy();
    await act(async () => { chip!.click(); });
    await flush();

    await act(async () => {
      form!.dispatchEvent(new Event("submit", { bubbles: true, cancelable: true }));
    });
    await flush();

    expect(submittedMeta).toEqual({ newUnpricedCount: 1 });
  });

  it("does not count already-priced newly-selected aliases as unpriced once table is ready", async () => {
    let gotMeta: { newUnpricedCount: number } | null = null;
    const emptyKey: KeyPublic = {
      ...initial,
      models: [],
    };
    await act(async () => {
      root = createRoot(container);
      root.render(
        <MemoryRouter>
          <KeyForm
            initial={emptyKey}
            pickPath="/keys/k1/edit/models"
            submitLabel="Save"
            onCancel={() => {}}
            onSubmit={async (_v, meta) => { gotMeta = meta; }}
          />
        </MemoryRouter>,
      );
    });
    await flush();

    // Select the already-priced global alias only.
    const chip = Array.from(container.querySelectorAll("button")).find((b) =>
      (b.textContent || "").trim() === "priced" || (b.textContent || "").startsWith("priced"),
    );
    expect(chip).toBeTruthy();
    await act(async () => { chip!.click(); });
    await flush();

    await act(async () => {
      container.querySelector("form")!.dispatchEvent(new Event("submit", { bubbles: true, cancelable: true }));
    });
    await flush();

    expect(gotMeta).toEqual({ newUnpricedCount: 0 });
  });

  it("shows current window usage and confirms a limit that would block immediately", async () => {
    const onSubmit = vi.fn(async () => undefined);
    const confirm = vi.fn(() => false);
    vi.stubGlobal("confirm", confirm);
    const used: KeyPublic = {
      ...initial,
      daily_limit_usd: 10,
      weekly_limit_usd: 20,
      monthly_limit_usd: 30,
      usage: {
        daily_usd: 8,
        weekly_usd: 12,
        monthly_usd: 18,
        daily_limit_usd: 10,
        weekly_limit_usd: 20,
        monthly_limit_usd: 30,
      },
    };
    await act(async () => {
      root = createRoot(container);
      root.render(
        <MemoryRouter>
          <KeyForm initial={used} submitLabel="Save" onCancel={() => {}} onSubmit={onSubmit} />
        </MemoryRouter>,
      );
    });
    await flush();
    const currentUsage = Array.from(container.querySelectorAll(".mobile-hidden .kf-current-usage")).map((element) => element.textContent);
    expect(currentUsage).toEqual(["Current usage 8.00", "Current usage 12.00", "Current usage 18.00"]);
    const numericInputs = container.querySelectorAll<HTMLInputElement>('.mobile-hidden input[type="number"]');
    const dailyInput = numericInputs[1];
    await act(async () => {
      Simulate.change(dailyInput, { target: { value: "7" } } as never);
    });
    await flush();
    await act(async () => {
      container.querySelector("form")!.dispatchEvent(new Event("submit", { bubbles: true, cancelable: true }));
    });
    await flush();
    expect(confirm).toHaveBeenCalledWith(expect.stringContaining("$8.00 → $7.00"));
    expect(onSubmit).not.toHaveBeenCalled();
    vi.unstubAllGlobals();
  });

  it("edits and submits per-alias daily limits without dropping alias refs", async () => {
    let submittedAliases: KeyPublic["aliases"] = undefined;
    await act(async () => {
      root = createRoot(container);
      root.render(
        <MemoryRouter>
          <KeyForm
            initial={initial}
            submitLabel="Save"
            onCancel={() => {}}
            onSubmit={async (values) => { submittedAliases = values.aliases; }}
          />
        </MemoryRouter>,
      );
    });
    await flush();

    const pricedLimit = container.querySelector<HTMLInputElement>('.mobile-hidden [data-alias-limit="priced"]');
    expect(pricedLimit?.value).toBe("2");
    await act(async () => {
      Simulate.change(pricedLimit!, { target: { value: "3.5" } } as never);
    });
    await act(async () => {
      container.querySelector("form")!.dispatchEvent(new Event("submit", { bubbles: true, cancelable: true }));
    });
    await flush();

    expect(submittedAliases).toEqual([
      expect.objectContaining({ alias: "priced", daily_limit_usd: 3.5, input_price_per_million: 4 }),
      expect.objectContaining({ alias: "unpriced", daily_limit_usd: 0 }),
    ]);
  });
});
