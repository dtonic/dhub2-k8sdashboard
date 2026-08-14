// 커서 인코딩입니다. offset이 아니라 **정렬 키**를 담습니다. (ADR 0003)
//
// 커서는 (경계 timestamp, 경계에서 이미 내려간 문서 id 목록)입니다.
// 다음 페이지는 timestamp ≤ 경계로 조회한 뒤 id 목록을 걸러서 만듭니다.
// offset과 달리 새 로그가 들어와도 페이지가 밀리거나 중복되지 않습니다.
package quickwit

import (
	"encoding/base64"
	"encoding/json"
)

// maxBoundaryIDs는 커서에 실을 수 있는 경계 id 수 상한입니다.
// 같은 밀리초에 이보다 많은 로그가 몰리는 경우 커서가 무한히 자라는 것을
// 막습니다. 상한을 넘으면 그 너머의 극단적 동시 로그는 중복될 수 있습니다 —
// 커서 폭주보다 낫다고 판단한 트레이드오프입니다.
const maxBoundaryIDs = 512

// cursor는 불투명 문자열로 인코딩되어 브라우저를 오갑니다.
// 내용을 신뢰하지 않습니다 — 해석에 실패하면 요청을 거절합니다.
type cursor struct {
	T   int64    `json:"t"`
	IDs []string `json:"ids,omitempty"`
}

func encodeCursor(c cursor) string {
	if len(c.IDs) > maxBoundaryIDs {
		c.IDs = c.IDs[:maxBoundaryIDs]
	}
	raw, _ := json.Marshal(c)
	return base64.RawURLEncoding.EncodeToString(raw)
}

func decodeCursor(s string) (cursor, bool) {
	if s == "" {
		return cursor{}, true
	}
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return cursor{}, false
	}
	var c cursor
	if err := json.Unmarshal(raw, &c); err != nil || c.T < 0 {
		return cursor{}, false
	}
	return c, true
}
