// 템플릿 렌더링입니다. escape가 이 파일 밖으로 새지 않습니다.
//
// $__scope  → 서버가 확정한 라벨 매처 (namespace·pod)
// $__rate   → Step에서 계산한 rate/increase 구간
// $variable → allowlist 검증을 통과한 값의 **라벨 값 리터럴** ("..." 형태)
//
// 변수가 라벨 값 리터럴로만 렌더링되므로, 어떤 값을 넣어도 matcher나
// 표현식 조각이 될 수 없습니다. (#9 완료 기준)
package querycatalog

import (
	"fmt"
	"regexp"
	"strings"
	"time"
)

// Scope는 서버가 확정한 조회 범위입니다. 어댑터가 Target·카탈로그(Pod 신원)에서
// 만들어 넘깁니다. 여기 없는 것은 매처가 되지 않습니다.
type Scope struct {
	// Namespace는 단일 namespace입니다. 비어 있으면 Namespaces를 봅니다.
	Namespace string
	// Namespaces는 허용 목록입니다. 비어 있으면 전체 허용입니다.
	Namespaces []string
	// PodName은 단일 Pod 이름입니다(UID → 이름 변환 후).
	PodName string
	// PodNames는 워크로드의 Pod 이름 집합입니다.
	PodNames []string
}

// matchers는 Scope를 PromQL 라벨 매처 텍스트로 바꿉니다.
func (s Scope) matchers() string {
	var parts []string
	switch {
	case s.Namespace != "":
		parts = append(parts, `namespace=`+quoteLabel(s.Namespace))
	case len(s.Namespaces) > 0:
		parts = append(parts, `namespace=~`+quoteLabel(anchoredAlternation(s.Namespaces)))
	}
	switch {
	case s.PodName != "":
		parts = append(parts, `pod=`+quoteLabel(s.PodName))
	case len(s.PodNames) > 0:
		parts = append(parts, `pod=~`+quoteLabel(anchoredAlternation(s.PodNames)))
	}
	return strings.Join(parts, ",")
}

// Render는 템플릿을 실행 가능한 표현식으로 바꿉니다.
//
// vars는 선언된 변수만 받습니다. 선언되지 않았거나 값 제한(values/pattern)을
// 통과하지 못하면 오류입니다 — 조용히 빼고 실행하지 않습니다.
func (q Query) Render(sc Scope, step time.Duration, vars map[string]string) (string, error) {
	for name := range vars {
		if !q.declares(name) {
			return "", fmt.Errorf("query %q: 선언되지 않은 변수 %q", q.Ref, name)
		}
	}

	expr := q.Expr
	scopeText := sc.matchers()
	// "$__scope," 형태로 쓰인 자리에서 Scope가 비면 콤마가 남습니다.
	// 전체 허용 사용자라 매처가 없는 경우입니다 — 자리 자체를 지웁니다.
	if scopeText == "" {
		expr = strings.ReplaceAll(expr, "$__scope,", "")
		expr = strings.ReplaceAll(expr, ",$__scope", "")
		expr = strings.ReplaceAll(expr, "$__scope", "")
	} else {
		expr = strings.ReplaceAll(expr, "$__scope", scopeText)
	}
	expr = strings.ReplaceAll(expr, "$__rate", rateWindow(step, q.MinStep))

	for i := range q.Variables {
		v := &q.Variables[i]
		val, ok := vars[v.Name]
		if !ok {
			return "", fmt.Errorf("query %q: 변수 %q 값이 없습니다", q.Ref, v.Name)
		}
		if !v.allows(val) {
			return "", fmt.Errorf("query %q: 변수 %q 값이 allowlist를 통과하지 못했습니다", q.Ref, v.Name)
		}
		// 라벨 값 리터럴로만 렌더링합니다. 템플릿은 container=$container 처럼 씁니다.
		expr = strings.ReplaceAll(expr, "$"+v.Name, quoteLabel(val))
	}
	return expr, nil
}

func (q Query) declares(name string) bool {
	for _, v := range q.Variables {
		if v.Name == name {
			return true
		}
	}
	return false
}

func (v *Variable) allows(val string) bool {
	for _, allowed := range v.Values {
		if val == allowed {
			return true
		}
	}
	if v.re != nil && v.re.MatchString(val) {
		return true
	}
	return false
}

// EffectiveStep은 서버 강제 Step에 MinStep과 MaxDataPoints를 적용합니다.
// Step은 넓어질 수만 있습니다 — 좁히면 포인트 수 계약이 깨집니다. (README §11)
func (q Query) EffectiveStep(step time.Duration, span time.Duration) time.Duration {
	if step <= 0 {
		step = time.Minute
	}
	if q.MinStep > 0 && step < q.MinStep {
		step = q.MinStep
	}
	if span <= 0 || q.Limits.MaxDataPoints <= 0 {
		return step
	}
	if int(span/step) <= q.Limits.MaxDataPoints {
		return step
	}
	widened := span / time.Duration(q.Limits.MaxDataPoints)
	if rem := widened % step; rem != 0 {
		widened += step - rem
	}
	return widened
}

// rateWindow는 rate/increase 구간입니다. Step보다 좁으면 샘플 간격에 따라 구멍이
// 생기므로 Step의 2배와 2분 중 큰 쪽을 씁니다.
func rateWindow(step, minStep time.Duration) string {
	if minStep > step {
		step = minStep
	}
	w := 2 * step
	if w < 2*time.Minute {
		w = 2 * time.Minute
	}
	sec := int(w.Seconds())
	if sec < 1 {
		sec = 1
	}
	return fmt.Sprintf("%ds", sec)
}

// anchoredAlternation은 이름 목록을 ^(a|b)$ 정규식으로 만듭니다.
// 각 이름은 QuoteMeta로 이스케이프해 정규식 연산자로 해석될 수 없게 합니다.
func anchoredAlternation(names []string) string {
	quoted := make([]string, len(names))
	for i, n := range names {
		quoted[i] = regexp.QuoteMeta(n)
	}
	return "^(" + strings.Join(quoted, "|") + ")$"
}

// quoteLabel은 PromQL 라벨 값 리터럴을 만듭니다. 따옴표·역슬래시를 이스케이프해
// 값이 매처 밖으로 빠져나갈 수 없게 합니다.
func quoteLabel(v string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)
	return `"` + r.Replace(v) + `"`
}
