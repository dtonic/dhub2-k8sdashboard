import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import type { ApiError, ResourceDetailResponse, ResourceDryRunResponse } from "@k8s-dashboard/contracts";
import { HttpError } from "@/api/client";
import { ChangeReviewPanel } from "./ChangeReviewPanel";
import {
  DRY_RUN_ABSOLUTE_MAX_BYTES,
  identityKey,
  localReject,
  manifestByteLength,
  REVIEW_FALLBACK_MESSAGE,
  REVIEW_IDENTITY_MISMATCH_MESSAGE,
  reviewErrorMessage,
} from "./dryrun";

/**
 * 변경 검토 패널 (ADR 0019 Phase 1)
 *
 * 전송 계층을 흉내 내지 않고 **실제 apiRequest 경로**로 내려보낸 뒤 fetch에서
 * 가로챕니다 — 경로·헤더·본문·signal이 실제로 무엇인지 확인해야 하기 때문입니다.
 */

const SEED = "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: api-config\n  namespace: payments\ndata:\n  LOG_LEVEL: info\n";

const DETAIL: ResourceDetailResponse = {
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
  yaml: SEED,
  redacted: ["metadata.managedFields"],
};

function dryRunResponse(over: Partial<ResourceDryRunResponse> = {}): ResourceDryRunResponse {
  return {
    clusterId: DETAIL.clusterId,
    group: DETAIL.group,
    version: DETAIL.version,
    resource: DETAIL.resource,
    apiVersion: DETAIL.apiVersion,
    kind: DETAIL.kind,
    namespace: DETAIL.namespace,
    name: DETAIL.name,
    uid: DETAIL.uid,
    resourceVersion: DETAIL.resourceVersion,
    generatedAt: "2026-08-22T00:00:01Z",
    fieldManager: "k8s-dashboard-dryrun",
    outcome: "changed",
    changes: [{ path: "data.LOG_LEVEL", op: "changed", before: '"info"', after: '"debug"' }],
    changeCount: 1,
    truncated: false,
    warnings: [],
    violations: [],
    redacted: ["metadata.managedFields"],
    ...over,
  };
}

let fetchMock: ReturnType<typeof vi.fn>;

function respondWith(body: unknown, status = 200) {
  return {
    ok: status >= 200 && status < 300,
    status,
    headers: new Headers({ "content-type": "application/json" }),
    json: async () => body,
    body: null,
  } as unknown as Response;
}

/**
 * microtask를 여러 번 비웁니다.
 *
 * fetch → res.ok → res.json() → 호출부 복귀까지 await가 여러 번 걸려 있어서,
 * 한 tick만 흘리면 "늦은 응답이 새 나가지 않았다"가 아니라 "아직 도착하지 않았다"를
 * 확인하게 됩니다.
 */
async function flushMicrotasks() {
  for (let i = 0; i < 20; i += 1) await Promise.resolve();
}

/** 응답을 테스트가 직접 풀 수 있게 만든 지연 응답입니다(취소·경합 시나리오용). */
function deferredResponse() {
  let settle!: (value: Response) => void;
  let fail!: (reason: unknown) => void;
  const promise = new Promise<Response>((resolve, reject) => {
    settle = resolve;
    fail = reject;
  });
  return { promise, settle, fail };
}

beforeEach(() => {
  fetchMock = vi.fn();
  vi.stubGlobal("fetch", fetchMock);
  /* 저장소 단언이 의미를 가지려면 매 테스트가 빈 상태에서 시작해야 합니다. */
  window.localStorage.clear();
  window.sessionStorage.clear();
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

function open(detail: ResourceDetailResponse = DETAIL) {
  return render(<ChangeReviewPanel detail={detail} />);
}

function editor(): HTMLTextAreaElement {
  return screen.getByLabelText("api-config 매니페스트 (YAML, 검토용 편집)") as HTMLTextAreaElement;
}

function reviewButton() {
  return screen.getByRole("button", { name: "변경 검토" });
}

function lastRequest(): [URL, RequestInit] {
  const call = fetchMock.mock.calls.at(-1);
  if (!call) throw new Error("fetch가 호출되지 않았습니다");
  return call as [URL, RequestInit];
}

/* ── 편집기 ─────────────────────────────────────────────────────────────── */

describe("ChangeReviewPanel 편집기", () => {
  it("정제된 detail.yaml로 seed하고 입력 보조를 끈다", () => {
    open();
    const area = editor();
    expect(area.value).toBe(SEED);
    expect(area.getAttribute("spellcheck")).toBe("false");
    expect(area.getAttribute("autocomplete")).toBe("off");
    expect(area.getAttribute("autocapitalize")).toBe("off");
  });

  it("적용·저장·삭제·생성·force 컨트롤이 없다", () => {
    open();
    for (const button of screen.getAllByRole("button")) {
      expect(button.textContent ?? "").not.toMatch(/적용|저장|삭제|생성|force|token|강제/i);
    }
    expect(screen.queryByRole("checkbox")).toBeNull();
    expect(document.body.textContent ?? "").toContain("검토만 하며 클러스터에 적용하지 않습니다");
  });

  it("처음에는 결과도 오류도 없다", () => {
    open();
    expect(screen.getByText("아직 검토하지 않았습니다")).toBeVisible();
    expect(screen.queryByRole("alert")).toBeNull();
  });
});

/* ── 클라이언트 거절 ────────────────────────────────────────────────────── */

describe("ChangeReviewPanel 클라이언트 거절", () => {
  it("빈 입력은 요청을 만들지 않는다", async () => {
    open();
    fireEvent.change(editor(), { target: { value: "   \n  " } });
    fireEvent.click(reviewButton());
    await waitFor(() => expect(screen.getByRole("alert")).toBeVisible());
    expect(screen.getByRole("alert").textContent ?? "").toContain("비어 있습니다");
    expect(fetchMock).not.toHaveBeenCalled();
  });

  it("멀티바이트 1MiB 초과는 요청을 만들지 않는다", async () => {
    open();
    /* "가"는 UTF-8 3바이트입니다 — 문자 수가 아니라 바이트가 상한의 단위입니다. */
    const huge = "가".repeat(350_000);
    expect(manifestByteLength(huge)).toBeGreaterThan(DRY_RUN_ABSOLUTE_MAX_BYTES);
    fireEvent.change(editor(), { target: { value: huge } });
    fireEvent.click(reviewButton());
    await waitFor(() => expect(screen.getByRole("alert")).toBeVisible());
    expect(screen.getByRole("alert").textContent ?? "").toContain("1MiB");
    expect(fetchMock).not.toHaveBeenCalled();
  });

  it("초기 detail.yaml이 1MiB를 넘으면 편집 상태에 복제하지 않는다", async () => {
    const marker = "RAW_SEED_MARKER";
    const huge = `${marker}\n${"가".repeat(350_000)}`;
    render(<ChangeReviewPanel detail={{ ...DETAIL, yaml: huge }} />);

    /* 큰 문자열이 편집기(=상태)로 들어오지 않았습니다. */
    expect(editor().value).toBe("");
    expect(screen.getByRole("alert").textContent ?? "").toContain("1MiB");
    expect(document.body.textContent ?? "").not.toContain(marker);
    expect(document.body.innerHTML).not.toContain(marker);

    /* 그 상태에서 눌러도 요청은 없습니다. */
    fireEvent.click(reviewButton());
    await flushMicrotasks();
    expect(fetchMock).not.toHaveBeenCalled();
    expect(screen.getByRole("alert").textContent ?? "").toContain("1MiB");
  });

  it("붙여넣기가 1MiB를 넘으면 비우고 거절을 유지하다가 다음 유효 입력에서 푼다", async () => {
    fetchMock.mockResolvedValue(respondWith(dryRunResponse()));
    open();
    const marker = "RAW_PASTE_MARKER";
    fireEvent.change(editor(), { target: { value: `${marker}${"가".repeat(350_000)}` } });

    expect(editor().value).toBe("");
    expect(screen.getByRole("alert").textContent ?? "").toContain("1MiB");
    expect(document.body.textContent ?? "").not.toContain(marker);
    expect(JSON.stringify(window.localStorage)).not.toContain(marker);
    expect(JSON.stringify(window.sessionStorage)).not.toContain(marker);
    expect(window.location.href).not.toContain(marker);

    /* 거절은 눌러도 유지되고 요청을 만들지 않습니다. */
    fireEvent.click(reviewButton());
    await flushMicrotasks();
    expect(fetchMock).not.toHaveBeenCalled();
    expect(screen.getByRole("alert").textContent ?? "").toContain("1MiB");

    /* 다음 유효 입력에서 풀립니다. */
    fireEvent.change(editor(), { target: { value: SEED } });
    expect(editor().value).toBe(SEED);
    expect(screen.queryByRole("alert")).toBeNull();
    fireEvent.click(reviewButton());
    await waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(1));
  });

  it("정확히 1MiB는 클라이언트가 막지 않는다", async () => {
    const exact = "a".repeat(DRY_RUN_ABSOLUTE_MAX_BYTES);
    expect(manifestByteLength(exact)).toBe(DRY_RUN_ABSOLUTE_MAX_BYTES);
    expect(localReject(exact)).toBeUndefined();

    fetchMock.mockResolvedValue(respondWith(dryRunResponse()));
    open();
    fireEvent.change(editor(), { target: { value: exact } });
    fireEvent.click(reviewButton());
    await waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(1));
  });
});

/* ── 전송 ───────────────────────────────────────────────────────────────── */

describe("ChangeReviewPanel 전송", () => {
  it("정확한 경로·메서드·헤더·본문·signal로 한 번만 보낸다", async () => {
    fetchMock.mockResolvedValue(respondWith(dryRunResponse()));
    open();
    fireEvent.change(editor(), { target: { value: `${SEED}  EXTRA: "1"\n` } });
    fireEvent.click(reviewButton());
    await waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(1));

    const [url, init] = lastRequest();
    const parsed = new URL(String(url));
    expect(parsed.pathname).toBe(
      "/api/v1/clusters/prod-seoul/resources/core/v1/configmaps/object/dry-run",
    );
    /* 매니페스트가 질의 문자열로 새지 않습니다. */
    expect(parsed.search).toBe("");
    expect(init.method).toBe("POST");

    const headers = init.headers as Headers;
    expect(headers.get("content-type")).toBe("application/json");
    expect(init.signal).toBeInstanceOf(AbortSignal);

    const body = JSON.parse(String(init.body)) as Record<string, unknown>;
    expect(Object.keys(body).sort()).toEqual(
      ["apiVersion", "kind", "manifest", "name", "namespace", "resourceVersion", "uid"].sort(),
    );
    expect(body).toMatchObject({
      apiVersion: "v1",
      kind: "ConfigMap",
      namespace: "payments",
      name: "api-config",
      uid: "uid-configmaps-001",
      resourceVersion: "4242",
    });
    expect(body.manifest).toBe(`${SEED}  EXTRA: "1"\n`);
    /* force·token·동사 같은 필드는 계약에 없고 보내지도 않습니다. */
    for (const forbidden of ["force", "token", "changeToken", "dryRun", "fieldManager", "verb"]) {
      expect(body).not.toHaveProperty(forbidden);
    }
  });

  it("cluster 범위 리소스에는 namespace를 보내지 않는다", async () => {
    fetchMock.mockResolvedValue(respondWith(dryRunResponse({ namespace: undefined })));
    const clusterScoped: ResourceDetailResponse = {
      ...DETAIL,
      group: "storage.k8s.io",
      resource: "storageclasses",
      apiVersion: "storage.k8s.io/v1",
      kind: "StorageClass",
      name: "api-config",
      namespace: undefined,
    };
    render(<ChangeReviewPanel detail={clusterScoped} />);
    fireEvent.click(reviewButton());
    await waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(1));
    const body = JSON.parse(String(lastRequest()[1].body)) as Record<string, unknown>;
    expect(body).not.toHaveProperty("namespace");
  });
});

/* ── 취소·경합 ──────────────────────────────────────────────────────────── */

describe("ChangeReviewPanel 취소와 경합", () => {
  it("재제출은 이전 요청을 끊고 취소를 오류로 보이지 않는다", async () => {
    const first = deferredResponse();
    fetchMock.mockReturnValueOnce(first.promise).mockResolvedValue(respondWith(dryRunResponse()));
    open();
    fireEvent.click(reviewButton());
    await waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(1));
    const firstSignal = lastRequest()[1].signal as AbortSignal;

    fireEvent.click(reviewButton());
    await waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(2));
    expect(firstSignal.aborted).toBe(true);

    /* 끊긴 요청이 뒤늦게 거절돼도 사용자에게는 오류가 아닙니다. */
    first.fail(new DOMException("aborted", "AbortError"));
    await waitFor(() => expect(screen.getByLabelText("검토 결과")).toBeVisible());
    expect(screen.queryByRole("alert")).toBeNull();
  });

  it("입력이 바뀌면 진행 중 요청을 끊고 결과·오류를 지운다", async () => {
    const pending = deferredResponse();
    fetchMock.mockReturnValue(pending.promise);
    open();
    fireEvent.click(reviewButton());
    await waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(1));
    const signal = lastRequest()[1].signal as AbortSignal;

    fireEvent.change(editor(), { target: { value: `${SEED}# edited\n` } });
    expect(signal.aborted).toBe(true);
    expect(screen.getByText("아직 검토하지 않았습니다")).toBeVisible();
    expect(screen.queryByRole("alert")).toBeNull();
    expect(screen.queryByLabelText("검토 결과")).toBeNull();

    /* 늦게 도착한 성공 응답이 화면을 덮지 않습니다. */
    pending.settle(respondWith(dryRunResponse()));
    await flushMicrotasks();
    expect(screen.queryByLabelText("검토 결과")).toBeNull();
  });

  it("언마운트하면 진행 중 요청을 끊는다", async () => {
    const pending = deferredResponse();
    fetchMock.mockReturnValue(pending.promise);
    const view = open();
    fireEvent.click(reviewButton());
    await waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(1));
    const signal = lastRequest()[1].signal as AbortSignal;
    view.unmount();
    expect(signal.aborted).toBe(true);
  });

  it("대상이 바뀌면 재seed하고 이전 대상의 응답을 버린다", async () => {
    const pending = deferredResponse();
    fetchMock.mockReturnValue(pending.promise);
    const view = open();
    fireEvent.change(editor(), { target: { value: `${SEED}# edited\n` } });
    fireEvent.click(reviewButton());
    await waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(1));

    const next: ResourceDetailResponse = {
      ...DETAIL,
      uid: "uid-configmaps-002",
      resourceVersion: "9999",
      yaml: `${SEED}# other\n`,
    };
    expect(identityKey(next)).not.toBe(identityKey(DETAIL));
    view.rerender(<ChangeReviewPanel detail={next} />);
    expect(editor().value).toBe(`${SEED}# other\n`);

    pending.settle(respondWith(dryRunResponse()));
    await flushMicrotasks();
    expect(screen.queryByLabelText("검토 결과")).toBeNull();
    expect(screen.getByText("아직 검토하지 않았습니다")).toBeVisible();
  });

  it("응답 신원이 요청과 다르면 내용을 렌더하지 않는다", async () => {
    fetchMock.mockResolvedValue(
      respondWith(
        dryRunResponse({
          uid: "uid-someone-else",
          changes: [{ path: "data.LEAK", op: "changed", before: '"a"', after: '"b"' }],
        }),
      ),
    );
    open();
    fireEvent.click(reviewButton());
    await waitFor(() => expect(screen.getByRole("alert")).toBeVisible());
    expect(screen.getByRole("alert").textContent ?? "").toContain(REVIEW_IDENTITY_MISMATCH_MESSAGE);
    expect(screen.queryByLabelText("검토 결과")).toBeNull();
    expect(document.body.textContent ?? "").not.toContain("data.LEAK");
  });
});

/* ── 결과 렌더 ──────────────────────────────────────────────────────────── */

describe("ChangeReviewPanel 결과", () => {
  async function submitWith(response: ResourceDryRunResponse) {
    fetchMock.mockResolvedValue(respondWith(response));
    open();
    fireEvent.click(reviewButton());
    await waitFor(() => expect(screen.getByLabelText("검토 결과")).toBeVisible());
  }

  it("변경 있음을 글리프가 아니라 텍스트로도 알린다", async () => {
    await submitWith(dryRunResponse());
    expect(screen.getByText("변경 있음")).toBeVisible();
    expect(screen.getByText("data.LOG_LEVEL")).toBeVisible();
    expect(screen.getByText("변경")).toBeVisible();
  });

  it("변경 없음", async () => {
    await submitWith(dryRunResponse({ outcome: "unchanged", changes: [], changeCount: 0 }));
    expect(screen.getByText("변경 없음")).toBeVisible();
  });

  it("거절과 사유를 텍스트로 알린다", async () => {
    await submitWith(
      dryRunResponse({
        outcome: "rejected",
        rejectedBy: "conflict",
        changes: [],
        changeCount: 0,
        violations: [{ message: "다른 field manager가 이 필드를 소유하고 있습니다." }],
      }),
    );
    expect(screen.getByText("거절됨")).toBeVisible();
    expect(document.body.textContent ?? "").toContain("소유권 충돌");
    expect(document.body.textContent ?? "").toContain("다른 field manager가 이 필드를 소유");
  });

  it("추가·제거도 텍스트 라벨을 쓴다", async () => {
    await submitWith(
      dryRunResponse({
        changes: [
          { path: "spec", op: "added", after: '{"a":1}' },
          { path: "data.OLD", op: "removed", before: '"x"' },
        ],
        changeCount: 2,
      }),
    );
    expect(screen.getByText("추가")).toBeVisible();
    expect(screen.getByText("제거")).toBeVisible();
    /* 적용을 암시하는 문구가 화면에 없어야 합니다. disclaimer 한 문장만 예외입니다. */
    const text = document.body.textContent ?? "";
    expect(text).toContain("검토만 하며 클러스터에 적용하지 않습니다");
    expect(text).not.toMatch(/적용하면|적용 시|적용해도/);
  });

  it("rejectedBy 세 가지를 각각 고정 문구로 옮긴다", async () => {
    for (const [reason, expected] of [
      ["validation", "서버 검증에서 거절"],
      ["admission", "admission webhook이 거절"],
      ["conflict", "다른 field manager와 소유권 충돌"],
    ] as const) {
      cleanup();
      await submitWith(
        dryRunResponse({ outcome: "rejected", rejectedBy: reason, changes: [], changeCount: 0 }),
      );
      expect(document.body.textContent ?? "").toContain(expected);
    }
  });

  it("모르는 enum 값은 원문을 text에도 attribute에도 반사하지 않는다", async () => {
    const marker = "EVIL_ENUM_MARKER";
    await submitWith(
      dryRunResponse({
        outcome: marker as never,
        rejectedBy: marker as never,
        changes: [{ path: "data.A", op: marker as never, before: '"a"', after: '"b"' }],
        changeCount: 1,
      }),
    );
    const outcomeEl = document.querySelector(".resource-review__outcome");
    expect(outcomeEl?.getAttribute("data-outcome")).toBe("unknown");
    expect(document.body.textContent ?? "").not.toContain(marker);
    expect(document.body.innerHTML).not.toContain(marker);
    expect(screen.getByText("결과를 해석할 수 없습니다")).toBeVisible();
    expect(document.body.textContent ?? "").toContain("알 수 없는 사유");
    expect(screen.getByText("알 수 없음")).toBeVisible();
  });

  it("프로토타입 키 enum도 던지지 않고 고정 문구로만 렌더한다", async () => {
    for (const hostile of ["__proto__", "constructor", "toString"]) {
      cleanup();
      await submitWith(
        dryRunResponse({
          outcome: hostile as never,
          rejectedBy: hostile as never,
          changes: [{ path: "data.A", op: hostile as never, before: '"a"', after: '"b"' }],
          changeCount: 1,
        }),
      );
      /* 여기까지 왔다는 것 자체가 렌더가 던지지 않았다는 뜻입니다. */
      const outcomeEl = document.querySelector(".resource-review__outcome");
      expect(outcomeEl?.getAttribute("data-outcome")).toBe("unknown");
      expect(screen.getByText("결과를 해석할 수 없습니다")).toBeVisible();
      expect(document.body.textContent ?? "").toContain("알 수 없는 사유");
      expect(screen.getByText("알 수 없음")).toBeVisible();

      const html = document.body.innerHTML;
      expect(html).not.toContain(hostile);
      /* 객체·함수가 문자열로 새어 나오지 않습니다. */
      expect(html).not.toContain("[object Object]");
      expect(html).not.toContain("function ");
      expect(html).not.toContain("native code");
    }
  });

  it("warnings·violations·redacted도 유계로만 그린다", async () => {
    await submitWith(
      dryRunResponse({
        outcome: "rejected",
        rejectedBy: "validation",
        changes: [],
        changeCount: 0,
        warnings: Array.from({ length: 40 }, (_, i) => `경고-${String(i).padStart(3, "0")}`),
        violations: Array.from({ length: 40 }, (_, i) => ({ message: `위반-${String(i).padStart(3, "0")}` })),
        redacted: Array.from({ length: 80 }, (_, i) => `path-${String(i).padStart(3, "0")}`),
      }),
    );
    const text = document.body.textContent ?? "";
    expect(text).toContain("서버 경고 32건");
    expect(text).toContain("거절 사유 32건");
    expect(text).toContain("비교에서 제외된 경로 64개");
    /* 상한 밖 항목은 그리지 않습니다. */
    expect(text).not.toContain("경고-032");
    expect(text).not.toContain("위반-032");
    expect(text).not.toContain("path-064");
  });

  it("valueRedacted면 값을 절대 렌더하지 않는다", async () => {
    await submitWith(
      dryRunResponse({
        changes: [
          {
            path: 'metadata.annotations["example.com/api-token"]',
            op: "changed",
            valueRedacted: true,
            /* 서버가 보내지 않는 값이지만, 보내더라도 화면에 나오면 안 됩니다. */
            before: "OLD_SECRET_VALUE",
            after: "NEW_SECRET_VALUE",
          },
        ],
        changeCount: 1,
      }),
    );
    expect(screen.getByText("값은 표시하지 않습니다")).toBeVisible();
    const rendered = document.body.textContent ?? "";
    expect(rendered).not.toContain("OLD_SECRET_VALUE");
    expect(rendered).not.toContain("NEW_SECRET_VALUE");
    expect(document.body.innerHTML).not.toContain("OLD_SECRET_VALUE");
  });

  it("절단·경고·정제 경로를 알린다", async () => {
    await submitWith(
      dryRunResponse({
        changeCount: 337,
        truncated: true,
        changes: [{ path: "data.A", op: "changed", before: '"a"', after: '"b"', valueTruncated: true }],
        warnings: ["API 서버가 검토 요청에 경고를 반환했습니다. 원문은 보안상 표시하지 않습니다."],
        redacted: ["status", "metadata.managedFields"],
      }),
    );
    const text = document.body.textContent ?? "";
    expect(text).toContain("변경 337건");
    expect(text).toContain("잘림");
    expect(text).toContain("값 잘림");
    expect(text).toContain("서버 경고 1건");
    expect(text).toContain("metadata.managedFields");
  });

  it("계약 상한을 넘겨 보내도 유계로만 그린다", async () => {
    const changes = Array.from({ length: 400 }, (_, i) => ({
      path: `data.K${String(i).padStart(4, "0")}`,
      op: "changed" as const,
      before: '"a"',
      after: '"b"',
    }));
    await submitWith(dryRunResponse({ changes, changeCount: 400, truncated: true }));
    expect(screen.getAllByText(/^data\.K\d{4}$/)).toHaveLength(200);
    expect(screen.queryByText("data.K0200")).toBeNull();
  });
});

/* ── 오류 ───────────────────────────────────────────────────────────────── */

describe("ChangeReviewPanel 오류", () => {
  it("서버 code를 고정 한국어로만 옮기고 원문 message를 쓰지 않는다", async () => {
    fetchMock.mockResolvedValue(
      respondWith(
        { code: "resource_version_mismatch", message: "RAW_SERVER_MESSAGE", requestId: "r" },
        409,
      ),
    );
    open();
    fireEvent.click(reviewButton());
    await waitFor(() => expect(screen.getByRole("alert")).toBeVisible());
    const alert = screen.getByRole("alert").textContent ?? "";
    expect(alert).toContain("객체가 그 사이에 바뀌었습니다");
    expect(alert).not.toContain("RAW_SERVER_MESSAGE");
  });

  it("알 수 없는 오류는 고정 문구로만 알린다", async () => {
    fetchMock.mockRejectedValue(new Error("SECRET_INTERNAL_DETAIL"));
    open();
    fireEvent.click(reviewButton());
    await waitFor(() => expect(screen.getByRole("alert")).toBeVisible());
    const rendered = document.body.textContent ?? "";
    expect(rendered).toContain(REVIEW_FALLBACK_MESSAGE);
    expect(rendered).not.toContain("SECRET_INTERNAL_DETAIL");
  });

  it("알 수 없는 code도 원문을 노출하지 않는다", async () => {
    fetchMock.mockResolvedValue(
      respondWith({ code: "brand_new_code", message: "RAW_SERVER_MESSAGE", requestId: "r" }, 500),
    );
    open();
    fireEvent.click(reviewButton());
    await waitFor(() => expect(screen.getByRole("alert")).toBeVisible());
    const rendered = document.body.textContent ?? "";
    expect(rendered).toContain(REVIEW_FALLBACK_MESSAGE);
    expect(rendered).not.toContain("RAW_SERVER_MESSAGE");
  });

  it("프로토타입 키 code 응답도 화면을 깨지 않는다", async () => {
    for (const hostile of ["__proto__", "constructor", "toString"]) {
      cleanup();
      fetchMock.mockResolvedValue(
        respondWith({ code: hostile, message: "RAW_SERVER_MESSAGE", requestId: "r" }, 500),
      );
      open();
      fireEvent.click(reviewButton());
      await waitFor(() => expect(screen.getByRole("alert")).toBeVisible());
      const text = document.body.textContent ?? "";
      expect(text).toContain(REVIEW_FALLBACK_MESSAGE);
      expect(text).not.toContain("RAW_SERVER_MESSAGE");
      expect(document.body.innerHTML).not.toContain("[object Object]");
      expect(document.body.innerHTML).not.toContain("function ");
    }
  });

  it("오류 화면에 매니페스트가 실리지 않는다", async () => {
    fetchMock.mockResolvedValue(
      respondWith({ code: "invalid_manifest", message: "x", requestId: "r" }, 400),
    );
    open();
    const marker = "RAW_MANIFEST_MARKER";
    fireEvent.change(editor(), { target: { value: `${SEED}  MARK: ${marker}\n` } });
    fireEvent.click(reviewButton());
    await waitFor(() => expect(screen.getByRole("alert")).toBeVisible());
    expect(screen.getByRole("alert").textContent ?? "").not.toContain(marker);
    /* 편집기에는 남아 있어야 합니다 — 사용자가 고치던 내용입니다. */
    expect(editor().value).toContain(marker);
    /* 저장소·URL 어디에도 없습니다. */
    expect(window.localStorage.getItem("resource-review")).toBeNull();
    expect(JSON.stringify(window.localStorage)).not.toContain(marker);
    expect(JSON.stringify(window.sessionStorage)).not.toContain(marker);
    expect(window.location.href).not.toContain(marker);
  });
});

/* ── 오류 매핑 단위 ─────────────────────────────────────────────────────── */

describe("reviewErrorMessage", () => {
  it("알려진 code 전부에 고정 문구가 있다", () => {
    /* ApiError["code"]로 못박습니다. 계약에 없는 code를 여기 적으면 **컴파일이**
       실패하므로, 서버가 내보내는 코드와 TypeScript 계약의 드리프트가 드러납니다. */
    const codes: ApiError["code"][] = [
      "bad_request",
      "unsupported_media_type",
      "invalid_manifest",
      "manifest_mismatch",
      "manifest_too_large",
      "resource_version_mismatch",
      "uid_mismatch",
      "not_found",
      "object_too_large",
      "dryrun_unavailable",
      "dryrun_resource_denied",
      "dryrun_forbidden",
      "dryrun_rate_limited",
      "upstream_unavailable",
      "upstream_timeout",
      "namespace_access_denied",
      "cluster_scope_required",
      "invalid_filter",
      "resource_not_allowlisted",
      "resource_not_served",
      "resource_syncing",
    ];
    const seen = new Set<string>();
    for (const code of codes) {
      const message = reviewErrorMessage(
        new HttpError(400, { code, message: "RAW_SERVER_MESSAGE", requestId: "r" }),
      );
      expect(message).not.toBe(REVIEW_FALLBACK_MESSAGE);
      expect(message).not.toContain("RAW_SERVER_MESSAGE");
      seen.add(code);
    }
    expect(seen.size).toBe(codes.length);
  });

  it("HttpError가 아닌 값은 전부 고정 fallback이다", () => {
    for (const value of [undefined, null, "문자열 오류", 42, { message: "객체" }, new Error("x")]) {
      expect(reviewErrorMessage(value)).toBe(REVIEW_FALLBACK_MESSAGE);
    }
  });

  /* 평범한 객체 조회는 프로토타입 키에 뚫려 있습니다 — table["__proto__"]는 객체를,
     table["toString"]은 함수를 돌려주고 둘 다 truthy입니다. 그대로 렌더로 흘러가면
     React가 던져 화면 전체가 무너집니다. */
  it("프로토타입 키 code도 던지지 않고 고정 fallback이다", () => {
    for (const hostile of ["__proto__", "constructor", "toString", "hasOwnProperty", "valueOf"]) {
      const message = reviewErrorMessage(
        new HttpError(500, {
          code: hostile as ApiError["code"],
          message: "RAW_SERVER_MESSAGE",
          requestId: "r",
        }),
      );
      expect(typeof message).toBe("string");
      expect(message).toBe(REVIEW_FALLBACK_MESSAGE);
    }
  });
});

describe("identityKey", () => {
  it("구분자가 값에 들어와도 서로 다른 대상은 서로 다른 키가 된다", () => {
    /* 구분자 하나를 골라 잇는 방식은 이 두 쌍에서 충돌합니다. */
    const left: ResourceDetailResponse = { ...DETAIL, clusterId: "a|b", group: "c" };
    const right: ResourceDetailResponse = { ...DETAIL, clusterId: "a", group: "b|c" };
    expect(identityKey(left)).not.toBe(identityKey(right));

    const quoted: ResourceDetailResponse = { ...DETAIL, name: 'a","b' };
    const plain: ResourceDetailResponse = { ...DETAIL, name: "a", uid: "b" };
    expect(identityKey(quoted)).not.toBe(identityKey(plain));

    /* 같은 대상은 같은 키입니다. */
    expect(identityKey({ ...DETAIL })).toBe(identityKey(DETAIL));

    /* namespace 유무도 구분됩니다. */
    expect(identityKey({ ...DETAIL, namespace: undefined })).not.toBe(identityKey(DETAIL));
  });
});
