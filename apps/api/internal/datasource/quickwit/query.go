// Elasticsearch 호환 질의 DSL 조립입니다.
//
// 구조상 우회가 불가능하게 만듭니다 — Scope·레벨·대상 필터는 전부 bool.filter의
// term/terms/range 노드이고, 사용자 검색어는 match 노드의 **값**으로만 들어갑니다.
// 문자열 연결로 질의를 만들지 않으므로 이스케이프 실수가 존재할 수 없습니다.
package quickwit

import (
	"github.com/xenx96/k8s-dashboard/apps/api/internal/contract"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/datasource"
)

// searchBody는 최초 scroll snapshot 조회 본문입니다. offset(from)은 쓰지 않습니다.
func (s *Source) searchBody(q datasource.LogQuery, size int) map[string]any {
	return map[string]any{
		"size": size,
		"sort": []any{
			map[string]any{s.cfg.Fields.Timestamp: map[string]any{"order": "desc"}},
		},
		"query": s.boolQuery(q),
	}
}

func (s *Source) boolQuery(q datasource.LogQuery) map[string]any {
	f := s.cfg.Fields
	var filter []any
	if s.cfg.ClusterScoped {
		filter = append(filter, term(f.Cluster, q.Target.ClusterID))
	}

	// 시간 범위는 initial scroll snapshot을 만들 때 한 번 강제합니다.
	rng := map[string]any{
		"gte": q.Window.From.UnixMilli(),
		"lte": q.Window.To.UnixMilli(),
	}
	filter = append(filter, map[string]any{"range": map[string]any{f.Timestamp: rng}})

	// Scope — 서버가 확정한 값입니다. 사용자 입력이 여기 들어올 경로는 없습니다.
	switch {
	case q.Target.Namespace != "":
		filter = append(filter, term(f.Namespace, q.Target.Namespace))
	case len(q.Target.Namespaces) > 0:
		filter = append(filter, terms(f.Namespace, q.Target.Namespaces...))
	}
	if q.Target.PodUID != "" {
		filter = append(filter, term(f.PodUID, q.Target.PodUID))
	}
	if q.Target.WorkloadName != "" {
		filter = append(filter, term(f.WorkloadName, q.Target.WorkloadName))
	}
	if q.Target.WorkloadKind != "" {
		filter = append(filter, term(f.WorkloadKind, q.Target.WorkloadKind))
	}
	if q.Container != "" {
		filter = append(filter, term(f.Container, q.Container))
	}
	if len(q.Levels) > 0 {
		var values []string
		for _, l := range q.Levels {
			values = append(values, levelVariants(l)...)
		}
		filter = append(filter, terms(f.Level, values...))
	}

	b := map[string]any{"filter": filter}

	// 사용자 검색어 — match 질의의 값입니다. 연산자·필드 지정이 불가능합니다.
	if q.Text != "" {
		b["must"] = []any{map[string]any{
			"match": map[string]any{
				f.Message: map[string]any{"query": q.Text, "operator": "AND"},
			},
		}}
	}
	return map[string]any{"bool": b}
}

func term(field, value string) map[string]any {
	return map[string]any{"term": map[string]any{field: map[string]any{"value": value}}}
}

func terms(field string, values ...string) map[string]any {
	anyValues := make([]any, len(values))
	for i, v := range values {
		anyValues[i] = v
	}
	return map[string]any{"terms": map[string]any{field: anyValues}}
}

// levelVariants는 저장된 레벨 표기의 대소문자 편차를 흡수합니다.
func levelVariants(l contract.LogLevel) []string {
	v := string(l)
	return []string{v, lower(v), title(v)}
}

func lower(s string) string {
	b := []byte(s)
	for i := range b {
		if b[i] >= 'A' && b[i] <= 'Z' {
			b[i] += 'a' - 'A'
		}
	}
	return string(b)
}

func title(s string) string {
	if s == "" {
		return s
	}
	l := lower(s)
	return string(s[0]) + l[1:]
}
