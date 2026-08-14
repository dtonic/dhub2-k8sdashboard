package querycatalog

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestPanelAndRefsLookup — 패널 id 조회와 진단용 ref 목록입니다.
func TestPanelAndRefsLookup(t *testing.T) {
	cat, err := LoadDefault()
	if err != nil {
		t.Fatal(err)
	}
	if p, ok := cat.Panel("cpu"); !ok || p.Title == "" || len(p.Series) != 2 {
		t.Fatalf("cpu 패널 조회: %+v", p)
	}
	if _, ok := cat.Panel("__nope"); ok {
		t.Fatal("없는 패널이 조회되었습니다")
	}
	refs := cat.Refs()
	if len(refs) == 0 {
		t.Fatal("ref 목록이 비었습니다")
	}
	for i := 1; i < len(refs); i++ {
		if refs[i-1] >= refs[i] {
			t.Fatal("ref 목록은 정렬되어야 합니다")
		}
	}
}

// TestLoadDirReplacesDefaults — 디렉터리 로드는 기본 카탈로그를 대체합니다.
// 절반은 파일, 절반은 임베드면 어느 정의가 실행되는지 추적할 수 없습니다.
func TestLoadDirReplacesDefaults(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "own.yaml"), []byte(`
version: 1
queries:
  - ref: custom.only
    type: promql_range
    expr: sum(x{$__scope})
`), 0o644); err != nil {
		t.Fatal(err)
	}

	cat, err := LoadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := cat.Query("custom.only"); !ok {
		t.Fatal("디렉터리 카탈로그가 로드되지 않았습니다")
	}
	if _, ok := cat.Query("metrics.cpu.used"); ok {
		t.Fatal("기본 카탈로그가 병합되었습니다 — 대체여야 합니다")
	}

	// 존재하지 않는 디렉터리·yaml 없는 디렉터리는 오류입니다.
	if _, err := LoadDir(filepath.Join(dir, "no-such")); err == nil {
		t.Fatal("없는 디렉터리가 통과했습니다")
	}
	empty := t.TempDir()
	if _, err := LoadDir(empty); err == nil {
		t.Fatal("빈 디렉터리가 통과했습니다")
	}
}

// TestLoadPathChoosesSource — 빈 값은 임베디드, 경로는 그 디렉터리입니다.
func TestLoadPathChoosesSource(t *testing.T) {
	if _, err := LoadPath(""); err != nil {
		t.Fatalf("기본 경로 로드 실패: %v", err)
	}
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "c.yaml"), []byte(`
version: 1
queries:
  - ref: from.dir
    type: promql_range
    expr: sum(x{$__scope})
`), 0o644) //nolint:errcheck
	cat, err := LoadPath(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := cat.Query("from.dir"); !ok {
		t.Fatal("경로 로드가 디렉터리를 보지 않았습니다")
	}
}

// TestVariableValuesEnumBranch — pattern이 아니라 values 열거로 제한하는 경우입니다.
func TestVariableValuesEnumBranch(t *testing.T) {
	cat := mustLoad(t, `
version: 1
queries:
  - ref: q
    type: promql_range
    expr: sum(m{$__scope,container=$container})
    variables:
      - name: container
        values: [app, sidecar]
`)
	q, _ := cat.Query("q")
	sc := Scope{Namespace: "payments"}

	if _, err := q.Render(sc, time.Minute, map[string]string{"container": "sidecar"}); err != nil {
		t.Fatalf("열거된 값이 거절되었습니다: %v", err)
	}
	if _, err := q.Render(sc, time.Minute, map[string]string{"container": "other"}); err == nil {
		t.Fatal("열거 밖 값이 통과했습니다")
	}
}

// TestMoreLoadRejections — 나머지 로드 거부 조건들입니다.
func TestMoreLoadRejections(t *testing.T) {
	for name, yaml := range map[string]string{
		"version 미지원": `
version: 2
queries: []
`,
		"빈 ref": `
version: 1
queries:
  - ref: ""
    type: promql_range
    expr: sum(x{$__scope})
`,
		"모르는 type": `
version: 1
queries:
  - ref: q
    type: sql_select
    expr: SELECT 1
`,
		"빈 expr": `
version: 1
queries:
  - ref: q
    type: promql_range
    expr: "  "
`,
		"clusterWide 없는 instant": `
version: 1
queries:
  - ref: q
    type: promql_instant
    expr: sum(x)
`,
		"이름 없는 변수": `
version: 1
queries:
  - ref: q
    type: promql_range
    expr: sum(x{$__scope})
    variables:
      - pattern: 'a+'
`,
		"__ 접두 변수": `
version: 1
queries:
  - ref: q
    type: promql_range
    expr: sum(x{$__scope})
    variables:
      - name: __rate
        values: [x]
`,
		"제한 없는 변수": `
version: 1
queries:
  - ref: q
    type: promql_range
    expr: sum(x{$__scope})
    variables:
      - name: v
`,
		"깨진 pattern": `
version: 1
queries:
  - ref: q
    type: promql_range
    expr: sum(x{$__scope})
    variables:
      - name: v
        pattern: '['
`,
		"패널 id 중복": `
version: 1
queries:
  - ref: q
    type: promql_range
    expr: sum(x{$__scope})
panels:
  - id: a
    title: A
    series: [{key: k, label: l, query: q}]
  - id: a
    title: A2
    series: [{key: k, label: l, query: q}]
`,
		"instant를 패널에 연결": `
version: 1
queries:
  - ref: q
    type: promql_instant
    clusterWide: true
    expr: sum(x)
panels:
  - id: a
    title: A
    series: [{key: k, label: l, query: q}]
`,
	} {
		if _, err := loadOne(t, yaml); err == nil {
			t.Fatalf("%s: 거부되지 않았습니다", name)
		}
	}
}

// TestRateWindowRespectsMinStep — rate 구간은 minStep 이상, 최소 2분입니다.
func TestRateWindowRespectsMinStep(t *testing.T) {
	cat := mustLoad(t, `
version: 1
queries:
  - ref: q
    type: promql_range
    expr: sum(rate(m{$__scope}[$__rate]))
    minStep: 300s
`)
	q, _ := cat.Query("q")
	got, err := q.Render(Scope{Namespace: "x"}, time.Minute, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "[600s]") { // minStep 300s의 2배
		t.Fatalf("rate 구간이 minStep을 무시했습니다: %s", got)
	}
}
