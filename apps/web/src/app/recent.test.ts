import { beforeEach, describe, expect, it } from "vitest";
import type { ResourceRecentItem } from "@k8s-dashboard/contracts";
import {
  chunkRecentRefs,
  clearRecent,
  encodeRecentRef,
  isValidRef,
  loadAllRecent,
  loadRecent,
  MAX_RECENT,
  MAX_RECENT_REQUEST_TARGET_BYTES,
  pruneRecent,
  recentRequestTarget,
  RecentRequestTooLargeError,
  rememberRecent,
  utf8Bytes,
  type RecentRef,
  type RecentRequestTarget,
} from "./recent";

const KEY = "k8s-dashboard.recent.v1";
const CLUSTER = "prod-seoul";

function ref(over: Partial<RecentRef> = {}): RecentRef {
  return {
    clusterId: CLUSTER,
    group: "core",
    version: "v1",
    resource: "services",
    namespace: "payments",
    name: "payments-api",
    uid: "uid-1",
    ...over,
  };
}

/** 서버가 되돌려준 항목. 클러스터는 요청 경로가 이미 정했으므로 들어 있지 않습니다. */
function resolved(r: RecentRef): ResourceRecentItem {
  return {
    group: r.group,
    version: r.version,
    resource: r.resource,
    kind: "Service",
    namespaced: true,
    namespace: r.namespace,
    name: r.name,
    uid: r.uid,
  };
}

function seedStorage(items: unknown[]) {
  window.localStorage.setItem(KEY, JSON.stringify({ v: 1, items }));
}

/** 서버 EncodeRecentRef와 같은 형식인지 되짚습니다(테스트 전용 디코더). */
function decode(encoded: string): string[] {
  const padded = encoded.replace(/-/g, "+").replace(/_/g, "/");
  return atob(padded + "=".repeat((4 - (padded.length % 4)) % 4)).split("\x1f");
}

beforeEach(() => {
  clearRecent();
});

describe("참조 인코딩", () => {
  it("서버와 같은 5개 세그먼트 base64url입니다", () => {
    const parts = decode(encodeRecentRef(ref()));
    expect(parts).toEqual(["1", "core/v1/services", "payments", "payments-api", "uid-1"]);
  });

  it("padding 없이 URL 안전 문자만 씁니다", () => {
    const encoded = encodeRecentRef(ref({ name: "a".repeat(40) }));
    expect(encoded).not.toContain("=");
    expect(encoded).toMatch(/^[A-Za-z0-9_-]+$/);
  });

  it("클러스터 범위 리소스는 namespace가 빈 세그먼트입니다", () => {
    const parts = decode(encodeRecentRef(ref({ namespace: "", resource: "storageclasses", group: "storage.k8s.io" })));
    expect(parts[1]).toBe("storage.k8s.io/v1/storageclasses");
    expect(parts[2]).toBe("");
  });
});

describe("저장소", () => {
  it("깨진 값·다른 버전·비배열을 예외 없이 빈 목록으로 답합니다", () => {
    for (const bad of ["{", "null", '{"v":2,"items":[]}', '{"v":1,"items":{}}', '{"items":[]}', "[]"]) {
      window.localStorage.setItem(KEY, bad);
      expect(loadRecent(CLUSTER)).toEqual([]);
    }
  });

  it("모양이 어긋난 항목만 골라 버립니다", () => {
    window.localStorage.setItem(
      KEY,
      JSON.stringify({
        v: 1,
        items: [
          ref(),
          { ...ref(), uid: "" }, // uid 없음
          { ...ref(), name: "bad name" }, // 허용되지 않는 문자
          { ...ref(), resource: "a/b" }, // GVR 세그먼트에 구분자
          ref({ uid: "uid-2", name: "indexer" }),
        ],
      }),
    );
    expect(loadRecent(CLUSTER).map((r) => r.uid)).toEqual(["uid-1", "uid-2"]);
  });

  it("최신이 앞에 오고 20개를 넘지 않습니다", () => {
    for (let i = 0; i < MAX_RECENT + 5; i++) {
      rememberRecent(ref({ uid: `uid-${i}`, name: `svc-${i}` }));
    }
    const items = loadRecent(CLUSTER);
    expect(items).toHaveLength(MAX_RECENT);
    expect(items[0].uid).toBe(`uid-${MAX_RECENT + 4}`);
  });

  it("같은 객체를 다시 고르면 중복되지 않고 맨 앞으로만 올라갑니다", () => {
    rememberRecent(ref({ uid: "uid-1" }));
    rememberRecent(ref({ uid: "uid-2", name: "indexer" }));
    rememberRecent(ref({ uid: "uid-1" }));
    const items = loadRecent(CLUSTER);
    expect(items.map((r) => r.uid)).toEqual(["uid-1", "uid-2"]);
  });

  it("서버가 해석하지 못한 참조는 저장소에서 사라집니다", () => {
    const kept = ref({ uid: "uid-2", name: "indexer" });
    rememberRecent(ref({ uid: "uid-1" }));
    rememberRecent(kept);
    rememberRecent(ref({ uid: "uid-3", name: "gone" }));
    /* 물어본 것은 셋, 서버가 되돌려준 것은 uid-2뿐입니다 — 나머지는 삭제·권한 상실·교체입니다. */
    const requested = loadRecent(CLUSTER);
    pruneRecent(requested, [resolved(kept)]);
    expect(loadRecent(CLUSTER).map((r) => r.uid)).toEqual(["uid-2"]);
  });

  it("물어보지 않은 항목은 지우지 않습니다 — 다른 탭이 방금 넣은 것을 잃지 않습니다", () => {
    const asked = ref({ uid: "uid-1" });
    rememberRecent(asked);
    const requested = loadRecent(CLUSTER);
    /* 요청을 보낸 뒤 다른 탭이 새 항목을 넣었습니다. 이 응답으로는 판단할 수 없습니다. */
    rememberRecent(ref({ uid: "uid-new", name: "just-opened" }));
    pruneRecent(requested, []);
    expect(loadRecent(CLUSTER).map((r) => r.uid)).toEqual(["uid-new"]);
  });

  it("다른 클러스터의 참조는 정리 대상이 아닙니다", () => {
    const mine = ref({ uid: "uid-1" });
    const theirs = ref({ clusterId: "stage-tokyo", uid: "uid-9", name: "other" });
    rememberRecent(theirs);
    rememberRecent(mine);
    pruneRecent(loadRecent(CLUSTER), []); // prod-seoul의 uid-1이 사라졌습니다
    expect(loadRecent(CLUSTER)).toEqual([]);
    expect(loadRecent("stage-tokyo").map((r) => r.uid)).toEqual(["uid-9"]);
  });

  it("isValidRef는 저장 전에도 같은 규칙으로 막습니다", () => {
    expect(isValidRef(ref())).toBe(true);
    expect(isValidRef({ ...ref(), version: "" })).toBe(false);
    expect(isValidRef(null)).toBe(false);
    expect(isValidRef({ ...ref(), uid: "u".repeat(65) })).toBe(false);
  });

  it("문자열이 아닌 값은 문자열로 바꿔서 통과시키지 않습니다", () => {
    /* String(42)는 "42"라 예전 판정을 통과했습니다 — 저장소가 오염된 채로 정상처럼
       보이고, 그 값이 인코딩되어 서버에는 400으로 도착했습니다. */
    expect(isValidRef({ ...ref(), uid: 42 })).toBe(false);
    expect(isValidRef({ ...ref(), name: ["payments-api"] })).toBe(false);
    expect(isValidRef({ ...ref(), group: { toString: () => "core" } })).toBe(false);
    expect(isValidRef({ ...ref(), namespace: null })).toBe(false);
    expect(isValidRef({ ...ref(), version: true })).toBe(false);
    /* 배열은 object지만 참조가 아닙니다. */
    expect(isValidRef(["core", "v1", "services"])).toBe(false);
  });

  it("유니코드·공백이 든 GVR 세그먼트를 막습니다 — allowlist에 그런 GVR은 없습니다", () => {
    expect(isValidRef({ ...ref(), group: "코어" })).toBe(false);
    expect(isValidRef({ ...ref(), resource: "service s" })).toBe(false);
    expect(isValidRef({ ...ref(), version: "v1\u001f" })).toBe(false);
    expect(isValidRef({ ...ref(), namespace: "페이먼츠" })).toBe(false);
    expect(isValidRef({ ...ref(), uid: "uid\u0000" })).toBe(false);
  });

  it("오염된 항목이 섞여도 저장소 읽기는 나머지를 지킵니다", () => {
    window.localStorage.setItem(
      KEY,
      JSON.stringify({ v: 1, items: [{ ...ref(), uid: 7 }, ref({ uid: "uid-2", name: "indexer" })] }),
    );
    expect(loadRecent(CLUSTER).map((r) => r.uid)).toEqual(["uid-2"]);
  });
});

describe("클러스터 격리", () => {
  it("읽기는 활성 클러스터의 참조만 돌려줍니다", () => {
    rememberRecent(ref({ clusterId: "stage-tokyo", uid: "uid-t", name: "tokyo-api" }));
    rememberRecent(ref({ uid: "uid-s", name: "seoul-api" }));
    expect(loadRecent(CLUSTER).map((r) => r.uid)).toEqual(["uid-s"]);
    expect(loadRecent("stage-tokyo").map((r) => r.uid)).toEqual(["uid-t"]);
    /* 저장은 하나의 목록입니다 — 클러스터별로 20개씩 부풀지 않습니다. */
    expect(loadAllRecent()).toHaveLength(2);
  });

  it("같은 UID라도 클러스터가 다르면 다른 객체입니다", () => {
    rememberRecent(ref({ uid: "uid-1" }));
    rememberRecent(ref({ clusterId: "stage-tokyo", uid: "uid-1" }));
    expect(loadAllRecent()).toHaveLength(2);
    expect(loadRecent(CLUSTER)).toHaveLength(1);
    expect(loadRecent("stage-tokyo")).toHaveLength(1);
  });

  it("클러스터 없는 항목은 유효하지 않습니다", () => {
    const noCluster: Record<string, unknown> = { ...ref() };
    delete noCluster.clusterId;
    expect(isValidRef(noCluster)).toBe(false);
    seedStorage([noCluster, ref({ uid: "uid-2", name: "indexer" })]);
    expect(loadAllRecent().map((r) => r.uid)).toEqual(["uid-2"]);
  });

  it("클러스터를 지정하지 않으면 아무것도 보내지 않습니다", () => {
    rememberRecent(ref());
    expect(loadRecent("")).toEqual([]);
  });
});

describe("GVR 세그먼트 parity (ValidateGVRSegments)", () => {
  it("group은 core이거나 DNS1123 subdomain입니다", () => {
    expect(isValidRef(ref({ group: "core" }))).toBe(true);
    expect(isValidRef(ref({ group: "apps" }))).toBe(true);
    expect(isValidRef(ref({ group: "storage.k8s.io" }))).toBe(true);
    expect(isValidRef(ref({ group: "Apps" }))).toBe(false); // 대문자
    expect(isValidRef(ref({ group: "apps." }))).toBe(false); // 빈 label
    expect(isValidRef(ref({ group: ".apps" }))).toBe(false);
    expect(isValidRef(ref({ group: "-apps" }))).toBe(false);
    expect(isValidRef(ref({ group: `${"a".repeat(64)}.io` }))).toBe(false); // label 63자 초과
    expect(isValidRef(ref({ group: "a".repeat(254) }))).toBe(false);
  });

  it("version·resource는 DNS1035 label입니다 — 글자로 시작해야 합니다", () => {
    expect(isValidRef(ref({ version: "v1beta1" }))).toBe(true);
    expect(isValidRef(ref({ version: "1v" }))).toBe(false); // 숫자로 시작
    expect(isValidRef(ref({ version: "v1-" }))).toBe(false);
    expect(isValidRef(ref({ version: "V1" }))).toBe(false);
    expect(isValidRef(ref({ resource: "custom.resources" }))).toBe(false); // label에 점 없음
    expect(isValidRef(ref({ resource: "r".repeat(64) }))).toBe(false);
  });

  it("namespace·name·UID는 safeCursorSegment와 길이 상한을 지킵니다", () => {
    expect(isValidRef(ref({ namespace: "" }))).toBe(true); // 클러스터 범위
    expect(isValidRef(ref({ namespace: "n".repeat(64) }))).toBe(false);
    expect(isValidRef(ref({ name: "n".repeat(253) }))).toBe(true);
    expect(isValidRef(ref({ name: "n".repeat(254) }))).toBe(false);
    expect(isValidRef(ref({ uid: "u".repeat(64) }))).toBe(true);
    expect(isValidRef(ref({ name: "has/slash" }))).toBe(false);
    expect(isValidRef(ref({ uid: "has space" }))).toBe(false);
  });
});

describe("요청 나누기", () => {
  const PATH = "/api/v1/clusters/prod-seoul/resources/recent";
  const target = (over: Partial<RecentRequestTarget> = {}): RecentRequestTarget => ({ pathname: PATH, ...over });

  /** 브라우저가 실제로 만들 request target. 프로덕션과 같은 함수를 씁니다. */
  const requestTarget = (chunk: string[], t: RecentRequestTarget) =>
    recentRequestTarget(t.pathname, chunk, t.extraParams ?? []);

  it("참조 20개까지는 한 번에 보냅니다", () => {
    const refs = Array.from({ length: MAX_RECENT }, (_, i) => ref({ uid: `uid-${i}`, name: `svc-${i}` }));
    const chunks = chunkRecentRefs(refs, target());
    expect(chunks).toHaveLength(1);
    expect(chunks[0]).toHaveLength(MAX_RECENT);
  });

  it("6KiB를 넘기지 않고 나눕니다 — 재는 것은 경로까지 포함한 request target입니다", () => {
    /* 긴 이름으로 참조 하나를 크게 만들어 길이가 먼저 걸리게 합니다. */
    const long = "n".repeat(200);
    const refs = Array.from({ length: 20 }, (_, i) => ref({ uid: `uid-${i}`, name: `${long}-${i}` }));
    const chunks = chunkRecentRefs(refs, target());
    expect(chunks.length).toBeGreaterThan(1);
    for (const chunk of chunks) {
      expect(utf8Bytes(requestTarget(chunk, target()))).toBeLessThanOrEqual(MAX_RECENT_REQUEST_TARGET_BYTES);
      expect(chunk.length).toBeLessThanOrEqual(MAX_RECENT);
    }
  });

  it("경로와 시나리오 파라미터도 예산에 넣습니다 — ref 조각만 재지 않습니다", () => {
    const long = "n".repeat(200);
    const refs = Array.from({ length: 20 }, (_, i) => ref({ uid: `uid-${i}`, name: `${long}-${i}` }));
    /* 경로가 길고 파라미터가 붙는 배포에서는 같은 참조라도 더 잘게 나뉘어야 합니다. */
    const wide = target({
      pathname: `/api/v1/clusters/${"c".repeat(300)}/resources/recent`,
      extraParams: [["scenario", "degraded"]],
    });
    const narrow = chunkRecentRefs(refs, wide);
    expect(narrow.flat()).toEqual(chunkRecentRefs(refs, target()).flat());
    for (const chunk of narrow) {
      expect(utf8Bytes(requestTarget(chunk, wide))).toBeLessThanOrEqual(MAX_RECENT_REQUEST_TARGET_BYTES);
    }
    const widest = Math.max(...narrow.map((c) => c.length));
    const base = Math.max(...chunkRecentRefs(refs, target()).map((c) => c.length));
    expect(widest).toBeLessThanOrEqual(base);
  });

  it("나눠도 원래 순서가 그대로 이어집니다", () => {
    const long = "n".repeat(200);
    const refs = Array.from({ length: 20 }, (_, i) => ref({ uid: `uid-${i}`, name: `${long}-${i}` }));
    const flat = chunkRecentRefs(refs, target()).flat();
    expect(flat).toEqual(refs.map(encodeRecentRef));
  });

  it("참조 하나가 서버 상한을 넘으면 그 항목만 빠집니다", () => {
    const huge = ref({ uid: "uid-huge", name: "n".repeat(1200) });
    const chunks = chunkRecentRefs([ref({ uid: "uid-ok" }), huge], target());
    expect(chunks.flat()).toHaveLength(1);
    expect(decode(chunks[0][0])[4]).toBe("uid-ok");
  });

  it("혼자서도 상한에 못 들어가면 자르지 않고 실패합니다", () => {
    /* 경로가 예산을 거의 다 먹는 배포. 잘라 보내면 서버가 다른 참조로 읽습니다. */
    const impossible = target({ pathname: `/api/v1/clusters/${"c".repeat(6100)}/resources/recent` });
    expect(() => chunkRecentRefs([ref()], impossible)).toThrow(RecentRequestTooLargeError);
  });

  it("0건 probe조차 들어가지 않으면 참조가 없어도 실패합니다", () => {
    /* 참조가 하나도 없어도 경로만으로 상한을 넘을 수 있습니다. 그때는 probe도
       보내면 안 되므로 호출부가 요청을 하나도 만들지 못하게 여기서 막습니다. */
    const impossible = target({ pathname: `/api/v1/clusters/${"c".repeat(6200)}/resources/recent` });
    expect(() => chunkRecentRefs([], impossible)).toThrow(RecentRequestTooLargeError);
  });

  it("`~`가 든 파라미터는 URLSearchParams가 늘리는 만큼 그대로 셉니다", () => {
    /* encodeURIComponent는 `~`를 그대로 두지만 URLSearchParams는 `%7E`로 3배로
       늘립니다. 산술 근사로는 이 차이만큼 작게 재고 브라우저는 크게 보냅니다. */
    const tildes = "~".repeat(600);
    const t = target({ extraParams: [["scenario", tildes]] });
    expect(utf8Bytes(requestTarget([], t))).toBeGreaterThan(PATH.length + 1 + "scenario=".length + tildes.length);
    expect(requestTarget([], t)).toContain("%7E");

    const long = "n".repeat(200);
    const refs = Array.from({ length: 20 }, (_, i) => ref({ uid: `uid-${i}`, name: `${long}-${i}` }));
    for (const chunk of chunkRecentRefs(refs, t)) {
      expect(utf8Bytes(requestTarget(chunk, t))).toBeLessThanOrEqual(MAX_RECENT_REQUEST_TARGET_BYTES);
    }
    /* 0건 probe도 같은 자로 잽니다. */
    expect(utf8Bytes(requestTarget([], t))).toBeLessThanOrEqual(MAX_RECENT_REQUEST_TARGET_BYTES);
  });

  it("`~`가 예산을 다 먹으면 0건 probe도 포기합니다", () => {
    const t = target({ extraParams: [["scenario", "~".repeat(2100)]] });
    expect(() => chunkRecentRefs([], t)).toThrow(RecentRequestTooLargeError);
  });

  it("오염된 참조는 인코딩 전에 빠지고 정상 참조는 그대로 나갑니다", () => {
    /* 하나가 서버에서 400을 받으면 같은 배치의 정상 참조까지 함께 죽습니다. */
    const poisoned = [
      ref({ uid: "uid-ok" }),
      { ...ref({ uid: "uid-bad-group" }), group: "Bad_Group" } as RecentRef,
      { ...ref({ uid: "uid-bad-version" }), version: "1v" } as RecentRef,
      { ...ref({ uid: "uid-bad-name" }), name: "has space" } as RecentRef,
      ref({ uid: "uid-ok-2", name: "indexer" }),
    ];
    const flat = chunkRecentRefs(poisoned, target()).flat();
    expect(flat.map((e) => decode(e)[4])).toEqual(["uid-ok", "uid-ok-2"]);
  });

  it("빈 목록은 요청을 만들지 않습니다 — 0건 probe는 호출부가 담당합니다", () => {
    expect(chunkRecentRefs([], target())).toEqual([]);
  });
});
