import { act } from "react";
import { createRoot, type Root } from "react-dom/client";
import { Simulate } from "react-dom/test-utils";
import { MemoryRouter, useLocation } from "react-router-dom";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import type { ChannelDetail, ChannelSummary, ChannelView } from "../types";

(globalThis as unknown as { IS_REACT_ACT_ENVIRONMENT?: boolean }).IS_REACT_ACT_ENVIRONMENT = true;
vi.mock("../api/channels", () => ({ fetchChannelData: vi.fn(), fetchChannelDetail: vi.fn(), patchChannel: vi.fn(), deleteChannel: vi.fn() }));
vi.mock("../api/adapters", async (importOriginal) => ({ ...(await importOriginal<typeof import("../api/adapters")>()), fetchAdapters: vi.fn(), previewAdapter: vi.fn(), saveAdapter: vi.fn(), tryAdapter: vi.fn() }));
// CPA's model registry: the xai auth file serves Grok, the codex key GPT and an image model.
vi.mock("../api/cpa", async (importOriginal) => ({
  ...(await importOriginal<typeof import("../api/cpa")>()),
  fetchCredentialModels: vi.fn(async (name: string) => (name === "xai-a.json" ? ["grok-4.7"] : name === "codex:apikey:inferhub" ? ["gpt-6.1-sol", "gpt-image-2"] : [])),
}));
const { translate } = vi.hoisted(() => ({ translate: (key: string, vars?: Record<string, string | number>) => key + (vars ? ` ${Object.values(vars).join(" ")}` : "") }));
vi.mock("../i18n", () => ({ useT: () => translate }));
import Channels from "./Channels";
import { deleteChannel, fetchChannelData, fetchChannelDetail, patchChannel } from "../api/channels";
import { fetchAdapters, previewAdapter, saveAdapter, tryAdapter, type AdapterPreview } from "../api/adapters";
import { AxiosError } from "axios";
import type { AdapterRule } from "../types";

const summary = (attempts: number, failures: number): ChannelSummary => ({
  attempts, failures, success_rate: attempts ? (attempts - failures) / attempts : 0, status_codes: failures ? { "502": failures } : undefined,
  latency_p50_ms: 2300, latency_p95_ms: 9100, ttft_p50_ms: 800, ttft_p95_ms: 2100, input_tokens: 1000, output_tokens: 200, list_cost_usd: 0.012,
});
const inferhub: ChannelView = {
  auth_index: "47c2d6c61505edc3", auth_id: "codex:apikey:1a2b3c4d5e6f", provider: "codex", base_url: "https://inferhub.net/v1",
  meta: { auth_index: "47c2d6c61505edc3", display_name: "Inferhub" }, last_24h: summary(10, 3), today: summary(4, 1), rules: 1,
  models_24h: [{
    model: "gpt-6.1-sol", attempts: 30, failures: 2, success_rate: 28 / 30, prompt_tokens: 1000, cached_tokens: 800, generated_tokens: 500,
    reasoning_tokens: 5600, cache_hit_rate: 0.8, reasoning_per_attempt: 200, tokens_per_second: 42.5, ttft_avg_ms: 900,
  }],
};
const gone: ChannelView = { auth_index: "9999000000000001", provider: "claude", base_url: "https://gone.example/v1", meta: { auth_index: "9999000000000001" }, last_24h: summary(0, 0), today: summary(0, 0), rules: 0 };
const detail: ChannelDetail = {
  ...inferhub, last_7d: summary(70, 5), models_7d: inferhub.models_24h, hours: [{ hour: "2026-10-18T15:00:00Z", attempts: 3, failures: 1, latency_p50_ms: 2100, list_cost_usd: 0.001 }],
  recent_errors: [{ at: "2026-10-18T15:20:00Z", status_code: 502, model: "gpt-6-sol", snippet: "upstream connect error" }],
};

function LocationProbe() {
  return <output data-testid="location">{useLocation().search}</output>;
}

let container: HTMLDivElement;
let root: Root;
const tick = () => new Promise((resolve) => setTimeout(resolve, 0));
async function render(path = "/channels") {
  await act(async () => { root.render(<MemoryRouter initialEntries={[path]}><Channels /><LocationProbe /></MemoryRouter>); await tick(); });
}
async function click(element: Element) { await act(async () => { (element as HTMLElement).click(); await tick(); }); }
const button = (label: string) => [...container.querySelectorAll<HTMLButtonElement>("button")].find((item) => item.textContent === label)!;

beforeEach(() => {
  vi.clearAllMocks();
  vi.mocked(fetchChannelData).mockResolvedValue({
    channels: [inferhub, gone],
    cpa: {
      config: [{ kind: "codex", provider: "codex", name: "", authIndex: "47c2d6c61505edc3", baseUrl: "https://inferhub.net/v1", models: ["gpt-6.1-sol"], excludedModels: [], prefix: "", disabled: false, position: 1, priority: -1 }],
      authFiles: [{ id: "xai-a.json", authIndex: "7d80e580c338085a", name: "xai-a.json", provider: "xai", label: "a@example.com", status: "active", statusMessage: "", disabled: false, unavailable: true, nextRetryAfter: "2026-10-19T01:00:00Z", cooldowns: [], success: 1189, failed: 5 }],
    },
    cpaError: null,
    ids: { "47c2d6c61505edc3": "codex:apikey:inferhub" },
  });
  vi.mocked(fetchChannelDetail).mockResolvedValue(detail);
  vi.mocked(patchChannel).mockImplementation(async (patch) => ({ ...inferhub.meta, ...patch, updated_at: "2026-10-19T01:00:00Z" }));
  container = document.createElement("div");
  document.body.appendChild(container);
  root = createRoot(container);
});
afterEach(async () => {
  await act(async () => root.unmount());
  container.remove();
});

describe("Channels page", () => {
  it("lists CPA credentials by vendor with the plugin's numbers and the channels CPA dropped", async () => {
    await render();
    const groups = [...container.querySelectorAll(".channel-group")];
    expect(groups.map((group) => group.getAttribute("aria-label"))).toEqual(["gone.example", "inferhub.net", "xai"]);
    const inferhubRow = groups[1].querySelector("tbody tr")!;
    expect(inferhubRow.textContent).toContain("Inferhub");
    expect(inferhubRow.textContent).toContain("channels.state.configured");
    expect(inferhubRow.textContent).toContain("channels.failurePill 30%");
    expect(inferhubRow.textContent).toContain("70%");
    expect(inferhubRow.textContent).toContain("2.3 s");
    expect(groups[0].textContent).toContain("channels.state.missing");
    expect(groups[2].textContent).toContain("a@example.com");
    expect(groups[2].textContent).toContain("channels.state.unavailable");
  });

  it("narrows the list by type, then by a model CPA routes to the channel, and keeps the filter in the URL", async () => {
    await render();
    const vendors = () => [...container.querySelectorAll(".channel-group")].map((group) => group.getAttribute("aria-label"));
    const select = (selector: string, value: string) => act(async () => {
      Simulate.change(container.querySelector(selector)!, { target: { value } as unknown as EventTarget & HTMLSelectElement });
      await tick();
    });
    const models = [...container.querySelectorAll<HTMLOptionElement>(".channel-filter-model option")].map((option) => option.value);
    expect(models).toEqual(["all", "gpt-6.1-sol", "gpt-image-2", "grok-4.7"]);
    await select(".channel-filter-model", "grok-4.7");
    expect(vendors()).toEqual(["xai"]);
    expect(container.querySelector('[data-testid="location"]')?.textContent).toBe("?model=grok-4.7");
    await select(".channel-filter-model", "all");
    await select(".channel-filter-type", "ai_providers:codex");
    expect(vendors()).toEqual(["inferhub.net"]);
    expect(container.querySelector(".channel-table tbody tr")?.textContent).toContain("gpt-6.1-sol, gpt-image-2");
    // A model chosen, the row shows what the channel did with it.
    await select(".channel-filter-model", "gpt-6.1-sol");
    const cells = [...container.querySelectorAll(".channel-table tbody tr td")].map((cell) => cell.textContent);
    expect(cells.slice(2)).toEqual(["30", "channels.state.configuredchannels.failurePill 6.7%", "93%", "200", "80%", "42.5/s"]);
    await select(".channel-filter-model", "all");
    // Opening a channel keeps the filter.
    await click(container.querySelector('a.channel-name[href*="47c2d6c61505edc3"]')!);
    expect(container.querySelector('[data-testid="location"]')?.textContent).toBe("?type=ai_providers%3Acodex&auth_index=47c2d6c61505edc3");
    // The detail lists the channel's models, each linking to its comparison.
    expect(container.querySelector('.channel-models-table a[href*="model=gpt-6.1-sol"]')).not.toBeNull();
  });

  it("opens a channel from the URL, copies a feedback report and saves notes", async () => {
    const writeText = vi.fn(async () => {});
    Object.defineProperty(navigator, "clipboard", { configurable: true, value: { writeText } });
    await render();
    await click(container.querySelector('a.channel-name[href*="47c2d6c61505edc3"]')!);
    expect(container.querySelector('[data-testid="location"]')?.textContent).toBe("?auth_index=47c2d6c61505edc3");
    expect(fetchChannelDetail).toHaveBeenCalledWith("47c2d6c61505edc3");
    const panel = container.querySelector(".channel-detail")!;
    expect(panel.textContent).toContain("gpt-6.1-sol");
    expect(panel.textContent).toContain("channels.cpa.ownedHint");
    expect(panel.textContent).toContain("upstream connect error");
    // A channel CPA still has cannot be deleted here.
    expect(panel.textContent).not.toContain("channels.delete.button");

    await click(button("channels.copyFeedback"));
    const report = (writeText.mock.calls[0] as unknown[])[0] as string;
    expect(report).toContain("channels.feedback.channel Inferhub https://inferhub.net/v1");
    expect(report).toContain("502 · gpt-6-sol · upstream connect error");
    // The vendor reads its own logs in another time zone.
    expect(report).toContain(`${new Date("2026-10-18T15:20:00Z").toLocaleString(undefined, { timeZoneName: "short" })} · 502`);
    expect(report).not.toContain("47c2d6c61505edc3");
    expect(panel.textContent).toContain("channels.copied");

    await click(button("channels.tab.notes"));
    expect(container.querySelector('[data-testid="location"]')?.textContent).toBe("?auth_index=47c2d6c61505edc3&tab=notes");
    const name = panel.querySelector<HTMLInputElement>("input")!;
    await act(async () => { Simulate.change(name, { target: { value: "Inferhub main" } as unknown as EventTarget & HTMLInputElement }); });
    await act(async () => { Simulate.submit(panel.querySelector("form")!); await tick(); });
    expect(patchChannel).toHaveBeenCalledWith({ auth_index: "47c2d6c61505edc3", display_name: "Inferhub main", vendor: "", note: "" });
    expect(panel.textContent).toContain("channels.saved");

    await click(container.querySelector('button[aria-label="models.close"]')!);
    expect(container.querySelector(".channel-detail")).toBeNull();
    expect(container.querySelector('[data-testid="location"]')?.textContent).toBe("");
  });

  it("deletes a channel CPA dropped after a confirmation and closes it", async () => {
    vi.mocked(deleteChannel).mockResolvedValue();
    await render("/channels?auth_index=9999000000000001");
    expect(container.querySelector(".channel-detail")?.textContent).toContain("channels.delete.hint");
    await click(button("channels.delete.button"));
    expect(container.querySelector('[role="dialog"]')?.textContent).toContain("channels.delete.message");
    const loads = vi.mocked(fetchChannelData).mock.calls.length;
    await click(button("channels.delete.confirm"));
    expect(deleteChannel).toHaveBeenCalledWith("9999000000000001");
    expect(container.querySelector(".channel-detail")).toBeNull();
    expect(container.querySelector('[data-testid="location"]')?.textContent).toBe("");
    expect(vi.mocked(fetchChannelData).mock.calls.length).toBeGreaterThan(loads);
  });

  it("keeps unsaved notes across tabs and asks before leaving the channel", async () => {
    await render("/channels?auth_index=47c2d6c61505edc3&tab=notes");
    const location = () => container.querySelector('[data-testid="location"]')?.textContent;
    const nameInput = () => container.querySelector<HTMLInputElement>('.channel-detail [role="tabpanel"]:not([hidden]) input')!;
    // Only panels already rendered are pointed at.
    expect(button("channels.tab.adapters").hasAttribute("aria-controls")).toBe(false);
    expect(document.getElementById(button("channels.tab.notes").getAttribute("aria-controls")!)).not.toBeNull();
    await act(async () => { Simulate.change(nameInput(), { target: { value: "Inferhub draft" } as unknown as EventTarget & HTMLInputElement }); });
    await click(button("channels.tab.overview"));
    expect(container.querySelector('[role="tab"].unsaved')?.textContent).toBe("channels.tab.notes");
    await click(button("channels.tab.notes"));
    expect(nameInput().value).toBe("Inferhub draft");

    // Another channel waits for an answer; staying keeps the draft.
    await click(container.querySelector('a.channel-name[href*="9999000000000001"]')!);
    expect(location()).toBe("?auth_index=47c2d6c61505edc3&tab=notes");
    expect(container.querySelector('[role="dialog"]')?.textContent).toContain("channels.leaveMessage");
    await click(button("channels.leaveStay"));
    expect(container.querySelector('[role="dialog"]')).toBeNull();
    expect(nameInput().value).toBe("Inferhub draft");

    await click(container.querySelector('button[aria-label="models.close"]')!);
    await click(button("channels.leaveDiscard"));
    expect(container.querySelector(".channel-detail")).toBeNull();
    expect(location()).toBe("");
    expect(patchChannel).not.toHaveBeenCalled();
  });

  it("asks before a rule switch changes the channel's live requests", async () => {
    const rule: AdapterRule = { id: "r1", channel: "47c2d6c61505edc3", type: "edits_image_field", paths: [], enabled: true, hits: { count: 3 }, updated_at: "2026-10-19T01:00:00Z" };
    vi.mocked(fetchAdapters).mockResolvedValue({ rules: [rule], types: [{ type: "edits_image_field", default_paths: ["/v1/images/edits"], needs_field: false, needs_value: false }] });
    vi.mocked(saveAdapter).mockImplementation(async (saved) => saved);
    await render("/channels?auth_index=47c2d6c61505edc3&tab=adapters");
    const toggle = () => container.querySelector<HTMLInputElement>(".adapter-rule input[type=checkbox]")!;
    expect(toggle().getAttribute("aria-label")).toBe("adapters.toggleLabel edits_image_field /v1/images/edits");

    await click(toggle());
    expect(container.querySelector('[role="dialog"]')?.textContent).toContain("adapters.toggleOffConfirm edits_image_field /v1/images/edits");
    await click(button("keyForm.cancel"));
    expect(container.querySelector('[role="dialog"]')).toBeNull();
    expect(saveAdapter).not.toHaveBeenCalled();

    await click(toggle());
    await click(button("adapters.toggleOffAction"));
    expect(saveAdapter).toHaveBeenCalledWith({ id: "r1", channel: "47c2d6c61505edc3", type: "edits_image_field", paths: [], enabled: false });
    expect(container.querySelector('[role="dialog"]')).toBeNull();
  });

  it("gives up a real request after the timeout and closes it when the form goes away", async () => {
    const rule: AdapterRule = { id: "r1", channel: "47c2d6c61505edc3", type: "delete_field", paths: ["/v1/responses"], field: "store", enabled: true };
    vi.mocked(fetchAdapters).mockResolvedValue({ rules: [rule], types: [{ type: "delete_field", default_paths: [], needs_field: true, needs_value: false }] });
    await render("/channels?auth_index=47c2d6c61505edc3&tab=adapters");
    const send = async () => {
      await click(button("keys.edit"));
      const sample = container.querySelector<HTMLTextAreaElement>(".adapter-preview textarea")!;
      await act(async () => { Simulate.change(sample, { target: { value: '{"model": "gpt-6"}' } as unknown as EventTarget & HTMLTextAreaElement }); });
      await click(button("adapters.try"));
      await click(button("adapters.tryApply"));
    };

    vi.mocked(tryAdapter).mockRejectedValueOnce(new AxiosError("timeout of 300000ms exceeded", AxiosError.ECONNABORTED));
    await send();
    expect(vi.mocked(tryAdapter).mock.calls[0][0]).toBe("r1");
    expect(container.querySelector(".adapter-preview [role=alert]")?.textContent).toBe("adapters.tryTimeout 5");

    let signal: AbortSignal | undefined;
    vi.mocked(tryAdapter).mockImplementationOnce((_id, _body, given) => { signal = given; return new Promise(() => {}); });
    await click(button("keyForm.cancel"));
    await send();
    expect(signal?.aborted).toBe(false);
    await click(button("keyForm.cancel"));
    expect(signal?.aborted).toBe(true);

    // Editing the sample drops the request sent with the old one, so its
    // answer cannot land under the new sample or a second request.
    let first: AbortSignal | undefined;
    vi.mocked(tryAdapter).mockImplementationOnce((_id, _body, given) => { first = given; return new Promise(() => {}); });
    await send();
    expect(container.querySelector(".adapter-preview [role=status]")?.textContent).toBe("adapters.trying");
    const sample = container.querySelector<HTMLTextAreaElement>(".adapter-preview textarea")!;
    await act(async () => { Simulate.change(sample, { target: { value: '{"model": "gpt-6.1"}' } as unknown as EventTarget & HTMLTextAreaElement }); });
    expect(first?.aborted).toBe(true);
  });

  it("drops a preview answer for a sample that was edited while it was on its way", async () => {
    const rule: AdapterRule = { id: "r1", channel: "47c2d6c61505edc3", type: "delete_field", paths: ["/v1/responses"], field: "store", enabled: true };
    vi.mocked(fetchAdapters).mockResolvedValue({ rules: [rule], types: [{ type: "delete_field", default_paths: [], needs_field: true, needs_value: false }] });
    let answer: (preview: AdapterPreview) => void = () => {};
    vi.mocked(previewAdapter).mockImplementationOnce(() => new Promise((resolve) => { answer = resolve; }));
    await render("/channels?auth_index=47c2d6c61505edc3&tab=adapters");
    await click(button("keys.edit"));
    const sample = container.querySelector<HTMLTextAreaElement>(".adapter-preview textarea")!;
    await act(async () => { Simulate.change(sample, { target: { value: '{"model": "gpt-6", "store": true}' } as unknown as EventTarget & HTMLTextAreaElement }); });
    await click(button("adapters.preview"));
    await act(async () => { Simulate.change(sample, { target: { value: '{"model": "gpt-6.1", "store": true}' } as unknown as EventTarget & HTMLTextAreaElement }); });
    await act(async () => { answer({ changed: true, body: { model: "gpt-6" } }); await tick(); });
    expect(container.querySelector(".adapter-preview .code-block")).toBeNull();
    expect(button("adapters.preview").disabled).toBe(false);
  });

  it("names the channel a duplicate config key shares its auth_index with", async () => {
    const shared = { kind: "openai-compatibility" as const, provider: "zy-b", name: "zy-b", authIndex: "47c2d6c61505edc3", baseUrl: "https://inferhub.net/v1", models: [], excludedModels: [], prefix: "", disabled: false, position: 2 };
    const data = await vi.mocked(fetchChannelData).getMockImplementation()!();
    vi.mocked(fetchChannelData).mockResolvedValue({ ...data, cpa: { ...data.cpa!, config: [...data.cpa!.config, shared] } });
    await render();
    const hints = [...container.querySelectorAll(".channel-table .field-hint")].map((hint) => hint.textContent);
    expect(hints).toEqual(["channels.sharedAuthIndex Inferhub"]);
  });
});
