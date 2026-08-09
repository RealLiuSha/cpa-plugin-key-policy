import { act } from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { createRoot } from "react-dom/client";
import { MemoryRouter } from "react-router-dom";

(globalThis as unknown as { IS_REACT_ACT_ENVIRONMENT?: boolean }).IS_REACT_ACT_ENVIRONMENT = true;

vi.mock("../api/credentialGroups", () => ({
  fetchClassifyRules: vi.fn(),
  deleteClassifyRule: vi.fn(),
  reorderClassifyRules: vi.fn(),
  upsertClassifyRule: vi.fn(),
  fetchCredentialDescriptors: vi.fn(),
  classifyPreview: vi.fn(),
}));
const { translate } = vi.hoisted(() => ({ translate: (key: string) => key }));
vi.mock("../i18n", () => ({ useT: () => translate }));

import { classifyPreview, deleteClassifyRule, fetchClassifyRules, fetchCredentialDescriptors, reorderClassifyRules } from "../api/credentialGroups";
import CredentialGroups from "./CredentialGroups";

const rules = [
  { name: "team", field: "filename", pattern: "-team\\.json$", group: "team", enabled: true },
  { name: "free", field: "filename", pattern: "-free\\.json$", group: "free", enabled: true },
];
const tick = () => new Promise((resolve) => setTimeout(resolve, 0));
let container: HTMLDivElement;
let root: ReturnType<typeof createRoot>;

beforeEach(() => {
  container = document.createElement("div");
  document.body.appendChild(container);
  vi.mocked(fetchClassifyRules).mockResolvedValue(rules);
  vi.mocked(deleteClassifyRule).mockResolvedValue(undefined);
  vi.mocked(reorderClassifyRules).mockResolvedValue(undefined);
  vi.mocked(fetchCredentialDescriptors).mockResolvedValue([{ id: "team.json", provider: "codex", attributes: { plan_type: "team" } }]);
  vi.mocked(classifyPreview).mockResolvedValue({ groups: { team: ["team.json"] }, group_counts: { team: 1 } });
  vi.stubGlobal("confirm", vi.fn(() => true));
});

afterEach(() => {
  act(() => root.unmount());
  container.remove();
  vi.unstubAllGlobals();
  vi.clearAllMocks();
});

describe("Credential group rules", () => {
  it("reorders rules and deletes only after confirmation", async () => {
    await act(async () => {
      root = createRoot(container);
      root.render(<MemoryRouter><CredentialGroups /></MemoryRouter>);
      await tick();
      await tick();
    });
    const rows = container.querySelectorAll(".rule-row");
    await act(async () => { rows[1].querySelectorAll<HTMLButtonElement>("button")[0].click(); await tick(); });
    expect(reorderClassifyRules).toHaveBeenCalledWith(["free", "team"]);

    const deleteButton = [...rows[0].querySelectorAll<HTMLButtonElement>("button")].find((button) => button.textContent === "credentialGroups.delete")!;
    await act(async () => { deleteButton.click(); await tick(); });
    expect(deleteClassifyRule).toHaveBeenCalledWith("team");
  });

  it("previews current rule matches against CPA credentials", async () => {
    await act(async () => {
      root = createRoot(container);
      root.render(<MemoryRouter><CredentialGroups /></MemoryRouter>);
      await tick();
      await tick();
    });
    const previewButton = [...container.querySelectorAll<HTMLButtonElement>("button")].find((button) => button.textContent === "credentialGroups.preview")!;
    await act(async () => { previewButton.click(); await tick(); await tick(); });
    expect(fetchCredentialDescriptors).toHaveBeenCalledTimes(1);
    expect(classifyPreview).toHaveBeenCalledWith(expect.any(Array), rules);
    expect(container.textContent).toContain("credentialGroups.matchCount");
  });
});
