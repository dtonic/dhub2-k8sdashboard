// Package quickwit은 Quickwit 로그 어댑터입니다. (#7)
//
// Quickwit의 Elasticsearch 호환 검색 API(/api/v1/_elastic)를 씁니다.
// 검색 문법과 응답 형식은 이 패키지 밖으로 새지 않습니다 — 핸들러는
// datasource.LogQuery만 알고, 질의 DSL은 여기서만 만들어집니다.
//
// 지키는 규칙:
//   - Scope(namespace·pod UID·workload)는 **서버가 filter로 강제 삽입**합니다.
//     사용자 검색어는 match 질의의 값으로만 들어가므로 연산자를 끼워 넣어
//     필터를 우회할 수 없습니다. (README §10)
//   - offset 페이징을 만들지 않습니다. Quickwit TTL scroll snapshot과 HMAC-bound
//     cursor로 다음 페이지를 계산합니다. (ADR 0006)
//   - 마스킹은 서버에서만 합니다. 원문은 응답에 실리지 않습니다. (ADR 0003)
package quickwit

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/xenx96/k8s-dashboard/apps/api/internal/contract"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/datasource"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/datasource/mask"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/datasource/upstream"
)

// FieldMap은 인덱스의 필드 이름입니다. 파이프라인(Vector 등)의 스키마에 맞게
// 환경변수로 바꿀 수 있습니다. 여기 나온 것이 기본값입니다.
//
// 주의: Namespace·PodName·Level·Container·WorkloadName은 필터·집계에 쓰이므로
// 인덱스에서 **fast field**(raw 토크나이저)여야 합니다.
type FieldMap struct {
	Timestamp    string
	Level        string
	Message      string
	Namespace    string
	PodName      string
	PodUID       string
	Container    string
	WorkloadKind string
	WorkloadName string
	Node         string
	TraceID      string
	SpanID       string
	EventID      string
}

func (f FieldMap) withDefaults() FieldMap {
	def := func(v *string, d string) {
		if *v == "" {
			*v = d
		}
	}
	def(&f.Timestamp, "timestamp")
	def(&f.Level, "level")
	def(&f.Message, "message")
	def(&f.Namespace, "namespace")
	def(&f.PodName, "pod_name")
	def(&f.PodUID, "pod_uid")
	def(&f.Container, "container")
	def(&f.WorkloadKind, "workload_kind")
	def(&f.WorkloadName, "workload_name")
	def(&f.Node, "node")
	def(&f.TraceID, "trace_id")
	def(&f.SpanID, "span_id")
	def(&f.EventID, "event_id")
	return f
}

// Config는 어댑터 구성입니다.
type Config struct {
	// BaseURL은 Quickwit 주소입니다. 예: http://quickwit:7280
	BaseURL string
	// Index는 로그 인덱스 id입니다.
	Index    string
	Username string
	Password string
	// Timeout은 검색 1건의 상한입니다. 0이면 10초입니다.
	Timeout time.Duration
	// MaxPageSize는 페이지 크기 상한입니다. 사용자가 요청해도 넘지 못합니다.
	MaxPageSize int
	// MaxLines는 한 조회 범위에서 훑는 총량 상한입니다. 넘으면 truncated로 알립니다.
	MaxLines int
	Fields   FieldMap
	Observer upstream.Observer
}

func (c Config) withDefaults() Config {
	if c.Index == "" {
		c.Index = "k8s-logs"
	}
	if c.Timeout <= 0 {
		c.Timeout = 10 * time.Second
	}
	if c.MaxPageSize <= 0 {
		c.MaxPageSize = 500
	}
	if c.MaxLines <= 0 {
		c.MaxLines = 5000
	}
	c.Fields = c.Fields.withDefaults()
	return c
}

// DefaultPageSize는 커서 한 페이지 크기입니다. 데모 어댑터와 같습니다.
const DefaultPageSize = 100

const scrollTTL = "1m"

// Source는 datasource.Logs 구현입니다.
type Source struct {
	cfg     Config
	client  *upstream.Client
	catalog datasource.PodCatalog
	hmacKey [32]byte
}

// New는 어댑터를 만듭니다. catalog는 facet의 Pod 신원(UID) 변환에 필요합니다.
func New(cfg Config, catalog datasource.PodCatalog) (*Source, error) {
	cfg = cfg.withDefaults()
	client, err := upstream.New(upstream.Config{
		BaseURL:  cfg.BaseURL,
		What:     "Quickwit",
		Username: cfg.Username,
		Password: cfg.Password,
		Timeout:  cfg.Timeout,
		Observer: cfg.Observer,
		Upstream: upstream.UpstreamQuickwit,
	})
	if err != nil {
		return nil, err
	}
	s := &Source{cfg: cfg, client: client, catalog: catalog}
	if _, err := rand.Read(s.hmacKey[:]); err != nil {
		return nil, fmt.Errorf("Quickwit 커서 키를 만들 수 없습니다: %w", err)
	}
	return s, nil
}

func (s *Source) searchPath() string {
	return "/api/v1/_elastic/" + s.cfg.Index + "/_search"
}

func (s *Source) scrollPath() string { return "/api/v1/_elastic/_search/scroll" }

/* ── Search ─────────────────────────────────────────────────────────────── */

func (s *Source) Search(ctx context.Context, q datasource.LogQuery) (datasource.LogPage, error) {
	cur, ok := decodeCursor(q.Cursor, s.cfg.MaxLines, s.hmacKey[:])
	if !ok {
		return datasource.LogPage{}, fmt.Errorf("커서를 해석할 수 없습니다")
	}
	size := q.PageSize
	if size <= 0 {
		size = DefaultPageSize
	}
	if size > s.cfg.MaxPageSize {
		size = s.cfg.MaxPageSize
	}
	if size > s.cfg.MaxLines {
		size = s.cfg.MaxLines
	}
	queryHash := s.queryHash(q, size)
	if q.Cursor != "" && !hmac.Equal([]byte(cur.QueryHash), []byte(queryHash)) {
		return datasource.LogPage{}, fmt.Errorf("커서가 현재 조회 범위와 일치하지 않습니다")
	}
	scanRemaining := s.cfg.MaxLines - cur.Scanned
	if q.Cursor != "" && scanRemaining < size {
		return datasource.LogPage{
			Lines:     []contract.LogLine{},
			MaxLines:  s.cfg.MaxLines,
			Truncated: cur.Total > cur.Scanned,
		}, nil
	}

	var res esResponse
	if q.Cursor == "" {
		body := s.searchBody(q, size)
		if err := s.client.PostJSONQuery(ctx, s.searchPath(), url.Values{"scroll": {scrollTTL}}, body, &res); err != nil {
			return datasource.LogPage{}, err
		}
		cur.QueryHash = queryHash
		cur.Total = res.Hits.Total.Value
		nonce := make([]byte, nonceBytes)
		if _, err := rand.Read(nonce); err != nil {
			return datasource.LogPage{}, fmt.Errorf("Quickwit 조회 nonce를 만들 수 없습니다: %w", err)
		}
		cur.Nonce = base64.RawURLEncoding.EncodeToString(nonce)
	} else {
		if err := s.client.GetJSONBodyOnce(ctx, s.scrollPath(), map[string]any{"scroll_id": cur.ScrollID}, &res); err != nil {
			return datasource.LogPage{}, err
		}
	}
	if len(res.Hits.Hits) > scanRemaining {
		return datasource.LogPage{}, fmt.Errorf("Quickwit scroll page exceeded scan limit: %w", datasource.ErrUnavailable)
	}

	page := datasource.LogPage{Lines: make([]contract.LogLine, 0, len(res.Hits.Hits)), MaxLines: s.cfg.MaxLines}
	idPrefix := traversalIDPrefix(s.hmacKey[:], cur.Nonce)
	for i, hit := range res.Hits.Hits {
		line, ok := s.lineAt(hit, idPrefix, cur.Scanned+i)
		if !ok {
			continue
		}
		page.Lines = append(page.Lines, line)
	}

	cur.Returned += len(page.Lines)
	cur.Scanned += len(res.Hits.Hits)
	cur.ScrollID = res.ScrollID
	scanRemaining = s.cfg.MaxLines - cur.Scanned
	more := cur.Scanned < cur.Total
	cur.Truncated = cur.Total > s.cfg.MaxLines || (more && scanRemaining < size)
	page.Truncated = cur.Truncated
	if more && scanRemaining >= size && len(res.Hits.Hits) > 0 {
		if len(cur.ScrollID) == 0 || len(cur.ScrollID) > maxScrollIDBytes {
			return datasource.LogPage{}, fmt.Errorf("Quickwit scroll cursor: %w", datasource.ErrUnavailable)
		}
		if next, ok := encodeCursor(cur, s.hmacKey[:]); ok {
			page.Next = next
		} else {
			page.Truncated = true
		}
	}

	return page, nil
}

/* ── Histogram ──────────────────────────────────────────────────────────── */

func (s *Source) Histogram(ctx context.Context, q datasource.LogQuery) ([]contract.LogHistogramBucket, error) {
	if q.Window.Step <= 0 {
		return nil, nil
	}
	body := map[string]any{
		"size":  0,
		"query": s.boolQuery(q),
		"aggs": map[string]any{
			"over_time": map[string]any{
				"date_histogram": map[string]any{
					"field":          s.cfg.Fields.Timestamp,
					"fixed_interval": fmt.Sprintf("%dms", q.Window.Step.Milliseconds()),
				},
				"aggs": map[string]any{
					"levels": map[string]any{
						"terms": map[string]any{"field": s.cfg.Fields.Level, "size": 8},
					},
				},
			},
		},
	}
	var res esResponse
	if err := s.client.PostJSON(ctx, s.searchPath(), body, &res); err != nil {
		return nil, err
	}
	buckets := res.Aggregations.OverTime.Buckets
	out := make([]contract.LogHistogramBucket, 0, len(buckets))
	for _, b := range buckets {
		hb := contract.LogHistogramBucket{T: int64(b.Key), Counts: map[contract.LogLevel]int{}}
		for _, lb := range b.Levels.Buckets {
			hb.Counts[normalizeLevel(fmt.Sprint(lb.Key))] += lb.DocCount
		}
		out = append(out, hb)
	}
	return out, nil
}

/* ── Facets ─────────────────────────────────────────────────────────────── */

// Facets는 현재 Scope에서 관측된 필터 후보입니다.
//
// Pod facet의 UID는 인덱스가 아니라 **informer 카탈로그**에서 가져옵니다.
// 인덱스의 uid 필드를 믿으면 이미 사라진 Pod나 다른 클러스터의 uid가 섞여
// 딥링크가 404가 됩니다. 카탈로그에 없는 Pod는 이름만 노출합니다.
func (s *Source) Facets(ctx context.Context, q datasource.LogQuery) (contract.LogFacets, error) {
	termsAgg := func(field string) map[string]any {
		return map[string]any{"terms": map[string]any{"field": field, "size": 50}}
	}
	body := map[string]any{
		"size":  0,
		"query": s.boolQuery(q),
		"aggs": map[string]any{
			"workloads":  termsAgg(s.cfg.Fields.WorkloadName),
			"pods":       termsAgg(s.cfg.Fields.PodName),
			"containers": termsAgg(s.cfg.Fields.Container),
		},
	}
	var res esResponse
	if err := s.client.PostJSON(ctx, s.searchPath(), body, &res); err != nil {
		return contract.LogFacets{}, err
	}

	byName := map[string]datasource.CatalogPod{}
	for _, p := range s.catalog.CatalogPods(q.Target.Namespace, 0) {
		if q.Target.AllowsNamespace(p.Namespace) {
			byName[p.Name] = p
		}
	}

	f := contract.LogFacets{
		Workloads:  []contract.LogFacetWorkload{},
		Pods:       []contract.LogFacetPod{},
		Containers: []contract.LogFacetContainer{},
	}
	for _, b := range res.Aggregations.Workloads.Buckets {
		name := fmt.Sprint(b.Key)
		kind := ""
		for _, p := range byName {
			if p.WorkloadName == name {
				kind = p.WorkloadKind
				break
			}
		}
		f.Workloads = append(f.Workloads, contract.LogFacetWorkload{Name: name, Kind: kind, Count: b.DocCount})
	}
	for _, b := range res.Aggregations.Pods.Buckets {
		name := fmt.Sprint(b.Key)
		uid := ""
		if p, ok := byName[name]; ok {
			uid = p.UID
		}
		f.Pods = append(f.Pods, contract.LogFacetPod{Name: name, UID: uid, Count: b.DocCount})
	}
	for _, b := range res.Aggregations.Containers.Buckets {
		f.Containers = append(f.Containers, contract.LogFacetContainer{Name: fmt.Sprint(b.Key), Count: b.DocCount})
	}
	sort.Slice(f.Workloads, func(a, b int) bool { return f.Workloads[a].Name < f.Workloads[b].Name })
	sort.Slice(f.Pods, func(a, b int) bool { return f.Pods[a].Name < f.Pods[b].Name })
	sort.Slice(f.Containers, func(a, b int) bool { return f.Containers[a].Name < f.Containers[b].Name })
	return f, nil
}

/* ── 문서 → LogLine ─────────────────────────────────────────────────────── */

func (s *Source) lineAt(hit esHit, idPrefix string, ordinal int) (contract.LogLine, bool) {
	src := hit.Source
	if src == nil {
		return contract.LogLine{}, false
	}
	f := s.cfg.Fields

	ts, ok := parseTimestamp(src[f.Timestamp])
	if !ok {
		return contract.LogLine{}, false
	}
	raw := str(src[f.Message])
	masked, spans := mask.Apply(raw)

	id := str(src[f.EventID])
	if id == "" {
		id = traversalID(idPrefix, ordinal)
	}

	return contract.LogLine{
		ID:            id,
		T:             ts,
		Level:         normalizeLevel(str(src[f.Level])),
		Message:       masked,
		Masked:        spans,
		Namespace:     str(src[f.Namespace]),
		PodName:       str(src[f.PodName]),
		PodUID:        str(src[f.PodUID]),
		ContainerName: str(src[f.Container]),
		WorkloadKind:  str(src[f.WorkloadKind]),
		WorkloadName:  str(src[f.WorkloadName]),
		NodeName:      str(src[f.Node]),
		TraceID:       str(src[f.TraceID]),
		SpanID:        str(src[f.SpanID]),
	}, true
}

func traversalIDPrefix(key []byte, nonce string) string {
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte(nonce))
	return "scroll-" + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func traversalID(prefix string, ordinal int) string {
	return prefix + "-" + strconv.Itoa(ordinal)
}

func (s *Source) queryHash(q datasource.LogQuery, pageSize int) string {
	q.Target.Namespaces = append([]string(nil), q.Target.Namespaces...)
	sort.Strings(q.Target.Namespaces)
	q.Levels = append([]contract.LogLevel(nil), q.Levels...)
	sort.Slice(q.Levels, func(i, j int) bool { return q.Levels[i] < q.Levels[j] })
	canonical := struct {
		Index, ClusterID string
		PageSize         int
		Query            map[string]any
	}{s.cfg.Index, q.Target.ClusterID, pageSize, s.boolQuery(q)}
	raw, _ := json.Marshal(canonical)
	sum := sha256.Sum256(raw)
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func str(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case nil:
		return ""
	default:
		return fmt.Sprint(t)
	}
}

// parseTimestamp는 인덱스마다 다른 타임스탬프 표기를 밀리초로 정규화합니다.
func parseTimestamp(v any) (int64, bool) {
	switch t := v.(type) {
	case float64:
		return normalizeEpoch(t), true
	case string:
		if ts, err := time.Parse(time.RFC3339Nano, t); err == nil {
			return ts.UnixMilli(), true
		}
		if n, err := strconv.ParseFloat(t, 64); err == nil {
			return normalizeEpoch(n), true
		}
	}
	return 0, false
}

// normalizeEpoch는 초·밀리초·마이크로초·나노초를 밀리초로 맞춥니다.
func normalizeEpoch(v float64) int64 {
	switch {
	case v > 1e17: // 나노초
		return int64(v / 1e6)
	case v > 1e14: // 마이크로초
		return int64(v / 1e3)
	case v > 1e11: // 밀리초
		return int64(v)
	default: // 초
		return int64(v * 1000)
	}
}

func normalizeLevel(v string) contract.LogLevel {
	switch strings.ToUpper(strings.TrimSpace(v)) {
	case "ERROR", "ERR", "FATAL", "CRITICAL":
		return contract.LevelError
	case "WARN", "WARNING":
		return contract.LevelWarn
	case "DEBUG", "TRACE":
		return contract.LevelDebug
	default:
		return contract.LevelInfo
	}
}
