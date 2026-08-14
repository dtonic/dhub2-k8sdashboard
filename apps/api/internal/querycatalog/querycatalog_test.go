package querycatalog

import (
	"strings"
	"testing"
	"testing/fstest"
	"time"
)

/* ── 기본 카탈로그 ──────────────────────────────────────────────────────── */

// TestDefaultCatalogIsValid — Git에 커밋된 기본 카탈로그가 항상 유효해야 합니다.
// 이 테스트가 CI에서 도는 것이 "Catalog 오류가 CI에서 검출됨"의 실체입니다. (#9)
func TestDefaultCatalogIsValid(t *testing.T) {
	cat, err := LoadDefault()
	if err != nil {
		t.Fatalf("기본 카탈로그가 깨져 있습니다: %v", err)
	}
	if len(cat.Panels()) != 4 {
		t.Fatalf("기본 패널은 4개여야 합니다: %d", len(cat.Panels()))
	}
	for _, want := range []string{"metrics.cpu.used", "metrics.usage.cpu_milli"} {
		if _, ok := cat.Query(want); !ok {
			t.Fatalf("기본 카탈로그에 %s가 없습니다", want)
		}
	}
	if cat.Logs().Search.MaxPageSize <= 0 {
		t.Fatal("로그 한계 선언이 없습니다")
	}
}

/* ── 로드 검증 ──────────────────────────────────────────────────────────── */

func loadOne(t *testing.T, yaml string) (Catalog, error) {
	t.Helper()
	return LoadFS(fstest.MapFS{"t.yaml": &fstest.MapFile{Data: []byte(yaml)}})
}

// TestRangeQueryWithoutScopeIsRejected — Scope 삽입 지점이 없는 range 질의는
// 존재할 수 없습니다. 설정 실수가 아니라 데이터 유출이기 때문입니다.
func TestRangeQueryWithoutScopeIsRejected(t *testing.T) {
	_, err := loadOne(t, `
version: 1
queries:
  - ref: bad.query
    type: promql_range
    expr: sum(rate(container_cpu_usage_seconds_total[5m]))
`)
	if err == nil || !strings.Contains(err.Error(), "$__scope") {
		t.Fatalf("$__scope 누락이 거부되지 않았습니다: %v", err)
	}
}

// TestUndeclaredVariableIsRejected — 변수는 allowlist입니다. 선언 없이
// 표현식에 나타나면 로드가 실패합니다.
func TestUndeclaredVariableIsRejected(t *testing.T) {
	_, err := loadOne(t, `
version: 1
queries:
  - ref: bad.query
    type: promql_range
    expr: sum(rate(x{$__scope,container=$container}[$__rate]))
`)
	if err == nil || !strings.Contains(err.Error(), "선언되지 않은 변수") {
		t.Fatalf("미선언 변수가 거부되지 않았습니다: %v", err)
	}
}

// TestUnknownBuiltinIsRejected — 오타 난 내장 자리표시자는 로드 오류입니다.
func TestUnknownBuiltinIsRejected(t *testing.T) {
	_, err := loadOne(t, `
version: 1
queries:
  - ref: bad.query
    type: promql_range
    expr: sum(x{$__scope}[$__interval])
`)
	if err == nil || !strings.Contains(err.Error(), "$__interval") {
		t.Fatalf("알 수 없는 내장 자리표시자가 거부되지 않았습니다: %v", err)
	}
}

// TestPanelReferencingUnknownQueryIsRejected — 등록되지 않은 queryRef를 가리키는
// 패널은 로드 단계에서 걸립니다. 실행 시점까지 가지 않습니다.
func TestPanelReferencingUnknownQueryIsRejected(t *testing.T) {
	_, err := loadOne(t, `
version: 1
queries:
  - ref: ok.query
    type: promql_range
    expr: sum(x{$__scope})
panels:
  - id: cpu
    title: CPU
    series:
      - { key: used, label: u, query: no.such.ref }
`)
	if err == nil || !strings.Contains(err.Error(), "no.such.ref") {
		t.Fatalf("미등록 queryRef 참조가 거부되지 않았습니다: %v", err)
	}
}

// TestDuplicateRefIsRejected — 같은 ref가 두 번 정의되면 어느 정의가 실행되는지
// 알 수 없으므로 거부합니다.
func TestDuplicateRefIsRejected(t *testing.T) {
	_, err := loadOne(t, `
version: 1
queries:
  - ref: dup.query
    type: promql_range
    expr: sum(a{$__scope})
  - ref: dup.query
    type: promql_range
    expr: sum(b{$__scope})
`)
	if err == nil || !strings.Contains(err.Error(), "중복") {
		t.Fatalf("중복 ref가 거부되지 않았습니다: %v", err)
	}
}

// TestUnknownYAMLFieldIsRejected — 오타 필드는 조용히 무시되면 안 됩니다.
// maxDataPoint(오타)를 쓰면 의도한 한계가 적용되지 않은 채 돌게 됩니다.
func TestUnknownYAMLFieldIsRejected(t *testing.T) {
	_, err := loadOne(t, `
version: 1
queries:
  - ref: ok.query
    type: promql_range
    expr: sum(x{$__scope})
    maxDataPoint: 100
`)
	if err == nil {
		t.Fatal("알 수 없는 필드가 조용히 무시되었습니다")
	}
}

/* ── 렌더링 ─────────────────────────────────────────────────────────────── */

func mustLoad(t *testing.T, yaml string) Catalog {
	t.Helper()
	cat, err := loadOne(t, yaml)
	if err != nil {
		t.Fatal(err)
	}
	return cat
}

// TestScopeIsRenderedAsMatchers — Scope가 라벨 매처로 삽입되고, 이름의 특수문자가
// 라벨 값·정규식 양쪽에서 이스케이프되는지 확인합니다.
func TestScopeIsRenderedAsMatchers(t *testing.T) {
	cat := mustLoad(t, `
version: 1
queries:
  - ref: q
    type: promql_range
    expr: sum(rate(m{$__scope,container!=""}[$__rate]))
`)
	q, _ := cat.Query("q")

	got, err := q.Render(Scope{Namespace: `pay"ments`}, time.Minute, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, `namespace="pay\"ments"`) {
		t.Fatalf("라벨 값 이스케이프가 깨졌습니다: %s", got)
	}

	got, err = q.Render(Scope{Namespaces: []string{"a.b", "c"}, PodNames: []string{"p-1"}}, time.Minute, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, `namespace=~"^(a\\.b|c)$"`) || !strings.Contains(got, `pod=~"^(p-1)$"`) {
		t.Fatalf("허용 목록 매처가 틀렸습니다: %s", got)
	}

	// 전체 허용 사용자 — 매처 자리가 깨끗이 사라져야 합니다.
	got, err = q.Render(Scope{}, time.Minute, nil)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(got, "$__scope") || strings.Contains(got, "{,") {
		t.Fatalf("빈 Scope 렌더링이 깨졌습니다: %s", got)
	}
}

// TestVariableValuesAreConstrainedAndQuoted — 변수 값은 allowlist를 통과해야 하고,
// 통과해도 라벨 값 리터럴로만 들어갑니다. matcher 조각 삽입은 불가능합니다. (#9 완료 기준)
func TestVariableValuesAreConstrainedAndQuoted(t *testing.T) {
	cat := mustLoad(t, `
version: 1
queries:
  - ref: q
    type: promql_range
    expr: sum(m{$__scope,container=$container})
    variables:
      - name: container
        pattern: '[a-z0-9-]{1,63}'
`)
	q, _ := cat.Query("q")
	sc := Scope{Namespace: "payments"}

	got, err := q.Render(sc, time.Minute, map[string]string{"container": "app"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, `container="app"`) {
		t.Fatalf("변수가 라벨 값 리터럴로 렌더링되지 않았습니다: %s", got)
	}

	// matcher 조각을 끼워 넣으려는 값 — pattern에서 거부됩니다.
	if _, err := q.Render(sc, time.Minute, map[string]string{"container": `app",namespace!="x`}); err == nil {
		t.Fatal("allowlist를 벗어난 변수 값이 통과했습니다")
	}
	// 선언되지 않은 변수 — 거부됩니다.
	if _, err := q.Render(sc, time.Minute, map[string]string{"evil": "x"}); err == nil {
		t.Fatal("선언되지 않은 변수가 통과했습니다")
	}
	// 값 누락 — 거부됩니다. 조용히 빼고 실행하지 않습니다.
	if _, err := q.Render(sc, time.Minute, nil); err == nil {
		t.Fatal("변수 값 누락이 통과했습니다")
	}
}

// TestEffectiveStepHonorsMinStepAndMaxDataPoints — Step은 넓어질 수만 있습니다.
func TestEffectiveStepHonorsMinStepAndMaxDataPoints(t *testing.T) {
	cat := mustLoad(t, `
version: 1
queries:
  - ref: q
    type: promql_range
    expr: sum(m{$__scope})
    minStep: 120s
    maxDataPoints: 100
`)
	q, _ := cat.Query("q")

	if got := q.EffectiveStep(time.Minute, time.Hour); got != 2*time.Minute {
		t.Fatalf("minStep이 적용되지 않았습니다: %v", got)
	}
	step := q.EffectiveStep(time.Minute, 30*24*time.Hour)
	if points := int((30 * 24 * time.Hour) / step); points > 100 {
		t.Fatalf("maxDataPoints를 넘습니다: %d포인트", points)
	}
	if step%(2*time.Minute) != 0 {
		t.Fatalf("넓힌 Step은 minStep 적용 후 Step의 배수여야 합니다: %v", step)
	}
}
