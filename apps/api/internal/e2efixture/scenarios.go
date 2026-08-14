//go:build e2efixture

// Package e2efixture는 통합 E2E(#22)를 위한 **테스트 전용** 서버 픽스처입니다.
//
// 기본 프로덕션 번들(mock-off dist)과 실제 httpapi를 loopback 오리진 하나로
// 묶고, 가짜 informer(testcluster)와 demo 호환 데이터소스 위에 네 장애 시나리오를
// 결정적으로 재현합니다. 프로덕션 코드는 이 패키지를 임포트하지 않습니다 —
// exclusion_test가 이를 강제하고, 실행 바이너리(cmd/e2efixture)는 빌드 태그
// `e2efixture` 뒤에 있습니다.
package e2efixture

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/xenx96/k8s-dashboard/apps/api/internal/clusterstate"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/contract"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/datasource"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/datasource/demo"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/testcluster"
)

// Scenario는 운영자 장애 흐름 하나의 근원(root) 신원과 네 신호의 좌표입니다.
// Pod UID·시간축이 metric/log/event/alert에서 **같은 사실**을 가리키게 하는 것이
// corpus의 존재 이유입니다. 신원은 지어내지 않고 testcluster 상수를 빌려옵니다.
type Scenario struct {
	ID            string
	Title         string
	Namespace     string
	PodName       string
	PodUID        string
	WorkloadKind  string
	WorkloadName  string
	AlertName     string
	AlertSeverity string
	AlertSummary  string
	// LogMessage는 %d(시도 횟수) 하나를 받는 결정적 ERROR 로그 서식입니다.
	LogMessage string
	// EventReason은 testcluster 픽스처가 이 루트에 심어 둔 Warning 이벤트의 Reason입니다.
	EventReason string
}

// 시나리오 ID — 릴리스 기준(#22)이 요구하는 네 가지 전부입니다.
const (
	ScenarioCrashLoop = "crashloop"
	ScenarioImagePull = "imagepull"
	ScenarioCPUSpike  = "cpuspike"
	ScenarioErrorLog  = "errorlog"
)

// Scenarios는 고정된 네 시나리오 corpus입니다. 순서·값 모두 결정적입니다.
func Scenarios() []Scenario {
	return []Scenario{
		{
			ID: ScenarioCrashLoop, Title: "CrashLoopBackOff",
			Namespace: "payments", PodName: "payments-api-7f-bbb", PodUID: testcluster.UIDPodCrashLoop,
			WorkloadKind: "Deployment", WorkloadName: "payments-api",
			AlertName: "PodCrashLooping", AlertSeverity: "critical",
			AlertSummary: "컨테이너가 반복해서 재시작합니다",
			LogMessage:   "panic: connection refused to settlement upstream (restart %d)",
			EventReason:  "BackOff",
		},
		{
			ID: ScenarioImagePull, Title: "ImagePullBackOff",
			Namespace: "media", PodName: "media-api-1a-eee", PodUID: testcluster.UIDPodImagePull,
			WorkloadKind: "Deployment", WorkloadName: "media-api",
			AlertName: "PodImagePullBackOff", AlertSeverity: "warning",
			AlertSummary: "이미지를 받아오지 못해 Pod가 시작되지 않습니다",
			LogMessage:   "image pull failed: manifest unknown for media-api:1.43.0 (attempt %d)",
			EventReason:  "Failed",
		},
		{
			ID: ScenarioCPUSpike, Title: "CPU spike",
			Namespace: "payments", PodName: "batch-sync-qq81z", PodUID: testcluster.UIDPodBatchSync,
			WorkloadKind: "CronJob", WorkloadName: "batch-sync",
			AlertName: "CPUThrottlingHigh", AlertSeverity: "warning",
			AlertSummary: "CPU 사용률이 limit에 붙어 throttling이 발생합니다",
			LogMessage:   "batch window overrun: ledger reconciliation took %dms",
			EventReason:  "Unhealthy",
		},
		{
			ID: ScenarioErrorLog, Title: "Error log",
			Namespace: "media", PodName: "media-api-zzz", PodUID: testcluster.UIDPodMedia,
			WorkloadKind: "Deployment", WorkloadName: "media-api",
			AlertName: "HighErrorRate", AlertSeverity: "critical",
			AlertSummary: "5xx 비율이 임계치를 넘었습니다",
			LogMessage:   "transcode request failed: HTTP 502 from origin shard (attempt %d)",
			EventReason:  "Unhealthy",
		},
	}
}

// scenarioByID는 선택 목록을 검증합니다. 모르는 ID는 기동 실패(fail-fast)로 이어집니다.
func scenariosFor(ids []string) ([]Scenario, error) {
	all := Scenarios()
	if len(ids) == 0 {
		return all, nil
	}
	byID := map[string]Scenario{}
	for _, s := range all {
		byID[s.ID] = s
	}
	out := make([]Scenario, 0, len(ids))
	seen := map[string]bool{}
	for _, id := range ids {
		s, ok := byID[id]
		if !ok {
			return nil, fmt.Errorf("알 수 없는 시나리오 %q (사용 가능: crashloop|imagepull|cpuspike|errorlog)", id)
		}
		if seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, s)
	}
	return out, nil
}

/* ── 시나리오 데이터소스 ────────────────────────────────────────────────── */

// scenarioLogCount는 루트당 주입하는 ERROR 로그 줄 수입니다. 유계입니다.
const scenarioLogCount = 12

// fixtureMaxLines bounds both materialization and pagination work. The fixture
// needs only a compact deterministic corpus to prove the operator journey.
const fixtureMaxLines = 500

// alertActiveFor는 시나리오 알림이 발화 중인 기간입니다.
const alertActiveFor = 20 * time.Minute

// Source는 demo 어댑터 위에 시나리오 신호를 얹습니다.
// demo와 같은 규칙(결정적 값 · Pod 신원은 카탈로그에서)을 유지하면서,
// 루트 Pod에 한해 metric/log/alert가 시나리오와 일치하게 만듭니다.
type Source struct {
	*demo.Source
	scenarios []Scenario
}

// NewSource는 시나리오 corpus가 얹힌 데이터소스를 만듭니다.
func NewSource(store *clusterstate.Store, scenarios []Scenario) *Source {
	return &Source{Source: demo.New(store), scenarios: scenarios}
}

func (s *Source) scenarioFor(uid string) (Scenario, bool) {
	for _, sc := range s.scenarios {
		if sc.PodUID == uid {
			return sc, true
		}
	}
	return Scenario{}, false
}

// Trends는 CPU spike 루트 Pod의 CPU used 계열을 스파이크 모양으로 바꿉니다.
// 마지막 15분이 95% 이상으로 치솟아 화면에서 이상이 눈에 보입니다.
func (s *Source) Trends(ctx context.Context, t datasource.Target, w datasource.Window, panels []string) ([]contract.TrendPanel, error) {
	out, err := s.Source.Trends(ctx, t, w, panels)
	if err != nil {
		return out, err
	}
	sc, ok := s.scenarioFor(t.PodUID)
	if !ok || sc.ID != ScenarioCPUSpike {
		return out, nil
	}
	spikeFrom := w.To.Add(-15 * time.Minute)
	for pi := range out {
		if out[pi].ID != "cpu" {
			continue
		}
		for si := range out[pi].Series {
			if out[pi].Series[si].Key != "used" {
				continue
			}
			for i := range out[pi].Series[si].Points {
				p := &out[pi].Series[si].Points[i]
				if time.UnixMilli(p.T).Before(spikeFrom) {
					p.V = 18 + float64(i%5) // 낮은 baseline · 결정적 잔물결
				} else {
					p.V = 96 + float64(i%3)
				}
			}
		}
	}
	return out, nil
}

// scenarioLines는 조회 조건에 맞는 시나리오 ERROR 로그를 만듭니다.
// ID는 유일·안정("e2e-<scenario>-<n>")이고, 시각은 조회 구간 끝에서 역순입니다.
func (s *Source) scenarioLines(q datasource.LogQuery) []contract.LogLine {
	out := make([]contract.LogLine, 0, scenarioLogCount*len(s.scenarios))
	// Demo's first 500 lines cover the newest 10% of a valid query window.
	// Keeping scenario evidence inside 5% makes the top-500 merge exhaustive.
	spacing := q.Window.To.Sub(q.Window.From) / (scenarioLogCount * 20)
	if spacing <= 0 {
		spacing = time.Millisecond
	}
	for _, sc := range s.scenarios {
		if !q.Target.AllowsNamespace(sc.Namespace) {
			continue
		}
		if q.Target.PodUID != "" && q.Target.PodUID != sc.PodUID {
			continue
		}
		if q.Target.WorkloadName != "" && q.Target.WorkloadName != sc.WorkloadName {
			continue
		}
		if q.Container != "" && q.Container != "app" {
			continue
		}
		if len(q.Levels) > 0 && !hasLevel(q.Levels, contract.LevelError) {
			continue
		}
		for i := 0; i < scenarioLogCount; i++ {
			ts := q.Window.To.Add(-time.Duration(i+1) * spacing)
			if ts.Before(q.Window.From) {
				break
			}
			msg := fmt.Sprintf(sc.LogMessage, i+1)
			if q.Text != "" && !strings.Contains(strings.ToLower(msg), strings.ToLower(q.Text)) {
				continue
			}
			out = append(out, contract.LogLine{
				ID:            "e2e-" + sc.ID + "-" + strconv.Itoa(i),
				T:             ts.UnixMilli(),
				Level:         contract.LevelError,
				Message:       msg,
				Masked:        []contract.MaskedSpan{},
				Namespace:     sc.Namespace,
				PodName:       sc.PodName,
				PodUID:        sc.PodUID,
				ContainerName: "app",
				WorkloadKind:  sc.WorkloadKind,
				WorkloadName:  sc.WorkloadName,
				TraceID:       fmt.Sprintf("%016x", uint64(0xe2e0)+uint64(i)),
			})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].T == out[j].T {
			return out[i].ID < out[j].ID
		}
		return out[i].T > out[j].T
	})
	return out
}

type fixtureCursor struct {
	Offset int `json:"o"`
}

const maxFixtureCursorLength = 64

func encodeFixtureCursor(c fixtureCursor) string {
	b, _ := json.Marshal(c)
	return "e2e." + base64.RawURLEncoding.EncodeToString(b)
}

func decodeFixtureCursor(raw string) (fixtureCursor, error) {
	if raw == "" {
		return fixtureCursor{}, nil
	}
	if len(raw) > maxFixtureCursorLength || !strings.HasPrefix(raw, "e2e.") {
		return fixtureCursor{}, errors.New("invalid fixture cursor")
	}
	b, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(raw, "e2e."))
	if err != nil {
		return fixtureCursor{}, errors.New("invalid fixture cursor")
	}
	var c fixtureCursor
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil || c.Offset < 0 || c.Offset > fixtureMaxLines {
		return fixtureCursor{}, errors.New("invalid fixture cursor")
	}
	if dec.Decode(&struct{}{}) != io.EOF {
		return fixtureCursor{}, errors.New("invalid fixture cursor")
	}
	return c, nil
}

func (s *Source) Search(ctx context.Context, q datasource.LogQuery) (datasource.LogPage, error) {
	cur, err := decodeFixtureCursor(q.Cursor)
	if err != nil {
		return datasource.LogPage{}, err
	}
	size := q.PageSize
	if size <= 0 {
		size = demo.DefaultPageSize
	}
	if size > fixtureMaxLines {
		size = fixtureMaxLines
	}

	// Materialize one bounded demo window, then stable-merge it with scenario
	// evidence. Recomputing this deterministic test corpus keeps the cursor small
	// while preserving global timestamp ordering across every page boundary.
	scenario := s.scenarioLines(q)
	baseQuery := q
	baseQuery.Cursor = ""
	baseQuery.PageSize = fixtureMaxLines
	base, err := s.Source.Search(ctx, baseQuery)
	if err != nil {
		return datasource.LogPage{}, err
	}
	lines := append(scenario, base.Lines...)
	sort.SliceStable(lines, func(i, j int) bool {
		if lines[i].T == lines[j].T {
			return lines[i].ID < lines[j].ID
		}
		return lines[i].T > lines[j].T
	})
	total := len(lines)
	if len(lines) > fixtureMaxLines {
		lines = lines[:fixtureMaxLines]
	}
	if cur.Offset > len(lines) {
		return datasource.LogPage{}, errors.New("invalid fixture cursor")
	}
	end := cur.Offset + size
	if end > len(lines) {
		end = len(lines)
	}
	page := datasource.LogPage{
		Lines:     append([]contract.LogLine(nil), lines[cur.Offset:end]...),
		MaxLines:  fixtureMaxLines,
		Truncated: total > fixtureMaxLines || base.Next != "" || base.Truncated,
	}
	if end < len(lines) {
		page.Next = encodeFixtureCursor(fixtureCursor{Offset: end})
	}
	return page, nil
}

// Histogram은 시나리오 로그 수를 해당 버킷에 더합니다.
func (s *Source) Histogram(ctx context.Context, q datasource.LogQuery) ([]contract.LogHistogramBucket, error) {
	out, err := s.Source.Histogram(ctx, q)
	if err != nil || len(out) == 0 || q.Window.Step <= 0 {
		return out, err
	}
	for _, l := range s.scenarioLines(datasource.LogQuery{
		Target: q.Target, Window: q.Window, Levels: q.Levels, Container: q.Container, Text: q.Text,
	}) {
		idx := int(time.UnixMilli(l.T).Sub(q.Window.From) / q.Window.Step)
		if idx < 0 || idx >= len(out) {
			continue
		}
		out[idx].Counts[l.Level]++
	}
	return out, nil
}

// Facets keeps the demo identities but adds the exact number of injected lines
// matching the current query to each affected facet.
func (s *Source) Facets(ctx context.Context, q datasource.LogQuery) (contract.LogFacets, error) {
	out, err := s.Source.Facets(ctx, q)
	if err != nil {
		return out, err
	}
	for _, line := range s.scenarioLines(q) {
		for i := range out.Pods {
			if out.Pods[i].UID == line.PodUID {
				out.Pods[i].Count++
			}
		}
		for i := range out.Workloads {
			if out.Workloads[i].Name == line.WorkloadName && out.Workloads[i].Kind == line.WorkloadKind {
				out.Workloads[i].Count++
			}
		}
		for i := range out.Containers {
			if out.Containers[i].Name == line.ContainerName {
				out.Containers[i].Count++
			}
		}
	}
	return out, nil
}

// List는 시나리오 알림을 demo 알림 앞에 얹습니다. Entity는 루트 Pod UID를 가리키고
// StartsAt은 조회 구간 안에 있습니다 — Alert → Pod 상세 딥링크가 그대로 통합니다.
func (s *Source) List(ctx context.Context, q datasource.AlertQuery) (datasource.AlertResult, error) {
	res, err := s.Source.List(ctx, q)
	if err != nil {
		return res, err
	}
	extra := make([]contract.AlertInstance, 0, len(s.scenarios))
	for _, sc := range s.scenarios {
		if !q.Target.AllowsNamespace(sc.Namespace) {
			continue
		}
		start := q.Window.To.Add(-alertActiveFor)
		if start.Before(q.Window.From) {
			start = q.Window.From
		}
		extra = append(extra, contract.AlertInstance{
			ID:       "e2e-alert-" + sc.ID,
			Name:     sc.AlertName,
			Severity: sc.AlertSeverity,
			Status:   "firing",
			StartsAt: start.UTC().Format(time.RFC3339),
			Labels: map[string]string{
				"alertname": sc.AlertName,
				"namespace": sc.Namespace,
				"workload":  sc.WorkloadName,
				"pod":       sc.PodName,
				"severity":  sc.AlertSeverity,
			},
			Annotations: map[string]string{"summary": sc.AlertSummary},
			Entity: &contract.EntityRef{
				ClusterID:    q.Target.ClusterID,
				Namespace:    sc.Namespace,
				WorkloadKind: sc.WorkloadKind,
				WorkloadName: sc.WorkloadName,
				PodName:      sc.PodName,
				PodUID:       sc.PodUID,
			},
			EntityName: sc.PodName,
			Source:     "alertmanager",
			GroupSize:  1,
			GroupKey:   fmt.Sprintf("%s/%s/%s", sc.AlertName, sc.Namespace, sc.WorkloadName),
		})
	}
	res.Firing = append(extra, res.Firing...)
	return res, nil
}

func hasLevel(list []contract.LogLevel, v contract.LogLevel) bool {
	for _, l := range list {
		if l == v {
			return true
		}
	}
	return false
}
