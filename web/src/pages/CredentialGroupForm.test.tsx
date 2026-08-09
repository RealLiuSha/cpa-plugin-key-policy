import { act } from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { createRoot } from "react-dom/client";
import { Simulate } from "react-dom/test-utils";
import { MemoryRouter, Route, Routes } from "react-router-dom";

(globalThis as unknown as { IS_REACT_ACT_ENVIRONMENT?: boolean }).IS_REACT_ACT_ENVIRONMENT = true;

vi.mock("../api/credentialGroups", () => ({
  fetchClassifyRules: vi.fn(),
  upsertClassifyRule: vi.fn(),
}));
const { translate } = vi.hoisted(() => ({ translate: (key: string) => key }));
vi.mock("../i18n", () => ({ useT: () => translate }));

import { upsertClassifyRule } from "../api/credentialGroups";
import CredentialGroupForm from "./CredentialGroupForm";

const tick = () => new Promise((resolve) => setTimeout(resolve, 0));
let container: HTMLDivElement;
let root: ReturnType<typeof createRoot>;

beforeEach(() => {
  container = document.createElement("div");
  document.body.appendChild(container);
  vi.mocked(upsertClassifyRule).mockResolvedValue({ name: "team", field: "plan_type", pattern: "^team$", group: "team", enabled: true });
});

afterEach(() => {
  act(() => root.unmount());
  container.remove();
  vi.clearAllMocks();
});

describe("CredentialGroupForm", () => {
  it("creates a rule and returns to the credential-group list", async () => {
    await act(async () => {
      root = createRoot(container);
      root.render(
        <MemoryRouter initialEntries={["/credential-groups/new"]}>
          <Routes>
            <Route path="/credential-groups/new" element={<CredentialGroupForm />} />
            <Route path="/credential-groups" element={<div>credential-group-list</div>} />
          </Routes>
        </MemoryRouter>,
      );
    });
    const inputs = container.querySelectorAll<HTMLInputElement>(".form-grid input");
    await act(async () => {
      Simulate.change(inputs[0], { target: { value: "team" } } as never);
      Simulate.change(inputs[1], { target: { value: "plan_type" } } as never);
      Simulate.change(inputs[2], { target: { value: "^team$" } } as never);
      Simulate.change(inputs[3], { target: { value: "team" } } as never);
    });
    await act(async () => {
      container.querySelector("form")!.dispatchEvent(new Event("submit", { bubbles: true, cancelable: true }));
      await tick();
    });
    expect(upsertClassifyRule).toHaveBeenCalledWith({
      name: "team",
      field: "plan_type",
      pattern: "^team$",
      group: "team",
      enabled: true,
    });
    expect(container.textContent).toContain("credential-group-list");
  });
});
