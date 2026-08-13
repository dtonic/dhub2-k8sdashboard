/**
 * 로그 mock (이슈 #16).
 * --------------------------------------------------------------------------
 * 실제 Quickwit이 붙기 전까지 Logs Explorer를 검증하기 위한 고정 코퍼스입니다.
 *
 * 재현해 둔 것들
 * - 최근 구간이 더 촘촘합니다. 장애가 나면 로그가 몰리는 실제 양상에 맞춥니다.
 * - **마스킹은 서버에서 합니다.** 원문은 응답에 들어가지 않고, 가려진 위치와 종류만
 *   `masked` 스팬으로 내려옵니다. UI는 표시만 하고 복원하지 못합니다. (README §10)
 * - 커서는 (timestamp, id) 복합키입니다. offset을 쓰면 새 로그가 들어올 때마다
 *   페이지 경계가 밀려 중복·누락이 생깁니다.
 */
import type { LogLevel, LogLine, MaskedSpan, WorkloadKind } from "@k8s-dashboard/contracts";
import { NOW_MS } from "./data";
import { primaryPod } from "./drilldown";

function hash(seed: string): number {
  let h = 2166136261;
  for (let i = 0; i < seed.length; i++) {
    h ^= seed.charCodeAt(i);
    h = Math.imul(h, 16777619);
  }
  return ((h >>> 0) % 100000) / 100000;
}
const pick = <T,>(seed: string, arr: readonly T[]): T => arr[Math.floor(hash(seed) * arr.length) % arr.length]!;
const hex = (seed: string, len: number) =>
  Array.from({ length: len }, (_, i) => "0123456789abcdef"[Math.floor(hash(seed + i) * 16)]).join("");

/**
 * 로그 소스.
 * --------------------------------------------------------------------------
 * Pod 이름과 UID는 drilldown mock의 **실제 Pod**에서 가져옵니다. 여기서 지어내면
 * 로그 라인 → Pod 상세 deep link가 404가 납니다. (mock끼리도 계약을 지켜야 합니다.)
 */
const SOURCE_DEF: Array<{ ns: string; workload: string; containers: string[]; errorRate: number; weight: number }> = [
  { ns: "payments", workload: "payments-api", containers: ["app", "otel-agent"], errorRate: 0.12, weight: 40 },
  { ns: "payments", workload: "ledger-worker", containers: ["app", "otel-agent"], errorRate: 0.34, weight: 22 },
  { ns: "payments", workload: "batch-sync", containers: ["app"], errorRate: 0.72, weight: 10 },
  { ns: "payments", workload: "auth-svc", containers: ["app"], errorRate: 0.02, weight: 18 },
  { ns: "search", workload: "indexer", containers: ["app"], errorRate: 0.4, weight: 10 },
];

type Source = {
  namespace: string;
  workloadName: string;
  workloadKind: WorkloadKind;
  podName: string;
  podUid: string;
  containers: string[];
  errorRate: number;
  weight: number;
};

let sourceCache: Source[] | null = null;

function sources(): Source[] {
  if (sourceCache) return sourceCache;
  sourceCache = SOURCE_DEF.map((d) => {
    const { workload, pod } = primaryPod(d.ns, d.workload);
    return {
      namespace: d.ns,
      workloadName: workload.name,
      workloadKind: workload.kind,
      podName: pod.name,
      podUid: pod.uid,
      containers: d.containers,
      errorRate: d.errorRate,
      weight: d.weight,
    };
  });
  return sourceCache;
}

const INFO_MSG = [
  "GET /api/v1/payments/{id} 200 in {ms}ms",
  "POST /api/v1/payments 201 in {ms}ms",
  "consumed batch offset={n} size=500",
  "readiness probe ok",
  "cache hit ratio 0.94 over last 1m",
  "settle batch committed entries={n}",
];
const WARN_MSG = [
  "upstream auth-svc p99={ms}ms exceeds slo=500ms",
  "connection pool near limit: {n}/50 (postgres-primary-0)",
  "kafka produce lag={n} partition=3",
  "readiness probe failed: /healthz returned 500 (attempt {n})",
  "retrying ledger settle attempt={n} backoff=400ms",
];
const ERROR_MSG = [
  "POST /api/v1/payments 503 upstream ledger-worker timeout after 2000ms",
  "postgres: canceling statement due to statement timeout (30s)",
  "dial tcp postgres-primary-0:5432: connect: connection refused",
  "panic: sql: database is closed",
  "settle batch failed, {n} entries requeued",
  "failed to renew lease: context deadline exceeded",
];
const DEBUG_MSG = [
  "resolved upstream endpoints={n}",
  "span exported trace_id={trace}",
  "config reload skipped, checksum unchanged",
];

/**
 * 민감정보가 섞인 로그. **원문을 만들지 않고** 처음부터 마스킹된 문자열과
 * 스팬을 함께 만듭니다. 원문이 코드 어디에도 존재하지 않아야 실수로 새지 않습니다.
 */
const SENSITIVE: Array<{ build: (seed: string) => { text: string; masked: MaskedSpan[] } }> = [
  {
    build: () => {
      const prefix = "auth failed for user=";
      const email = "•".repeat(18);
      const mid = " token=";
      const token = "•".repeat(24);
      return {
        text: `${prefix}${email}${mid}${token} (401)`,
        masked: [
          { start: prefix.length, length: email.length, kind: "email" },
          { start: prefix.length + email.length + mid.length, length: token.length, kind: "token" },
        ],
      };
    },
  },
  {
    build: () => {
      const prefix = "db connect failed dsn=postgres://svc:";
      const pw = "•".repeat(12);
      return {
        text: `${prefix}${pw}@postgres-primary-0:5432/ledger`,
        masked: [{ start: prefix.length, length: pw.length, kind: "password" }],
      };
    },
  },
  {
    build: () => {
      const prefix = "webhook signature mismatch secret=";
      const sec = "•".repeat(16);
      return {
        text: `${prefix}${sec} source=partner-a`,
        masked: [{ start: prefix.length, length: sec.length, kind: "secret" }],
      };
    },
  },
  {
    build: () => {
      const prefix = "refund rejected card=";
      const card = "•".repeat(12);
      return {
        text: `${prefix}${card} reason=insufficient_funds`,
        masked: [{ start: prefix.length, length: card.length, kind: "card" }],
      };
    },
  },
];

function fill(tpl: string, seed: string) {
  return tpl
    .replace("{ms}", String(20 + Math.floor(hash(seed + "ms") * 1800)))
    .replace("{n}", String(1 + Math.floor(hash(seed + "n") * 9000)))
    .replace("{trace}", hex(seed + "tr", 32));
}

const TOTAL = 12_000;
const HOUR_MS = 3600 * 1000;
const DAY_MS = 86400 * 1000;
const SPAN_MS = 30 * DAY_MS;

let corpus: LogLine[] | null = null;

/** 전체 코퍼스. 최근일수록 촘촘하도록 시간 축을 제곱으로 압축합니다. */
export function logCorpus(): LogLine[] {
  if (corpus) return corpus;
  const lines: LogLine[] = [];
  for (let i = 0; i < TOTAL; i++) {
    const seed = `log/${i}`;
    /* 밴드를 나눠 배치합니다. 단일 지수 분포를 쓰면 1시간 뷰에서는 마지막 몇 분에만
       로그가 몰려 히스토그램이 쓸모없어집니다. 어느 배율로 봐도 밀도가 있어야 합니다.
         30% → 최근 1시간, 30% → 최근 1일, 40% → 최근 30일 */
    const band = i / TOTAL;
    const span = band < 0.3 ? HOUR_MS : band < 0.6 ? DAY_MS : SPAN_MS;
    const t = Math.round(NOW_MS - hash(seed + "t") * span - hash(seed + "j") * 900);

    const srcList = sources();
    const roll = hash(seed + "s") * srcList.reduce((s, x) => s + x.weight, 0);
    let acc = 0;
    const src = srcList.find((x) => (acc += x.weight) >= roll) ?? srcList[0]!;

    const r = hash(seed + "lv");
    const level: LogLevel =
      r < src.errorRate ? "ERROR" : r < src.errorRate + 0.18 ? "WARN" : r < 0.94 ? "INFO" : "DEBUG";

    const container = pick(seed + "c", src.containers);
    const sensitive = level === "ERROR" && hash(seed + "sen") > 0.82;
    const built = sensitive
      ? pick(seed + "sm", SENSITIVE).build(seed)
      : {
          text: fill(
            pick(
              seed + "m",
              level === "ERROR" ? ERROR_MSG : level === "WARN" ? WARN_MSG : level === "DEBUG" ? DEBUG_MSG : INFO_MSG,
            ),
            seed,
          ),
          masked: [] as MaskedSpan[],
        };

    lines.push({
      /* id는 코퍼스 인덱스를 포함해 **충돌이 불가능**해야 합니다.
         해시 접미사만 쓰면 같은 밀리초에 동일 id가 생겨 커서 페이징에서 중복이 납니다. */
      id: `${t}-${i.toString(36).padStart(4, "0")}`,
      t,
      level,
      message: built.text,
      masked: built.masked,
      namespace: src.namespace,
      podName: src.podName,
      podUid: src.podUid,
      containerName: container,
      workloadKind: src.workloadKind,
      workloadName: src.workloadName,
      nodeName: `ip-10-0-${1 + Math.floor(hash(seed + "nd") * 60)}-${1 + Math.floor(hash(seed + "nd2") * 250)}`,
      traceId: hash(seed + "hastrace") > 0.35 ? hex(seed + "trace", 32) : undefined,
      spanId: hash(seed + "hastrace") > 0.35 ? hex(seed + "span", 16) : undefined,
      attributes:
        level === "ERROR"
          ? { "service.version": `2.${1 + Math.floor(hash(seed + "v") * 20)}.0`, "http.status_code": "503" }
          : undefined,
    });
  }
  /* 최신 우선. 커서는 이 정렬을 전제로 동작합니다. */
  lines.sort((a, b) => b.t - a.t || (a.id < b.id ? 1 : -1));
  corpus = lines;
  return lines;
}

/* ── 커서 ────────────────────────────────────────────────────────────────── */

export const encodeCursor = (line: LogLine) => btoa(`${line.t}|${line.id}`);

export function decodeCursor(cursor: string): { t: number; id: string } | null {
  try {
    const [t, id] = atob(cursor).split("|");
    if (!t || !id) return null;
    return { t: Number(t), id };
  } catch {
    return null;
  }
}

/** 커서보다 "뒤"(더 과거)인 첫 인덱스. 경계에서 중복도 누락도 없어야 합니다. */
export function afterCursor(lines: LogLine[], cursor: string): number {
  const c = decodeCursor(cursor);
  if (!c) return 0;
  const idx = lines.findIndex((l) => l.t === c.t && l.id === c.id);
  if (idx >= 0) return idx + 1;
  /* 커서가 가리키던 줄이 사라졌으면(보존기간 만료 등) 시간 기준으로 이어붙입니다. */
  const byTime = lines.findIndex((l) => l.t < c.t);
  return byTime >= 0 ? byTime : lines.length;
}
