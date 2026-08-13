// Package timerange은 시간 범위와 Step을 **서버에서 강제**합니다.
//
// Step을 사용자가 고르게 하면 30일 범위를 1분 간격으로 요청하는 순간
// 메트릭 백엔드가 43,200 포인트를 계산합니다. 범위마다 Step을 고정합니다. (README §11)
package timerange

import (
	"errors"
	"strconv"
	"time"

	"github.com/xenx96/k8s-dashboard/apps/api/internal/contract"
)

// MaxCustomSpan은 Custom Range의 상한입니다. 디자인 명세와 같은 30일입니다.
const MaxCustomSpan = 30 * 24 * time.Hour

// ErrInvalid는 범위 자체가 성립하지 않을 때입니다. 화면 전체 에러로 올라갑니다.
var ErrInvalid = errors.New("invalid range")

// stepSeconds는 범위별 강제 Step입니다. packages/contracts의 STEP_SECONDS와 같아야 합니다.
var stepSeconds = map[contract.RangeKey]int{
	contract.Range1h:  60,
	contract.Range1d:  300,
	contract.Range7d:  900,
	contract.Range30d: 3600,
}

var spans = map[contract.RangeKey]time.Duration{
	contract.Range1h:  time.Hour,
	contract.Range1d:  24 * time.Hour,
	contract.Range7d:  7 * 24 * time.Hour,
	contract.Range30d: 30 * 24 * time.Hour,
}

// Window는 확정된 조회 구간입니다. 핸들러는 이 값만 보고 데이터소스를 호출합니다.
type Window struct {
	Key  contract.RangeKey
	From time.Time
	To   time.Time
	Step time.Duration
}

// Contract는 응답에 실을 형태로 변환합니다.
func (w Window) Contract() contract.TimeWindow {
	return contract.TimeWindow{
		Key:         w.Key,
		From:        w.From.UTC().Format(time.RFC3339),
		To:          w.To.UTC().Format(time.RFC3339),
		StepSeconds: int(w.Step.Seconds()),
	}
}

// Buckets는 Step 간격으로 몇 개의 포인트가 나오는지입니다.
func (w Window) Buckets() int {
	if w.Step <= 0 {
		return 0
	}
	n := int(w.To.Sub(w.From) / w.Step)
	if n < 1 {
		return 1
	}
	return n
}

// Parse는 쿼리 파라미터를 확정 구간으로 바꿉니다.
//
// from/to가 오면 Custom으로 처리하되 **30일을 넘으면 거절**합니다.
// 잘라서 조용히 처리하지 않는 이유는, 사용자가 본 범위와 응답 범위가 달라지면
// 화면의 숫자를 신뢰할 수 없기 때문입니다.
func Parse(rangeKey, from, to string, now time.Time) (Window, error) {
	if from != "" || to != "" {
		f, err := parseMillisOrRFC3339(from)
		if err != nil {
			return Window{}, ErrInvalid
		}
		t, err := parseMillisOrRFC3339(to)
		if err != nil {
			return Window{}, ErrInvalid
		}
		if !t.After(f) {
			return Window{}, ErrInvalid
		}
		if t.Sub(f) > MaxCustomSpan {
			return Window{}, ErrInvalid
		}
		return Window{Key: contract.RangeCustom, From: f, To: t, Step: customStep(t.Sub(f))}, nil
	}

	key := contract.RangeKey(rangeKey)
	if key == "" {
		key = contract.Range1h
	}
	span, ok := spans[key]
	if !ok {
		return Window{}, ErrInvalid
	}
	return Window{
		Key:  key,
		From: now.Add(-span),
		To:   now,
		Step: time.Duration(stepSeconds[key]) * time.Second,
	}, nil
}

// customStep은 Custom 범위에 가장 가까운 프리셋 Step을 씁니다.
// 포인트 수가 프리셋과 같은 크기로 유지되어 차트와 백엔드 부하가 예측 가능해집니다.
func customStep(span time.Duration) time.Duration {
	switch {
	case span <= time.Hour:
		return 60 * time.Second
	case span <= 24*time.Hour:
		return 300 * time.Second
	case span <= 7*24*time.Hour:
		return 900 * time.Second
	default:
		return 3600 * time.Second
	}
}

func parseMillisOrRFC3339(v string) (time.Time, error) {
	if v == "" {
		return time.Time{}, ErrInvalid
	}
	if ms, err := strconv.ParseInt(v, 10, 64); err == nil {
		return time.UnixMilli(ms).UTC(), nil
	}
	t, err := time.Parse(time.RFC3339, v)
	if err != nil {
		return time.Time{}, ErrInvalid
	}
	return t.UTC(), nil
}
