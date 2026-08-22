import { useEffect, useState } from "react";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import type { ResourceDetailResponse } from "@k8s-dashboard/contracts";
import { HttpError } from "@/api/client";
import { ResourceDetailDrawer } from "./ResourceDetailDrawer";

/* 검토 패널 자체는 ChangeReviewPanel.test.tsx가 덮습니다. 여기서 확인할 것은
   **어떤 인스턴스가 언제 서고 사라지는가**입니다.
   그래서 대역은 상태를 가집니다 — 인스턴스마다 고유 id를 잡고, 그 인스턴스가 본
   detail.uid를 전부 기록합니다. 한 인스턴스가 두 uid를 봤다면 신원 전환에서
   인스턴스가 재사용된 것이고, 그 순간 이전 초안·진행 중 요청이 새 대상으로
   넘어갑니다. */
const panelLog: { mounted: string[]; unmounted: string[]; seen: string[] } = {
  mounted: [],
  unmounted: [],
  seen: [],
};
let panelInstances = 0;

vi.mock("./ChangeReviewPanel", () => ({
  ChangeReviewPanel: ({ detail }: { detail: ResourceDetailResponse }) => {
    const [instance] = useState(() => {
      panelInstances += 1;
      return `panel-${panelInstances}`;
    });
    panelLog.seen.push(`${instance}:${detail.uid}`);
    useEffect(() => {
      panelLog.mounted.push(instance);
      return () => {
        panelLog.unmounted.push(instance);
      };
    }, [instance]);
    return (
      <div data-testid="change-review-panel" data-instance={instance}>
        review:{detail.uid}
      </div>
    );
  },
}));

beforeEach(() => {
  panelLog.mounted = [];
  panelLog.unmounted = [];
  panelLog.seen = [];
  panelInstances = 0;
});

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

/* ── 변경 검토 서브탭 (ADR 0019 Phase 1) ──────────────────────────────────── */

const CONFIGMAP: ResourceDetailResponse = {
  clusterId: "prod-seoul",
  group: "core",
  version: "v1",
  resource: "configmaps",
  apiVersion: "v1",
  kind: "ConfigMap",
  namespace: "payments",
  name: "api-config",
  uid: "uid-configmaps-001",
  resourceVersion: "4242",
  generatedAt: "2026-08-22T00:00:00Z",
  yaml: "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: api-config\n",
  redacted: ["metadata.managedFields"],
};

function openReviewable(over: Partial<Parameters<typeof ResourceDetailDrawer>[0]> = {}) {
  const onClose = vi.fn();
  const view = render(
    <ResourceDetailDrawer
      open
      loading={false}
      detail={CONFIGMAP}
      error={undefined}
      onClose={onClose}
      canReview
      {...over}
    />,
  );
  return { onClose, view };
}

function tab(name: "매니페스트" | "변경 검토") {
  return screen.getByRole("tab", { name });
}

/** 쓰기처럼 보이는 컨트롤이 있는지는 **역할과 이름**으로만 봅니다.
 *  본문 문구(“클러스터에 적용하지 않습니다”)를 정규식으로 잡으면 안내문이 위반으로 셉니다. */
function forbiddenControls() {
  const named = [...screen.queryAllByRole("button"), ...screen.queryAllByRole("link")];
  return named.filter((el) => /적용|저장|삭제|생성|force|token|강제|apply|delete|create/i.test(el.textContent ?? ""));
}

describe("ResourceDetailDrawer 변경 검토 진입점", () => {
  it("capability가 없으면(Secret 포함) 진입점도 편집기도 없다", () => {
    for (const props of [{}, { canReview: false }, { canReview: undefined }]) {
      cleanup();
      render(
        <ResourceDetailDrawer open loading={false} detail={DETAIL} error={undefined} onClose={vi.fn()} {...props} />,
      );
      expect(screen.queryByRole("tablist")).toBeNull();
      expect(screen.queryByRole("tab")).toBeNull();
      expect(screen.queryByTestId("change-review-panel")).toBeNull();
      expect(screen.queryByRole("textbox")).toBeNull();
      expect(document.querySelector("textarea")).toBeNull();
      /* 기존 읽기 전용 화면은 그대로입니다. */
      expect(screen.getByLabelText("db 매니페스트 (YAML, 읽기 전용)")).toBeVisible();
      expect(forbiddenControls()).toHaveLength(0);
    }
  });

  it("로딩·오류 상태에서는 진입점을 만들지 않는다", () => {
    render(<ResourceDetailDrawer open loading detail={undefined} error={undefined} onClose={vi.fn()} canReview />);
    expect(screen.queryByRole("tablist")).toBeNull();
    cleanup();
    render(
      <ResourceDetailDrawer
        open
        loading={false}
        detail={undefined}
        error={new HttpError(409, { code: "uid_mismatch", message: "", requestId: "r" })}
        onClose={vi.fn()}
        canReview
      />,
    );
    expect(screen.queryByRole("tablist")).toBeNull();
    expect(screen.getByRole("alert")).toBeVisible();
  });

  it("capability가 있으면 탭 두 개가 생기고 기본은 매니페스트다", () => {
    openReviewable();
    const list = screen.getByRole("tablist", { name: "상세 보기 방식" });
    expect(list).toBeVisible();
    expect(screen.getAllByRole("tab")).toHaveLength(2);

    expect(tab("매니페스트")).toHaveAttribute("aria-selected", "true");
    expect(tab("매니페스트")).toHaveAttribute("tabindex", "0");
    expect(tab("매니페스트")).toHaveAttribute("aria-controls", "resource-drawer-panel-view");
    expect(tab("변경 검토")).toHaveAttribute("aria-selected", "false");
    expect(tab("변경 검토")).toHaveAttribute("tabindex", "-1");
    expect(tab("변경 검토")).toHaveAttribute("aria-controls", "resource-drawer-panel-review");

    /* 기본 탭에서는 검토 패널이 **마운트되지 않습니다.** */
    expect(screen.queryByTestId("change-review-panel")).toBeNull();
    expect(screen.getByRole("tabpanel")).toHaveAttribute("id", "resource-drawer-panel-view");
    expect(screen.getByLabelText("api-config 매니페스트 (YAML, 읽기 전용)")).toBeVisible();
  });

  it("id·속성에 이름이나 UID를 싣지 않는다", () => {
    openReviewable();
    for (const el of [...screen.getAllByRole("tab"), screen.getByRole("tabpanel")]) {
      const attrs = Array.from(el.attributes).map((a) => `${a.name}=${a.value}`).join(" ");
      expect(attrs).not.toContain(CONFIGMAP.uid);
      expect(attrs).not.toContain(CONFIGMAP.name);
    }
  });

  it("클릭하면 검토 패널이 마운트되고 View로 돌아오면 언마운트된다", () => {
    openReviewable();
    fireEvent.click(tab("변경 검토"));
    expect(tab("변경 검토")).toHaveAttribute("aria-selected", "true");
    expect(tab("매니페스트")).toHaveAttribute("aria-selected", "false");
    expect(screen.getByTestId("change-review-panel").textContent).toBe(`review:${CONFIGMAP.uid}`);
    expect(screen.getByRole("tabpanel")).toHaveAttribute("id", "resource-drawer-panel-review");
    /* 읽기 전용 화면은 그동안 DOM에 남지 않습니다. */
    expect(screen.queryByLabelText("api-config 매니페스트 (YAML, 읽기 전용)")).toBeNull();

    fireEvent.click(tab("매니페스트"));
    expect(screen.queryByTestId("change-review-panel")).toBeNull();
    expect(screen.getByLabelText("api-config 매니페스트 (YAML, 읽기 전용)")).toBeVisible();
  });

  it("화살표·Home·End가 포커스와 선택을 함께 옮긴다", () => {
    openReviewable();
    tab("매니페스트").focus();

    fireEvent.keyDown(tab("매니페스트"), { key: "ArrowRight" });
    expect(tab("변경 검토")).toHaveAttribute("aria-selected", "true");
    expect(document.activeElement).toBe(tab("변경 검토"));
    expect(screen.getByTestId("change-review-panel")).toBeVisible();

    fireEvent.keyDown(tab("변경 검토"), { key: "ArrowLeft" });
    expect(tab("매니페스트")).toHaveAttribute("aria-selected", "true");
    expect(document.activeElement).toBe(tab("매니페스트"));

    fireEvent.keyDown(tab("매니페스트"), { key: "End" });
    expect(tab("변경 검토")).toHaveAttribute("aria-selected", "true");
    expect(document.activeElement).toBe(tab("변경 검토"));

    fireEvent.keyDown(tab("변경 검토"), { key: "Home" });
    expect(tab("매니페스트")).toHaveAttribute("aria-selected", "true");
    expect(document.activeElement).toBe(tab("매니페스트"));

    /* 화살표는 순환합니다. */
    fireEvent.keyDown(tab("매니페스트"), { key: "ArrowLeft" });
    expect(tab("변경 검토")).toHaveAttribute("aria-selected", "true");
  });

  it("대상이 바뀌면 View로 돌아가고 검토 패널이 사라진다", () => {
    const { view } = openReviewable();
    fireEvent.click(tab("변경 검토"));
    expect(screen.getByTestId("change-review-panel")).toBeVisible();

    view.rerender(
      <ResourceDetailDrawer
        open
        loading={false}
        detail={{ ...CONFIGMAP, uid: "uid-configmaps-002", resourceVersion: "9999" }}
        error={undefined}
        onClose={vi.fn()}
        canReview
      />,
    );
    expect(screen.queryByTestId("change-review-panel")).toBeNull();
    expect(tab("매니페스트")).toHaveAttribute("aria-selected", "true");
  });

  it("신원이 바뀌면 검토 패널 인스턴스 자체가 교체된다", () => {
    const { view } = openReviewable();
    fireEvent.click(tab("변경 검토"));
    const first = screen.getByTestId("change-review-panel").getAttribute("data-instance");
    expect(first).toBeTruthy();
    expect(panelLog.mounted).toContain(first);
    expect(panelLog.unmounted).not.toContain(first);

    /* 같은 자리에서 detail만 바뀝니다. key가 없으면 리셋 effect가 돌기 **전에**
       같은 인스턴스가 새 uid로 한 번 렌더되고, 그 렌더에서 이전 초안과 진행 중
       요청이 새 대상에 붙습니다. */
    view.rerender(
      <ResourceDetailDrawer
        open
        loading={false}
        detail={{ ...CONFIGMAP, uid: "uid-configmaps-002", resourceVersion: "9999" }}
        error={undefined}
        onClose={vi.fn()}
        canReview
      />,
    );

    /* 옛 인스턴스는 반드시 언마운트되었습니다. */
    expect(panelLog.unmounted).toContain(first);

    /* 어떤 인스턴스도 두 개의 uid를 보지 않았습니다 — 이 단언이 key 없이는 깨집니다. */
    const uidsByInstance = new Map<string, Set<string>>();
    for (const entry of panelLog.seen) {
      const separator = entry.indexOf(":");
      const instance = entry.slice(0, separator);
      const uid = entry.slice(separator + 1);
      const set = uidsByInstance.get(instance) ?? new Set<string>();
      set.add(uid);
      uidsByInstance.set(instance, set);
    }
    for (const [, uids] of uidsByInstance) {
      /* 실패하면 어떤 uid들이 한 인스턴스에 섞였는지 그대로 보입니다. */
      expect([...uids]).toHaveLength(1);
    }

    /* 새 uid를 본 것은 **새 인스턴스**뿐입니다. */
    const sawNew = panelLog.seen.filter((entry) => entry.endsWith(":uid-configmaps-002"));
    for (const entry of sawNew) expect(entry.startsWith(`${first}:`)).toBe(false);

    /* 리셋 규칙은 그대로 — 결국 View로 돌아갑니다. */
    expect(screen.queryByTestId("change-review-panel")).toBeNull();
    expect(tab("매니페스트")).toHaveAttribute("aria-selected", "true");
  });

  it("같은 uid라도 namespace·GVR이 바뀌면 View로 돌아간다", () => {
    const { view } = openReviewable();
    fireEvent.click(tab("변경 검토"));
    expect(screen.getByTestId("change-review-panel")).toBeVisible();
    view.rerender(
      <ResourceDetailDrawer
        open
        loading={false}
        detail={{ ...CONFIGMAP, namespace: "media" }}
        error={undefined}
        onClose={vi.fn()}
        canReview
      />,
    );
    expect(screen.queryByTestId("change-review-panel")).toBeNull();
  });

  it("닫았다 다시 열면 View로 돌아간다", () => {
    const { view } = openReviewable();
    fireEvent.click(tab("변경 검토"));
    expect(screen.getByTestId("change-review-panel")).toBeVisible();

    view.rerender(
      <ResourceDetailDrawer open={false} loading={false} detail={CONFIGMAP} error={undefined} onClose={vi.fn()} canReview />,
    );
    expect(screen.queryByTestId("change-review-panel")).toBeNull();

    view.rerender(
      <ResourceDetailDrawer open loading={false} detail={CONFIGMAP} error={undefined} onClose={vi.fn()} canReview />,
    );
    expect(tab("매니페스트")).toHaveAttribute("aria-selected", "true");
    expect(screen.queryByTestId("change-review-panel")).toBeNull();
  });

  it("capability를 잃으면 진입점과 패널이 함께 사라진다", () => {
    const { view } = openReviewable();
    fireEvent.click(tab("변경 검토"));
    expect(screen.getByTestId("change-review-panel")).toBeVisible();

    view.rerender(
      <ResourceDetailDrawer
        open
        loading={false}
        detail={CONFIGMAP}
        error={undefined}
        onClose={vi.fn()}
        canReview={false}
      />,
    );
    expect(screen.queryByRole("tablist")).toBeNull();
    expect(screen.queryByTestId("change-review-panel")).toBeNull();
    expect(screen.getByLabelText("api-config 매니페스트 (YAML, 읽기 전용)")).toBeVisible();
  });

  it("탭이 있어도 닫기 포커스·Escape·읽기 전용 단언이 그대로다", () => {
    const { onClose } = openReviewable();
    expect(document.activeElement).toBe(screen.getByRole("button", { name: "닫기" }));

    fireEvent.keyDown(window, { key: "Escape" });
    expect(onClose).toHaveBeenCalledTimes(1);
    fireEvent.click(screen.getByRole("button", { name: "닫기" }));
    expect(onClose).toHaveBeenCalledTimes(2);

    expect(screen.getByText(CONFIGMAP.uid)).toBeVisible();
    expect(screen.getByText("4242")).toBeVisible();
    expect(screen.getByRole("note").textContent ?? "").toContain("서버에서 제거된 뒤");
    expect(forbiddenControls()).toHaveLength(0);
  });

  it("focus trap이 탭 버튼까지 포함해 대화상자 안에 머문다", () => {
    openReviewable();
    const dialog = screen.getByRole("dialog");
    const focusable = dialog.querySelectorAll<HTMLElement>(
      'button, [href], textarea, input, select, [tabindex]:not([tabindex="-1"])',
    );
    /* 닫기 + 탭 2개가 최소한 잡혀야 하고, 전부 대화상자 안에 있어야 합니다. */
    expect(focusable.length).toBeGreaterThanOrEqual(3);
    for (const el of focusable) expect(dialog.contains(el)).toBe(true);

    const last = focusable[focusable.length - 1];
    last.focus();
    fireEvent.keyDown(window, { key: "Tab" });
    expect(dialog.contains(document.activeElement)).toBe(true);
  });

  it("오버레이 클릭은 닫고 대화상자 안 클릭은 닫지 않는다", () => {
    const { onClose } = openReviewable();
    fireEvent.click(screen.getByRole("dialog"));
    expect(onClose).not.toHaveBeenCalled();
    /* role="presentation"은 접근성 트리에 없으므로 클래스로 집습니다. */
    const overlay = document.querySelector(".resource-drawer__overlay");
    expect(overlay).not.toBeNull();
    fireEvent.click(overlay as Element);
    expect(onClose).toHaveBeenCalledTimes(1);
  });
});
