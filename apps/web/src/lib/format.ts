/** 표시용 포맷터. 숫자는 항상 tabular-nums 클래스와 함께 씁니다. */

export const num = (n: number) => Math.round(n).toLocaleString("ko-KR");

export const pct = (n: number, digits = 0) => `${n.toFixed(digits)}%`;

/** 서버 시계열의 단위 키 — Query Catalog의 unit 값과 1:1입니다. (#31) */
export type MetricUnit = "percent" | "bytes" | "bytes_per_sec" | "count" | "cores" | "millicores" | "mebibytes";

const BYTE_UNITS = ["B", "KiB", "MiB", "GiB", "TiB", "PiB"] as const;

/** 원시 byte 값을 자동 단위로 환산합니다. 43,4억 → "40.4 GiB". */
export function humanBytes(v: number, fracDigits = 1) {
  let x = Math.abs(v);
  let i = 0;
  while (x >= 1024 && i < BYTE_UNITS.length - 1) {
    x /= 1024;
    i++;
  }
  const digits = i === 0 || x >= 100 ? 0 : fracDigits;
  return `${(v < 0 ? -x : x).toFixed(digits)} ${BYTE_UNITS[i]}`;
}

/** CPU cores 값. 1 미만은 millicores로 내려 표기합니다. */
export function humanCores(v: number) {
  if (Math.abs(v) < 1) return `${Math.round(v * 1000)}m`;
  return `${v.toFixed(v >= 100 ? 0 : v >= 10 ? 1 : 2)} cores`;
}

/** 상태가 유지된 시간. "41분", "2시간 3분" */
export function duration(seconds: number) {
  if (seconds < 60) return `${Math.round(seconds)}초`;
  const m = Math.floor(seconds / 60);
  if (m < 60) return `${m}분`;
  const h = Math.floor(m / 60);
  const rest = m % 60;
  if (h < 24) return rest ? `${h}시간 ${rest}분` : `${h}시간`;
  return `${Math.floor(h / 24)}일 ${h % 24}시간`;
}

/** 기준 시각 대비 상대 시간. 서버가 준 generatedAt을 기준으로 계산합니다. */
export function since(iso: string, referenceIso: string) {
  const diff = (Date.parse(referenceIso) - Date.parse(iso)) / 1000;
  if (diff < 5) return "방금";
  return `${duration(diff)} 전`;
}

const KST = "Asia/Seoul";

export function clock(ms: number) {
  return new Intl.DateTimeFormat("ko-KR", {
    hour: "2-digit",
    minute: "2-digit",
    hour12: false,
    timeZone: KST,
  }).format(new Date(ms));
}

export function dayClock(ms: number) {
  return new Intl.DateTimeFormat("ko-KR", {
    month: "2-digit",
    day: "2-digit",
    hour: "2-digit",
    minute: "2-digit",
    hour12: false,
    timeZone: KST,
  }).format(new Date(ms));
}

export function axisTime(ms: number, stepSeconds: number) {
  return stepSeconds >= 3600 ? dayClock(ms).replace(/\s\d{2}:\d{2}$/, "") : clock(ms);
}

/** 값 표시는 항상 원시 단위(bytes·cores 등)에서 자동 환산합니다. (#31)
    실데이터는 44,600,000,000 같은 원시 byte로 오므로 고정 접미사는 거짓말이 됩니다. */
export function unitFormat(unit: MetricUnit, v: number) {
  switch (unit) {
    case "percent":
      return pct(v, 1);
    case "bytes_per_sec":
      return `${humanBytes(v)}/s`;
    case "bytes":
      return humanBytes(v);
    case "mebibytes":
      return humanBytes(v * 1024 * 1024);
    case "cores":
      return humanCores(v);
    case "millicores":
      return `${num(v)}m`;
    default:
      return num(v);
  }
}

/** 패널 부제에 적는 기준 단위. 값 자체는 unitFormat이 자동 환산해 단위를 답니다. */
export function unitSuffix(unit: MetricUnit) {
  switch (unit) {
    case "percent":
      return "%";
    case "bytes_per_sec":
      return "bytes/s (자동 환산)";
    case "bytes":
      return "bytes (자동 환산)";
    case "mebibytes":
      return "MiB (자동 환산)";
    case "cores":
      return "cores";
    case "millicores":
      return "millicores";
    default:
      return "건";
  }
}

/** 좁은 라벨용 축약 수치. 1,234,000 → 1.2M */
export function compact(n: number) {
  if (n >= 1_000_000) return `${(n / 1_000_000).toFixed(n >= 10_000_000 ? 0 : 1)}M`;
  if (n >= 1_000) return `${(n / 1_000).toFixed(n >= 10_000 ? 0 : 1)}K`;
  return String(Math.round(n));
}
