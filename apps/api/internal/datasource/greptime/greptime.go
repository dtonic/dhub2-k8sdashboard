// Package greptime은 GreptimeDB 메트릭 어댑터입니다. (#6)
//
// GreptimeDB의 Prometheus 호환 HTTP API(/v1/prometheus/api/v1/*)로 조회합니다.
// 프런트엔드는 PromQL을 알지 못합니다 — 질의는 전부 이 패키지의 **서버 측
// 쿼리 카탈로그**(queries.go)에서 나오고, Scope(namespace·pod)는 서버가
// 라벨 매처로 강제 삽입합니다. (README §10)
//
// Pod 신원은 informer 캐시(PodCatalog)에서 빌려옵니다. 메트릭 저장소의 라벨은
// pod **이름**이지만 화면의 신원은 **UID**이므로, UID → 이름 변환은 항상
// 카탈로그를 거칩니다. 여기서 이름을 지어내면 딥링크가 404가 됩니다. (CLAUDE.md)
package greptime

import (
	"context"
	"fmt"
	"math"
	"net/url"
	"sort"
	"strconv"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/xenx96/k8s-dashboard/apps/api/internal/contract"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/datasource"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/datasource/upstream"
)

// Config는 어댑터 구성입니다. 기본값은 데이터소스에 가장 안전한 쪽입니다.
type Config struct {
	// BaseURL은 GreptimeDB HTTP 주소입니다. 예: http://greptimedb:4000
	BaseURL string
	// DB는 GreptimeDB 데이터베이스 이름입니다. 비우면 public입니다.
	DB       string
	Username string
	Password string
	// Timeout은 질의 1건의 상한입니다. 0이면 10초입니다.
	Timeout time.Duration
	// MaxDataPoints는 시리즈당 최대 포인트 수입니다. 범위가 넓어 Step으로
	// 이 수를 넘으면 **Step을 서버가 넓힙니다.** 브라우저에 대량 포인트를
	// 그대로 보내지 않기 위한 마지막 방어선입니다. (README §11)
	MaxDataPoints int
	// MaxConcurrent는 화면 1회 그리기에서 GreptimeDB로 나가는 동시 질의 상한입니다.
	MaxConcurrent int
}

func (c Config) withDefaults() Config {
	if c.DB == "" {
		c.DB = "public"
	}
	if c.Timeout <= 0 {
		c.Timeout = 10 * time.Second
	}
	if c.MaxDataPoints <= 0 {
		c.MaxDataPoints = 1000
	}
	if c.MaxConcurrent <= 0 {
		c.MaxConcurrent = 4
	}
	return c
}

// Source는 datasource.Metrics 구현입니다.
type Source struct {
	cfg     Config
	client  *upstream.Client
	catalog datasource.PodCatalog
}

// New는 어댑터를 만듭니다. catalog는 Pod UID → 이름 변환에 필요합니다.
func New(cfg Config, catalog datasource.PodCatalog) (*Source, error) {
	cfg = cfg.withDefaults()
	client, err := upstream.New(upstream.Config{
		BaseURL:  cfg.BaseURL,
		What:     "GreptimeDB",
		Username: cfg.Username,
		Password: cfg.Password,
		Timeout:  cfg.Timeout,
		Headers:  map[string]string{"X-Greptime-DB-Name": cfg.DB},
	})
	if err != nil {
		return nil, err
	}
	return &Source{cfg: cfg, client: client, catalog: catalog}, nil
}

/* ── Trends ─────────────────────────────────────────────────────────────── */

// Trends는 화면에 그릴 패널 묶음을 돌려줍니다.
//
// 시리즈마다 range query 1건이 나가지만, 동시성은 MaxConcurrent로 묶습니다.
// 화면 하나가 데이터소스를 두들기는 총량이 예측 가능해야 합니다. (ADR 0002)
func (s *Source) Trends(ctx context.Context, t datasource.Target, w datasource.Window, panels []string) ([]contract.TrendPanel, error) {
	if len(panels) == 0 {
		panels = defaultPanelOrder
	}

	sel, found := s.scopeSelector(t)
	step := s.effectiveStep(w)

	out := make([]contract.TrendPanel, 0, len(panels))
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(s.cfg.MaxConcurrent)

	for _, id := range panels {
		def, ok := panelDefs[id]
		if !ok {
			continue
		}
		p := contract.TrendPanel{
			ID:          id,
			Title:       def.title,
			StepSeconds: int(step.Seconds()),
			Series:      make([]contract.TrendSeries, len(def.series)),
		}
		out = append(out, p)
		idx := len(out) - 1

		for si, sd := range def.series {
			out[idx].Series[si] = contract.TrendSeries{
				Key:    sd.key,
				Label:  sd.label,
				Unit:   sd.unit,
				Points: []contract.TrendPoint{},
			}
			// 대상 Pod가 카탈로그에 없으면(재시작 직후 등) 질의 없이 빈 시리즈를 둡니다.
			// "데이터 없음"이지 "장애"가 아닙니다.
			if !found {
				continue
			}
			si, sd := si, sd
			g.Go(func() error {
				pts, err := s.rangeQuery(gctx, sd.expr(sel, step), w.From, w.To, step)
				if err != nil {
					return err
				}
				out[idx].Series[si].Points = pts
				return nil
			})
		}
	}
	if err := g.Wait(); err != nil {
		return nil, err
	}
	return out, nil
}

// effectiveStep은 서버가 강제한 Step을 MaxDataPoints에 맞춰 넓힙니다.
// Step은 좁히지 않습니다 — 좁히면 포인트 수 계약이 깨집니다.
func (s *Source) effectiveStep(w datasource.Window) time.Duration {
	step := w.Step
	if step <= 0 {
		step = time.Minute
	}
	span := w.To.Sub(w.From)
	if span <= 0 {
		return step
	}
	if int(span/step) <= s.cfg.MaxDataPoints {
		return step
	}
	// 올림한 뒤 원래 Step의 배수로 맞춥니다. 차트 눈금이 어긋나지 않습니다.
	widened := time.Duration(math.Ceil(float64(span) / float64(s.cfg.MaxDataPoints)))
	if rem := widened % step; rem != 0 {
		widened += step - rem
	}
	return widened
}

/* ── Usage ──────────────────────────────────────────────────────────────── */

// usageRateWindow는 현재 사용량 계산의 rate 구간입니다.
const usageRateWindow = 2 * time.Minute

// Usage는 Pod UID → 현재 사용량 스냅숏입니다.
//
// 메트릭 라벨(namespace, pod 이름)을 카탈로그의 UID로 되돌립니다.
// 카탈로그에 없는 Pod(이미 사라진 Pod의 잔여 시계열)는 버립니다 —
// UID 없이 이름만으로 신원을 만들지 않습니다. (README §5)
func (s *Source) Usage(ctx context.Context, clusterID string) (map[string]contract.ContainerUsage, error) {
	uidOf := map[string]string{}
	for _, p := range s.catalog.CatalogPods("", 0) {
		uidOf[p.Namespace+"/"+p.Name] = p.UID
	}

	cpu, err := s.instantQuery(ctx, fmt.Sprintf(
		`1000 * sum by (namespace, pod) (rate(container_cpu_usage_seconds_total{container!=""}[%s]))`,
		promDuration(usageRateWindow)))
	if err != nil {
		return nil, err
	}
	mem, err := s.instantQuery(ctx,
		`sum by (namespace, pod) (container_memory_working_set_bytes{container!=""}) / 1048576`)
	if err != nil {
		return nil, err
	}

	out := map[string]contract.ContainerUsage{}
	for key, v := range cpu {
		if uid, ok := uidOf[key]; ok {
			u := out[uid]
			u.CPUMilli = int(math.Round(v))
			out[uid] = u
		}
	}
	for key, v := range mem {
		if uid, ok := uidOf[key]; ok {
			u := out[uid]
			u.MemoryMib = int(math.Round(v))
			out[uid] = u
		}
	}
	return out, nil
}

/* ── Prometheus 호환 API 호출 ───────────────────────────────────────────── */

// promResponse는 Prometheus 호환 응답 봉투입니다.
type promResponse struct {
	Status string `json:"status"`
	Data   struct {
		ResultType string `json:"resultType"`
		Result     []struct {
			Metric map[string]string `json:"metric"`
			// range query(matrix)
			Values [][2]any `json:"values"`
			// instant query(vector)
			Value [2]any `json:"value"`
		} `json:"result"`
	} `json:"data"`
}

func (s *Source) rangeQuery(ctx context.Context, expr string, from, to time.Time, step time.Duration) ([]contract.TrendPoint, error) {
	q := url.Values{}
	q.Set("db", s.cfg.DB)
	q.Set("query", expr)
	q.Set("start", strconv.FormatInt(from.Unix(), 10))
	q.Set("end", strconv.FormatInt(to.Unix(), 10))
	q.Set("step", strconv.Itoa(int(step.Seconds())))

	var res promResponse
	if err := s.client.GetJSON(ctx, "/v1/prometheus/api/v1/query_range", q, &res); err != nil {
		return nil, err
	}
	if len(res.Data.Result) == 0 {
		return []contract.TrendPoint{}, nil
	}
	// 카탈로그 질의는 전부 sum(...)이라 시리즈가 하나여야 합니다.
	// 여러 개가 오면 첫 시리즈만 씁니다 — 합치면 이중 집계가 됩니다.
	return cleanPoints(res.Data.Result[0].Values), nil
}

// instantQuery는 "namespace/pod" → 값 맵을 돌려줍니다.
func (s *Source) instantQuery(ctx context.Context, expr string) (map[string]float64, error) {
	q := url.Values{}
	q.Set("db", s.cfg.DB)
	q.Set("query", expr)

	var res promResponse
	if err := s.client.GetJSON(ctx, "/v1/prometheus/api/v1/query", q, &res); err != nil {
		return nil, err
	}
	out := make(map[string]float64, len(res.Data.Result))
	for _, r := range res.Data.Result {
		v, ok := sampleValue(r.Value)
		if !ok {
			continue
		}
		out[r.Metric["namespace"]+"/"+r.Metric["pod"]] = v
	}
	return out, nil
}

// cleanPoints는 Prometheus 샘플을 화면 포인트로 바꿉니다.
//
// upstream을 전적으로 믿지 않습니다 — NaN·Inf는 버리고, 순서가 어긋난 샘플은
// 정렬하고, 같은 타임스탬프는 마지막 값만 남깁니다. (#6 작업 범위)
func cleanPoints(values [][2]any) []contract.TrendPoint {
	pts := make([]contract.TrendPoint, 0, len(values))
	for _, pair := range values {
		ts, ok := sampleTime(pair[0])
		if !ok {
			continue
		}
		v, ok := parseSample(pair[1])
		if !ok || math.IsNaN(v) || math.IsInf(v, 0) {
			continue
		}
		pts = append(pts, contract.TrendPoint{T: ts, V: v})
	}
	sort.SliceStable(pts, func(a, b int) bool { return pts[a].T < pts[b].T })
	dedup := pts[:0]
	for _, p := range pts {
		if n := len(dedup); n > 0 && dedup[n-1].T == p.T {
			dedup[n-1] = p
			continue
		}
		dedup = append(dedup, p)
	}
	return dedup
}

func sampleValue(pair [2]any) (float64, bool) {
	if pair[0] == nil && pair[1] == nil {
		return 0, false
	}
	v, ok := parseSample(pair[1])
	if !ok || math.IsNaN(v) || math.IsInf(v, 0) {
		return 0, false
	}
	return v, true
}

// sampleTime은 Prometheus 타임스탬프(초, 소수 허용)를 밀리초로 바꿉니다.
func sampleTime(v any) (int64, bool) {
	switch t := v.(type) {
	case float64:
		return int64(t * 1000), true
	case string:
		f, err := strconv.ParseFloat(t, 64)
		if err != nil {
			return 0, false
		}
		return int64(f * 1000), true
	}
	return 0, false
}

func parseSample(v any) (float64, bool) {
	switch s := v.(type) {
	case string:
		f, err := strconv.ParseFloat(s, 64)
		return f, err == nil
	case float64:
		return s, true
	}
	return 0, false
}

// promDuration은 PromQL 구간 표기를 만듭니다. 초 단위면 충분합니다.
func promDuration(d time.Duration) string {
	sec := int(d.Seconds())
	if sec < 1 {
		sec = 1
	}
	return strconv.Itoa(sec) + "s"
}
