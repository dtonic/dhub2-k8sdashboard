package quickwit

import (
	"context"
	"encoding/base64"
	"strings"
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

func TestTraversalIDIsDeterministicAndOrdinalUnique(t *testing.T) {
	key := make([]byte, 32)
	prefix := traversalIDPrefix(key, "nonce")
	a := traversalID(prefix, 1)
	if a != traversalID(prefix, 1) {
		t.Fatal("같은 traversal ordinal의 id가 달라졌습니다")
	}
	if a == traversalID(prefix, 2) {
		t.Fatal("다른 ordinal이 같은 id를 받았습니다")
	}
}

// TestHitWithoutEventIDGetsTraversalID는 event_id 없는 hit이 traversal ID를 받는지 확인합니다.
func TestHitWithoutEventIDGetsTraversalID(t *testing.T) {
	s := &Source{cfg: Config{}.withDefaults()}
	line, ok := s.lineAt(esHit{Source: map[string]any{
		"timestamp": float64(1_700_000_000_000), "message": "m", "level": "info",
		"namespace": "payments", "pod_name": "p", "pod_uid": "u", "container": "app",
	}}, traversalIDPrefix(s.hmacKey[:], "nonce"), 0)
	if !ok || line.ID == "" {
		t.Fatalf("id가 비었습니다: %+v", line)
	}
	// 타임스탬프 없는 문서는 버립니다 — 정렬 키가 없으면 커서가 성립하지 않습니다.
	if _, ok := s.lineAt(esHit{Source: map[string]any{"message": "m"}}, "prefix", 0); ok {
		t.Fatal("타임스탬프 없는 문서가 통과했습니다")
	}
	if _, ok := s.lineAt(esHit{}, "prefix", 0); ok {
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

// TestCursorHMACRoundTripAndRejection은 커서 왕복과 payload splice/HMAC 불일치를 검증합니다.
func TestCursorHMACRoundTripAndRejection(t *testing.T) {
	key := make([]byte, 32)
	q := base64.RawURLEncoding.EncodeToString(make([]byte, digestBytes))
	nonce := base64.RawURLEncoding.EncodeToString(make([]byte, nonceBytes))
	c := cursor{ScrollID: "scroll-1", QueryHash: q, Nonce: nonce, Returned: 2, Scanned: 2, Total: 10}
	enc, ok := encodeCursor(c, key)
	if !ok {
		t.Fatal("커서가 8KiB 상한을 넘었습니다")
	}
	for _, invalid := range []string{"", strings.Repeat("s", maxScrollIDBytes+1)} {
		bad := c
		bad.ScrollID = invalid
		if _, ok := encodeCursor(bad, key); ok {
			t.Fatal("invalid scroll id cursor가 발행됐습니다")
		}
	}
	dec, ok := decodeCursor(enc, 100, key)
	if !ok || dec.ScrollID != "scroll-1" || dec.Returned != 2 {
		t.Fatalf("왕복 실패: %+v", dec)
	}
	allMalformed := c
	allMalformed.Returned = 0
	allMalformed.Scanned = 1
	encMalformed, ok := encodeCursor(allMalformed, key)
	if !ok {
		t.Fatal("all-malformed page cursor encoding failed")
	}
	if decoded, ok := decodeCursor(encMalformed, 100, key); !ok || decoded.Returned != 0 || decoded.Scanned != 1 {
		t.Fatalf("all-malformed page cursor rejected: %+v", decoded)
	}
	if _, ok := decodeCursor("!!!", 100, key); ok {
		t.Fatal("base64 아닌 커서가 통과했습니다")
	}
	if _, ok := decodeCursor("bm90LWpzb24", 100, key); ok { // "not-json"
		t.Fatal("JSON 아닌 커서가 통과했습니다")
	}
	if c, ok := decodeCursor("", 100, key); !ok || c.ScrollID != "" {
		t.Fatal("빈 커서는 첫 페이지입니다")
	}

	// payload만 바꿔 checksum이 불일치한 입력과 oversized/malformed 입력을 거절합니다.
	raw, err := base64.RawURLEncoding.DecodeString(enc)
	if err != nil {
		t.Fatal(err)
	}
	tampered := strings.Replace(string(raw), `"n":2`, `"n":3`, 1)
	if _, ok := decodeCursor(base64.RawURLEncoding.EncodeToString([]byte(tampered)), 100, key); ok {
		t.Fatal("HMAC이 불일치한 커서가 통과했습니다")
	}
	if _, ok := decodeCursor(strings.Repeat("A", maxEncodedCursorBytes+1), 100, key); ok {
		t.Fatal("oversized 커서가 통과했습니다")
	}
	bad := c
	bad.QueryHash = "not-a-digest"
	badEncoded, _ := encodeCursor(bad, key)
	if _, ok := decodeCursor(badEncoded, 100, key); ok {
		t.Fatal("malformed query digest가 통과했습니다")
	}
	wrongKey := make([]byte, 32)
	wrongKey[0] = 1
	if _, ok := decodeCursor(enc, 100, wrongKey); ok {
		t.Fatal("다른 Source의 HMAC key로 커서가 통과했습니다")
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
