package resourcecatalog

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8sjson "k8s.io/apimachinery/pkg/runtime/serializer/json"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/metadata"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/util/flowcontrol"
)

// 상세 조회는 **사용자가 항목을 연 순간에만** 나가는 live GET입니다.
// ADR 0004("요청 경로에서 API 서버를 호출하지 않는다")의 명시적 예외이므로
// 클라이언트·rate limit·timeout·본문 상한을 조회 경로와 완전히 분리합니다. (ADR 0018 결정 5)

// DetailRequest는 화면에서 연 항목 하나입니다. 이름만으로는 부족합니다 —
// 같은 이름의 다른 객체로 교체되었는지 UID로 확인합니다. (README §5)
type DetailRequest struct {
	Group    string
	Version  string
	Resource string
	// Namespace는 namespaced 리소스에서만 채웁니다.
	Namespace string
	Name      string
	// ExpectedUID는 목록에서 본 객체의 UID입니다. 다르면 ErrUIDMismatch입니다.
	ExpectedUID string
}

// Detail은 정제된 읽기 전용 매니페스트입니다.
type Detail struct {
	APIVersion      string
	Kind            string
	Namespace       string
	Name            string
	UID             string
	ResourceVersion string
	// YAML은 서버가 정제한 매니페스트입니다. Secret 값은 들어 있지 않습니다.
	YAML string
	// Redacted는 제거한 필드 경로입니다. 무엇이 가려졌는지 화면이 알 수 있어야 합니다.
	Redacted []string
}

// Get은 격리된 dynamic client로 객체 하나를 읽어 정제한 YAML을 돌려줍니다.
func (s *Service) Get(ctx context.Context, req DetailRequest) (Detail, error) {
	if !s.Available() {
		return Detail{}, ErrUnavailable
	}
	gvr := schema.GroupVersionResource{Group: req.Group, Version: req.Version, Resource: req.Resource}
	desc, err := s.Describe(gvr)
	if err != nil {
		return Detail{}, err
	}
	// 상세는 **ready인 카탈로그 항목**에서만 열립니다. syncing·unsupported·forbidden·
	// missing 상태가 읽기 전용 캐시 경계를 우회해 live GET으로 새어나가면 안 됩니다.
	if err := stateError(desc.State); err != nil {
		return Detail{}, err
	}
	if !hasVerb(desc.Verbs, "get") {
		return Detail{}, ErrUnsupported
	}
	if req.Name == "" || !safeCursorSegment(req.Name) || len(req.Name) > maxCursorName {
		return Detail{}, ErrInvalidFilter
	}
	if desc.Namespaced {
		if req.Namespace == "" || !safeCursorSegment(req.Namespace) || len(req.Namespace) > maxCursorNSLen {
			return Detail{}, ErrInvalidFilter
		}
	} else if req.Namespace != "" {
		return Detail{}, ErrInvalidFilter
	}
	if req.ExpectedUID == "" || len(req.ExpectedUID) > 64 || !safeCursorSegment(req.ExpectedUID) {
		return Detail{}, ErrInvalidFilter
	}

	// **목록에서 실제로 본 행만** 열 수 있습니다. (namespace, name)이 로컬 metadata
	// 인덱스에 있고 그 행의 UID가 요청한 UID와 같아야 API 서버로 나갑니다.
	// 사용자가 지어낸 이름·UID로는 live GET 자체가 발생하지 않습니다.
	entry, ok := s.entries[gvr]
	if !ok {
		return Detail{}, ErrNotAllowlisted
	}
	// 인덱스 조회는 **잠금 없이** 포인터 하나를 집어 끝냅니다. 서비스 전역 잠금에
	// 묶이지 않으므로, 상세 요청이 다른 GVR의 게시를 밀지 않습니다.
	//
	// **신원 판정은 목록 스냅숏 하나입니다.** (ADR 0018 그대로)
	//
	// 증분 검색 인덱스를 여기에 끌어들이면 상세의 신원이 검색 인덱스의 수명·회수
	// 상태에 묶입니다. 숨겨진 namespace 하나가 회수 대기라는 이유로 허용된 참조의
	// 해석이 달라질 수 있고, 그것은 볼 수 없는 데이터의 상태가 응답에 비치는 것입니다.
	// 그래서 이 경로는 baseline 목록 스냅숏만 봅니다.
	index := entry.baselineIndex()
	if index == nil {
		return Detail{}, ErrSyncing
	}
	var cachedUID string
	row, found := index.lookup(req.Namespace, req.Name)
	if !found {
		return Detail{}, ErrObjectNotFound
	}
	if row.obj != nil {
		cachedUID = string(row.obj.UID)
	}
	if cachedUID != req.ExpectedUID {
		return Detail{}, ErrUIDMismatch
	}

	// 이미 취소된 요청은 rate 토큰도 동시성 슬롯도 쓰지 않습니다.
	if err := ctx.Err(); err != nil {
		return Detail{}, err
	}
	release, err := s.guard.acquire()
	if err != nil {
		return Detail{}, err
	}
	defer release()

	// 취소는 데이터소스 요청까지 전파합니다. timeout은 이 경로 전용입니다.
	callCtx, cancel := context.WithTimeout(ctx, s.cfg.DetailTimeout)
	defer cancel()

	client := s.clients.Dynamic.Resource(gvr)
	var obj *unstructured.Unstructured
	if desc.Namespaced {
		obj, err = client.Namespace(req.Namespace).Get(callCtx, req.Name, metav1.GetOptions{})
	} else {
		obj, err = client.Get(callCtx, req.Name, metav1.GetOptions{})
	}
	if err != nil {
		if errors.Is(err, errBodyTooLarge) {
			return Detail{}, ErrTooLarge
		}
		return Detail{}, err
	}
	if obj == nil {
		return Detail{}, ErrNotServed
	}
	// UID가 다르면 화면이 보고 있던 객체가 아닙니다. 다른 객체의 매니페스트를 대신 보여주지 않습니다.
	if string(obj.GetUID()) != req.ExpectedUID {
		return Detail{}, ErrUIDMismatch
	}

	redacted := sanitize(obj, gvr)
	manifest, err := encodeYAML(obj)
	if err != nil {
		return Detail{}, fmt.Errorf("매니페스트를 직렬화하지 못했습니다: %w", err)
	}
	if len(manifest) > s.cfg.MaxObjectBytes {
		return Detail{}, ErrTooLarge
	}
	return Detail{
		APIVersion:      obj.GetAPIVersion(),
		Kind:            obj.GetKind(),
		Namespace:       obj.GetNamespace(),
		Name:            obj.GetName(),
		UID:             string(obj.GetUID()),
		ResourceVersion: obj.GetResourceVersion(),
		YAML:            manifest,
		Redacted:        redacted,
	}, nil
}

var yamlEncoder = k8sjson.NewSerializerWithOptions(k8sjson.DefaultMetaFactory, nil, nil,
	k8sjson.SerializerOptions{Yaml: true})

func encodeYAML(obj *unstructured.Unstructured) (string, error) {
	var buf bytes.Buffer
	if err := yamlEncoder.Encode(obj, &buf); err != nil {
		return "", err
	}
	return buf.String(), nil
}

/* ── 정제 ────────────────────────────────────────────────────────────────
   값이 밖으로 나가면 안 되는 것은 브라우저가 아니라 **서버에서** 지웁니다. */

// secretGVR은 값 전체를 제거해야 하는 리소스입니다. 값 조회 경로는 ADR 0014의
// 전용 관리 화면 하나뿐이고 Explorer에는 없습니다. (ADR 0018 결정 6)
var secretGVR = schema.GroupVersionResource{Group: "", Version: "v1", Resource: "secrets"}

// sensitiveAnnotationFragments는 값이 들어 있을 가능성이 큰 annotation 키 조각입니다.
var sensitiveAnnotationFragments = []string{
	"token", "secret", "password", "passwd", "credential",
	"private-key", "privatekey", "apikey", "api-key", "session-key",
}

// sanitize는 매니페스트에서 값·서버 관리 필드를 제거하고 제거 목록을 돌려줍니다.
func sanitize(obj *unstructured.Unstructured, gvr schema.GroupVersionResource) []string {
	var redacted []string
	if meta, ok := obj.Object["metadata"].(map[string]any); ok {
		if _, had := meta["managedFields"]; had {
			delete(meta, "managedFields")
			redacted = append(redacted, "metadata.managedFields")
		}
	}
	if gvr == secretGVR {
		for _, field := range []string{"data", "stringData"} {
			if _, had := obj.Object[field]; had {
				delete(obj.Object, field)
				redacted = append(redacted, field)
			}
		}
	}
	if annotations := obj.GetAnnotations(); len(annotations) > 0 {
		kept := make(map[string]string, len(annotations))
		for k, v := range annotations {
			if isSensitiveAnnotation(k) {
				redacted = append(redacted, "metadata.annotations["+k+"]")
				continue
			}
			kept[k] = v
		}
		if len(kept) != len(annotations) {
			if len(kept) == 0 {
				unstructured.RemoveNestedField(obj.Object, "metadata", "annotations")
			} else {
				obj.SetAnnotations(kept)
			}
		}
	}
	sort.Strings(redacted)
	return redacted
}

func isSensitiveAnnotation(key string) bool {
	if key == "kubectl.kubernetes.io/last-applied-configuration" {
		return true // 원본 매니페스트 전체가 들어 있어 Secret 값을 그대로 담을 수 있습니다.
	}
	lower := strings.ToLower(key)
	for _, fragment := range sensitiveAnnotationFragments {
		if strings.Contains(lower, fragment) {
			return true
		}
	}
	return false
}

/* ── 유계 guard ──────────────────────────────────────────────────────────── */

// detailGuard는 상세 조회 전용 token bucket + 동시성 상한입니다.
// 조회 경로의 queryprotect와 예산을 공유하지 않습니다.
type detailGuard struct {
	mu          sync.Mutex
	tokens      float64
	last        time.Time
	rate        float64
	burst       int
	inflight    int
	maxInflight int
	now         func() time.Time
}

func (g *detailGuard) acquire() (func(), error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	now := g.now()
	g.tokens += now.Sub(g.last).Seconds() * g.rate
	if g.tokens > float64(g.burst) {
		g.tokens = float64(g.burst)
	}
	g.last = now
	if g.tokens < 1 {
		return nil, ErrRateLimited
	}
	if g.inflight >= g.maxInflight {
		return nil, ErrRateLimited
	}
	g.tokens--
	g.inflight++
	var once sync.Once
	return func() {
		once.Do(func() {
			g.mu.Lock()
			g.inflight--
			g.mu.Unlock()
		})
	}, nil
}

/* ── 클라이언트 ──────────────────────────────────────────────────────────── */

// ClientOptions는 Resource Explorer 전용 클라이언트의 상한입니다.
type ClientOptions struct {
	// QPS/Burst는 metadata informer·discovery용 client-side rate limit입니다.
	QPS   float32
	Burst int
	// DetailQPS/DetailBurst는 상세 live GET 전용이며 훨씬 낮게 둡니다.
	DetailQPS   float32
	DetailBurst int
	UserAgent   string
	// MaxObjectBytes는 상세 응답 본문 상한입니다. 넘으면 읽는 중에 끊습니다.
	MaxObjectBytes int64
	// DetailTimeout은 상세 HTTP 요청 하나의 상한입니다.
	DetailTimeout time.Duration
	// DiscoveryTimeout은 discovery 조회 하나의 상한입니다.
	DiscoveryTimeout time.Duration
}

func (o *ClientOptions) setDefaults() {
	if o.QPS <= 0 {
		o.QPS = 10
	}
	if o.Burst <= 0 {
		o.Burst = 20
	}
	if o.DetailQPS <= 0 {
		o.DetailQPS = 2
	}
	if o.DetailBurst <= 0 {
		o.DetailBurst = 5
	}
	if o.UserAgent == "" {
		o.UserAgent = "k8s-dashboard-resources"
	}
	if o.MaxObjectBytes <= 0 {
		o.MaxObjectBytes = DefaultMaxObjectBytes
	}
	if o.DetailTimeout <= 0 {
		o.DetailTimeout = DefaultDetailTimeout
	}
	if o.DiscoveryTimeout <= 0 {
		o.DiscoveryTimeout = 15 * time.Second
	}
}

// NewClients는 관측 경로와 분리된 클라이언트 묶음을 만듭니다.
// 상세 GET용 dynamic client만 본문 상한 transport를 답니다.
func NewClients(base *rest.Config, o ClientOptions) (Clients, error) {
	if base == nil {
		return Clients{}, fmt.Errorf("resourcecatalog: rest 설정이 필요합니다")
	}
	o.setDefaults()

	// metadata·discovery는 PartialObjectMetadata/discovery 자체의 콘텐츠 협상을 씁니다.
	metaCfg := rest.CopyConfig(base)
	metaCfg.AcceptContentTypes, metaCfg.ContentType = "", ""
	metaCfg.UserAgent = o.UserAgent
	metaCfg.QPS, metaCfg.Burst = o.QPS, o.Burst
	metaCfg.RateLimiter = flowcontrol.NewTokenBucketRateLimiter(o.QPS, o.Burst)

	// discovery는 metadata 협상을 쓰지 않으므로 아래 wrap 이전 설정을 복사합니다.
	discCfg := rest.CopyConfig(metaCfg)
	discCfg.Timeout = o.DiscoveryTimeout
	discCfg.RateLimiter = flowcontrol.NewTokenBucketRateLimiter(o.QPS, o.Burst)

	// **metadata 클라이언트만** full-object로 떨어지는 Accept 대안을 잘라냅니다.
	// Wrap은 base 설정에 이미 있던 WrapTransport 위에 합성됩니다.
	metaCfg.Wrap(func(rt http.RoundTripper) http.RoundTripper { return &metadataOnlyTransport{base: rt} })
	metaClient, err := metadata.NewForConfig(metaCfg)
	if err != nil {
		return Clients{}, fmt.Errorf("metadata 클라이언트 생성 실패: %w", err)
	}

	discClient, err := discovery.NewDiscoveryClientForConfig(discCfg)
	if err != nil {
		return Clients{}, fmt.Errorf("discovery 클라이언트 생성 실패: %w", err)
	}

	detailCfg := rest.CopyConfig(base)
	detailCfg.AcceptContentTypes, detailCfg.ContentType = "", ""
	detailCfg.UserAgent = o.UserAgent + "-detail"
	detailCfg.QPS, detailCfg.Burst = o.DetailQPS, o.DetailBurst
	detailCfg.RateLimiter = flowcontrol.NewTokenBucketRateLimiter(o.DetailQPS, o.DetailBurst)
	detailCfg.Timeout = o.DetailTimeout
	maxBytes := o.MaxObjectBytes
	detailCfg.Wrap(func(rt http.RoundTripper) http.RoundTripper {
		return &limitedTransport{base: rt, max: maxBytes}
	})
	dynClient, err := dynamic.NewForConfig(detailCfg)
	if err != nil {
		return Clients{}, fmt.Errorf("dynamic 클라이언트 생성 실패: %w", err)
	}
	return Clients{Metadata: metaClient, Discovery: discClient, Dynamic: dynClient}, nil
}

/* ── metadata 전용 협상 ───────────────────────────────────────────────────
   client-go의 metadata 클라이언트는 LIST/WATCH의 Accept에 세 가지를 싣습니다 —
   PartialObjectMetadata protobuf · PartialObjectMetadata JSON · 그리고 **평범한
   application/json**. 마지막 하나가 문제입니다. metadata 협상을 모르는 aggregated
   API는 406을 주는 대신 그 대안으로 내려와 **전체 객체 목록**을 그대로 보냅니다.
   나중에 PartialObjectMetadataList로 unmarshal하면서 필드가 버려지더라도, 그 시점엔
   이미 Secret data가 전선과 프로세스 메모리를 지나간 뒤입니다.

   그래서 전송 전에 그 대안을 잘라냅니다. 남는 것은 metadata 미디어 타입 둘뿐이므로
   지원하지 않는 API는 조용한 fallback 대신 406을 돌려주고, 그 406은 기존 watch 오류
   경로가 unsupported로 드러냅니다. (ADR 0018 결정 4) */

// partialObjectMetadataMarker는 PartialObjectMetadata(+List) 미디어 타입 표식입니다.
const partialObjectMetadataMarker = "as=PartialObjectMetadata"

// metadataOnlyTransport는 metadata 클라이언트 요청에서만 full-object 대안을 제거합니다.
type metadataOnlyTransport struct{ base http.RoundTripper }

func (t *metadataOnlyTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	stripped, changed := stripFullObjectAccept(req.Header.Get("Accept"))
	if !changed {
		return t.base.RoundTrip(req)
	}
	// 공유될 수 있는 요청·헤더를 제자리에서 고치지 않습니다.
	clone := req.Clone(req.Context())
	clone.Header.Set("Accept", stripped)
	return t.base.RoundTrip(clone)
}

// stripFullObjectAccept는 metadata 협상이 섞인 Accept에서 metadata 미디어 타입만 남깁니다.
// metadata 협상이 없는 Accept(typed·discovery·dynamic)는 그대로 통과시킵니다.
func stripFullObjectAccept(accept string) (string, bool) {
	if !strings.Contains(accept, partialObjectMetadataMarker) {
		return accept, false
	}
	parts := strings.Split(accept, ",")
	kept := make([]string, 0, len(parts))
	for _, part := range parts {
		if strings.Contains(part, partialObjectMetadataMarker) {
			kept = append(kept, part)
		}
	}
	// 남길 것이 없거나 버릴 것이 없으면 건드리지 않습니다.
	if len(kept) == 0 || len(kept) == len(parts) {
		return accept, false
	}
	return strings.Join(kept, ","), true
}

// errBodyTooLarge는 상한을 넘긴 응답 본문입니다.
var errBodyTooLarge = errors.New("response body exceeds the configured limit")

// limitedTransport는 상세 GET 응답 본문을 상한에서 끊습니다.
// 거대한 객체 하나가 API 프로세스 메모리를 밀어내지 못하게 합니다.
type limitedTransport struct {
	base http.RoundTripper
	max  int64
}

func (t *limitedTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.base.RoundTrip(req)
	if err != nil || resp == nil || resp.Body == nil {
		return resp, err
	}
	if resp.ContentLength > t.max {
		resp.Body.Close()
		return nil, errBodyTooLarge
	}
	resp.Body = &limitedBody{inner: resp.Body, remaining: t.max}
	return resp, nil
}

type limitedBody struct {
	inner     io.ReadCloser
	remaining int64
}

func (b *limitedBody) Read(p []byte) (int, error) {
	if b.remaining <= 0 {
		return 0, errBodyTooLarge
	}
	if int64(len(p)) > b.remaining {
		p = p[:b.remaining]
	}
	n, err := b.inner.Read(p)
	b.remaining -= int64(n)
	return n, err
}

func (b *limitedBody) Close() error { return b.inner.Close() }
