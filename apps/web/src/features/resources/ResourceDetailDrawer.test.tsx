import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import type { ResourceDetailResponse } from "@k8s-dashboard/contracts";
import { HttpError } from "@/api/client";
import { ResourceDetailDrawer } from "./ResourceDetailDrawer";

afterEach(cleanup);

const DETAIL: ResourceDetailResponse = {
  clusterId: "prod-seoul",
  group: "core",
  version: "v1",
  resource: "secrets",
  apiVersion: "v1",
  kind: "Secret",
  namespace: "payments",
  name: "db",
  uid: "uid-secret-db",
  resourceVersion: "4242",
  generatedAt: "2026-08-13T04:00:00Z",
  yaml: "apiVersion: v1\nkind: Secret\nmetadata:\n  name: db\ntype: Opaque\n",
  redacted: ["data", "metadata.managedFields"],
};

function open(over: Partial<Parameters<typeof ResourceDetailDrawer>[0]> = {}) {
  const onClose = vi.fn();
  render(
    <ResourceDetailDrawer open loading={false} detail={DETAIL} error={undefined} onClose={onClose} {...over} />,
  );
  return { onClose };
}

describe("ResourceDetailDrawer", () => {
  it("정제된 YAML을 읽기 전용으로만 보여준다", () => {
    open();
    const yaml = screen.getByLabelText("db 매니페스트 (YAML, 읽기 전용)");
    expect(yaml.tagName).toBe("PRE");
    expect(yaml.textContent).toContain("kind: Secret");
    /* 편집 가능한 컨트롤이 없어야 합니다. */
    expect(screen.queryByRole("textbox")).toBeNull();
    expect(document.querySelector("textarea")).toBeNull();
    expect(document.querySelector("input")).toBeNull();
  });

  it("무엇이 제거됐는지 숨기지 않는다", () => {
    open();
    const note = screen.getByRole("note");
    expect(note.textContent ?? "").toContain("서버에서 제거된 뒤");
    expect(note.textContent ?? "").toContain("data, metadata.managedFields");
  });

  it("UID와 resourceVersion을 함께 보여준다", () => {
    open();
    expect(screen.getByText("uid-secret-db")).toBeVisible();
    expect(screen.getByText("4242")).toBeVisible();
  });

  it("모달로 열리고 닫기 버튼에 포커스를 준다", () => {
    open();
    const dialog = screen.getByRole("dialog");
    expect(dialog).toHaveAttribute("aria-modal", "true");
    expect(dialog).toHaveAttribute("aria-labelledby", "resource-drawer-title");
    expect(document.activeElement).toBe(screen.getByRole("button", { name: "닫기" }));
  });

  it("Escape와 닫기 버튼으로 닫힌다", () => {
    const { onClose } = open();
    fireEvent.keyDown(window, { key: "Escape" });
    expect(onClose).toHaveBeenCalledTimes(1);
    fireEvent.click(screen.getByRole("button", { name: "닫기" }));
    expect(onClose).toHaveBeenCalledTimes(2);
  });

  it("UID 교체는 빈 화면이 아니라 이유를 알린다", () => {
    open({ detail: undefined, error: new HttpError(409, { code: "uid_mismatch", message: "", requestId: "r" }) });
    expect(screen.getByRole("alert").textContent ?? "").toContain("교체되었습니다");
  });

  it("닫혀 있으면 아무것도 렌더링하지 않는다", () => {
    render(<ResourceDetailDrawer open={false} loading={false} detail={DETAIL} error={undefined} onClose={vi.fn()} />);
    expect(screen.queryByRole("dialog")).toBeNull();
  });
});
