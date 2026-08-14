// 서버 측 쿼리 카탈로그입니다.
//
// 프런트엔드는 PromQL을 보내지 못합니다. 화면이 고를 수 있는 것은 패널 id뿐이고,
// 실제 질의는 여기 정의된 표현식에 서버가 확정한 Scope 매처를 끼워 만듭니다.
// (README §10 — Raw Query 전달 금지, #9 Query Catalog의 최소 구현)
//
// 단위는 정직하게 둡니다. 데모 어댑터는 percent를 흉내 냈지만, 실제 저장소의
// 값은 cores·bytes이고 화면 계약(TrendSeries.Unit)은 시리즈별 단위를 그대로
// 전달할 수 있습니다. 요청량(request) 시리즈를 같은 단위로 나란히 두면
// 사용률은 화면에서 읽힙니다.
package greptime

import (
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/xenx96/k8s-dashboard/apps/api/internal/datasource"
)

// defaultPanelOrder는 패널 순서입니다. 데모 어댑터·UI와 같은 어휘를 씁니다.
var defaultPanelOrder = []string{"cpu", "memory", "network", "restarts"}

type seriesDef struct {
	key   string
	label string
	unit  string
	// expr은 Scope 매처와 rate 구간으로 완성되는 표현식입니다.
	expr func(sel string, step time.Duration) string
}

type panelDef struct {
	title  string
	series []seriesDef
}

// rateWindow는 rate/increase의 구간입니다. Step보다 좁으면 샘플 간격에 따라
// 구멍이 생기므로, Step의 2배와 2분 중 큰 쪽을 씁니다.
func rateWindow(step time.Duration) string {
	w := 2 * step
	if w < 2*time.Minute {
		w = 2 * time.Minute
	}
	return promDuration(w)
}

var panelDefs = map[string]panelDef{
	"cpu": {
		title: "CPU 사용량",
		series: []seriesDef{
			{key: "used", label: "사용", unit: "cores", expr: func(sel string, step time.Duration) string {
				return fmt.Sprintf(`sum(rate(container_cpu_usage_seconds_total{%s}[%s]))`,
					joinMatchers(sel, `container!=""`), rateWindow(step))
			}},
			{key: "requested", label: "Request", unit: "cores", expr: func(sel string, _ time.Duration) string {
				return fmt.Sprintf(`sum(kube_pod_container_resource_requests{%s})`,
					joinMatchers(sel, `resource="cpu"`))
			}},
		},
	},
	"memory": {
		title: "메모리 사용량",
		series: []seriesDef{
			{key: "used", label: "사용", unit: "bytes", expr: func(sel string, _ time.Duration) string {
				return fmt.Sprintf(`sum(container_memory_working_set_bytes{%s})`,
					joinMatchers(sel, `container!=""`))
			}},
			{key: "requested", label: "Request", unit: "bytes", expr: func(sel string, _ time.Duration) string {
				return fmt.Sprintf(`sum(kube_pod_container_resource_requests{%s})`,
					joinMatchers(sel, `resource="memory"`))
			}},
		},
	},
	"network": {
		title: "네트워크",
		series: []seriesDef{
			{key: "rx", label: "수신", unit: "bytes_per_sec", expr: func(sel string, step time.Duration) string {
				return fmt.Sprintf(`sum(rate(container_network_receive_bytes_total{%s}[%s]))`, sel, rateWindow(step))
			}},
			{key: "tx", label: "송신", unit: "bytes_per_sec", expr: func(sel string, step time.Duration) string {
				return fmt.Sprintf(`sum(rate(container_network_transmit_bytes_total{%s}[%s]))`, sel, rateWindow(step))
			}},
		},
	},
	"restarts": {
		title: "컨테이너 재시작",
		series: []seriesDef{
			{key: "restarts", label: "재시작", unit: "count", expr: func(sel string, step time.Duration) string {
				return fmt.Sprintf(`sum(increase(kube_pod_container_status_restarts_total{%s}[%s]))`, sel, rateWindow(step))
			}},
		},
	},
}

/* ── Scope 매처 ─────────────────────────────────────────────────────────── */

// scopeSelector는 Target을 라벨 매처로 바꿉니다. 두 번째 반환값이 false면
// 대상 Pod가 카탈로그에 없다는 뜻입니다 — 질의하지 말고 빈 시리즈를 둡니다.
//
// 메트릭 라벨은 pod **이름**이므로 UID·워크로드는 카탈로그에서 이름 집합으로
// 풀어서 매칭합니다. 신원을 카탈로그에서 빌려오는 규칙 그대로입니다. (CLAUDE.md)
func (s *Source) scopeSelector(t datasource.Target) (string, bool) {
	var parts []string

	switch {
	case t.Namespace != "":
		parts = append(parts, `namespace=`+quoteLabel(t.Namespace))
	case len(t.Namespaces) > 0:
		parts = append(parts, `namespace=~`+quoteLabel(anchoredAlternation(t.Namespaces)))
	}

	switch {
	case t.PodUID != "":
		pod, ok := s.podByUID(t)
		if !ok {
			return "", false
		}
		parts = append(parts, `pod=`+quoteLabel(pod.Name), `namespace=`+quoteLabel(pod.Namespace))
	case t.WorkloadName != "":
		names := s.workloadPodNames(t)
		if len(names) == 0 {
			return "", false
		}
		parts = append(parts, `pod=~`+quoteLabel(anchoredAlternation(names)))
	}

	return strings.Join(parts, ","), true
}

func (s *Source) podByUID(t datasource.Target) (datasource.CatalogPod, bool) {
	for _, p := range s.catalog.CatalogPods(t.Namespace, 0) {
		if p.UID == t.PodUID {
			return p, true
		}
	}
	return datasource.CatalogPod{}, false
}

func (s *Source) workloadPodNames(t datasource.Target) []string {
	var names []string
	for _, p := range s.catalog.CatalogPods(t.Namespace, 0) {
		if p.WorkloadName != t.WorkloadName {
			continue
		}
		if t.WorkloadKind != "" && !strings.EqualFold(p.WorkloadKind, t.WorkloadKind) {
			continue
		}
		if !t.AllowsNamespace(p.Namespace) {
			continue
		}
		names = append(names, p.Name)
	}
	return names
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

func joinMatchers(sel string, extra string) string {
	if sel == "" {
		return extra
	}
	return sel + "," + extra
}
