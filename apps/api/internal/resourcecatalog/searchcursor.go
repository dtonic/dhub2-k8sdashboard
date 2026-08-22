package resourcecatalog

// 검색 cursor는 offset이 아니라 **keyset**입니다. (ADR 0003과 같은 원칙)
//
// 조회 경로가 둘이므로 cursor도 두 가지 모드를 가집니다.
//
//	"i" 색인 순서  — 클러스터 전체 접근. 위치는 (token, namespace, name, gvr, uid)입니다.
//	"s" 순회 순서  — 범위 제한 접근. 위치는 (gvr, namespace, name, uid)이고,
//	                여기에 두 가지를 더 싣습니다.
//	                  · **이 namespace를 여기까지 훑는 동안 센 label 토큰 수** —
//	                    MaxLabelTokensPerNamespace 판정을 페이지 사이에서 이어갑니다.
//	                  · **누락 사유 비트** — 앞 페이지에서만 만난 누락(잘린 label,
//	                    건너뛴 비-ready 리소스)이 뒤 페이지에서 사라지지 않게 합니다.
//	                그래서 cursor가 "마지막으로 훑은 위치"의 의미를 온전히 담습니다.
//	                순회 cursor의 namespace는 언제나 실재하는 허용 namespace이므로
//	                비어 있을 수 없습니다 — 빈 값은 위조로 보고 거절합니다.
//
// 어느 모드든 위치 번호를 담지 않으므로 인덱스가 다시 만들어져도 그대로 이어집니다.
// 지문이 다르면 거절합니다 — 질의나 Scope가 바뀐 cursor를 이어 붙이면 중복이나
// 누락이 조용히 생깁니다. 모드가 다르면 거절합니다 — Scope가 바뀌면 순서가 바뀝니다.
//
// **형식 버전은 2입니다.** 버전 1(모드·label 카운터 없음) cursor는 해석하지 않고
// ErrInvalidCursor로 거절합니다. cursor는 불투명하고 수명이 짧으므로, 클라이언트는
// 질의·Scope가 바뀌었을 때와 똑같이 첫 페이지부터 다시 조회하면 됩니다.

import (
	"encoding/base64"
	"encoding/hex"
	"hash/fnv"
	"sort"
	"strconv"
	"strings"
)

const (
	searchCursorVersion = "2"
	// cursorModeIndex / cursorModeScan은 어떤 순서로 만든 위치인지입니다.
	cursorModeIndex = "i"
	cursorModeScan  = "s"

	// MaxSearchCursorLen은 검색 cursor 문자열의 상한입니다.
	//
	// 원문 최대치는 버전(1) + 지문(16) + 모드(1) + 토큰(64) + namespace(63) +
	// 이름(253) + GVR(349) + UID(64) + label 카운터(20) + 누락 비트(10) +
	// 구분자(9) = 850바이트이고 base64url로 1134자입니다.
	// 계약(OpenAPI)의 maxLength와 같아야 합니다.
	MaxSearchCursorLen = 1536

	maxCursorGVRLen    = 349
	maxCursorUIDLen    = 64
	maxCursorCounterLn = 20
	maxCursorFlagsLen  = 10
)

// searchCursorKey는 결과 순서의 한 지점입니다.
type searchCursorKey struct {
	mode      string
	token     string
	namespace string
	name      string
	gvr       string
	uid       string
	// nsLabelTokens와 scanFlags는 순회 모드에서만 씁니다.
	nsLabelTokens int64
	scanFlags     uint32
}

func encodeSearchCursor(key searchCursorKey, fingerprint string) string {
	raw := strings.Join([]string{
		searchCursorVersion, fingerprint, key.mode, key.token, key.namespace,
		key.name, key.gvr, key.uid,
		strconv.FormatInt(key.nsLabelTokens, 10),
		strconv.FormatUint(uint64(key.scanFlags), 10),
	}, cursorSep)
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

// decodeSearchCursor는 형식·길이·문자·지문·모드를 모두 검사합니다.
// 하나라도 어긋나면 ErrInvalidCursor입니다.
func decodeSearchCursor(encoded, fingerprint, wantMode string) (searchCursorKey, error) {
	if len(encoded) > MaxSearchCursorLen {
		return searchCursorKey{}, ErrInvalidCursor
	}
	raw, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return searchCursorKey{}, ErrInvalidCursor
	}
	parts := strings.Split(string(raw), cursorSep)
	if len(parts) != 10 || parts[0] != searchCursorVersion || parts[1] != fingerprint || parts[2] != wantMode {
		return searchCursorKey{}, ErrInvalidCursor
	}
	key := searchCursorKey{
		mode: parts[2], token: parts[3], namespace: parts[4],
		name: parts[5], gvr: parts[6], uid: parts[7],
	}
	if len(parts[8]) > maxCursorCounterLn || len(parts[9]) > maxCursorFlagsLen {
		return searchCursorKey{}, ErrInvalidCursor
	}
	counter, convErr := strconv.ParseInt(parts[8], 10, 64)
	if convErr != nil || counter < 0 || counter > MaxLabelTokensPerNamespace {
		return searchCursorKey{}, ErrInvalidCursor
	}
	flags, flagErr := strconv.ParseUint(parts[9], 10, 32)
	if flagErr != nil || uint32(flags)&^uint32(scanFlagAll) != 0 {
		return searchCursorKey{}, ErrInvalidCursor
	}
	key.nsLabelTokens, key.scanFlags = counter, uint32(flags)

	switch key.mode {
	case cursorModeIndex:
		// 색인 순서는 토큰이 위치의 첫 성분입니다. label 카운터와 누락 비트는 쓰지 않습니다.
		if len(key.token) == 0 || len(key.token) > tokenPrefixBytes || !safeToken(key.token) ||
			counter != 0 || flags != 0 {
			return searchCursorKey{}, ErrInvalidCursor
		}
	case cursorModeScan:
		// 순회 순서는 토큰을 쓰지 않습니다. 대신 label 카운터와 누락 비트가 의미를 가집니다.
		// namespace는 언제나 실재하는 허용 namespace이므로 빈 값일 수 없습니다.
		if key.token != "" || key.namespace == "" {
			return searchCursorKey{}, ErrInvalidCursor
		}
	default:
		return searchCursorKey{}, ErrInvalidCursor
	}
	switch {
	case len(key.namespace) > maxCursorNSLen || !safeCursorSegment(key.namespace):
		return searchCursorKey{}, ErrInvalidCursor
	case len(key.name) == 0 || len(key.name) > maxCursorName || !safeCursorSegment(key.name):
		return searchCursorKey{}, ErrInvalidCursor
	case len(key.gvr) == 0 || len(key.gvr) > maxCursorGVRLen || !safeToken(key.gvr):
		return searchCursorKey{}, ErrInvalidCursor
	case len(key.uid) > maxCursorUIDLen || !safeCursorSegment(key.uid):
		return searchCursorKey{}, ErrInvalidCursor
	}
	return key, nil
}

// searchFingerprint는 이 cursor가 유효한 질의를 식별합니다.
//
// 클러스터·질의·Scope가 전부 들어갑니다. **limit은 넣지 않습니다** — keyset
// 페이징에서 페이지 크기는 위치와 무관하고, 넣으면 크기만 바꿔도 이어보기가 끊깁니다.
func searchFingerprint(clusterID, query string, all bool, namespaces []string) string {
	h := fnv.New64a()
	write := func(parts ...string) {
		for _, p := range parts {
			_, _ = h.Write([]byte(p))
			_, _ = h.Write([]byte{0})
		}
	}
	write("search", clusterID, query, strconv.FormatBool(all))
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
