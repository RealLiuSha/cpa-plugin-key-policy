import { act } from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { createRoot } from "react-dom/client";
import { Simulate } from "react-dom/test-utils";
import { MemoryRouter, Route, Routes, useLocation } from "react-router-dom";
import type { KeyPublic, ModelDefinition } from "../types";

(globalThis as unknown as { IS_REACT_ACT_ENVIRONMENT?: boolean }).IS_REACT_ACT_ENVIRONMENT = true;

vi.mock("../api/modelDefinitions", () => ({ fetchModelDefinitions: vi.fn() }));
const { translate } = vi.hoisted(() => ({ translate: (key: string) => key }));
vi.mock("../i18n", () => ({ useT: () => translate }));

import { fetchModelDefinitions } from "../api/modelDefinitions";
import KeyForm, { keyWriteRequestFromForm, type KeyFormValues } from "./KeyForm";

const definitions: ModelDefinition[] = [
  {
    name: "fast",
    targets: [{ provider: "codex", target_model: "gpt-5" }],
    dispatch: "round-robin",
    billing_mode: "tokens",
    free: false,
    input_price_per_million: 1,
    output_price_per_million: 2,
    cache_read_price_per_million: 0.2,
  },
  {
    name: "community",
    targets: [{ provider: "openai", target_model: "small" }],
    dispatch: "priority",
    billing_mode: "tokens",
    free: true,
  },
];

const initial: KeyPublic = {
  id: "team-a",
  name: "Team A",
  enabled: true,
  key_preview: "cpa_te...am-a",
  rpm: 60,
  models: [{ name: "fast", daily_limit_usd: 2 }],
  daily_limit_usd: 10,
  weekly_limit_usd: 50,
  monthly_limit_usd: 100,
  usage: { daily_usd: 1, weekly_usd: 2, daily_limit_usd: 10, weekly_limit_usd: 50 },
};

const tick = () => new Promise((resolve) => setTimeout(resolve, 0));
let container: HTMLDivElement;
let root: ReturnType<typeof createRoot> | undefined;

beforeEach(() => {
  container = document.createElement("div");
  document.body.appendChild(container);
  root = undefined;
  vi.mocked(fetchModelDefinitions).mockResolvedValue(definitions);
});

afterEach(() => {
  if (root) act(() => root?.unmount());
  container.remove();
  vi.clearAllMocks();
});

async function flush() {
  await act(async () => { await tick(); await tick(); });
}

function collectStyleRules(rules: CSSRuleList): CSSStyleRule[] {
  return [...rules].flatMap((rule) => {
    if ("selectorText" in rule && "style" in rule) return [rule as CSSStyleRule];
    if ("cssRules" in rule) return collectStyleRules((rule as CSSMediaRule).cssRules);
    return [];
  });
}

function scanColumnRuleConflicts(css: string): { columnRuleCount: number; conflictingSelectors: string[] } {
  const style = document.createElement("style");
  style.textContent = css;
  document.head.appendChild(style);
  const fixture = document.createElement("div");
  fixture.innerHTML = '<div class="form-grid"><label class="check-row"><input type="checkbox">enabled</label></div><div class="field-row"><label class="check-row"><input type="checkbox">free</label></div>';
  document.body.appendChild(fixture);
  const checkRows = [...fixture.querySelectorAll(".check-row")];
  const columnRules = (style.sheet ? collectStyleRules(style.sheet.cssRules) : []).filter((rule) =>
    rule.style.getPropertyValue("flex-direction") === "column",
  );
  const conflictingSelectors = columnRules.flatMap((rule) =>
    rule.selectorText.split(",").map((selector) => selector.trim()).filter((selector) =>
      checkRows.some((row) => row.matches(selector)),
    ),
  );
  fixture.remove();
  style.remove();
  return { columnRuleCount: columnRules.length, conflictingSelectors };
}

describe("KeyForm v3 model references", () => {
  it("keeps the shared card layout and field hints wired", async () => {
    await act(async () => {
      root = createRoot(container);
      root.render(<MemoryRouter><KeyForm initial={initial} showCurrentUsage submitLabel="save" onCancel={() => {}} onSubmit={async () => {}} /></MemoryRouter>);
    });
    await flush();

    expect(container.querySelector("form.key-form")?.classList.contains("card")).toBe(true);
    expect(container.querySelector<HTMLInputElement>('input[placeholder="keyForm.idPlaceholder"]')).not.toBeNull();
    expect(container.querySelector<HTMLInputElement>('input[placeholder="keyForm.namePlaceholder"]')).not.toBeNull();
    expect(container.textContent).toContain("keyForm.statusLabel");
    expect(container.textContent).toContain("keyForm.currentUsage");
  });

  it("keeps checkbox rows outside every column-flow rule, including nested media rules", async () => {
    const { readFileSync } = await import("node:fs");
    const { resolve } = await import("node:path");
    const css = readFileSync(resolve(__dirname, "../styles.css"), "utf8");
    const current = scanColumnRuleConflicts(css);
    expect(current.columnRuleCount).toBeGreaterThan(0);
    expect(current.conflictingSelectors).toEqual([]);

    const withoutExclusion = scanColumnRuleConflicts(css.replaceAll("> label:not(.check-row)", "> label"));
    expect(withoutExclusion.conflictingSelectors).toEqual(expect.arrayContaining([".form-grid > label", ".field-row > label"]));

    const nestedMediaConflict = scanColumnRuleConflicts(`${css}\n@media (max-width: 640px) { .form-grid > label { flex-direction: column; } }`);
    expect(nestedMediaConflict.conflictingSelectors).toContain(".form-grid > label");
  });

  it("builds one complete request without target or price fields", () => {
    const request = keyWriteRequestFromForm({
      id: "team-a",
      name: " ",
      enabled: true,
      rpm: 60,
      models: [{ name: "fast", daily_limit_usd: 3 }],
      daily_limit_usd: 10,
      weekly_limit_usd: 20,
      monthly_limit_usd: 30,
      allow_models_endpoint: true,
    });
    expect(request).toEqual({
      id: "team-a",
      name: undefined,
      enabled: true,
      rpm: 60,
      models: [{ name: "fast", daily_limit_usd: 3 }],
      daily_limit_usd: 10,
      weekly_limit_usd: 20,
      monthly_limit_usd: 30,
      allow_models_endpoint: true,
    });
    expect(JSON.stringify(request)).not.toContain("target_model");
    expect(JSON.stringify(request)).not.toContain("price_per_million");
  });

  it("shows model prices read-only and submits selected names with per-model daily limits", async () => {
    let submitted: KeyFormValues | null = null;
    await act(async () => {
      root = createRoot(container);
      root.render(<MemoryRouter><KeyForm initial={initial} submitLabel="save" onCancel={() => {}} onSubmit={async (values) => { submitted = values; }} /></MemoryRouter>);
    });
    await flush();

    expect(container.textContent).toContain("models.priceTokenSummary");
    expect(container.textContent).toContain("models.free");
    const modelChecks = container.querySelectorAll<HTMLInputElement>(".model-definition-main input");
    expect(modelChecks).toHaveLength(2);
    expect(modelChecks[0].checked).toBe(true);
    expect(modelChecks[1].checked).toBe(false);

    const fastLimit = container.querySelector<HTMLInputElement>(".model-limit-field input");
    expect(fastLimit?.value).toBe("2");
    await act(async () => {
      Simulate.change(fastLimit!, { target: { value: "3.5" } } as never);
      modelChecks[1].click();
    });
    await act(async () => {
      container.querySelector("form")!.dispatchEvent(new Event("submit", { bubbles: true, cancelable: true }));
      await tick();
    });

    expect(submitted).not.toBeNull();
    expect(submitted!.models).toEqual([
      { name: "fast", daily_limit_usd: 3.5 },
      { name: "community", daily_limit_usd: 0 },
    ]);
    expect(JSON.stringify(submitted!.models)).not.toContain("provider");
  });

  it("applies bulk model selection and clearing only to the visible result", async () => {
    await act(async () => {
      root = createRoot(container);
      root.render(<MemoryRouter><KeyForm initial={initial} submitLabel="save" onCancel={() => {}} onSubmit={async () => {}} /></MemoryRouter>);
    });
    await flush();

    const search = container.querySelector<HTMLInputElement>('input[placeholder="keyForm.searchModelsPlaceholder"]')!;
    await act(async () => Simulate.change(search, { target: { value: "community" } } as never));
    expect(container.querySelectorAll(".model-definition-row")).toHaveLength(1);
    expect(container.textContent).toContain("community");

    const selectAll = [...container.querySelectorAll<HTMLButtonElement>("button")].find((button) => button.textContent === "picker.selectAll")!;
    await act(async () => selectAll.click());
    expect(container.querySelector<HTMLInputElement>(".model-definition-main input")?.checked).toBe(true);

    const selectedOnly = [...container.querySelectorAll<HTMLButtonElement>("button")].find((button) => button.textContent === "keyForm.showSelectedOnly")!;
    await act(async () => selectedOnly.click());
    expect(selectedOnly.getAttribute("aria-pressed")).toBe("true");
    expect(selectAll.disabled).toBe(true);

    const clearAll = [...container.querySelectorAll<HTMLButtonElement>("button")].find((button) => button.textContent === "picker.clearAll")!;
    await act(async () => clearAll.click());
    expect(container.textContent).toContain("keyForm.noModelMatch");
    await act(async () => Simulate.change(search, { target: { value: "" } } as never));
    const remaining = container.querySelectorAll<HTMLInputElement>(".model-definition-main input");
    expect(remaining).toHaveLength(1);
    expect(remaining[0].checked).toBe(true);
    expect(container.textContent).toContain("fast");
  });

  it("blocks negative total or model limits at the form boundary", async () => {
    const onSubmit = vi.fn();
    await act(async () => {
      root = createRoot(container);
      root.render(<MemoryRouter><KeyForm initial={initial} submitLabel="save" onCancel={() => {}} onSubmit={onSubmit} /></MemoryRouter>);
    });
    await flush();
    const fastLimit = container.querySelector<HTMLInputElement>(".model-limit-field input");
    await act(async () => Simulate.change(fastLimit!, { target: { value: "-1" } } as never));
    await act(async () => container.querySelector("form")!.dispatchEvent(new Event("submit", { bubbles: true, cancelable: true })));
    expect(onSubmit).not.toHaveBeenCalled();
    expect(container.textContent).toContain("keyForm.modelLimitInvalid");
  });

  it("opens model creation with a safe return path and the complete key draft", async () => {
    function Destination() {
      const location = useLocation();
      return <pre data-testid="destination">{JSON.stringify(location.state)}</pre>;
    }
    await act(async () => {
      root = createRoot(container);
      root.render(
        <MemoryRouter initialEntries={["/keys/team-a/edit"]}>
          <Routes>
            <Route path="/keys/team-a/edit" element={<KeyForm initial={initial} returnPath="/keys/team-a/edit" submitLabel="save" onCancel={() => {}} onSubmit={async () => {}} />} />
            <Route path="/models/new" element={<Destination />} />
          </Routes>
        </MemoryRouter>,
      );
    });
    await flush();
    const create = Array.from(container.querySelectorAll<HTMLButtonElement>("button")).find((button) => button.textContent === "keyForm.newModel");
    await act(async () => create!.click());
    const state = JSON.parse(container.querySelector('[data-testid="destination"]')!.textContent ?? "{}") as { returnTo: string; draftKey: KeyFormValues };
    expect(state.returnTo).toBe("/keys/team-a/edit");
    expect(state.draftKey).toMatchObject({ id: "team-a", models: [{ name: "fast", daily_limit_usd: 2 }] });
  });
});
