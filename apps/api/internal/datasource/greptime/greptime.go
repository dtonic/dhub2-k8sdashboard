// Package greptime은 GreptimeDB 메트릭 어댑터입니다. (#6)
//
// GreptimeDB의 Prometheus 호환 HTTP API(/v1/prometheus/api/v1/*)로 조회합니다.
// 프런트엔드는 PromQL을 알지 못합니다 — 실행 가능한 질의는 **등록형 쿼리
// 카탈로그**(internal/querycatalog, #9)에 있는 것뿐이고, Scope(namespace·pod)는
// 렌더링 시점에 서버가 라벨 매처로 강제 삽입합니다. (README §10)
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
	"github.com/xenx96/k8s-dashboard/apps/api/internal/querycatalog"
)

// Config는 어댑터 구성입니다. 기본값은 데이터소스에 가장 안전한 쪽입니다.
type Config struct {
	// BaseURL은 GreptimeDB HTTP 주소입니다. 예: http://greptimedb:4000
	BaseURL string
	// DB는 GreptimeDB 데이터베이스 이름입니다. 비우면 public입니다.
	DB       string
	Username string
	Password string
	// Timeout은 질의 1건의 상한입니다. 카탈로그의 쿼리별 timeout과 함께 적용되며
	// 짧은 쪽이 이깁니다. 0이면 10초입니다.
	Timeout time.Duration
	// MaxDataPoints는 전역 포인트 상한입니다. 카탈로그의 쿼리별 maxDataPoints보다
	// 작으면 이 값이 이깁니다. 운영자가 카탈로그 수정 없이 전체를 조일 때 씁니다.
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
	queries querycatalog.Catalog
}

// New는 어댑터를 만듭니다. catalog는 Pod UID → 이름 변환에,
// queries는 실행 가능한 질의의 유일한 원천으로 쓰입니다.
func New(cfg Config, catalog datasource.PodCatalog, queries querycatalog.Catalog) (*Source, error) {
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
	return &Source{cfg: cfg, client: client, catalog: catalog, queries: queries}, nil
}

/* ── Trends ─────────────────────────────────────────────────────────────── */

// Trends는 화면에 그릴 패널 묶음을 돌려줍니다.
//
// 패널 id가 카탈로그에 없으면 **실행되지 않고 조용히 빠집니다** — 등록되지 않은
// queryRef의 실행 경로는 없습니다. (#9 완료 기준)
// 시리즈마다 range query 1건이 나가지만, 동시성은 MaxConcurrent로 묶습니다.
func (s *Source) Trends(ctx context.Context, t datasource.Target, w datasource.Window, panels []string) ([]contract.TrendPanel, error) {
	if len(panels) == 0 {
		for _, p := range s.queries.Panels() {
			panels = append(panels, p.ID)
		}
	}

	sc, found := s.scope(t)
	span := w.To.Sub(w.From)

	out := make([]contract.TrendPanel, 0, len(panels))
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(s.cfg.MaxConcurrent)

	for _, id := range panels {
		def, ok := s.queries.Panel(id)
		if !ok {
			continue
		}
		p := contract.TrendPanel{
			ID:     def.ID,
			Title:  def.Title,
			Series: make([]contract.TrendSeries, len(def.Series)),
		}
		out = append(out, p)
		idx := len(out) - 1

		for si, sd := range def.Series {
			q, ok := s.queries.Query(sd.QueryRef)
			if !ok {
				// 로드 검증이 이미 거르지만, 여기서도 실행하지 않습니다.
				continue
			}
			step := s.effectiveStep(q, w.Step, span)
			if sec := int(step.Seconds()); sec > out[idx].StepSeconds {
				out[idx].StepSeconds = sec
			}
			out[idx].Series[si] = contract.TrendSeries{
				Key:    sd.Key,
				Label:  sd.Label,
				Unit:   q.Unit,
				Points: []contract.TrendPoint{},
			}
			// 대상 Pod가 카탈로그에 없으면(재시작 직후 등) 질의 없이 빈 시리즈를 둡니다.
			// "데이터 없음"이지 "장애"가 아닙니다.
			if !found {
				continue
			}
			if q.Limits.MaxRange > 0 && span > q.Limits.MaxRange {
				return nil, fmt.Errorf("query %s: 최대 조회 기간을 넘습니다", q.Ref)
			}
			expr, err := q.Render(sc, step, nil)
			if err != nil {
				return nil, err
			}
			si, q, step := si, q, step
			g.Go(func() error {
				qctx, cancel := context.WithTimeout(gctx, q.Limits.Timeout)
				defer cancel()
				pts, err := s.rangeQuery(qctx, expr, w.From, w.To, step)
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

// effectiveStep은 카탈로그가 선언한 한계에 전역 상한(cfg.MaxDataPoints)을 겹칩니다.
func (s *Source) effectiveStep(q querycatalog.Query, step, span time.Duration) time.Duration {
	eff := q.EffectiveStep(step, span)
	if s.cfg.MaxDataPoints > 0 && span > 0 && int(span/eff) > s.cfg.MaxDataPoints {
		widened := span / time.Duration(s.cfg.MaxDataPoints)
		if rem := widened % eff; rem != 0 {
			widened += eff - rem
		}
		eff = widened
	}
	return eff
}

/* ── Usage ──────────────────────────────────────────────────────────────── */

// Usage는 Pod UID → 현재 사용량 스냅숏입니다.
//
// 질의는 카탈로그의 metrics.usage.* 정의를 씁니다(clusterWide 명시).
// 메트릭 라벨(namespace, pod 이름)을 카탈로그의 UID로 되돌리고,
// 카탈로그에 없는 Pod(이미 사라진 Pod의 잔여 시계열)는 버립니다. (README §5)
func (s *Source) Usage(ctx context.Context, clusterID string) (map[string]contract.ContainerUsage, error) {
	uidOf := map[string]string{}
	for _, p := range s.catalog.CatalogPods("", 0) {
		uidOf[p.Namespace+"/"+p.Name] = p.UID
	}

	run := func(ref string) (map[string]float64, error) {
		q, ok := s.queries.Query(ref)
		if !ok {
			return nil, fmt.Errorf("카탈로그에 %s 정의가 없습니다", ref)
		}
		expr, err := q.Render(querycatalog.Scope{}, time.Minute, nil)
		if err != nil {
			return nil, err
		}
		qctx, cancel := context.WithTimeout(ctx, q.Limits.Timeout)
		defer cancel()
		return s.instantQuery(qctx, expr)
	}

	cpu, err := run("metrics.usage.cpu_milli")
	if err != nil {
		return nil, err
	}
	mem, err := run("metrics.usage.memory_mib")
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
