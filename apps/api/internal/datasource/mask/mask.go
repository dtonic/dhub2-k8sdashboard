// Package mask는 로그 본문의 민감 정보를 **서버에서** 가립니다.
//
// 마스킹을 프런트에서 하면 원문이 이미 브라우저까지 간 뒤입니다. 네트워크 탭,
// 확장 프로그램, 캐시 어디에나 남습니다. 그래서 원문은 서버 밖으로 나가지 않고,
// 응답에는 **가려진 본문 + 어디가 가려졌는지(MaskedSpan)** 만 실립니다. (ADR 0003)
package mask

import (
	"regexp"
	"sort"
	"strings"

	"github.com/xenx96/k8s-dashboard/apps/api/internal/contract"
)

// Char는 가림 문자입니다. 자릿수를 그대로 노출하지 않도록 고정 길이로 바꿉니다.
const Char = "•"

// maskedLen은 가린 뒤의 길이입니다. 원문 길이를 유추할 수 없게 상수로 둡니다.
const maskedLen = 6

type rule struct {
	kind string
	re   *regexp.Regexp
	// group이 0보다 크면 그 캡처 그룹만 가립니다(키 이름은 남기고 값만 가리는 경우).
	group int
}

// rules는 위에서부터 적용됩니다. 좁은 규칙을 먼저 두어야 넓은 규칙이 삼키지 않습니다.
var rules = []rule{
	{kind: "card", re: regexp.MustCompile(`\b(?:\d[ -]?){13,19}\b`)},
	{kind: "token", re: regexp.MustCompile(`\b[Bb]earer\s+([A-Za-z0-9._\-]{12,})`), group: 1},
	{kind: "token", re: regexp.MustCompile(`\beyJ[A-Za-z0-9._\-]{16,}`)},
	{kind: "password", re: regexp.MustCompile(`(?i)\b(?:password|passwd|pwd)["']?\s*[:=]\s*["']?([^\s"',}]+)`), group: 1},
	{kind: "secret", re: regexp.MustCompile(`(?i)\b(?:secret|api[_-]?key|access[_-]?key|token)["']?\s*[:=]\s*["']?([^\s"',}]+)`), group: 1},
	{kind: "email", re: regexp.MustCompile(`\b[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}\b`)},
	{kind: "ip", re: regexp.MustCompile(`\b(?:\d{1,3}\.){3}\d{1,3}\b`)},
}

type span struct {
	start, end int
	kind       string
}

// Apply는 본문을 가리고 가려진 구간 목록을 함께 돌려줍니다.
//
// 반환된 message에는 원문 조각이 남아 있지 않습니다. MaskedSpan의 offset은
// **가린 뒤 본문 기준**이라 UI가 그대로 밑줄을 그을 수 있습니다.
func Apply(message string) (string, []contract.MaskedSpan) {
	found := collect(message)
	if len(found) == 0 {
		return message, []contract.MaskedSpan{}
	}

	var b strings.Builder
	out := make([]contract.MaskedSpan, 0, len(found))
	prev := 0
	for _, s := range found {
		b.WriteString(message[prev:s.start])
		start := len([]rune(b.String()))
		b.WriteString(strings.Repeat(Char, maskedLen))
		out = append(out, contract.MaskedSpan{Start: start, Length: maskedLen, Kind: s.kind})
		prev = s.end
	}
	b.WriteString(message[prev:])
	return b.String(), out
}

// collect는 규칙을 적용해 겹치지 않는 구간 목록을 만듭니다.
func collect(message string) []span {
	var found []span
	for _, r := range rules {
		for _, m := range r.re.FindAllStringSubmatchIndex(message, -1) {
			start, end := m[0], m[1]
			if r.group > 0 && len(m) > 2*r.group+1 && m[2*r.group] >= 0 {
				start, end = m[2*r.group], m[2*r.group+1]
			}
			if end <= start {
				continue
			}
			found = append(found, span{start: start, end: end, kind: r.kind})
		}
	}
	if len(found) == 0 {
		return nil
	}
	sort.Slice(found, func(a, b int) bool {
		if found[a].start != found[b].start {
			return found[a].start < found[b].start
		}
		return found[a].end > found[b].end
	})
	// 겹치는 구간은 먼저 잡힌(더 좁은 규칙의) 것을 남깁니다.
	out := found[:1]
	for _, s := range found[1:] {
		last := out[len(out)-1]
		if s.start < last.end {
			continue
		}
		out = append(out, s)
	}
	return out
}
