package httpapi

// 요청 상관관계 ID입니다. (#5)
//
// 규칙은 두 가지입니다.
//   - 인바운드 X-Request-ID는 **안전한 형식일 때만** 재사용합니다. 그 외에는
//     서버가 새 ID를 만듭니다 — 헤더·로그 injection을 여기서 차단합니다.
//   - 모든 응답(프로브·에러 포함)은 X-Request-ID 헤더를 달고, 에러 본문의
//     requestId·감사 로그의 requestId가 같은 값을 가리킵니다.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
)

const requestIDHeader = "X-Request-ID"

// requestIDMaxLen — 인바운드 ID 재사용 상한입니다. 로그 한 줄을 도배할 수 없게 합니다.
const requestIDMaxLen = 128

type requestIDKey struct{}

// safeRequestID는 인바운드 ID가 재사용 가능한 형식인지 확인합니다.
// 허용 문자는 [A-Za-z0-9._:-] 뿐입니다 — CR/LF·공백·비ASCII는 전부 거절합니다.
func safeRequestID(s string) bool {
	if len(s) == 0 || len(s) > requestIDMaxLen {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case 'a' <= c && c <= 'z', 'A' <= c && c <= 'Z', '0' <= c && c <= '9':
		case c == '.' || c == '_' || c == ':' || c == '-':
		default:
			return false
		}
	}
	return true
}

// newRequestID는 암호학적 난수 128bit를 소문자 hex 32자로 만듭니다.
func newRequestID() string {
	var b [16]byte
	// crypto/rand.Read는 실패하면 프로그램을 중단시키므로(Go 1.24) 오류 분기가 없습니다.
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

func withRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, requestIDKey{}, id)
}

// requestIDFrom은 컨텍스트의 요청 ID입니다. 미들웨어를 안 거친 컨텍스트면 빈 문자열입니다.
func requestIDFrom(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey{}).(string)
	return id
}
