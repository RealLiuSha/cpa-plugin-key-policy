import { act } from "react";
import { createRoot, type Root } from "react-dom/client";
import { Simulate } from "react-dom/test-utils";
import { MemoryRouter } from "react-router-dom";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import type { FingerprintResult, QualityConfig, QualityCredential, QualityState, TraceResult } from "../quality/types";
import type { ChannelSummary } from "../types";

(globalThis as unknown as { IS_REACT_ACT_ENVIRONMENT?: boolean }).IS_REACT_ACT_ENVIRONMENT = true;
vi.mock("../api/quality", () => ({
  fetchQuality: vi.fn(), runQuality: vi.fn(), cancelQuality: vi.fn(), clearQualityResults: vi.fn(), fetchQualityRecord: vi.fn(), isRecordGone: vi.fn(() => false),
}));
vi.mock("../api/channels", () => ({ fetchChannelData: vi.fn() }));
vi.mock("../api/cpa", () => ({ fetchCredentialModels: vi.fn(async () => []) }));
vi.mock("../api/modelPrices", () => ({ fetchModelPrices: vi.fn() }));
vi.mock("../polling", () => ({ useVisiblePolling: vi.fn() }));
const { translate } = vi.hoisted(() => ({ translate: (key: string, vars?: Record<string, string | number>) => key + (vars ? ` ${Object.values(vars).join(" ")}` : "") }));
vi.mock("../i18n", () => ({ useT: () => translate, getLocale: () => "en" }));
import Quality from "./Quality";
import { fetchChannelData } from "../api/channels";
import { fetchModelPrices } from "../api/modelPrices";
import { fetchQuality, fetchQualityRecord, runQuality } from "../api/quality";

const config: QualityConfig = {
  candy: { max_runs: 10, prompt: "How many candies?", answer: "21" },
  fingerprint: { modes: [{ id: "quick", name: "快速", cells: 4, samples_per_cell: 15 }], default_concurrency: 2, max_concurrency: 6 },
  modeltrace: { requests: 3, default_concurrency: 3, max_concurrency: 3, bank_revision: "sha256:bank" },
  pelican: { prompt: "Draw a pelican" },
};
const credential = (patch: Partial<QualityCredential>): QualityCredential => ({
  id: "", provider: "codex", source: "auth_files", name: "", disabled: false, results: [], fingerprints: [], modeltraces: [], pelicans: [], ...patch,
});
const trace: TraceResult = {
  id: "t1", time: "2026-10-19T02:00:00Z", model: "gpt-6.1-sol", status: "completed", bank_revision: "sha256:bank", duration_ms: 9000, input_tokens: 42000, output_tokens: 3000, reasoning_tokens: 0,
  attribution: { prediction: "gpt-6.1-sol", probability: 0.82, used_outputs: 3, family_prediction_name: "GPT-6", family_probability: 0.9 },
};
const alice = credential({
  id: "codex-alice.json", auth_index: "a1", name: "codex-alice.json", email: "alice@example.com", plan_type: "pro",
  results: [{ time: "2026-10-19T01:00:00Z", model: "gpt-6.1-sol", effort: "low", ok: true, answer: "21", input_tokens: 14000, output_tokens: 20, reasoning_tokens: 0, duration_ms: 2300 }],
  modeltraces: [trace],
});
const bob = credential({ id: "codex-bob.json", name: "codex-bob.json", email: "bob@example.com", disabled: true });
const carol = credential({
  id: "codex-carol.json", name: "codex-carol.json", email: "carol@example.com", unavailable: true,
  status_message: '{"error":{"message":"usage limit reached"}}', next_retry_after: "2026-10-19T05:00:00Z",
});
const apiKey = credential({ id: "codex:apikey:3f2a", source: "ai_providers", name: "sk-e2e…aaaa", group: "ZhiYuan", base_url: "https://api.zhiyuan.example/v1" });
const state = (auths: QualityCredential[], configError = ""): QualityState => ({ auths, storage_error: "", config_error: configError, config });
const summary: ChannelSummary = {
  attempts: 0, failures: 0, success_rate: 0, latency_p50_ms: 0, latency_p95_ms: 0, ttft_p50_ms: 0, ttft_p95_ms: 0, input_tokens: 0, output_tokens: 0, list_cost_usd: 0,
};

let container: HTMLDivElement;
let root: Root;
const tick = () => new Promise((resolve) => setTimeout(resolve, 0));
async function render(path = "/quality") {
  await act(async () => { root.render(<MemoryRouter initialEntries={[path]}><Quality /></MemoryRouter>); await tick(); });
}
async function click(element: Element) { await act(async () => { (element as HTMLElement).click(); await tick(); }); }
const button = (label: string) => [...container.querySelectorAll<HTMLButtonElement>("button")].find((item) => item.textContent === label)!;
const rows = () => [...container.querySelectorAll(".quality-row")];

beforeEach(() => {
  vi.clearAllMocks();
  vi.mocked(fetchChannelData).mockResolvedValue({
    channels: [{ auth_index: "a1", provider: "codex", meta: { auth_index: "a1", display_name: "Alice main" }, last_24h: summary, today: summary, rules: 0 }],
    cpa: { config: [], authFiles: [] },
    cpaError: null,
    ids: {},
  });
  vi.mocked(fetchModelPrices).mockResolvedValue({ models: [], unpriced: [] });
  container = document.createElement("div");
  document.body.appendChild(container);
  root = createRoot(container);
});
afterEach(async () => {
  await act(async () => root.unmount());
  container.remove();
});

describe("Quality page", () => {
  it("lists every credential under its channel name and keeps disabled or cooling ones unselectable", async () => {
    vi.mocked(fetchQuality).mockResolvedValue(state([alice, bob, carol, apiKey], "open config.yaml: permission denied"));
    await render();
    expect(container.querySelector(".notice")?.textContent).toBe("quality.configError open config.yaml: permission denied");
    expect(rows()).toHaveLength(4);
    const link = rows()[0].querySelector("a")!;
    expect(link.textContent).toBe("Alice main");
    expect(link.getAttribute("href")).toBe("/channels?auth_index=a1");
    expect(rows()[0].textContent).toContain("alice@example.com");
    expect(rows()[0].textContent).toContain("quality.candy.rate 100% 1 1");
    expect(rows().map((row) => row.querySelector<HTMLInputElement>(".quality-select")!.disabled)).toEqual([false, true, true, false]);
    expect(rows()[1].textContent).toContain("quality.state.disabled");
    expect(rows()[2].querySelector(".badge.warn")?.getAttribute("title")).toContain("usage limit reached");
    // A config API key leads with its group, its masked key and base URL beside it.
    expect(rows()[3].querySelector(".channel-name")?.textContent).toBe("ZhiYuan");
    expect(rows()[3].textContent).toContain("sk-e2e…aaaa");
    expect(rows()[3].querySelector(".quality-url")?.getAttribute("title")).toBe("https://api.zhiyuan.example/v1");

    await act(async () => { Simulate.change(container.querySelector(".channel-filter-type")!, { target: { value: "auth_files:codex" } as unknown as EventTarget & HTMLSelectElement }); await tick(); });
    expect(rows()).toHaveLength(3);
  });

  it("confirms the request count, then starts a candy run on the selected credential and shows its progress", async () => {
    vi.mocked(fetchQuality).mockResolvedValueOnce(state([alice, bob])).mockResolvedValue(state([{ ...alice, running: { done: 0, total: 1 } }, bob]));
    vi.mocked(runQuality).mockResolvedValue({ started: 1, busy: 0 });
    await render();
    await click(rows()[0].querySelector(".quality-select")!);
    await click(button("quality.runSelected 1"));
    expect(container.querySelector('[role="dialog"]')?.textContent).toContain("quality.confirm.codex 1 1 14,000");
    expect(runQuality).not.toHaveBeenCalled();

    await click(button("quality.confirm.start"));
    expect(runQuality).toHaveBeenCalledWith({ test: "candy", auth_ids: ["codex-alice.json"], model: "gpt-6.1-sol", effort: "low", runs: 1 });
    expect(container.querySelector('[role="dialog"]')).toBeNull();
    expect(container.textContent).toContain("quality.started 1");
    expect(rows()[0].textContent).toContain("quality.phase.testing");
    expect(rows()[0].querySelector("progress")?.getAttribute("max")).toBe("1");
    expect(button("quality.cancel")).toBeTruthy();
  });

  it("opens a ModelTrace record with its candidates and challenges", async () => {
    vi.mocked(fetchQuality).mockResolvedValue(state([alice]));
    vi.mocked(fetchQualityRecord).mockResolvedValue({
      ...trace,
      samples: [{ prompt: "Write 300 integers", expected_count: 300, attempts: 2, text: "17, 203, 88", parsed_numbers: 3, accepted: false }],
      attribution: {
        ...trace.attribution!,
        results: [{ model: "gpt-6.1-sol", display_name: "GPT-6.1 Sol", probability: 0.82 }, { model: "gpt-5.5", display_name: "GPT-5.5", probability: 0.18 }],
        family_probabilities: [{ family: "gpt-6", display_name: "GPT-6", probability: 0.9 }],
        diagnostics: [{ index: 0, parsed_numbers: 3, minimum_numbers: 165, accepted: false }],
      },
    });
    await render("/quality?tab=modeltrace");
    expect(rows()[0].textContent).toContain("quality.modeltrace.match");
    await click(button("quality.historyToggle 1"));
    await click(button("quality.details"));
    expect(fetchQualityRecord).toHaveBeenCalledWith("modeltrace", "codex-alice.json", "t1");
    const dialog = container.querySelector('[role="dialog"]')!;
    expect(dialog.textContent).toContain("Alice main");
    expect(dialog.textContent).toContain("GPT-5.5");
    expect(dialog.textContent).toContain("18.0%");
    expect(dialog.textContent).toContain("quality.modeltrace.challenge 1");
    expect(dialog.textContent).toContain("Write 300 integers");
    expect(dialog.textContent).toContain("quality.modeltrace.retries 1");
  });

  it("loads a candy answer by the record's time, since the list leaves answers out", async () => {
    vi.mocked(fetchQuality).mockResolvedValue(state([{ ...alice, results: [{ ...alice.results[0], answer: "" }] }]));
    vi.mocked(fetchQualityRecord).mockResolvedValue({ ...alice.results[0], answer: "There are 21 candies." });
    await render();
    await click(rows()[0].querySelector(".quality-mark")!);
    expect(fetchQualityRecord).toHaveBeenCalledWith("candy", "codex-alice.json", "2026-10-19T01:00:00Z");
    expect(container.querySelector('[role="dialog"] .quality-answer')?.textContent).toBe("There are 21 candies.");
  });

  it("shows a fingerprint record without the plugin's Chinese text outside zh-CN", async () => {
    const record: FingerprintResult = {
      id: "f1", time: "2026-10-19T03:00:00Z", model: "gpt-6.1-sol", mode: "strict", status: "failed", total: 400, done: 12, valid: 4, errors: 8,
      error: "连续 8 次请求失败，已停止采集：模型 3 分钟内未完成回答，已中断请求",
      attribution: {
        status: "consistent", message: "与同名基准未发现显著差异", nearest: "gpt-6.1-sol", alpha: 0.01, self_jsd: 0.3,
        warnings: ["本次样本自一致性较低，建议使用严格模式复测", "gpt-5.5 基准自一致性较低"],
        comparisons: [{ model: "gpt-6.1-sol", mean_jsd: 0.1, p_value: 0.5, verdict: "close", self_jsd: 0.1 }, { model: "gpt-5.5", mean_jsd: 0.4, p_value: 0.001, verdict: "far", self_jsd: 0.31 }],
      },
    };
    vi.mocked(fetchQuality).mockResolvedValue(state([{ ...alice, fingerprints: [{ ...record, attribution: { status: "consistent", nearest: "gpt-6.1-sol" } }] }]));
    vi.mocked(fetchQualityRecord).mockResolvedValue(record);
    await render("/quality?tab=fingerprint");
    await click(button("quality.historyToggle 1"));
    await click(button("quality.details"));
    const dialog = container.querySelector('[role="dialog"]')!;
    expect(dialog.textContent).toContain("quality.recordError.failedInARow 8 quality.recordError.timeout 3");
    expect([...dialog.querySelectorAll(".notice")].map((item) => item.textContent)).toEqual(["quality.fingerprint.warningSelf", "quality.fingerprint.warningBaseline gpt-5.5"]);
    expect(dialog.textContent).not.toMatch(/[\u4e00-\u9fff]/);
  });
});
