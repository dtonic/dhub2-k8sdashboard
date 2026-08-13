package timerange_test

import (
	"testing"
	"time"

	"github.com/xenx96/k8s-dashboard/apps/api/internal/contract"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/timerange"
)

var now = time.Date(2026, 8, 13, 4, 0, 0, 0, time.UTC)

func TestStepIsForcedByRange(t *testing.T) {
	// Step을 사용자가 고를 수 있으면 30일 범위를 1분 간격으로 요청할 수 있습니다.
	// 그 순간 메트릭 백엔드가 43,200 포인트를 계산합니다.
	want := map[contract.RangeKey]int{"1h": 60, "1d": 300, "7d": 900, "30d": 3600}
	for key, step := range want {
		w, err := timerange.Parse(string(key), "", "", now)
		if err != nil {
			t.Fatalf("%s: %v", key, err)
		}
		if got := int(w.Step.Seconds()); got != step {
			t.Errorf("%s: step=%d, want %d", key, got, step)
		}
		if w.To != now {
			t.Errorf("%s: to=%v, want %v", key, w.To, now)
		}
	}
}

func TestDefaultRangeIsOneHour(t *testing.T) {
	w, err := timerange.Parse("", "", "", now)
	if err != nil {
		t.Fatal(err)
	}
	if w.Key != contract.Range1h {
		t.Fatalf("key=%s, want 1h", w.Key)
	}
}

func TestCustomRangeOverThirtyDaysIsRejected(t *testing.T) {
	// 잘라서 조용히 처리하지 않습니다. 사용자가 본 범위와 응답 범위가 다르면
	// 화면의 숫자를 신뢰할 수 없습니다.
	from := now.Add(-31 * 24 * time.Hour)
	if _, err := timerange.Parse("custom", millis(from), millis(now), now); err == nil {
		t.Fatal("31일 범위가 통과했습니다")
	}

	from = now.Add(-30 * 24 * time.Hour)
	w, err := timerange.Parse("custom", millis(from), millis(now), now)
	if err != nil {
		t.Fatalf("정확히 30일이 거절되었습니다: %v", err)
	}
	if w.Key != contract.RangeCustom {
		t.Errorf("key=%s, want custom", w.Key)
	}
	if int(w.Step.Seconds()) != 3600 {
		t.Errorf("step=%v, want 3600s", w.Step)
	}
}

func TestInvalidRanges(t *testing.T) {
	cases := []struct{ name, key, from, to string }{
		{"알 수 없는 프리셋", "90d", "", ""},
		{"from이 to보다 늦음", "custom", millis(now), millis(now.Add(-time.Hour))},
		{"from만 있음", "custom", millis(now), ""},
		{"숫자가 아닌 값", "custom", "yesterday", "today"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := timerange.Parse(c.key, c.from, c.to, now); err == nil {
				t.Fatal("거절되지 않았습니다")
			}
		})
	}
}

func TestRFC3339CustomRange(t *testing.T) {
	from := now.Add(-2 * time.Hour)
	w, err := timerange.Parse("custom", from.Format(time.RFC3339), now.Format(time.RFC3339), now)
	if err != nil {
		t.Fatal(err)
	}
	if !w.From.Equal(from) {
		t.Errorf("from=%v, want %v", w.From, from)
	}
}

func TestBucketsMatchStep(t *testing.T) {
	w, _ := timerange.Parse("1h", "", "", now)
	if got := w.Buckets(); got != 60 {
		t.Errorf("1시간/1분 = %d 버킷, want 60", got)
	}
}

func millis(t time.Time) string {
	return time.Unix(0, t.UnixNano()).UTC().Format(time.RFC3339)
}
