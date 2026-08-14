package contract

import (
	"testing"
)

// TestWorseOf — 집계 롤업의 심각도 비교입니다. 더 나쁜 쪽이 이깁니다.
func TestWorseOf(t *testing.T) {
	for _, tc := range []struct{ a, b, want Severity }{
		{SeverityHealthy, SeverityCritical, SeverityCritical},
		{SeverityCritical, SeverityHealthy, SeverityCritical},
		{SeverityWarning, SeverityDegraded, SeverityDegraded},
		{SeverityUnknown, SeverityHealthy, SeverityUnknown},
		{SeverityProgressing, SeverityProgressing, SeverityProgressing},
	} {
		if got := WorseOf(tc.a, tc.b); got != tc.want {
			t.Fatalf("WorseOf(%s,%s)=%s want %s", tc.a, tc.b, got, tc.want)
		}
	}
}

// TestSectionConstructors — 네 상태의 봉투 규칙입니다. (ADR 0002)
//
//	OK(값)        → ok, 값 포함. 단 빈 슬라이스면 자동으로 empty
//	Empty         → empty, 값 없음
//	Forbidden     → forbidden, **데이터를 절대 담지 않음**
//	Degraded      → degraded, stale 값이 있으면 함께
func TestSectionConstructors(t *testing.T) {
	ok := OK(PodHealth{Total: 3})
	if ok.Status != StatusOK || ok.Data == nil || ok.Data.Total != 3 || ok.ObservedAt == "" {
		t.Fatalf("OK 섹션이 틀렸습니다: %+v", ok)
	}

	// 빈 슬라이스는 "결과 0건"입니다 — ok로 두면 화면이 빈 표를 정상처럼 그립니다.
	emptyList := OK([]LogLine{})
	if emptyList.Status != StatusEmpty {
		t.Fatalf("빈 슬라이스는 empty여야 합니다: %s", emptyList.Status)
	}
	filled := OK([]LogLine{{ID: "x"}})
	if filled.Status != StatusOK {
		t.Fatalf("값 있는 슬라이스는 ok여야 합니다: %s", filled.Status)
	}

	e := Empty[PodHealth]()
	if e.Status != StatusEmpty || e.Data != nil {
		t.Fatalf("Empty 섹션이 틀렸습니다: %+v", e)
	}

	f := Forbidden[PodHealth]("권한이 없습니다")
	if f.Status != StatusForbidden || f.Data != nil || f.Reason == "" {
		t.Fatalf("Forbidden 섹션이 틀렸습니다: %+v", f)
	}

	stale := PodHealth{Total: 1}
	d := Degraded(SourceGreptimeDB, "응답 없음", &stale)
	if d.Status != StatusDegraded || d.Source != SourceGreptimeDB || d.Data == nil {
		t.Fatalf("Degraded 섹션이 틀렸습니다: %+v", d)
	}
	dNoStale := Degraded[PodHealth](SourceQuickwit, "응답 없음", nil)
	if dNoStale.Data != nil {
		t.Fatal("stale 없는 degraded에 값이 있습니다")
	}
}

// TestIsEmptyCoversEveryListType — isEmpty의 타입 스위치가 계약의 목록 타입을
// 전부 알고 있는지 확인합니다. 여기 빠진 타입은 빈 목록이 ok로 내려갑니다.
func TestIsEmptyCoversEveryListType(t *testing.T) {
	for name, s := range map[string]SectionStatus{
		"UnhealthyEntity":    OK([]UnhealthyEntity{}).Status,
		"ClusterEvent":       OK([]ClusterEvent{}).Status,
		"NamespaceSummary":   OK([]NamespaceSummary{}).Status,
		"WorkloadSummary":    OK([]WorkloadSummary{}).Status,
		"PodSummary":         OK([]PodSummary{}).Status,
		"ContainerStatus":    OK([]ContainerStatus{}).Status,
		"OwnerRef":           OK([]OwnerRef{}).Status,
		"TrendPanel":         OK([]TrendPanel{}).Status,
		"TrendSeries":        OK([]TrendSeries{}).Status,
		"LogLine":            OK([]LogLine{}).Status,
		"AlertInstance":      OK([]AlertInstance{}).Status,
		"LogHistogramBucket": OK([]LogHistogramBucket{}).Status,
	} {
		if s != StatusEmpty {
			t.Fatalf("%s: 빈 목록이 empty가 아닙니다", name)
		}
	}
	// 구조체(목록 아님)는 값이 0이어도 ok입니다 — "0개"가 사실인 값이기 때문입니다.
	if OK(PodHealth{}).Status != StatusOK {
		t.Fatal("구조체 0값이 empty로 접혔습니다")
	}
}

// TestResourceUsageNormalizeAndAdd — 비율은 서버가 한 번만 계산하고,
// limit 합계는 한쪽이라도 없으면 없음입니다.
func TestResourceUsageNormalizeAndAdd(t *testing.T) {
	limit := 1000
	u := ResourceUsage{CPUMilli: 500, CPURequestMilli: 250, MemoryMib: 256, MemoryRequestMib: 512, CPULimitMilli: &limit}
	u.Normalize()
	if u.CPUVsRequest != 2.0 || u.MemoryVsRequest != 0.5 {
		t.Fatalf("request 대비 비율: %+v", u)
	}
	if u.CPUVsLimit == nil || *u.CPUVsLimit != 0.5 {
		t.Fatalf("limit 대비 비율: %+v", u.CPUVsLimit)
	}
	if u.MemoryVsLimit != nil {
		t.Fatal("limit 없는 비율은 nil이어야 합니다")
	}

	// request가 0이면 비율도 0입니다 — 0으로 나누지 않습니다.
	zero := ResourceUsage{CPUMilli: 100}
	zero.Normalize()
	if zero.CPUVsRequest != 0 {
		t.Fatal("request 0의 비율은 0이어야 합니다")
	}

	// limit 합계: 한쪽이라도 limit이 없으면 합계도 없습니다.
	a := ResourceUsage{CPUMilli: 1, CPULimitMilli: &limit}
	a.Add(ResourceUsage{CPUMilli: 2})
	if a.CPUMilli != 3 {
		t.Fatalf("합계: %d", a.CPUMilli)
	}
	if a.CPULimitMilli != nil {
		t.Fatal("limit 없는 Pod가 섞이면 limit 합계는 없어야 합니다")
	}
}
