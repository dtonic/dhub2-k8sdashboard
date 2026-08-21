package resourcecatalog

import (
	"encoding/base64"
	"encoding/hex"
	"hash/fnv"
	"sort"
	"strconv"
	"strings"
)

// cursor는 offset이 아니라 **keyset**입니다. (ADR 0003과 같은 원칙)
//
// 값은 (namespace, name) 위치 하나와, 이 cursor를 만든 질의의 지문입니다.
// 지문이 다르면 거절합니다 — 필터·정렬이 바뀐 cursor를 그대로 이어가면
// 중복이나 누락이 조용히 생깁니다.
const (
	cursorVersion  = "1"
	cursorSep      = "\x1f"
	maxCursorLen   = 512
	maxCursorNSLen = 63
	maxCursorName  = 253
)

type cursorKey struct {
	namespace string
	name      string
}

func encodeCursor(key cursorKey, fingerprint string) string {
	raw := cursorVersion + cursorSep + fingerprint + cursorSep + key.namespace + cursorSep + key.name
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

// decodeCursor는 형식·길이·지문을 모두 검사합니다. 하나라도 어긋나면 ErrInvalidCursor입니다.
func decodeCursor(encoded, fingerprint string) (cursorKey, error) {
	if len(encoded) > maxCursorLen {
		return cursorKey{}, ErrInvalidCursor
	}
	raw, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return cursorKey{}, ErrInvalidCursor
	}
	parts := strings.Split(string(raw), cursorSep)
	if len(parts) != 4 || parts[0] != cursorVersion || parts[1] != fingerprint {
		return cursorKey{}, ErrInvalidCursor
	}
	ns, name := parts[2], parts[3]
	if len(ns) > maxCursorNSLen || len(name) > maxCursorName || name == "" {
		return cursorKey{}, ErrInvalidCursor
	}
	if !safeCursorSegment(ns) || !safeCursorSegment(name) {
		return cursorKey{}, ErrInvalidCursor
	}
	return cursorKey{namespace: ns, name: name}, nil
}

// safeCursorSegment는 Kubernetes 객체 이름에 나타날 수 있는 문자만 허용합니다.
func safeCursorSegment(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == '-', c == '.', c == '_', c == ':':
		default:
			return false
		}
	}
	return true
}

// fingerprint는 이 cursor가 유효한 질의를 식별합니다.
// GVR·정렬 방향·필터·Scope namespace 집합이 모두 들어갑니다.
func fingerprint(gvr string, req ListRequest, namespaces []string, all bool) string {
	h := fnv.New64a()
	write := func(parts ...string) {
		for _, p := range parts {
			_, _ = h.Write([]byte(p))
			_, _ = h.Write([]byte{0})
		}
	}
	write(gvr, strconv.FormatBool(req.Descending), req.NamePrefix, req.LabelSelector, strconv.FormatBool(all))
	sorted := append([]string(nil), namespaces...)
	sort.Strings(sorted)
	write(sorted...)
	var buf [8]byte
	sum := h.Sum64()
	for i := 0; i < 8; i++ {
		buf[i] = byte(sum >> (8 * i))
	}
	return hex.EncodeToString(buf[:])
}
