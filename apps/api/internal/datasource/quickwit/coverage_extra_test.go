package quickwit

import (
	"context"
	"testing"
	"time"
)

// TestTimestampNormalization — 인덱스마다 다른 타임스탬프 표기를 전부
// 밀리초로 정규화합니다. (#7 작업 범위)
func TestTimestampNormalization(t *testing.T) {
	base := int64(1_700_000_000_000) // ms
	for name, tc := range map[string]struct {
		in   any
		want int64
		ok   bool
	}{
		"초(float)":     {float64(1_700_000_000), base, true},
		"밀리초(float)":   {float64(1_700_000_000_000), base, true},
		"마이크로초(float)": {float64(1_700_000_000_000_000), base, true},
		"나노초(float)":   {float64(1_700_000_000_000_000_000), base, true},
		"RFC3339":      {"2023-11-14T22:13:20Z", base, true},
		"RFC3339 나노초":  {"2023-11-14T22:13:20.5Z", base + 500, true},
		"문자열 숫자(초)":    {"1700000000", base, true},
		"해석 불가 문자열":    {"yesterday", 0, false},
		"nil":          {nil, 0, false},
		"지원하지 않는 타입":   {[]int{1}, 0, false},
	} {
		got, ok := parseTimestamp(tc.in)
		if ok != tc.ok || (ok && got != tc.want) {
			t.Fatalf("%s: got (%d,%v) want (%d,%v)", name, got, ok, tc.want, tc.ok)
		}
	}
}

// TestStableIDIsDeterministic — _id가 없는 인덱스에서도 같은 문서는 재조회 시
// 같은 id여야 커서 경계의 중복 제거가 성립합니다.
func TestStableIDIsDeterministic(t *testing.T) {
	fields := FieldMap{}.withDefaults()
	src := map[string]any{
		"pod_uid": "uid-a", "pod_name": "p", "container": "app", "message": "hello",
	}
	a := stableID(1000, src, fields)
	b := stableID(1000, src, fields)
	if a != b {
		t.Fatalf("같은 문서의 id가 다릅니다: %s %s", a, b)
	}
	src2 := map[string]any{
		"pod_uid": "uid-a", "pod_name": "p", "container": "app", "message": "world",
	}
	if a == stableID(1000, src2, fields) {
		t.Fatal("다른 문서가 같은 id를 받았습니다")
	}
}

// TestHitWithoutIDGetsStableID — 실제 검색 경로에서 _id 없는 hit이
// 결정적 id를 받는지 확인합니다.
func TestHitWithoutIDGetsStableID(t *testing.T) {
	s := &Source{cfg: Config{}.withDefaults()}
	line, ok := s.line(esHit{Source: map[string]any{
		"timestamp": float64(1_700_000_000_000), "message": "m", "level": "info",
		"namespace": "payments", "pod_name": "p", "pod_uid": "u", "container": "app",
	}})
	if !ok || line.ID == "" {
		t.Fatalf("id가 비었습니다: %+v", line)
	}
	// 타임스탬프 없는 문서는 버립니다 — 정렬 키가 없으면 커서가 성립하지 않습니다.
	if _, ok := s.line(esHit{Source: map[string]any{"message": "m"}}); ok {
		t.Fatal("타임스탬프 없는 문서가 통과했습니다")
	}
	if _, ok := s.line(esHit{}); ok {
		t.Fatal("_source 없는 hit이 통과했습니다")
	}
}

// TestLevelNormalizationTable — 저장된 레벨 표기 편차의 정규화 표입니다.
func TestLevelNormalizationTable(t *testing.T) {
	for in, want := range map[string]string{
		"ERROR": "ERROR", "err": "ERROR", "Fatal": "ERROR", "CRITICAL": "ERROR",
		"warn": "WARN", "Warning": "WARN",
		"debug": "DEBUG", "TRACE": "DEBUG",
		"info": "INFO", "": "INFO", "whatever": "INFO", " info ": "INFO",
	} {
		if got := string(normalizeLevel(in)); got != want {
			t.Fatalf("%q: got %s want %s", in, got, want)
		}
	}
}

// TestCursorRoundTripAndRejection — 커서 인코딩 왕복과 변조 거절입니다.
func TestCursorRoundTripAndRejection(t *testing.T) {
	c := cursor{T: 1234, IDs: []string{"a", "b"}}
	enc := encodeCursor(c)
	dec, ok := decodeCursor(enc)
	if !ok || dec.T != 1234 || len(dec.IDs) != 2 {
		t.Fatalf("왕복 실패: %+v", dec)
	}
	if _, ok := decodeCursor("!!!"); ok {
		t.Fatal("base64 아닌 커서가 통과했습니다")
	}
	if _, ok := decodeCursor("bm90LWpzb24"); ok { // "not-json"
		t.Fatal("JSON 아닌 커서가 통과했습니다")
	}
	if c, ok := decodeCursor(""); !ok || c.T != 0 {
		t.Fatal("빈 커서는 첫 페이지입니다")
	}

	// 경계 id 상한 — 커서가 무한히 자라지 않습니다.
	big := cursor{T: 1}
	for i := 0; i < maxBoundaryIDs+100; i++ {
		big.IDs = append(big.IDs, "id")
	}
	dec, _ = decodeCursor(encodeCursor(big))
	if len(dec.IDs) > maxBoundaryIDs {
		t.Fatalf("경계 id가 상한을 넘습니다: %d", len(dec.IDs))
	}
}

// TestHistogramSkipsWithoutStep — Step 없는 히스토그램 요청은 조회 없이 빈 값입니다.
func TestHistogramSkipsWithoutStep(t *testing.T) {
	s := &Source{cfg: Config{}.withDefaults()}
	q := baseQuery("")
	q.Window.Step = 0
	buckets, err := s.Histogram(context.Background(), q)
	if err != nil || buckets != nil {
		t.Fatalf("Step 없는 히스토그램: %v %v", buckets, err)
	}
	_ = time.Now
}
