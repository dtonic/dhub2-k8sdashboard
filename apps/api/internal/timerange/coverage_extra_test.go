package timerange_test

import (
	"strconv"
	"testing"
	"time"

	"github.com/xenx96/k8s-dashboard/apps/api/internal/contract"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/timerange"
)

var nowExtra = time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC)

// TestContractSerialization — 응답에 실리는 형태입니다. UTC RFC3339와 초 단위 Step.
func TestContractSerialization(t *testing.T) {
	w, err := timerange.Parse("1d", "", "", nowExtra)
	if err != nil {
		t.Fatal(err)
	}
	c := w.Contract()
	if c.Key != contract.Range1d || c.StepSeconds != 300 {
		t.Fatalf("계약 변환: %+v", c)
	}
	if c.From != nowExtra.Add(-24*time.Hour).Format(time.RFC3339) || c.To != nowExtra.Format(time.RFC3339) {
		t.Fatalf("시각 표기: %+v", c)
	}
}

// TestBucketsEdgeCases — Step 0·음수 범위의 방어입니다.
func TestBucketsEdgeCases(t *testing.T) {
	if (timerange.Window{}).Buckets() != 0 {
		t.Fatal("Step 없는 Buckets는 0이어야 합니다")
	}
	w := timerange.Window{From: nowExtra, To: nowExtra.Add(time.Second), Step: time.Hour}
	if w.Buckets() != 1 {
		t.Fatal("Step보다 짧은 범위는 1버킷입니다")
	}
	w = timerange.Window{From: nowExtra, To: nowExtra.Add(time.Hour), Step: time.Minute}
	if w.Buckets() != 60 {
		t.Fatalf("1시간/1분: %d", w.Buckets())
	}
}

// TestCustomStepTiers — Custom 범위의 Step은 가장 가까운 프리셋을 따릅니다.
// 포인트 수가 예측 가능해야 차트와 백엔드 부하도 예측 가능합니다.
func TestCustomStepTiers(t *testing.T) {
	for span, wantStep := range map[time.Duration]time.Duration{
		30 * time.Minute:    time.Minute,
		time.Hour:           time.Minute,
		6 * time.Hour:       5 * time.Minute,
		24 * time.Hour:      5 * time.Minute,
		3 * 24 * time.Hour:  15 * time.Minute,
		7 * 24 * time.Hour:  15 * time.Minute,
		20 * 24 * time.Hour: time.Hour,
		30 * 24 * time.Hour: time.Hour,
	} {
		from := nowExtra.Add(-span)
		w, err := timerange.Parse("",
			strconv.FormatInt(from.UnixMilli(), 10),
			strconv.FormatInt(nowExtra.UnixMilli(), 10), nowExtra)
		if err != nil {
			t.Fatalf("%v: %v", span, err)
		}
		if w.Step != wantStep {
			t.Fatalf("%v 범위의 Step: got %v want %v", span, w.Step, wantStep)
		}
		if w.Key != contract.RangeCustom {
			t.Fatalf("from/to는 Custom이어야 합니다: %s", w.Key)
		}
	}
}

// TestParseRejections — 성립하지 않는 범위는 자르지 않고 거절합니다.
// 조용히 자르면 사용자가 본 범위와 응답 범위가 달라집니다.
func TestParseRejections(t *testing.T) {
	valid := nowExtra.Format(time.RFC3339)
	for name, in := range map[string][2]string{
		"from 형식 오류":  {"not-a-time", valid},
		"to 형식 오류":    {valid, "not-a-time"},
		"to가 from 이전": {valid, nowExtra.Add(-time.Hour).Format(time.RFC3339)},
		"같은 시각":       {valid, valid},
		"30일 초과":      {nowExtra.Add(-31 * 24 * time.Hour).Format(time.RFC3339), valid},
		"from만 있음":    {valid, ""},
	} {
		if _, err := timerange.Parse("", in[0], in[1], nowExtra); err == nil {
			t.Fatalf("%s: 거절되지 않았습니다", name)
		}
	}
	if _, err := timerange.Parse("2h", "", "", nowExtra); err == nil {
		t.Fatal("알 수 없는 range 키가 통과했습니다")
	}
	// RFC3339 문자열 from/to도 받습니다.
	w, err := timerange.Parse("", nowExtra.Add(-time.Hour).Format(time.RFC3339), valid, nowExtra)
	if err != nil || w.Step != time.Minute {
		t.Fatalf("RFC3339 해석: %v %v", w, err)
	}
	// 키가 비면 기본 1h입니다.
	if w, _ := timerange.Parse("", "", "", nowExtra); w.Key != contract.Range1h {
		t.Fatalf("기본 키: %s", w.Key)
	}
}
