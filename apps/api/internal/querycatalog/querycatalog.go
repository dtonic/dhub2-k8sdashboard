// Package querycatalog는 **등록형 쿼리 카탈로그**입니다. (#9)
//
// UI는 Raw PromQL/SQL/Quickwit Query를 보낼 수 없습니다(README §10). 화면이 아는 것은
// queryRef(패널 id·시리즈 키)뿐이고, 실제 질의문은 Git에 커밋된 카탈로그 파일에서
// 로드됩니다. 같은 지표는 어느 화면에서든 같은 정의를 씁니다 — 정의가 한 곳이기
// 때문입니다.
//
// 안전 장치는 세 겹입니다.
//  1. **등록되지 않은 queryRef는 실행 경로가 없습니다.** 어댑터는 카탈로그가 돌려준
//     정의만 실행할 수 있습니다.
//  2. **템플릿 변수는 allowlist입니다.** 선언되지 않은 변수가 표현식에 있으면 로드가
//     실패하고, 변수 값은 항상 escape된 라벨 값 리터럴로만 렌더링됩니다.
//     matcher·SQL fragment를 끼워 넣을 자리가 없습니다.
//  3. **Scope는 렌더링 시점에 서버가 삽입합니다.** range 질의 표현식에 $__scope가
//     없으면 로드 단계에서 거부합니다. Scope를 깜빡한 질의는 존재할 수 없습니다.
//
// 카탈로그 오류는 시작 단계(main)와 CI(TestDefaultCatalogIsValid)에서 검출됩니다.
// Raw query expert mode는 MVP 비목표입니다 — 이 패키지에 그 경로를 만들지 않습니다.
package querycatalog

import (
	"embed"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

//go:embed defaults/*.yaml
var defaultsFS embed.FS

// Type은 질의 종류입니다. 어댑터는 자신이 아는 Type만 실행합니다.
type Type string

const (
	// TypePromRange는 Prometheus 호환 range query입니다. $__scope가 필수입니다.
	TypePromRange Type = "promql_range"
	// TypePromInstant는 instant query입니다. clusterWide 선언 시 $__scope를 생략할 수 있습니다.
	TypePromInstant Type = "promql_instant"
)

// Limits는 쿼리별 실행 한계 선언입니다. 어댑터 공통 어휘입니다 —
// 메트릭·로그 어댑터가 같은 구조를 읽습니다. (#9 "공통 실행 인터페이스")
type Limits struct {
	// Timeout은 질의 1건의 상한입니다.
	Timeout time.Duration `yaml:"timeout"`
	// MaxRange는 최대 조회 기간입니다. 넘는 요청은 실행하지 않습니다.
	MaxRange time.Duration `yaml:"maxRange"`
	// MaxDataPoints는 시리즈당 최대 포인트입니다. 넘으면 Step을 넓힙니다.
	MaxDataPoints int `yaml:"maxDataPoints"`
	// MaxPageSize·MaxLines는 로그 조회 한계입니다.
	MaxPageSize int `yaml:"maxPageSize"`
	MaxLines    int `yaml:"maxLines"`
}

// Variable은 표현식이 받을 수 있는 추가 변수의 allowlist 항목입니다.
// Values(열거)나 Pattern(정규식) 중 하나로 값을 제한합니다. 둘 다 없으면 로드 오류입니다.
type Variable struct {
	Name    string   `yaml:"name"`
	Values  []string `yaml:"values"`
	Pattern string   `yaml:"pattern"`

	re *regexp.Regexp
}

// Query는 등록된 질의 하나입니다.
type Query struct {
	Ref  string `yaml:"ref"`
	Type Type   `yaml:"type"`
	Unit string `yaml:"unit"`
	// Expr은 질의 템플릿입니다. 자리표시자는 $__scope, $__rate와 선언된 변수뿐입니다.
	Expr string `yaml:"expr"`
	// ClusterWide는 Scope 없이 클러스터 전체를 읽는 질의임을 **명시적으로** 선언합니다.
	// (사용량 스냅숏처럼 서버 내부 용도만 해당합니다. 화면 질의에 쓰지 않습니다.)
	ClusterWide bool       `yaml:"clusterWide"`
	Variables   []Variable `yaml:"variables"`
	// MinStep보다 좁은 Step 요청은 MinStep으로 올립니다.
	MinStep time.Duration `yaml:"minStep"`
	Limits  Limits        `yaml:",inline"`
}

// PanelSeries는 화면 시리즈 → queryRef 연결입니다.
type PanelSeries struct {
	Key      string `yaml:"key"`
	Label    string `yaml:"label"`
	QueryRef string `yaml:"query"`
}

// Panel은 화면이 고를 수 있는 패널입니다. UI는 이 id만 보낼 수 있습니다.
type Panel struct {
	ID     string        `yaml:"id"`
	Title  string        `yaml:"title"`
	Series []PanelSeries `yaml:"series"`
}

// LogLimits는 로그 어댑터가 읽는 한계 선언입니다.
type LogLimits struct {
	Search    Limits `yaml:"search"`
	Histogram Limits `yaml:"histogram"`
	Facets    Limits `yaml:"facets"`
}

// Catalog는 로드·검증이 끝난 카탈로그입니다. 이후에는 읽기 전용입니다.
type Catalog struct {
	queries map[string]Query
	panels  []Panel
	logs    LogLimits
}

// Query는 등록된 질의를 돌려줍니다. 없는 ref는 실행할 방법이 없습니다.
func (c Catalog) Query(ref string) (Query, bool) {
	q, ok := c.queries[ref]
	return q, ok
}

// Panels는 화면 패널 정의를 선언 순서대로 돌려줍니다.
func (c Catalog) Panels() []Panel { return c.panels }

// Panel은 id로 패널을 찾습니다.
func (c Catalog) Panel(id string) (Panel, bool) {
	for _, p := range c.panels {
		if p.ID == id {
			return p, true
		}
	}
	return Panel{}, false
}

// Logs는 로그 조회 한계를 돌려줍니다.
func (c Catalog) Logs() LogLimits { return c.logs }

// Refs는 등록된 queryRef 목록입니다(진단·문서용).
func (c Catalog) Refs() []string {
	out := make([]string, 0, len(c.queries))
	for r := range c.queries {
		out = append(out, r)
	}
	sort.Strings(out)
	return out
}

/* ── 로드 ───────────────────────────────────────────────────────────────── */

// catalogFile은 YAML 파일 하나의 형식입니다.
type catalogFile struct {
	Version int       `yaml:"version"`
	Queries []Query   `yaml:"queries"`
	Panels  []Panel   `yaml:"panels"`
	Logs    LogLimits `yaml:"logs"`
}

// LoadDefault는 바이너리에 임베드된 기본 카탈로그를 로드합니다.
// Git에 커밋된 defaults/*.yaml이 그대로 실행 파일에 들어갑니다 —
// "Git에서 로드"가 배포 산출물에서도 성립합니다.
func LoadDefault() (Catalog, error) {
	sub, err := fs.Sub(defaultsFS, "defaults")
	if err != nil {
		return Catalog{}, err
	}
	return LoadFS(sub)
}

// LoadDir은 운영 환경에서 카탈로그 디렉터리를 지정할 때 씁니다(QUERY_CATALOG_DIR).
// 기본 카탈로그를 **대체**합니다. 병합하지 않습니다 — 절반은 파일, 절반은 임베드면
// 어느 정의가 실행되는지 추적할 수 없습니다.
func LoadDir(dir string) (Catalog, error) {
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() {
		return Catalog{}, fmt.Errorf("쿼리 카탈로그 디렉터리를 열 수 없습니다: %s", dir)
	}
	return LoadFS(os.DirFS(dir))
}

// LoadFS는 파일시스템의 *.yaml을 전부 로드하고 검증합니다.
// 오류는 파일·ref 단위로 모아 한 번에 보고합니다 — CI 로그에서 한 번에 고칠 수 있어야 합니다.
func LoadFS(fsys fs.FS) (Catalog, error) {
	files, err := fs.Glob(fsys, "*.yaml")
	if err != nil {
		return Catalog{}, err
	}
	sort.Strings(files)
	if len(files) == 0 {
		return Catalog{}, fmt.Errorf("쿼리 카탈로그가 비어 있습니다 (*.yaml 없음)")
	}

	cat := Catalog{queries: map[string]Query{}}
	var errs []string
	fail := func(file, format string, args ...any) {
		errs = append(errs, file+": "+fmt.Sprintf(format, args...))
	}

	for _, name := range files {
		raw, err := fs.ReadFile(fsys, name)
		if err != nil {
			fail(name, "읽기 실패: %v", err)
			continue
		}
		var f catalogFile
		dec := yaml.NewDecoder(strings.NewReader(string(raw)))
		dec.KnownFields(true) // 오타 필드는 조용히 무시하지 않고 오류로 만듭니다.
		if err := dec.Decode(&f); err != nil {
			fail(name, "YAML 해석 실패: %v", err)
			continue
		}
		if f.Version != 1 {
			fail(name, "지원하지 않는 version: %d", f.Version)
			continue
		}
		for i := range f.Queries {
			q := f.Queries[i]
			if msg := validateQuery(&q); msg != "" {
				fail(name, "query %q: %s", q.Ref, msg)
				continue
			}
			if _, dup := cat.queries[q.Ref]; dup {
				fail(name, "query %q: ref가 중복됩니다", q.Ref)
				continue
			}
			cat.queries[q.Ref] = q
		}
		cat.panels = append(cat.panels, f.Panels...)
		if f.Logs != (LogLimits{}) {
			cat.logs = f.Logs
		}
	}

	// 패널 검증은 모든 파일의 query가 모인 뒤에 합니다.
	seenPanel := map[string]bool{}
	for _, p := range cat.panels {
		if p.ID == "" || len(p.Series) == 0 {
			errs = append(errs, fmt.Sprintf("panel %q: id와 series가 필요합니다", p.ID))
			continue
		}
		if seenPanel[p.ID] {
			errs = append(errs, fmt.Sprintf("panel %q: id가 중복됩니다", p.ID))
			continue
		}
		seenPanel[p.ID] = true
		for _, s := range p.Series {
			q, ok := cat.queries[s.QueryRef]
			if !ok {
				errs = append(errs, fmt.Sprintf("panel %q/%s: 등록되지 않은 query %q", p.ID, s.Key, s.QueryRef))
				continue
			}
			if q.Type != TypePromRange {
				errs = append(errs, fmt.Sprintf("panel %q/%s: 화면 시리즈는 %s여야 합니다", p.ID, s.Key, TypePromRange))
			}
		}
	}

	if len(errs) > 0 {
		return Catalog{}, fmt.Errorf("쿼리 카탈로그 검증 실패:\n  %s", strings.Join(errs, "\n  "))
	}
	return cat, nil
}

/* ── 검증 ───────────────────────────────────────────────────────────────── */

// placeholderRe는 표현식의 자리표시자를 찾습니다. $__builtin과 $variable 두 종류입니다.
var placeholderRe = regexp.MustCompile(`\$(__)?[a-zA-Z_][a-zA-Z0-9_]*`)

// builtins는 서버가 렌더링하는 자리표시자입니다. 이 밖의 $__는 오류입니다.
var builtins = map[string]bool{"$__scope": true, "$__rate": true}

func validateQuery(q *Query) string {
	if q.Ref == "" {
		return "ref가 비어 있습니다"
	}
	if q.Type != TypePromRange && q.Type != TypePromInstant {
		return fmt.Sprintf("알 수 없는 type %q", q.Type)
	}
	if strings.TrimSpace(q.Expr) == "" {
		return "expr이 비어 있습니다"
	}

	declared := map[string]*Variable{}
	for i := range q.Variables {
		v := &q.Variables[i]
		if v.Name == "" {
			return "이름 없는 변수가 있습니다"
		}
		if strings.HasPrefix(v.Name, "__") {
			return fmt.Sprintf("변수 %q: __ 접두사는 내장 자리표시자 전용입니다", v.Name)
		}
		if len(v.Values) == 0 && v.Pattern == "" {
			return fmt.Sprintf("변수 %q: values 또는 pattern으로 값을 제한해야 합니다", v.Name)
		}
		if v.Pattern != "" {
			re, err := regexp.Compile("^(?:" + v.Pattern + ")$")
			if err != nil {
				return fmt.Sprintf("변수 %q: pattern이 정규식이 아닙니다: %v", v.Name, err)
			}
			v.re = re
		}
		declared["$"+v.Name] = v
	}

	usesScope := false
	for _, ph := range placeholderRe.FindAllString(q.Expr, -1) {
		if strings.HasPrefix(ph, "$__") {
			if !builtins[ph] {
				return fmt.Sprintf("알 수 없는 내장 자리표시자 %s", ph)
			}
			if ph == "$__scope" {
				usesScope = true
			}
			continue
		}
		if _, ok := declared[ph]; !ok {
			return fmt.Sprintf("선언되지 않은 변수 %s — 변수는 allowlist입니다", ph)
		}
	}

	// Scope 강제 — range 질의가 Scope를 잊는 것은 설정 실수가 아니라 데이터 유출입니다.
	// 로드 단계에서 거부해 그런 질의가 존재할 수 없게 합니다.
	if q.Type == TypePromRange && !usesScope {
		return "$__scope가 없습니다 — range 질의는 Scope 삽입 지점이 필수입니다"
	}
	if q.Type == TypePromInstant && !usesScope && !q.ClusterWide {
		return "$__scope가 없습니다 — 클러스터 전체 질의라면 clusterWide: true를 명시하세요"
	}

	if q.Limits.Timeout <= 0 {
		q.Limits.Timeout = 10 * time.Second
	}
	if q.Limits.MaxRange <= 0 {
		q.Limits.MaxRange = 30 * 24 * time.Hour
	}
	if q.Limits.MaxDataPoints <= 0 {
		q.Limits.MaxDataPoints = 1000
	}
	return ""
}

// LoadPath는 설정값 하나로 LoadDefault/LoadDir을 고릅니다.
func LoadPath(dir string) (Catalog, error) {
	if dir == "" {
		return LoadDefault()
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return Catalog{}, err
	}
	return LoadDir(abs)
}
