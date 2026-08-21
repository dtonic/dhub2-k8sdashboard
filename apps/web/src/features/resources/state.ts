import type { ResourceDescriptor, ResourceState } from "@k8s-dashboard/contracts";
import { HttpError } from "@/api/client";

/**
 * Resource Explorer 표시 상태 (ADR 0018)
 * --------------------------------------------------------------------------
 * "데이터 없음 · 권한 없음 · 미지원 · 동기화 중 · 기능 없음"을 같은 빈 화면으로
 * 만들지 않습니다. 서버가 카탈로그의 `state`와 오류 코드로 이미 구분해 주므로
 * UI는 그것을 지우지 않고 그대로 옮기기만 합니다.
 */
export type ExplorerState = ResourceState | "unavailable" | "empty";

export type StateNotice = {
  state: ExplorerState;
  title: string;
  detail: string;
  /** 상태 화면의 시각 변형. 색 단독으로 전달하지 않고 글리프·문구가 항상 함께 갑니다. */
  tone: "forbidden" | "degraded" | "error" | "neutral";
};

const NOTICE: Record<ExplorerState, Omit<StateNotice, "state">> = {
  ready: { title: "조회 준비 완료", detail: "필터를 좁혀 항목을 찾으세요.", tone: "neutral" },
  empty: {
    title: "조건에 맞는 리소스가 없습니다",
    detail: "조회는 성공했고 결과가 0건입니다. 이름·라벨 필터나 Namespace를 넓혀 보세요.",
    tone: "neutral",
  },
  syncing: {
    title: "리소스 캐시를 동기화하는 중입니다",
    detail: "아직 목록이 준비되지 않았습니다. 0건이 아니라 준비 중입니다 — 잠시 후 다시 조회하세요.",
    tone: "degraded",
  },
  unsupported: {
    title: "이 API는 metadata 전용 조회를 지원하지 않습니다",
    detail:
      "서버가 전체 객체 watch로 물러나지 않고 미지원으로 알립니다. 다른 Kind를 선택하거나 클러스터 관리자에게 문의하세요.",
    tone: "degraded",
  },
  forbidden: {
    title: "서버에 이 리소스의 조회 권한이 없습니다",
    detail: "데이터가 없는 것이 아니라 대시보드 ServiceAccount의 RBAC에 이 리소스가 없습니다.",
    tone: "forbidden",
  },
  missing: {
    title: "클러스터가 이 API를 제공하지 않습니다",
    detail: "CRD가 설치되지 않았거나 이 버전을 제공하지 않습니다.",
    tone: "neutral",
  },
  unavailable: {
    title: "이 배포에서는 리소스 탐색을 사용할 수 없습니다",
    detail: "중앙(central) 모드이거나 기능이 꺼져 있습니다. 관리자가 Helm 값으로 켤 수 있습니다.",
    tone: "degraded",
  },
};

export function stateNotice(state: ExplorerState, detail?: string): StateNotice {
  const base = NOTICE[state];
  return { state, ...base, detail: detail?.trim() ? detail : base.detail };
}

/** 서버 오류 코드를 표시 상태로 옮깁니다. 알 수 없는 코드는 상태로 위장하지 않습니다. */
export function stateFromError(error: unknown): ExplorerState | undefined {
  if (!(error instanceof HttpError)) return undefined;
  switch (error.body.code) {
    case "resources_unavailable":
      return "unavailable";
    case "resource_syncing":
      return "syncing";
    case "resource_unsupported":
      return "unsupported";
    case "resource_forbidden":
      return "forbidden";
    case "resource_not_served":
    case "resource_not_allowlisted":
      return "missing";
    default:
      return undefined;
  }
}

/** 카탈로그 항목과 목록 오류를 합쳐 최종 표시 상태를 정합니다. */
export function explorerState(
  descriptor: ResourceDescriptor | undefined,
  listError: unknown,
  itemCount: number,
  loaded: boolean,
): ExplorerState {
  const fromError = stateFromError(listError);
  if (fromError) return fromError;
  if (descriptor && descriptor.state !== "ready") return descriptor.state;
  if (loaded && itemCount === 0) return "empty";
  return "ready";
}

/** 목록/상세 오류가 상태가 아니면(400·409·429 등) 사용자에게 그 이유를 그대로 알립니다. */
export function requestErrorMessage(error: unknown): string | undefined {
  if (!(error instanceof HttpError)) return undefined;
  if (stateFromError(error)) return undefined;
  switch (error.body.code) {
    case "invalid_filter":
      return "필터 값이 서버 상한을 벗어났습니다. 이름·라벨 조건을 줄여 보세요.";
    case "invalid_cursor":
      return "이어보기 위치가 만료되었습니다. 처음부터 다시 조회하세요.";
    case "detail_rate_limited":
      return "상세 조회 한도를 초과했습니다. 잠시 후 다시 시도하세요.";
    case "uid_mismatch":
      return "같은 이름의 다른 객체로 교체되었습니다. 목록을 새로고침하세요.";
    case "object_too_large":
      return "객체 크기가 응답 한도를 넘어 표시할 수 없습니다.";
    case "not_found":
      return "목록에 없는 항목입니다. 목록을 새로고침하세요.";
    case "cluster_scope_required":
      return "클러스터 범위 리소스는 클러스터 전체 권한이 필요합니다.";
    case "namespace_access_denied":
      return "이 Namespace에 대한 권한이 없습니다.";
    case "forbidden":
      return "리소스 탐색 권한이 없습니다.";
    default:
      return error.body.message || "요청을 처리하지 못했습니다.";
  }
}
