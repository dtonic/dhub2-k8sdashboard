/** 표시용 포맷터. 숫자는 항상 tabular-nums 클래스와 함께 씁니다. */

export const num = (n: number) => Math.round(n).toLocaleString("ko-KR");

export const pct = (n: number, digits = 0) => `${n.toFixed(digits)}%`;

export function bytesPerSec(mib: number) {
  if (mib >= 1024) return `${(mib / 1024).toFixed(1)} GiB/s`;
  return `${Math.round(mib)} MiB/s`;
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

export function unitFormat(unit: "percent" | "bytes" | "bytes_per_sec" | "count", v: number) {
  switch (unit) {
    case "percent":
      return pct(v, 1);
    case "bytes_per_sec":
      return bytesPerSec(v);
    case "bytes":
      return `${v.toFixed(1)} GiB`;
    default:
      return num(v);
  }
}

/** 축 눈금에서 뺀 단위는 패널 부제에 한 번만 적습니다. */
export function unitSuffix(unit: "percent" | "bytes" | "bytes_per_sec" | "count") {
  switch (unit) {
    case "percent":
      return "%";
    case "bytes_per_sec":
      return "MiB/s";
    case "bytes":
      return "GiB";
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
