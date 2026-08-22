package resourcecatalog

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/metadata"
	"k8s.io/client-go/metadata/metadatainformer"
	"k8s.io/client-go/tools/cache"

	"github.com/xenx96/k8s-dashboard/apps/api/internal/contract"
)

// State는 allowlist 한 항목의 현재 상태입니다. 빈 목록으로 뭉개지 않기 위해 필요합니다.
type State string

const (
	// StateReady는 metadata informer가 동기화를 마쳐 목록을 줄 수 있는 상태입니다.
	StateReady State = "ready"
	// StateSyncing은 아직 최초 동기화 중입니다. "0건"과 다릅니다.
	StateSyncing State = "syncing"
	// StateUnsupported는 PartialObjectMetadata를 받아주지 않는 API(406)입니다.
	StateUnsupported State = "unsupported"
	// StateForbidden은 서버 ServiceAccount에 list/watch 권한이 없는 상태입니다.
	StateForbidden State = "forbidden"
	// StateMissing은 discovery에 없는 GVR입니다(미설치 CRD·제거된 API·다른 버전만 제공).
	StateMissing State = "missing"
)

// 기본값. 전부 "클러스터에 가장 안전한 쪽"으로 둡니다.
const (
	DefaultRefreshInterval = 10 * time.Minute
	DefaultResync          = 10 * time.Minute
	DefaultIndexInterval   = 2 * time.Second
	DefaultSyncTimeout     = 20 * time.Second
	DefaultDetailTimeout   = 5 * time.Second
	DefaultMaxObjectBytes  = 1 << 20

	// 변경 검토 전용 기본값. 상세보다 더 조입니다 — 요청 하나가 API 서버에서
	// admission chain 전체를 돌리므로 상세 GET보다 훨씬 비쌉니다. (ADR 0019)
	DefaultDryRunTimeout    = 8 * time.Second
	DefaultDryRunRate       = 1.0
	DefaultDryRunBurst      = 3
	DefaultDryRunConcurrent = 1

	// MinRefreshInterval/MaxRefreshInterval은 discovery 갱신 주기의 허용 범위입니다.
	MinRefreshInterval = time.Minute
	MaxRefreshInterval = 24 * time.Hour
)

// Clients는 Resource Explorer 전용 클라이언트 묶음입니다.
//
// Metadata/Discovery는 목록·카탈로그용이고, Dynamic은 상세 live GET **전용**입니다.
// 관측 경로(clusterstate)의 클라이언트와 공유하지 않습니다. (ADR 0018 결정 5)
type Clients struct {
	Metadata  metadata.Interface
	Discovery discovery.DiscoveryInterface
	Dynamic   dynamic.Interface
	// DryRun은 변경 검토 전용 dynamic 클라이언트입니다. (ADR 0019 Phase 1)
	//
	// **nil이면 검토 기능이 없는 것**이고, 그 상태가 기본값입니다. Dynamic(상세 live
	// GET)과 공유하지 않습니다 — rate·timeout·본문 상한·UserAgent가 다르고, 검토가
	// 예산을 소진했다고 상세 조회까지 멈추면 안 됩니다. 반대도 같습니다.
	DryRun dynamic.Interface
}

// Config는 이 기능이 클러스터에 주는 부하의 상한입니다.
type Config struct {
	ClusterID string
	// Allowlist는 informer를 붙일 GVR입니다. 여기 없는 것은 목록도 상세도 없습니다.
	Allowlist []schema.GroupVersionResource
	// AllowCRDs는 내장 API group이 아닌(=CRD) 항목의 명시적 opt-in입니다. (ADR 0018 결정 3)
	AllowCRDs bool
	// RefreshInterval은 discovery snapshot 갱신 주기입니다.
	RefreshInterval time.Duration
	// Resync는 metadata informer의 resync 주기입니다. 짧게 두지 마세요. (ADR 0004)
	Resync time.Duration
	// IndexInterval은 변경이 있을 때 정렬 인덱스를 다시 만드는 최소 간격입니다.
	IndexInterval time.Duration
	// SyncTimeout은 기동 시 최초 동기화를 기다리는 상한입니다. 넘긴 항목은
	// 실패가 아니라 syncing으로 남습니다.
	SyncTimeout time.Duration

	// Detail은 상세 live GET의 상한입니다.
	DetailTimeout    time.Duration
	DetailRate       float64
	DetailBurst      int
	DetailConcurrent int
	MaxObjectBytes   int

	// SearchEnabled는 전역 검색(ADR 0023)의 opt-out 스위치입니다.
	// 끄면 검색·최근 항목 경로만 사라지고 카탈로그·목록·상세는 그대로입니다.
	SearchEnabled bool
	// SearchIncremental은 watch 이벤트로 검색 인덱스를 증분 갱신할지입니다. (Round 6)
	//
	// 끄면 오늘까지의 경로 그대로 dirty → 전체 검색 재구성으로 돌아갑니다.
	// 전체 빌드 경로는 부트스트랩·회수가 계속 쓰므로 죽은 코드가 되지 않습니다.
	SearchIncremental bool
	// MaxSearchIndexBytes는 **모든 GVR이 동시에 보유하는** 검색 인덱스 바이트 합의 상한입니다.
	// GVR별 상한만 두면 allowlist 크기만큼 곱해져 상한이 되지 못합니다. (P1-1)
	MaxSearchIndexBytes int64

	/* ── 변경 검토 dry-run (ADR 0019 Phase 1) ─────────────────────────────
	   기본은 전부 꺼짐입니다. 켜도 대상 목록이 비어 있으면 아무 GVR도 검토되지
	   않습니다 — 두 번 opt-in해야 실제로 열립니다. */

	// DryRunEnabled는 기능 스위치입니다. false면 DryRun이 항상 ErrDryRunDisabled입니다.
	DryRunEnabled bool
	// DryRunAllowlist는 검토를 허용할 GVR입니다. Allowlist의 부분집합이어야 하고
	// hard-deny에 걸리면 기동이 실패합니다.
	DryRunAllowlist []schema.GroupVersionResource
	// DryRunDeny는 배포가 추가로 빼는 GVR입니다. hard-deny 위에 얹힙니다.
	DryRunDeny []schema.GroupVersionResource
	// DryRunTimeout은 검토 한 건(live GET + dry-run patch)의 상한입니다.
	DryRunTimeout time.Duration
	// DryRunRate/DryRunBurst/DryRunConcurrent는 검토 전용 예산입니다.
	// 상세 조회 예산과 공유하지 않습니다.
	DryRunRate       float64
	DryRunBurst      int
	DryRunConcurrent int
	// MaxManifestBytes는 파싱 **전에** 적용하는 매니페스트 바이트 상한입니다.
	// contract.MaxDryRunManifestBytes를 넘길 수 없습니다.
	MaxManifestBytes int

	// NewTicker는 100ms 합치기 창의 **테스트 seam**입니다. env·Helm 노브가 아닙니다.
	NewTicker tickerFactory

	Logger *slog.Logger
	Now    func() time.Time
}

func (c *Config) setDefaults() {
	if c.RefreshInterval <= 0 {
		c.RefreshInterval = DefaultRefreshInterval
	}
	if c.Resync <= 0 {
		c.Resync = DefaultResync
	}
	if c.IndexInterval <= 0 {
		c.IndexInterval = DefaultIndexInterval
	}
	if c.SyncTimeout <= 0 {
		c.SyncTimeout = DefaultSyncTimeout
	}
	if c.DetailTimeout <= 0 {
		c.DetailTimeout = DefaultDetailTimeout
	}
	if c.DetailRate <= 0 {
		c.DetailRate = 2
	}
	if c.DetailBurst <= 0 {
		c.DetailBurst = 5
	}
	if c.DetailConcurrent <= 0 {
		c.DetailConcurrent = 2
	}
	if c.MaxObjectBytes <= 0 {
		c.MaxObjectBytes = DefaultMaxObjectBytes
	}
	if c.MaxSearchIndexBytes <= 0 {
		c.MaxSearchIndexBytes = DefaultMaxSearchIndexBytes
	}
	if c.DryRunTimeout <= 0 {
		c.DryRunTimeout = DefaultDryRunTimeout
	}
	if c.DryRunRate <= 0 {
		c.DryRunRate = DefaultDryRunRate
	}
	if c.DryRunBurst <= 0 {
		c.DryRunBurst = DefaultDryRunBurst
	}
	if c.DryRunConcurrent <= 0 {
		c.DryRunConcurrent = DefaultDryRunConcurrent
	}
	// 상한은 위아래 양쪽으로 조입니다. 설정이 절대 상한을 넘기면 조용히 따르지 않고
	// 절대 상한으로 되돌립니다 — 계약(OpenAPI maxLength)이 그 값이기 때문입니다.
	if c.MaxManifestBytes <= 0 {
		c.MaxManifestBytes = contract.DefaultDryRunManifestBytes
	}
	if c.MaxManifestBytes > contract.MaxDryRunManifestBytes {
		c.MaxManifestBytes = contract.MaxDryRunManifestBytes
	}
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
	if c.Now == nil {
		c.Now = time.Now
	}
	if c.ClusterID == "" {
		c.ClusterID = "default"
	}
}

/* ── discovery snapshot ──────────────────────────────────────────────────
   불변입니다. 요청은 이 snapshot만 읽고 discovery를 호출하지 않습니다. */

type discoveryEntry struct {
	gvr        schema.GroupVersionResource
	kind       string
	namespaced bool
	verbs      []string
	preferred  string
	served     bool
}

type discoverySnapshot struct {
	refreshedAt time.Time
	entries     []discoveryEntry
	byGVR       map[schema.GroupVersionResource]int
	// failure는 discovery 자체가 실패했을 때의 짧은 사유입니다.
	// 내부 주소·질의·스택트레이스는 담지 않습니다.
	failure string
}

func (d *discoverySnapshot) get(gvr schema.GroupVersionResource) (discoveryEntry, bool) {
	if d == nil {
		return discoveryEntry{}, false
	}
	i, ok := d.byGVR[gvr]
	if !ok {
		return discoveryEntry{}, false
	}
	return d.entries[i], true
}

// Descriptor는 카탈로그 한 줄입니다. discovery 사실 + 현재 informer 상태입니다.
type Descriptor struct {
	Group            string
	Version          string
	Resource         string
	Kind             string
	Namespaced       bool
	Verbs            []string
	PreferredVersion string
	State            State
	Reason           string
	Count            int
	// DryRun은 이 GVR에 변경 검토를 요청할 수 있는지입니다. (ADR 0019 Phase 1)
	//
	// **capability이지 권한이 아닙니다.** 사용자 권한은 Scope가 정하고 U3 핸들러가
	// 강제합니다. 이 값은 "이 배포에서 이 GVR이 검토 대상으로 열려 있고, 지금 캐시가
	// ready이며, API가 get·patch를 제공한다"는 사실만 말합니다.
	DryRun bool
}

// Snapshot은 한 시점의 카탈로그 전체입니다.
type Snapshot struct {
	RefreshedAt time.Time
	Failure     string
	Descriptors []Descriptor
}

/* ── entry ───────────────────────────────────────────────────────────────
   allowlist 항목 하나와 그 informer의 수명입니다. */

type entryStatus struct {
	state  State
	reason string
}

// entrySnapshot은 한 번의 재구성이 만든 **완성된 한 벌**입니다. (P1-2)
//
// 목록 인덱스와 검색 인덱스를 따로 게시하면 요청이 "새 목록 + 낡은 검색"을 볼 수
// 있고, 두 화면이 서로 다른 시각을 말하게 됩니다. 그래서 둘을 한 구조체에 담아
// 포인터 하나로 교체하고, `Store.List()`도 정렬도 한 번만 합니다 — observedAt은
// 언제나 index.builtAt 하나입니다.
type entrySnapshot struct {
	index  *indexSnapshot
	search *searchSnapshot
	// searchState/searchReason은 이 GVR이 검색에 참여하는지와 그 사유입니다.
	// "결과 0건"과 "예산 초과로 색인하지 못함"을 구분합니다.
	searchState  SearchState
	searchReason string

	// ── Round 6 증분 경로 ────────────────────────────────────────────────
	//
	// sindex는 지속 구조 기반 증분 검색 인덱스입니다. 있으면 클러스터 전체 접근
	// 질의가 이것을 씁니다. 없으면(=롤백·레거시) 위의 search를 그대로 씁니다.
	//
	// indexVer/searchVer는 두 절반이 서로를 덮어쓰지 못하게 하는 단조 버전입니다.
	// 게시는 언제나 쓰기 잠금 안에서 **반대쪽 절반을 그대로 옮겨 담고** 자기 절반만
	// 바꾸므로 구조적으로 덮어쓸 수 없고, 버전은 그 사실을 테스트가 못박는 장치입니다.
	//
	// coversThroughSeq는 이 목록 스냅숏이 **덮는** eventSeq입니다. Store를 읽기
	// **전에** 캡처하므로, 이 값 이하의 seq를 가진 이벤트는 이미 Store에 반영되어
	// 있었습니다. 회수는 coversThroughSeq >= markerSeq일 때만 시작합니다.
	sindex           *searchIndex
	indexVer         uint64
	searchVer        uint64
	coversThroughSeq uint64
}

type resourceEntry struct {
	gvr schema.GroupVersionResource

	// informer 수명(informer/stop/done/lifecycle/generation)과 게시된 스냅숏(snap)은
	// **전부 Service.snapMu 아래에서만** 읽고 씁니다. 잠금은 이 하나뿐입니다.
	//
	// 잠금을 둘로 나누면 재구성 시작 시점의 신원을 원자적으로 집을 수 없습니다.
	// informer를 A 잠금에서, 세대를 B 잠금에서 따로 읽으면 그 사이에 discard가
	// 끼어들어 **옛 informer + 새 세대** 조합이 잡히고, 멈춘 캐시로 만든 결과가
	// 유효한 자격으로 게시됩니다. (P1-1)
	//
	// 스냅숏을 atomic 포인터로 두지 않는 이유도 같은 줄기입니다. 읽기 잠금 아래에서만
	// 쓰면 게시가 끝난 순간 옛 세대를 붙잡은 요청이 존재할 수 없으므로, 살아 있는
	// 세대는 언제나 (게시본, 만드는 중) 둘뿐입니다.
	informer cache.SharedIndexInformer
	stop     chan struct{}
	done     chan struct{}
	// lifecycle은 informer **인스턴스** 번호입니다. startEntry가 올립니다.
	// generation은 **정지** 세대입니다. discard가 올립니다.
	// 둘을 함께 봐야 "멈췄다가 다시 시작한 informer"와 "계속 돌던 informer"가 구분됩니다.
	lifecycle  uint64
	generation uint64
	// snapPtr은 게시된 스냅숏입니다. **쓰기는 언제나 snapMu 아래**이지만 읽기는
	// 두 경로가 있습니다.
	//
	//	read()  스냅숏을 쥔 동안 게시를 막아야 하는 짧은 구간(목록·카탈로그·상세).
	//	load()  잠금 없이 한 번만 집는 긴 구간(검색 순회·병합·계측).
	//
	// 검색이 서비스 전역 읽기 잠금을 쥔 채 수만 행을 훑으면, 그동안 **다른 GVR의
	// 목록 게시까지** 밀립니다. 스냅숏은 불변이므로 긴 경로는 포인터만 원자적으로
	// 집어 가면 충분합니다. (Round 7 §6)
	snapPtr atomic.Pointer[entrySnapshot]

	// baseline은 **목록 스냅숏만** 담은 별도 포인터입니다. (Round 7 §P1-3)
	//
	// Catalog·Describe·List·Detail·Recent는 이것만 봅니다. 검색 스냅숏과 수명이
	// 분리되어 있으므로, 그 경로들이 검색 세대를 간접적으로 붙잡지 않습니다 —
	// 목록 요청 하나가 은퇴한 검색 인덱스를 회계에 붙잡아 두는 일이 없습니다.
	baseline atomic.Pointer[indexSnapshot]
	// baseCovers는 baseline이 덮는 eventSeq입니다.
	baseCovers atomic.Uint64
	// leasePtr은 지금 게시된 **검색 세대의 소유권**입니다. 은퇴한 세대는 마지막
	// 독자가 놓을 때까지 회계에 남습니다.
	leasePtr atomic.Pointer[searchLease]
	// ownerRetained는 이 GVR이 소유한 **모든 세대**(게시본 + 아직 독자가 붙잡고 있는
	// 은퇴 세대)의 바이트 합입니다. I-B(GVR별 상한)의 판정 기준입니다.
	//
	// leasePtr 하나만 보면, 느린 독자가 은퇴 세대를 여럿 붙잡은 GVR이 자기 몫의
	// 몇 배를 차지해도 승인이 통과합니다. 게시가 적립하고 은퇴가 감액하며,
	// 두 방향 모두 **정확히 한 번씩만** 반영됩니다.
	ownerRetained atomic.Int64

	status atomic.Pointer[entryStatus]
	dirty  atomic.Bool
	// tokenPacked는 지금 살아 있는 informer의 수명 신원입니다(lifecycle<<32|generation).
	// 콜백은 **설치 시점에 고정된 binding**과 이 값을 비교해, 옛 informer의 지연
	// 콜백이 새 세대의 큐에 섞이지 못하게 합니다. (Round 7 §2)
	tokenPacked atomic.Uint64
	// bootstrapped는 이 informer 인스턴스에서 증분 인덱스를 한 번 세웠는지입니다.
	bootstrapped atomic.Bool
	// lastInputEstimate/lastNeeded/lastAvail은 부트스트랩 예산 회로입니다.
	//
	// **입력 추정치가 줄거나 실제 가용 용량이 늘었을 때만** 다시 빌드합니다.
	// 최종 게시에서 거절된 경우에도 여기 지문을 남겨야 다음 tick이 같은 입력으로
	// 전체 인덱스를 또 만들지 않습니다.
	//
	// lastAvail이 설정 상한(max)이 아니라 **가용 용량**인 것이 핵심입니다. 설정은
	// 그대로인 채 다른 GVR이 놓거나 은퇴 세대가 빠져 자리가 생기는 경우가 실제
	// 재시도 사유이고, max만 보면 그 경우를 영원히 놓칩니다.
	lastInputEstimate atomic.Int64
	lastNeeded        atomic.Int64
	lastAvail         atomic.Int64
}

// buildToken은 재구성이 시작될 때의 **수명 신원**입니다.
//
// informer 인스턴스 번호와 정지 세대를 같은 잠금 아래에서 함께 집습니다.
// 게시는 둘 다 그대로일 때만 통과하므로, 멈춘(또는 재시작된) informer의 캐시로
// 만든 결과가 새 자격을 빌려 게시되는 일이 없습니다.
type buildToken struct {
	lifecycle  uint64
	generation uint64
}

// searchBytesOf는 스냅숏 한 벌이 붙잡고 있는 검색 인덱스 바이트입니다.
// 레거시 배열 표현과 증분 지속 표현을 **둘 다** 셉니다.
func (es *entrySnapshot) searchBytesOf() int64 {
	if es == nil {
		return 0
	}
	var total int64
	if es.search != nil {
		total += es.search.bytes
	}
	if es.sindex != nil {
		total += es.sindex.bytes
	}
	return total
}

// read는 게시된 스냅숏을 read lock 아래에서 넘겨줍니다.
//
// **잠금은 서비스 단위 하나뿐입니다.** 항목마다 잠금을 두고 요청 중간에 하나씩
// 더 잡으면, 대기 중인 writer와 얽혀 순환 대기가 생길 수 있습니다. 게시는 포인터
// 교체와 카운터 갱신뿐이라 쓰기 구간이 매우 짧고, 읽기 구간에서는 네트워크 호출을
// 하지 않습니다 — 상세 live GET은 UID를 복사한 뒤 잠금을 놓고 나갑니다.
func (e *resourceEntry) read(s *Service, fn func(*entrySnapshot)) {
	s.snapMu.RLock()
	defer s.snapMu.RUnlock()
	fn(e.snapPtr.Load())
}

// load는 게시된 스냅숏을 **잠금 없이** 집습니다.
//
// 스냅숏은 불변이므로 한 번 집은 값은 그 요청이 끝날 때까지 일관됩니다. 검색처럼
// 수만 행을 훑는 경로가 서비스 전역 잠금을 쥐면 다른 GVR의 목록 게시가 그만큼
// 밀리므로, 긴 경로는 이 접근자를 씁니다.
func (e *resourceEntry) load() *entrySnapshot { return e.snapPtr.Load() }

// setSnap은 스냅숏을 직접 갈아 끼웁니다. 게시 경로와 테스트 하네스만 씁니다.
// baseline 포인터도 함께 맞춥니다 — 두 값이 갈라지면 목록 경로가 낡은 것을 봅니다.
func (e *resourceEntry) setSnap(es *entrySnapshot) {
	e.snapPtr.Store(es)
	if es == nil {
		e.baseline.Store(nil)
		e.baseCovers.Store(0)
		return
	}
	e.baseline.Store(es.index)
	e.baseCovers.Store(es.coversThroughSeq)
}

// searchLease는 **검색 스냅숏 한 세대의 소유권**입니다. (Round 7 §P1-3)
//
// 게시는 새 세대를 설치하고 옛 세대를 은퇴시킵니다. 은퇴한 세대의 바이트는
// **마지막 독자가 놓을 때까지** retained/live에 그대로 남습니다 — 요청이 아직
// 그 인덱스를 훑고 있는데 회계에서 먼저 빼면 상한이 상한이 아니게 됩니다.
//
// acquire는 짧습니다: 포인터 하나를 읽고 refcount를 올릴 뿐이고, 순회는 잠금 밖에서
// 합니다. 그래서 긴 검색이 다른 GVR의 게시를 붙잡지 않습니다.
type searchLease struct {
	// snapPtr은 이 세대가 **지금** 대표하는 스냅숏입니다.
	//
	// 목록만 갱신된 게시는 검색 payload를 그대로 물려받아 같은 세대를 계속 씁니다.
	// 그때 이 포인터를 새 스냅숏으로 갈아 끼워야 **이후 대여자**가 최신 상태·사유를
	// 봅니다. 이미 빌려 간 요청은 자기 지역 사본을 들고 있으므로 영향받지 않습니다.
	snapPtr atomic.Pointer[entrySnapshot]
	bytes   int64
	// owner는 이 세대를 소유한 항목입니다. 은퇴할 때 **서비스와 소유자 원장을 함께**
	// 줄이기 위해 필요합니다.
	owner *resourceEntry
	refs  atomic.Int64
	// retired는 더 이상 게시본이 아니라는 표시입니다. refs가 0이 되는 순간 회계에서 빠집니다.
	retired atomic.Bool
	// released는 회계 반영이 이미 끝났다는 표시입니다. 이중 해제를 막습니다.
	released atomic.Bool
}

func (l *searchLease) snapshot() *entrySnapshot {
	if l == nil {
		return nil
	}
	return l.snapPtr.Load()
}

// acquireSearch는 지금 게시된 검색 세대를 빌리고 **그 세대의 스냅숏**을 함께 줍니다.
//
// 두 값을 같은 읽기 잠금 안에서 집는 것이 핵심입니다. 빌린 뒤에 스냅숏을 다시
// 읽으면, 그 사이의 게시로 **빌리지 않은 세대**를 훑으면서 회계는 엉뚱한 옛 세대를
// 붙잡게 됩니다. 반드시 releaseSearch와 짝지으세요.
func (e *resourceEntry) acquireSearch(s *Service) (*searchLease, *entrySnapshot) {
	if e == nil {
		return nil, nil
	}
	s.snapMu.RLock()
	defer s.snapMu.RUnlock()
	if l := e.leasePtr.Load(); l != nil {
		l.refs.Add(1)
		return l, l.snapshot()
	}
	// 아직 세대가 설치되지 않은 항목(부트스트랩 전·테스트 하네스)입니다.
	// 빌릴 것은 없지만 이 요청이 볼 스냅숏은 여기서 **한 번** 고정합니다.
	return nil, e.snapPtr.Load()
}

// searchView는 요청 하나가 보는 **고정된** 자료입니다.
//
// base와 search는 Service.order와 같은 순서·길이입니다. 질의 경로는 이 배열만 보고
// baseline/snapPtr을 다시 읽지 않습니다 — 한 요청이 GVR마다 서로 다른 시점을 섞지 않고,
// 훑는 세대와 회계가 붙잡는 세대가 정확히 같아집니다.
//
// **범위 제한(Scope≠All) 요청은 search를 채우지 않고 lease도 빌리지 않습니다.**
// 그 경로는 검색 인덱스를 아예 훑지 않으므로, 빌리면 볼 수도 없는 세대의 수명을
// 늘리고 회계를 붙잡을 뿐입니다.
type searchView struct {
	base   []*indexSnapshot
	search []*entrySnapshot
	leases []*searchLease
}

// baseAt은 order[i] 항목의 고정 목록 스냅숏입니다.
func (v *searchView) baseAt(i int) *indexSnapshot {
	if v == nil || i < 0 || i >= len(v.base) {
		return nil
	}
	return v.base[i]
}

// searchAt은 order[i] 항목의 **빌린 세대** 스냅숏입니다.
// 범위 제한 요청에서는 언제나 nil입니다.
func (v *searchView) searchAt(i int) *entrySnapshot {
	if v == nil || i < 0 || i >= len(v.search) {
		return nil
	}
	return v.search[i]
}

// baselineView는 목록 스냅숏만 고정한 요청 뷰입니다. lease를 빌리지 않습니다.
func (s *Service) baselineView() *searchView {
	v := &searchView{base: make([]*indexSnapshot, len(s.order))}
	for i, gvr := range s.order {
		v.base[i] = s.entries[gvr].baselineIndex()
	}
	return v
}

// acquireView는 검색 세대를 한 번씩 빌려 요청 뷰를 만듭니다.
// 클러스터 전체 접근 요청만 씁니다.
func (s *Service) acquireView() *searchView {
	v := &searchView{
		base:   make([]*indexSnapshot, len(s.order)),
		search: make([]*entrySnapshot, len(s.order)),
	}
	for i, gvr := range s.order {
		e := s.entries[gvr]
		lease, snap := e.acquireSearch(s)
		v.search[i] = snap
		// 목록 카운트는 검색 세대가 아니라 baseline에서 옵니다. 목록만 갱신된
		// 게시에서 검색 세대를 다시 만들지 않기 때문입니다.
		v.base[i] = e.baselineIndex()
		if lease != nil {
			v.leases = append(v.leases, lease)
		}
	}
	return v
}

// releaseView는 **빌린 것만 정확히 한 번** 놓습니다. 두 번 불러도 안전합니다.
func (s *Service) releaseView(v *searchView) {
	if v == nil {
		return
	}
	leases := v.leases
	v.leases = nil
	for _, l := range leases {
		s.releaseSearch(l)
	}
}

// releaseSearch는 빌린 세대를 놓습니다. 은퇴한 세대의 마지막 참조면 회계에서 뺍니다.
func (s *Service) releaseSearch(l *searchLease) {
	if l == nil {
		return
	}
	if l.refs.Add(-1) != 0 {
		return
	}
	if !l.retired.Load() {
		return // 아직 게시본입니다. 회계에 남아 있어야 합니다.
	}
	s.retireLeaseBytes(l)
}

// retireLeaseBytes는 은퇴 세대의 바이트를 **정확히 한 번** 회계에서 뺍니다.
//
// 서비스 전역 원장과 **소유자(GVR) 원장을 같은 한 번에** 줄입니다. 한쪽만 줄이면
// I-B가 영영 닫히거나(소유자만 남음) I-B가 헐거워집니다(전역만 남음).
func (s *Service) retireLeaseBytes(l *searchLease) {
	if l == nil || l.bytes == 0 || !l.released.CompareAndSwap(false, true) {
		return
	}
	s.searchBytes.Add(-l.bytes)
	if l.owner != nil {
		l.owner.ownerRetained.Add(-l.bytes)
	}
	s.budget.releaseRetained(l.bytes)
}

// admitLeaseLocked는 새 세대의 **소유권을 원자적으로 승인**합니다.
//
// 승인 대상은 이번에 늘어나는 몫뿐이지만, 판정은 현재 게시본과 아직 독자가
// 붙잡고 있는 은퇴 세대까지 모두 포함된 retained·live 위에서 이뤄집니다.
// 통과하지 못하면 아무것도 설치하지 않습니다 — 검사와 적립이 갈라지지 않습니다.
//
// I-B(GVR별 상한)의 기준은 **그 GVR이 소유한 모든 세대의 합**인 ownerRetained입니다.
// 직전 세대 하나만 세면, 느린 독자가 은퇴 세대를 여럿 붙잡은 GVR이 자기 몫을
// 몇 배로 넘겨도 통과합니다.
func (s *Service) admitLeaseLocked(e *resourceEntry, next *entrySnapshot) bool {
	old := e.leasePtr.Load()
	if old != nil && next != nil && sameSearchHalf(old.snapshot(), next) {
		return true // 검색 절반이 그대로입니다. 늘어나는 몫이 없습니다.
	}
	var delta int64
	if next != nil {
		delta = next.searchBytesOf()
	}
	if delta <= 0 {
		return true
	}
	if !s.budget.admitRetained(delta, e.ownerRetained.Load(), s.perResourceSearchBudget()) {
		return false
	}
	// 소유자 원장은 승인이 통과한 뒤에 적립합니다. 실패한 시도가 GVR 몫을
	// 부풀려 이후 게시를 스스로 막는 일이 없어야 합니다.
	e.ownerRetained.Add(delta)
	return true
}

// availableRetained는 이 항목이 **지금 실제로 더 받을 수 있는** 바이트입니다.
//
// 전역 retained 여유·GVR별 여유·live 여유의 최솟값입니다. 예산 거절 지문에
// 설정값(max)이 아니라 이 값을 적어야, "설정은 그대로인데 다른 GVR이 놓아서
// 자리가 생긴" 경우를 재시도 조건으로 잡을 수 있습니다.
func (s *Service) availableRetained(e *resourceEntry) int64 {
	avail := s.budget.availableRetained(s.perResourceSearchBudget(), e.ownerRetained.Load())
	if avail < 0 {
		return 0
	}
	return avail
}

// publishLeaseLocked는 **승인과 설치를 한 쌍으로** 묶습니다.
//
// 승인이 적립까지 끝내므로, 설치만 따로 부르면 회계가 비고 검사만 따로 부르면
// 검사와 적립이 갈라집니다. 게시 경로는 반드시 이 하나를 씁니다.
// 승인에 실패하면 아무것도 설치하지 않습니다.
func (s *Service) publishLeaseLocked(e *resourceEntry, snap *entrySnapshot) bool {
	if !s.admitLeaseLocked(e, snap) {
		return false
	}
	s.installLeaseLocked(e, snap)
	return true
}

// sameSearchHalf는 두 스냅숏의 **검색 절반이 같은 객체**인지입니다.
//
// 목록만 갱신된 게시는 검색 payload를 그대로 물려받습니다. 그때 새 lease를 만들면
// 같은 검색 객체를 한 세대 더 계상하고, 옛 세대를 은퇴시켜 독자를 붙잡습니다 —
// 실제로는 아무것도 바뀌지 않았는데 회계와 수명만 흔들립니다.
func sameSearchHalf(a, b *entrySnapshot) bool {
	return a != nil && b != nil && a.sindex == b.sindex && a.search == b.search
}

// installLeaseLocked는 새 세대를 설치하고 옛 세대를 은퇴시킵니다.
// snapMu 쓰기 잠금 아래에서만 부릅니다. 새 바이트는 **이미 예약되어 있어야** 합니다.
func (s *Service) installLeaseLocked(e *resourceEntry, snap *entrySnapshot) {
	old := e.leasePtr.Load()
	// 검색 절반이 그대로면 세대를 바꾸지 않습니다(목록만 갱신된 게시).
	//
	// 세대는 그대로지만 **스냅숏 메타데이터는 바뀝니다** — 상태·사유·목록·커버 seq가
	// 그렇습니다. 이 세대가 대표하는 스냅숏을 갈아 끼워 두어야 이후 대여자가
	// syncing→ready 전이를 봅니다. 이미 빌려 간 요청 뷰는 자기가 집은 포인터를
	// 들고 있으므로 이 store에 영향받지 않습니다.
	if old != nil && snap != nil && sameSearchHalf(old.snapshot(), snap) {
		old.snapPtr.Store(snap)
		return
	}
	var bytes int64
	if snap != nil {
		bytes = snap.searchBytesOf()
	}
	if snap == nil {
		e.leasePtr.Store(nil)
	} else {
		next := &searchLease{bytes: bytes, owner: e}
		next.snapPtr.Store(snap)
		next.refs.Store(1) // 게시본 자신의 참조
		e.leasePtr.Store(next)
		if bytes != 0 {
			// 적립은 admitLeaseLocked가 이미 원자적으로 끝냈습니다. 여기서는
			// 계측용 합계만 맞춥니다 — 같은 바이트를 두 번 적립하지 않습니다.
			s.searchBytes.Add(bytes)
			s.observePeak(s.budget.live.Load())
		}
	}
	if old == nil {
		return
	}
	old.retired.Store(true)
	if old.refs.Add(-1) == 0 {
		s.retireLeaseBytes(old)
	}
}

// baselineIndex는 목록 스냅숏을 **잠금 없이** 집습니다.
//
// 검색 포인터를 함께 붙잡지 않으므로, 목록·상세·최근 항목 요청이 은퇴한 검색
// 세대의 수명을 늘리지 않습니다.
func (e *resourceEntry) baselineIndex() *indexSnapshot {
	if e == nil {
		return nil
	}
	return e.baseline.Load()
}

// packToken은 수명 신원을 원자적으로 읽을 수 있는 하나의 값으로 접습니다.
func packToken(lifecycle, generation uint64) uint64 {
	return lifecycle<<32 | (generation & 0xffffffff)
}

// handlerBinding은 informer 인스턴스 **하나**에 고정된 콜백 신원입니다.
//
// 콜백이 매번 현재 세대를 다시 읽으면(동적 태그), 재시작 직후 도착한 옛 informer의
// 이벤트가 새 세대의 신원을 빌려 큐에 들어갑니다. 그래서 신원은 설치 시점에
// 한 번 정해지고 그 뒤로 바뀌지 않습니다.
type handlerBinding struct {
	entry  *resourceEntry
	packed uint64
	// namespaced는 이 리소스가 namespace를 갖는지입니다. **설치 시점에 고정**됩니다.
	//
	// key-only tombstone은 문자열 하나뿐이라, 이 성질을 모르면 "ns/name"과 "name"을
	// 구분할 근거가 없습니다. discovery를 그때그때 다시 읽으면 갱신 사이에 값이
	// 바뀌어 같은 informer의 이벤트가 서로 다른 규칙으로 해석됩니다.
	namespaced bool
}

// beginBuild는 재구성 대상 informer와 그 수명 신원을 **한 번의 잠금에서** 집습니다.
// 빌드 자체(Store.List·정렬·색인)는 잠금 밖에서 돕니다.
func (e *resourceEntry) beginBuild(s *Service) (cache.SharedIndexInformer, buildToken, bool) {
	s.snapMu.RLock()
	defer s.snapMu.RUnlock()
	return e.informer, buildToken{lifecycle: e.lifecycle, generation: e.generation}, e.snapPtr.Load() != nil
}

// publish는 수명 신원이 그대로일 때만 완성된 한 벌을 게시합니다.
//
// 게시와 보유 바이트 갱신을 같은 쓰기 잠금 안에서 합니다. 둘을 나누면 그 틈에
// discard가 끼어들어 회계가 음수가 되거나 멈춘 항목이 되살아납니다.
// 반환값은 실제로 게시했는지입니다.
func (e *resourceEntry) publish(s *Service, token buildToken, index *indexSnapshot, result searchBuildResult) bool {
	return e.publishList(s, token, index, result, listPublishExtra{}) != publishRejectedLifecycle
}

// publishOutcome은 목록 게시 한 번의 결과입니다.
//
// **수명 게시**와 **검색 인덱스 설치**를 구분합니다. 둘을 하나의 bool로 뭉개면
// 부트스트랩이 예산으로 거절됐는데도 "게시 성공"으로 읽혀 bootstrapped가 서고,
// 덮지도 않은 이벤트를 ack하게 됩니다.
type publishOutcome uint8

const (
	// publishRejectedLifecycle은 세대가 바뀌어 게시 자체가 무효인 경우입니다.
	publishRejectedLifecycle publishOutcome = iota
	// publishedListOnly는 목록만 게시한 경우입니다(검색 절반을 이번에 정하지 않음).
	publishedListOnly
	// publishedWithIndex는 최종 잠금 승인까지 통과해 **검색 인덱스를 설치**한 경우입니다.
	publishedWithIndex
	// publishedBudgetRejected는 목록은 게시했지만 검색 절반이 예산으로 거절된 경우입니다.
	// 명시적으로 unavailable이며, 입력이나 예산이 바뀌면 다시 시도할 수 있습니다.
	publishedBudgetRejected
)

// listPublishExtra는 목록 게시가 함께 옮기는 증분 경로 값입니다.
type listPublishExtra struct {
	// setIndex가 참이면 sindex 절반도 이번 게시가 정합니다(부트스트랩).
	setIndex bool
	sindex   *searchIndex
	// keepSearch가 참이면 검색 절반(상태·사유·스냅숏)을 **그대로 옮겨 담습니다.**
	// 이미 부트스트랩된 증분 모드에서 목록만 갱신할 때 씁니다.
	keepSearch bool
	// setCovers가 참이면 coversThroughSeq를 이 값으로 갱신합니다.
	setCovers bool
	covers    uint64
}

// publishList는 목록 절반을 게시합니다. **검색 절반은 그대로 옮겨 담습니다.**
//
// 쓰기 잠금 안에서 현재 스냅숏을 읽어 반대쪽 절반을 복사하므로, 목록 게시가
// 더 새로운 검색 게시를 덮어쓸 수 없습니다. indexVer는 그 사실을 단언하기 위한
// 단조 카운터입니다.
func (e *resourceEntry) publishList(s *Service, token buildToken, index *indexSnapshot,
	result searchBuildResult, extra listPublishExtra) publishOutcome {

	s.snapMu.Lock()
	defer s.snapMu.Unlock()
	// 인스턴스 번호와 정지 세대를 **둘 다** 봅니다. 하나만 보면 "멈췄다가 다시
	// 시작한" 경우와 "계속 돌던" 경우를 구분하지 못합니다.
	if e.lifecycle != token.lifecycle || e.generation != token.generation {
		// 이 빌드가 도는 사이에 항목이 멈췄거나 다시 시작했습니다.
		return publishRejectedLifecycle
	}
	cur := e.snapPtr.Load()
	next := &entrySnapshot{
		index: index, search: result.snapshot,
		searchState: result.state, searchReason: result.reason,
	}
	if next.searchState == "" {
		next.searchState = SearchSyncing
	}
	if cur != nil {
		// 검색 절반은 언제나 현재 값을 옮겨 담습니다.
		next.sindex = cur.sindex
		next.searchVer = cur.searchVer
		next.coversThroughSeq = cur.coversThroughSeq
		next.indexVer = cur.indexVer
		if extra.keepSearch {
			next.search = cur.search
			next.searchState, next.searchReason = cur.searchState, cur.searchReason
		}
	}
	if extra.setIndex {
		next.sindex = extra.sindex
		next.searchVer++
	}
	if extra.setCovers {
		next.coversThroughSeq = extra.covers
	}
	next.indexVer++

	// 부트스트랩 게시도 **최종 잠금 안에서** 소유권 승인을 받습니다.
	// preflight는 빌드 시작 시점의 사실이고, 그 사이 다른 GVR이 예산을 가져갔을 수
	// 있습니다. 검사와 적립이 한 임계 구역이라 그 틈으로 둘 다 통과하는 일이 없습니다.
	installed := extra.setIndex && extra.sindex != nil
	budgetRejected := false
	// 회계는 **소유권 원장(lease)** 하나에서만 움직입니다. 새 세대를 설치하고 옛
	// 세대를 은퇴시키면, 아직 그 세대를 훑고 있는 요청이 있는 동안 바이트가 남습니다.
	if !s.publishLeaseLocked(e, next) {
		// 검색 절반을 버리고 **목록만** 게시합니다 — 반쪽을 올리지 않습니다.
		next.sindex, next.search = nil, nil
		next.searchVer = 0
		if cur != nil {
			next.sindex, next.search = cur.sindex, cur.search
			next.searchVer = cur.searchVer
		}
		next.searchState, next.searchReason = SearchUnavailable, reasonBudget
		extra.setIndex, installed, budgetRejected = false, false, true
		// 되돌린 뒤의 몫으로 다시 승인받습니다(검색 절반이 그대로라 delta가 0입니다).
		if !s.publishLeaseLocked(e, next) {
			return publishRejectedLifecycle
		}
	}
	e.setSnap(next)

	switch {
	case installed:
		// **설치가 실제로 끝났을 때만** 덮는 이벤트와 stale 마커를 지웁니다.
		if extra.setCovers {
			s.ackCoveredLocked(e.gvr, extra.covers)
		}
		return publishedWithIndex
	case budgetRejected:
		// 적용할 색인이 없습니다. 비울 수 없는 큐를 남기지 않고, 곧바로 다시
		// 빌드하지 않도록 쿨다운을 겁니다. 상태는 명시적 unavailable이라 입력이나
		// 예산이 바뀌면 다시 시도할 수 있습니다.
		// 리소스는 계속 살아 있습니다 — 큐는 남기고 대기분만 비웁니다.
		s.purgeQueueLocked(e.gvr, false)
		s.startBudgetCooldown()
		return publishedBudgetRejected
	default:
		return publishedListOnly
	}
}

// startBudgetCooldown은 예산 거절 뒤 곧바로 다시 빌드하지 않도록 쿨다운을 겁니다.
func (s *Service) startBudgetCooldown() {
	if s.delta == nil {
		return
	}
	s.delta.mu.Lock()
	s.delta.cooldownUntil = s.nowOrDefault().Add(recoveryCooldown)
	s.delta.mu.Unlock()
}

// publishSearchIndex는 증분 검색 절반만 낙관적으로 게시합니다.
//
// 기대 기준 버전이 그대로일 때만 통과합니다. 충돌하면 호출자가 드레인한 키를
// 큐로 되돌립니다(유실 없음). 여기서 I-A·I-B를 다시 봅니다.
func (s *Service) publishSearchIndex(e *resourceEntry, token buildToken, baseVersion uint64, next *searchIndex) recoveryOutcome {
	if next == nil {
		return recoveryVersionConflict
	}
	s.snapMu.Lock()
	defer s.snapMu.Unlock()
	if e.lifecycle != token.lifecycle || e.generation != token.generation {
		return recoveryVersionConflict
	}
	cur := e.snapPtr.Load()
	if cur == nil || cur.searchVer != baseVersion {
		return recoveryVersionConflict
	}
	out := *cur
	out.sindex = next
	out.searchVer = cur.searchVer + 1
	// 승인과 설치를 한 쌍으로 합니다(I-A/I-B/I-C를 한 임계 구역에서 판정·적립).
	//
	// **여기서 거절되면 버전 문제가 아니라 예산 문제입니다.** 되돌려서 다시 시도하면
	// 같은 결과가 100ms마다 반복되므로, 호출자는 명시적 stale + 쿨다운으로 바꿉니다.
	if !s.publishLeaseLocked(e, &out) {
		return recoveryBudgetRejected
	}
	e.setSnap(&out)
	return recoveryPublished
}

// sindexBytesOf/entryRetained는 회계 보조입니다.
func sindexBytesOf(es *entrySnapshot) int64 {
	if es == nil || es.sindex == nil {
		return 0
	}
	return es.sindex.bytes
}

func entryRetained(es *entrySnapshot) int64 { return es.searchBytesOf() }

// publishRecovery는 **이미 조각 단위로 다 만들어 둔** 측면 인덱스를 게시합니다.
//
// 게시 시점에 목록 행을 다시 훑지 않습니다 — 티켓이 고정한 원본으로 슬라이스마다
// 조금씩 쌓아 왔고, 여기서는 마지막으로 보류 키를 반영한 뒤 CAS만 합니다.
//
// CAS 조건은 셋입니다.
//   - 수명 신원(lifecycle·generation)이 티켓이 고정한 그대로일 것
//   - GVR 전체 회수면 searchVer가 티켓이 고정한 그대로일 것
//   - namespace 회수면 대상 파티션의 partVersion이 그대로일 것
//
// 하나라도 어긋나면 게시하지 않습니다(버전 충돌 — 예산 거절과 구분합니다).
// recoveryPublishInput은 게시가 쓰는 **불변 입력 전부**입니다.
//
// 게시가 티켓 필드를 직접 읽으면 delta.mu 밖에서 mutable 상태를 만지게 됩니다.
// 그래서 캡처 시점에 잠금 아래에서 통째로 복사해 넘깁니다.
type recoveryPublishInput struct {
	token        buildToken
	whole        bool
	namespace    string
	side         *searchIndex
	partVersion  uint64
	srcSearchVer uint64
	store        cache.Store
	held         []deltaEvent
}

func (s *Service) publishRecovery(e *resourceEntry, in recoveryPublishInput) recoveryOutcome {
	if in.side == nil {
		return recoveryVersionConflict
	}
	side := in.side
	// 보류 키를 **지금의 Store**로 다시 해석해 마지막 조각으로 얹습니다.
	// 같은 키가 여럿이면 마지막 것만 씁니다(last-event-wins) — applyPartOps에
	// 같은 이름이 두 번 들어가지 않도록 여기서 방어적으로 눌러 둡니다.
	byNS := make(map[string][]partOp, 2)
	if len(in.held) > 0 {
		seen := make(map[string]int, len(in.held))
		var keyBuf, tokBuf []string
		store := in.store
		for _, ev := range in.held {
			key := ev.name
			if ev.namespace != "" {
				key = ev.namespace + "/" + ev.name
			}
			op := partOp{name: ev.name}
			if store != nil {
				if obj, exists, err := store.GetByKey(key); err == nil && exists {
					if m, ok := obj.(*metav1.PartialObjectMetadata); ok && m != nil {
						row := indexRow{namespace: m.Namespace, name: m.Name, obj: m}
						var truncated bool
						tokBuf, truncated, keyBuf = labelTokensOf(&row, keyBuf, tokBuf)
						labels := make([]string, len(tokBuf))
						copy(labels, tokBuf)
						op.input = &rowInput{name: m.Name, uid: string(m.UID),
							labels: labels, keysTruncated: truncated}
					}
				}
			}
			id := ev.namespace + "\x00" + ev.name
			if at, dup := seen[id]; dup {
				byNS[ev.namespace][at] = op
				continue
			}
			seen[id] = len(byNS[ev.namespace])
			byNS[ev.namespace] = append(byNS[ev.namespace], op)
		}
		var st applyStats
		side = side.applyOps(s.nowOrDefault(), byNS, &st)
		if st.slotExhausted {
			return recoveryBudgetRejected
		}
	}

	s.snapMu.Lock()
	defer s.snapMu.Unlock()
	if e.lifecycle != in.token.lifecycle || e.generation != in.token.generation {
		return recoveryVersionConflict
	}
	live := e.snapPtr.Load()
	if live == nil || live.sindex == nil {
		return recoveryVersionConflict
	}
	var out entrySnapshot = *live
	if in.whole {
		// 전체 회수는 인덱스를 통째로 바꿉니다 — 그 사이 델타 게시가 있었다면 무효입니다.
		if live.searchVer != in.srcSearchVer {
			return recoveryVersionConflict
		}
		out.sindex = side
	} else {
		part := side.dir.find(in.namespace)
		if part == nil {
			part = newNsPart(in.namespace, live.sindex.kindTok, s.nowOrDefault())
		}
		latest := live.sindex.dir.find(in.namespace)
		if latest != nil && latest.partVersion != in.partVersion {
			return recoveryVersionConflict
		}
		merged := *live.sindex
		merged.version = live.sindex.version + 1
		var copied int64
		merged.dir = live.sindex.dir.upsert(part, &copied)
		merged.bytes = merged.dir.bytes() + int64(len(merged.kind)+len(merged.kindTok)) + 128
		out.sindex = &merged
	}
	out.searchVer = live.searchVer + 1
	// 게시 잠금 안에서 승인과 설치를 한 쌍으로 합니다.
	// 넘으면 예산 거절이지 충돌이 아닙니다.
	if !s.publishLeaseLocked(e, &out) {
		return recoveryBudgetRejected
	}
	e.setSnap(&out)
	return recoveryPublished
}

// recoveryOutcome은 회수 게시의 결과입니다. **버전 충돌과 예산 거절을 구분합니다** —
// 충돌은 곧바로 다시 시도할 수 있지만, 예산 거절은 쿨다운과 회로를 타야 합니다.
type recoveryOutcome uint8

const (
	recoveryPublished recoveryOutcome = iota
	recoveryVersionConflict
	recoveryBudgetRejected
)

// discard는 세대를 올리고 게시된 스냅숏·회계·informer를 **한 번의 잠금에서** 지웁니다.
// informer가 이미 없더라도 반드시 지웁니다 — "멈췄는데 인덱스만 남는" 상태를 없앱니다.
//
// 채널 닫기와 종료 대기는 잠금 밖에서 합니다.
func (e *resourceEntry) discard(s *Service) (stop, done chan struct{}) {
	s.snapMu.Lock()
	// 세대와 인스턴스 번호를 **둘 다** 올립니다. 세대만 올리면 사라진 인스턴스의
	// 번호를 그대로 주장하는 토큰(옛 informer + 새 세대)이 유효해집니다.
	e.generation++
	e.lifecycle++
	// 검색 세대를 은퇴시킵니다. 아직 훑고 있는 요청이 있으면 그 바이트는 남아 있다가
	// 마지막 독자가 놓을 때 빠집니다 — 회계가 실제보다 먼저 줄지 않습니다.
	s.installLeaseLocked(e, nil)
	e.setSnap(nil)
	e.tokenPacked.Store(packToken(e.lifecycle, e.generation))
	e.bootstrapped.Store(false)
	e.clearBudgetRejection()
	stop, done = e.stop, e.done
	e.informer, e.stop, e.done = nil, nil, nil
	// 대기 중인 델타는 죽은 세대의 것입니다. **잠금 순서 snapMu → queueMu 그대로**
	// 여기서만 겹쳐 잡습니다(역순은 어디에도 없습니다).
	s.purgeQueueLocked(e.gvr, true)
	s.snapMu.Unlock()
	return stop, done
}

// purgeQueueLocked는 이 GVR의 대기·보류 키를 버리고 예약을 정확히 한 번 해제합니다.
// snapMu를 잡은 상태에서만 호출됩니다.
//
// terminal이 참이면 **리소스가 멈추는 경로**입니다. 그때는 이벤트·보류·티켓을
// 정리한 뒤 큐 구조 자체의 고정 몫까지 되돌리고 큐를 지웁니다 — 멈춘 리소스는
// 원장에서 0으로 수렴해야 합니다. 거짓이면 큐는 계속 살아 있고(다음 이벤트를
// 받아야 하므로) 고정 몫도 그 큐의 것으로 남습니다.
func (s *Service) purgeQueueLocked(gvr schema.GroupVersionResource, terminal bool) {
	if s.delta == nil {
		return
	}
	s.delta.mu.Lock()
	defer s.delta.mu.Unlock()
	// **티켓을 먼저 걷어냅니다.** 큐가 아직 만들어지지 않은 GVR에도 회수 티켓은
	// 있을 수 있습니다(드롭 없이 압축 요청만으로 잡힌 경우 등). 큐 존재 여부를
	// 먼저 보고 반환하면 그런 티켓이 죽은 세대에 살아남아, 뒤늦은 게시가
	// 새 세대의 인덱스를 덮습니다.
	//
	// 여기서 죽은 티켓은 finishRecovery/abandon이 소유권 검사에 걸려 되살리지 못하고,
	// 예약은 dropTicketLocked가 정확히 한 번 해제합니다.
	if t := s.delta.ticket; t != nil && t.gvr == gvr {
		s.dropTicketLocked(t)
	}
	if terminal {
		// 멈춘 리소스의 회로도 함께 걷어냅니다. 회로는 큐와 같은 수명이며,
		// 남겨 두면 회로 상한만 갉아먹고 원장도 0으로 돌아가지 않습니다.
		s.delta.dropCircuitsForLocked(gvr)
	}
	q, ok := s.delta.queues[gvr]
	if !ok {
		return
	}
	for _, ev := range q.events {
		s.budget.releaseQueued(ev.reserved)
	}
	q.events = q.events[:0]
	s.releaseHeldLocked(q)
	q.reindex()
	// 낡음 집합과 그 **동적 문자열 몫**을 함께 되돌립니다.
	s.delta.clearStaleLocked(q)
	q.gvrStale = false
	q.markerSeq = 0
	q.staleEpoch++
	if !terminal {
		// 리소스는 계속 삽니다. 배열·버킷은 비었으므로 **도달 가능한 저장 용량**을
		// 압축으로 되돌립니다 — 비었는데 고수위가 남으면 원장이 실제보다 큽니다.
		s.delta.compactQueueLocked(q)
		return
	}
	// **이벤트·보류·티켓·낡음을 모두 되돌린 뒤에** 구조 몫을 놓습니다.
	// 순서를 뒤집으면 아직 살아 있는 예약이 있는 큐를 원장에서 먼저 지우게 됩니다.
	//
	// 고정 구조뿐 아니라 **도달 가능한 저장 용량**(배열 용량·색인 고수위)까지
	// 함께 놓습니다. 하나라도 남기면 멈춘 리소스가 0으로 수렴하지 않습니다.
	delete(s.delta.queues, gvr)
	s.budget.releaseQueued(q.fixed + q.capCharged)
	q.fixed, q.capCharged = 0, 0
	q.eventCap, q.holdCap, q.indexCharged = 0, 0, 0
}

// install은 새 informer 인스턴스를 등록하고 인스턴스 번호를 올립니다.
// 이미 돌고 있으면 false이고, 그때는 호출자가 만든 informer를 버립니다.
func (e *resourceEntry) install(s *Service, informer cache.SharedIndexInformer, stop, done chan struct{}) bool {
	return e.installWithBinding(s, informer, stop, done, nil)
}

// installWithBinding은 설치와 동시에 콜백 신원을 확정합니다.
func (e *resourceEntry) installWithBinding(s *Service, informer cache.SharedIndexInformer, stop, done chan struct{},
	binding *handlerBinding) bool {
	s.snapMu.Lock()
	defer s.snapMu.Unlock()
	if e.informer != nil {
		return false
	}
	e.lifecycle++
	e.informer, e.stop, e.done = informer, stop, done
	e.tokenPacked.Store(packToken(e.lifecycle, e.generation))
	e.bootstrapped.Store(false)
	if binding != nil {
		// 이 informer 인스턴스의 콜백 신원을 **여기서 한 번** 확정합니다.
		// informer.Run은 아직 시작되지 않았으므로 콜백은 이 값을 완성된 상태로만 봅니다.
		binding.entry = e
		binding.packed = packToken(e.lifecycle, e.generation)
	}
	return true
}

func (e *resourceEntry) setStatus(state State, reason string) {
	e.status.Store(&entryStatus{state: state, reason: reason})
}

func (e *resourceEntry) currentStatus() entryStatus {
	if s := e.status.Load(); s != nil {
		return *s
	}
	return entryStatus{state: StateMissing}
}

func (e *resourceEntry) running(s *Service) bool {
	s.snapMu.RLock()
	defer s.snapMu.RUnlock()
	return e.informer != nil
}

// Service는 이 기능의 유일한 상태 소유자입니다.
type Service struct {
	cfg     Config
	clients Clients

	order   []schema.GroupVersionResource
	entries map[schema.GroupVersionResource]*resourceEntry

	disc atomic.Pointer[discoverySnapshot]

	// snapMu는 게시된 스냅숏과 세대의 **유일한** 경계입니다. (P1-C)
	// 읽는 쪽은 RLock, 게시·폐기는 Lock입니다. 쓰기 구간은 포인터 교체뿐입니다.
	snapMu sync.RWMutex

	// searchBytes는 모든 GVR이 지금 보유 중인 검색 인덱스 바이트 합입니다.
	// searchPeak는 재구성 중 (기존 + 신규 + 작업용)이 동시에 살아 있던 최대치입니다.
	searchBytes atomic.Int64
	searchPeak  atomic.Int64

	// budget은 I-A/I-B/I-C 세 불변식의 유일한 회계입니다. (Round 6)
	budget searchBudget
	// delta는 증분 경로의 큐·마커·회수 티켓입니다. queueMu는 그 안에 있습니다.
	delta *deltaState
	// eventSeq는 콜백·드롭이 찍는 단조 번호입니다. 목록 스냅숏의 coversThroughSeq와
	// 짝을 이뤄 "이 스냅숏이 그 이벤트를 덮는가"를 판정합니다.
	eventSeq atomic.Uint64

	guard *detailGuard
	// dryRunGuard는 검토 전용 예산입니다. guard와 **별도 인스턴스**여야 합니다 —
	// 하나를 공유하면 검토 한 건이 상세 조회 토큰을 먹습니다. (ADR 0019)
	dryRunGuard *detailGuard
	// dryRunAllow는 검토가 열린 GVR 집합입니다. 기동 시 확정되고 이후 바뀌지 않습니다.
	dryRunAllow map[schema.GroupVersionResource]bool

	mu      sync.Mutex
	cancel  context.CancelFunc
	wg      sync.WaitGroup
	started atomic.Bool
	closed  atomic.Bool
}

// New는 서비스를 만듭니다. 아직 discovery도 watch도 시작하지 않습니다.
func New(clients Clients, cfg Config) (*Service, error) {
	cfg.setDefaults()
	if clients.Metadata == nil || clients.Discovery == nil || clients.Dynamic == nil {
		return nil, fmt.Errorf("resourcecatalog: metadata·discovery·dynamic 클라이언트가 모두 필요합니다")
	}
	allow, err := NormalizeAllowlist(cfg.Allowlist, cfg.AllowCRDs)
	if err != nil {
		return nil, fmt.Errorf("resourcecatalog: %w", err)
	}
	// 검토 대상은 조회 allowlist가 확정된 **뒤에** 그 부분집합으로 확정합니다.
	// 오타·범위 초과는 조용한 "대상 없음"이 아니라 기동 실패입니다.
	dryRunAllow, err := NormalizeDryRunAllowlist(cfg.DryRunAllowlist, allow, cfg.DryRunDeny)
	if err != nil {
		return nil, fmt.Errorf("resourcecatalog: dry-run %w", err)
	}
	s := &Service{
		cfg:     cfg,
		clients: clients,
		order:   allow,
		entries: make(map[schema.GroupVersionResource]*resourceEntry, len(allow)),
		guard: &detailGuard{
			rate: cfg.DetailRate, burst: cfg.DetailBurst,
			maxInflight: cfg.DetailConcurrent, tokens: float64(cfg.DetailBurst),
			last: cfg.Now(), now: cfg.Now,
		},
		dryRunGuard: &detailGuard{
			rate: cfg.DryRunRate, burst: cfg.DryRunBurst,
			maxInflight: cfg.DryRunConcurrent, tokens: float64(cfg.DryRunBurst),
			last: cfg.Now(), now: cfg.Now,
		},
		dryRunAllow: make(map[schema.GroupVersionResource]bool, len(dryRunAllow)),
	}
	for _, gvr := range dryRunAllow {
		s.dryRunAllow[gvr] = true
	}
	s.delta = newDeltaState()
	// 큐·회로 고정 구조를 계상할 원장을 연결합니다. 이것이 빠지면 큐 부속이
	// 원장 밖에서 살아 있게 됩니다.
	s.delta.budget = &s.budget
	s.budget.max.Store(cfg.MaxSearchIndexBytes)
	for _, gvr := range allow {
		e := &resourceEntry{gvr: gvr}
		e.setStatus(StateMissing, "discovery를 아직 조회하지 않았습니다")
		s.entries[gvr] = e
	}
	return s, nil
}

// Start는 discovery snapshot을 만들고 allowlist informer를 띄웁니다.
//
// ctx가 끝나면 informer와 갱신 루프가 모두 정지합니다. 최초 동기화를 기다리되
// 상한을 넘긴 항목은 실패가 아니라 syncing 상태로 남습니다 — 하나의 aggregated
// API 때문에 서버 기동이 막히면 안 됩니다.
func (s *Service) Start(ctx context.Context) error {
	s.mu.Lock()
	if s.started.Load() || s.closed.Load() {
		s.mu.Unlock()
		return fmt.Errorf("resourcecatalog: 이미 시작했거나 종료된 서비스입니다")
	}
	runCtx, cancel := context.WithCancel(ctx)
	s.cancel = cancel
	s.started.Store(true)
	s.mu.Unlock()

	s.refresh(runCtx)
	s.waitForSync(runCtx)
	s.reapTerminal()
	s.rebuildIndexes(true)

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.run(runCtx)
	}()
	// 증분 델타 루프는 **별도 고루틴**입니다. 목록 2s 경로와 서로 굶기지 않고,
	// ctx가 끝나면 둘 다 즉시 멈춥니다.
	if s.cfg.SearchEnabled && s.cfg.SearchIncremental {
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			s.runDeltaLoop(runCtx)
		}()
	}
	return nil
}

// Close는 informer와 갱신 루프를 멈추고 정리가 끝날 때까지 기다립니다. 여러 번 불러도 안전합니다.
func (s *Service) Close() {
	if s == nil || !s.closed.CompareAndSwap(false, true) {
		return
	}
	s.mu.Lock()
	cancel := s.cancel
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	for _, gvr := range s.order {
		s.stopEntry(s.entries[gvr])
	}
	s.wg.Wait()
}

// Available은 이 배포에서 Resource Explorer를 쓸 수 있는지입니다.
// central 모드는 서비스 자체를 만들지 않으므로 nil 리시버도 안전하게 false입니다.
func (s *Service) Available() bool {
	return s != nil && s.started.Load() && !s.closed.Load()
}

// ClusterID는 이 서비스가 담당하는 클러스터입니다.
func (s *Service) ClusterID() string {
	if s == nil {
		return ""
	}
	return s.cfg.ClusterID
}

func (s *Service) run(ctx context.Context) {
	refresh := time.NewTicker(s.cfg.RefreshInterval)
	defer refresh.Stop()
	index := time.NewTicker(s.cfg.IndexInterval)
	defer index.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-refresh.C:
			s.refresh(ctx)
		case <-index.C:
			s.reapTerminal()
			s.rebuildIndexes(false)
		}
	}
}

/* ── discovery ───────────────────────────────────────────────────────────── */

// refresh는 discovery를 조회해 불변 snapshot을 만들고 informer를 reconcile합니다.
// **요청 경로에서는 절대 호출되지 않습니다.**
func (s *Service) refresh(ctx context.Context) {
	groups, lists, err := s.clients.Discovery.ServerGroupsAndResources()
	if ctx.Err() != nil {
		return
	}
	failure := ""
	if err != nil {
		// 일부 group만 실패하는 것은 흔한 일입니다(죽은 aggregated API).
		// 받아온 만큼은 그대로 씁니다. 사유는 짧게만 남깁니다.
		if len(lists) == 0 {
			failure = "discovery 조회에 실패했습니다"
			s.cfg.Logger.Warn("resource discovery 실패", "clusterId", s.cfg.ClusterID)
		} else {
			failure = "일부 API group을 조회하지 못했습니다"
		}
	}
	snap := buildDiscoverySnapshot(s.cfg.Now(), s.order, groups, lists, failure)
	s.disc.Store(snap)
	s.reconcile(ctx, snap)
}

func buildDiscoverySnapshot(at time.Time, allow []schema.GroupVersionResource, groups []*metav1.APIGroup, lists []*metav1.APIResourceList, failure string) *discoverySnapshot {
	preferred := make(map[string]string, len(groups))
	for _, g := range groups {
		if g == nil {
			continue
		}
		preferred[g.Name] = g.PreferredVersion.Version
	}
	served := make(map[schema.GroupVersionResource]metav1.APIResource, 64)
	for _, list := range lists {
		if list == nil {
			continue
		}
		gv, err := schema.ParseGroupVersion(list.GroupVersion)
		if err != nil {
			continue
		}
		for _, r := range list.APIResources {
			if r.Name == "" || containsByte(r.Name, '/') {
				continue // subresource는 대상이 아닙니다.
			}
			served[gv.WithResource(r.Name)] = r
		}
	}
	snap := &discoverySnapshot{
		refreshedAt: at,
		entries:     make([]discoveryEntry, 0, len(allow)),
		byGVR:       make(map[schema.GroupVersionResource]int, len(allow)),
		failure:     failure,
	}
	for _, gvr := range allow {
		entry := discoveryEntry{gvr: gvr, preferred: preferred[gvr.Group]}
		if r, ok := served[gvr]; ok {
			entry.served = true
			entry.kind = r.Kind
			entry.namespaced = r.Namespaced
			entry.verbs = append([]string(nil), r.Verbs...)
			sort.Strings(entry.verbs)
		}
		snap.byGVR[gvr] = len(snap.entries)
		snap.entries = append(snap.entries, entry)
	}
	return snap
}

func containsByte(s string, b byte) bool {
	for i := 0; i < len(s); i++ {
		if s[i] == b {
			return true
		}
	}
	return false
}

func hasVerb(verbs []string, want string) bool {
	for _, v := range verbs {
		if v == want {
			return true
		}
	}
	return false
}

// reconcile은 discovery 사실에 맞춰 informer를 만들거나 멈춥니다.
func (s *Service) reconcile(ctx context.Context, snap *discoverySnapshot) {
	for _, gvr := range s.order {
		e := s.entries[gvr]
		entry, _ := snap.get(gvr)
		switch {
		case !entry.served:
			s.stopEntry(e)
			reason := "클러스터가 이 API를 제공하지 않습니다"
			if entry.preferred != "" && entry.preferred != gvr.Version {
				reason = "이 버전을 제공하지 않습니다(preferred: " + entry.preferred + ")"
			}
			e.setStatus(StateMissing, reason)
		case !hasVerb(entry.verbs, "list") || !hasVerb(entry.verbs, "watch"):
			// verb가 없으면 informer를 만들지 않습니다. full-object watch로 물러나지도 않습니다.
			s.stopEntry(e)
			e.setStatus(StateUnsupported, "list/watch verb를 제공하지 않습니다")
		default:
			s.startEntry(ctx, e)
		}
	}
}

func (s *Service) startEntry(ctx context.Context, e *resourceEntry) {
	if ctx.Err() != nil || e.running(s) {
		return
	}
	// informer 생성은 잠금 밖에서 합니다. 설치만 잠금 안에서 하고, 그때 인스턴스
	// 번호를 올려 이 인스턴스의 수명 신원을 확정합니다.
	//
	// **PartialObjectMetadata informer만 만듭니다.** 실패해도 full object로 물러나지 않습니다.
	informer := metadatainformer.NewFilteredMetadataInformer(
		s.clients.Metadata, e.gvr, metav1.NamespaceAll, s.cfg.Resync, cache.Indexers{}, nil,
	).Informer()
	if err := informer.SetWatchErrorHandler(func(_ *cache.Reflector, err error) {
		s.recordWatchError(e, err)
	}); err != nil {
		e.setStatus(StateUnsupported, "informer를 준비하지 못했습니다")
		return
	}
	// 콜백은 **키만** 넣습니다. 라벨을 복사하지 않고, 실제 상태는 적용 직전에
	// 같은 세대의 Store에서 다시 읽습니다. informer를 새로 붙이지 않습니다.
	//
	// 신원은 이 informer 인스턴스에 **고정**됩니다(binding). 매번 현재 세대를 다시
	// 읽으면 재시작 직후 도착한 옛 informer의 이벤트가 새 세대 신원을 빌려 갑니다.
	//
	// 최초 LIST는 HasSynced 이전에 배달되므로 그 구간의 이벤트는 담지 않습니다 —
	// 그것이 곧 부트스트랩 배리어이고, 10만 개 add 폭풍을 큐에 쌓지 않는 이유입니다.
	dentry, _ := s.disc.Load().get(e.gvr)
	binding := &handlerBinding{namespaced: dentry.namespaced}
	capture := func(obj any) {
		e.dirty.Store(true)
		if !informer.HasSynced() {
			return
		}
		s.enqueueObject(binding, obj)
	}
	if _, err := informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: capture,
		UpdateFunc: func(old, cur any) {
			e.dirty.Store(true)
			if !informer.HasSynced() {
				return
			}
			// 키가 바뀌는 경우(방어적)에는 **옛 키와 새 키를 모두** 넣습니다.
			oldNS, oldName := metaKeyOf(old, binding.namespaced)
			newNS, newName := metaKeyOf(cur, binding.namespaced)
			if oldName != "" && (oldNS != newNS || oldName != newName) {
				s.enqueueKey(binding, oldNS, oldName)
			}
			s.enqueueObject(binding, cur)
		},
		DeleteFunc: capture,
	}); err != nil {
		e.setStatus(StateUnsupported, "informer를 준비하지 못했습니다")
		return
	}
	stop := make(chan struct{})
	done := make(chan struct{})
	if !e.installWithBinding(s, informer, stop, done, binding) {
		return // 그 사이에 다른 경로가 먼저 설치했습니다. 만든 informer는 그냥 버립니다.
	}
	if e.currentStatus().state != StateReady {
		e.setStatus(StateSyncing, "")
	}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer close(done)
		informer.Run(stop)
	}()
	// ctx 취소가 곧 watch 종료입니다. 종료 신호가 informer까지 도달해야 합니다.
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		select {
		case <-ctx.Done():
			s.stopEntry(e)
		case <-done:
		}
	}()
}

// stopEntry는 informer를 멈추고 **게시된 인덱스와 그 회계를 함께** 지웁니다.
//
// 세대를 먼저 올리므로, 지금 돌고 있는 재구성이 나중에 게시를 시도해도 버려집니다.
// 예전에는 informer가 이미 없으면 곧바로 돌아가면서 스냅숏을 남겼고, 그 스냅숏은
// 아무도 지우지 않은 채 보유 바이트에 계속 잡혀 있었습니다.
func (s *Service) stopEntry(e *resourceEntry) {
	if e == nil {
		return
	}
	stop, done := e.discard(s)
	if stop == nil {
		return
	}
	select {
	case <-stop:
	default:
		close(stop)
	}
	if done != nil {
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			s.cfg.Logger.Warn("resource informer 종료가 지연되었습니다", "resource", FormatGVR(e.gvr))
		}
	}
}

// recordWatchError는 reflector 실패를 상태로 번역합니다.
// 406(metadata 미지원)은 조용한 fallback 대신 unsupported로 드러납니다.
//
// 여기서는 상태만 바꿉니다. informer 정리는 reflector 고루틴 밖(reapTerminal)에서
// 합니다 — 자기 자신의 종료를 기다리면 교착이 됩니다.
func (s *Service) recordWatchError(e *resourceEntry, err error) {
	switch {
	case err == nil:
		return
	case apierrors.IsNotAcceptable(err), apierrors.IsUnsupportedMediaType(err):
		e.setStatus(StateUnsupported, "이 API는 metadata 전용 조회를 지원하지 않습니다")
	case apierrors.IsForbidden(err), apierrors.IsUnauthorized(err):
		e.setStatus(StateForbidden, "서버에 이 리소스의 list/watch 권한이 없습니다")
	case apierrors.IsNotFound(err):
		e.setStatus(StateMissing, "클러스터가 이 API를 제공하지 않습니다")
	default:
		if e.currentStatus().state == StateReady {
			return // 일시적 watch 재시도는 준비된 캐시를 무너뜨리지 않습니다.
		}
		e.setStatus(StateSyncing, "클러스터 응답을 기다리는 중입니다")
	}
}

// reapTerminal은 되지 않을 LIST를 백오프로 계속 시도하는 informer를 멈춥니다.
// 되지 않는 watch를 계속 재시도하는 것 자체가 클러스터 부하입니다.
// 다음 discovery 갱신에서 다시 시도합니다.
func (s *Service) reapTerminal() {
	for _, gvr := range s.order {
		e := s.entries[gvr]
		switch e.currentStatus().state {
		case StateUnsupported, StateForbidden, StateMissing:
			if e.running(s) {
				s.stopEntry(e)
			}
		}
	}
}

func (s *Service) waitForSync(ctx context.Context) {
	deadline := time.NewTimer(s.cfg.SyncTimeout)
	defer deadline.Stop()
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		if s.markSynced() {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-deadline.C:
			s.markSynced()
			return
		case <-ticker.C:
		}
	}
}

// markSynced는 동기화가 끝난 informer를 ready로 올리고, 전부 끝났는지 알려줍니다.
func (s *Service) markSynced() bool {
	all := true
	for _, gvr := range s.order {
		e := s.entries[gvr]
		s.snapMu.RLock()
		informer := e.informer
		s.snapMu.RUnlock()
		if informer == nil {
			continue // missing/unsupported는 기다릴 대상이 아닙니다.
		}
		if informer.HasSynced() {
			if st := e.currentStatus(); st.state == StateSyncing {
				e.setStatus(StateReady, "")
			}
			continue
		}
		if st := e.currentStatus(); st.state == StateSyncing || st.state == StateReady {
			all = false
		}
	}
	return all
}

// rebuildIndexes는 변경이 있었던 GVR의 인덱스를 다시 만듭니다.
// **요청 경로가 아니라 여기서** 정렬과 색인 비용을 지불합니다.
//
// 목록 인덱스와 검색 인덱스를 여기서 **함께** 만듭니다. (P1-2)
// informer 캐시 열람도 정렬도 한 번이고, 완성된 한 벌만 원자적으로 게시하므로
// 요청이 반쪽 갱신 상태를 볼 수 없습니다.
func (s *Service) rebuildIndexes(force bool) {
	now := s.cfg.Now()
	for _, gvr := range s.order {
		e := s.entries[gvr]
		informer, token, published := e.beginBuild(s)
		if informer == nil {
			continue
		}
		dirty := e.dirty.Swap(false)
		if !dirty && !force && published {
			continue
		}
		if !informer.HasSynced() {
			// 아직 못 만들었으므로 변경 표시를 되돌려 다음 tick에서 다시 시도합니다.
			if dirty {
				e.dirty.Store(true)
			}
			continue
		}
		// coversThroughSeq는 Store를 읽기 **전에** 캡처합니다. 이 값 이하의 seq를
		// 가진 이벤트는 이미 Store에 반영되어 있었으므로, 회수 게이트가
		// coversThroughSeq >= markerSeq 로 성립합니다.
		startEventSeq := s.eventSeq.Load()
		// Store.List·정렬·색인은 전부 잠금 밖에서 돕니다.
		if s.delta != nil {
			s.delta.storeListCalls.Add(1)
		}
		index := buildIndexSnapshot(informer.GetStore().List(), now)
		extra := listPublishExtra{setCovers: true, covers: startEventSeq}
		var result searchBuildResult
		switch {
		case !s.cfg.SearchEnabled:
			result = searchBuildResult{state: SearchDisabled}
		case !s.cfg.SearchIncremental:
			// 롤백 경로: 오늘까지와 **똑같이** dirty tick마다 전체 재구성입니다.
			result = s.buildSearch(e, index)
		case !e.bootstrapped.Load():
			// 증분 모드의 부트스트랩은 informer 인스턴스마다 한 번뿐입니다.
			r := s.buildSearchIndexFor(e, index)
			extra.setIndex, extra.sindex = true, r.index
			result = searchBuildResult{state: r.state, reason: r.reason}
			if r.peak > 0 {
				s.observePeak(s.searchBytes.Load() + r.peak)
			}
		default:
			// 이미 부트스트랩된 증분 모드: **검색 절반을 건드리지 않습니다.**
			extra.keepSearch = true
		}
		switch e.publishList(s, token, index, result, extra) {
		case publishRejectedLifecycle:
			// 빌드 도중 멈춘 항목입니다. 변경 표시를 되돌리지 않습니다 —
			// 다시 시작하면 그때 force 경로로 처음부터 만듭니다.
			continue
		case publishedWithIndex:
			// **설치가 끝났을 때만** 부트스트랩 완료로 표시합니다. 예산으로 거절된
			// 게시를 성공으로 읽으면 색인이 없는데도 증분 경로로 넘어갑니다.
			e.bootstrapped.Store(true)
		case publishedBudgetRejected:
			// 최종 승인에서 거절되었습니다. **빌드는 성공했지만 설치되지 않았으므로**
			// 여기서 입력·상한 지문을 남겨야 다음 tick이 같은 입력으로 전체 인덱스를
			// 또 만들지 않습니다. bootstrapped는 서지 않으니 입력이 줄거나 상한이
			// 커지면 다시 시도합니다.
			if d, derr := s.describeBaseline(gvr); derr == nil {
				est := searchInputEstimate(index, normalizeToken(d.Kind), d.Namespaced)
				e.noteBudgetRejection(s, est, est)
			}
		}
		if st := e.currentStatus(); st.state == StateSyncing {
			e.setStatus(StateReady, "")
		}
	}
}

// buildSearch는 이 GVR의 검색 인덱스를 서비스 전체 예산 안에서 만듭니다. (P1-1)
//
// 예산은 두 겹이고 **둘 다 큰 할당 전에** 판정됩니다.
//
//   - 보유 예산: 모든 GVR이 동시에 붙잡는 합이 MaxSearchIndexBytes를 넘지 않도록,
//     이 GVR이 지금 붙잡고 있는 몫을 뺀 나머지만 씁니다. 한 리소스가 전체를
//     독점하지 못하도록 GVR 몫(searchPerResourceDivisor = 전체의 1/2)으로 다시 조입니다.
//   - 정점 예산: 재구성 순간에는 (모든 GVR의 기존 보유량) + (신규 스냅숏) +
//     (작업용 배열)이 함께 삽니다. 빌더는 작업용까지 계상한 값이 이 한도 안일
//     때만 배열을 잡습니다. 그래서 **실패하는 빌드가 먼저 메모리를 먹는 일이 없습니다.**
func (s *Service) buildSearch(e *resourceEntry, index *indexSnapshot) searchBuildResult {
	if !s.cfg.SearchEnabled {
		return searchBuildResult{state: SearchDisabled}
	}
	desc, err := s.Describe(e.gvr)
	if err != nil {
		return searchBuildResult{state: SearchSyncing}
	}
	retained := s.searchBytes.Load()
	// 이 항목의 기존 스냅숏은 교체될 때까지 살아 있으므로 정점 계산에서 빼지 않습니다.
	var mine int64
	e.read(s, func(es *entrySnapshot) { mine = es.searchBytesOf() })
	retainedBudget := s.cfg.MaxSearchIndexBytes - (retained - mine)
	if perResource := s.perResourceSearchBudget(); retainedBudget > perResource {
		retainedBudget = perResource
	}
	peakAllowance := s.MaxSearchPeakBytes() - retained
	if retainedBudget <= 0 || peakAllowance <= 0 {
		return searchBuildResult{state: SearchUnavailable, reason: reasonBudget}
	}
	result := buildSearchSnapshot(index, desc.Kind, desc.Namespaced, retainedBudget, peakAllowance)
	if result.peak > 0 {
		s.observePeak(retained + result.peak)
	}
	return result
}

// buildSearchIndexFor는 증분 인덱스를 서비스 전체 예산 안에서 부트스트랩합니다.
//
// 예산 실패는 조용한 낡음이 아니라 **명시적 SearchUnavailable**입니다. 그리고
// 재시도는 무한 반복이 아니라 회로입니다 — 입력 추정치가 **엄격히 줄었을 때만**
// 다시 시도합니다(프로세스 재시작이나 예산 증가도 같은 효과를 냅니다).
func (s *Service) buildSearchIndexFor(e *resourceEntry, index *indexSnapshot) searchIndexResult {
	desc, err := s.Describe(e.gvr)
	if err != nil {
		return searchIndexResult{state: SearchSyncing}
	}
	estimate := searchInputEstimate(index, normalizeToken(desc.Kind), desc.Namespaced)
	// 직전 실패 이후 **입력이 줄지도, 실제 가용 용량이 늘지도** 않았으면 다시
	// 만들지 않습니다. 최종 게시 거절도 이 지문을 남기므로, 같은 입력으로 전체
	// 인덱스를 매 tick 다시 만드는 회전이 생기지 않습니다.
	//
	// 비교 대상이 설정 상한이 아니라 availableRetained라는 점이 핵심입니다.
	// 경쟁 GVR이 세대를 놓거나 은퇴 세대가 빠지면 설정은 그대로여도 자리가
	// 생기고, 그때 이 항목은 **다시 자격을 얻습니다.**
	if prev := e.lastInputEstimate.Load(); prev > 0 && !s.bootstrapRetryEligible(e, estimate, prev) {
		return searchIndexResult{state: SearchUnavailable, reason: reasonBudget, needed: e.lastNeeded.Load()}
	}
	// 회로를 통과했습니다 — 여기서부터가 **실제 전체 빌드**입니다. 계측은 이
	// 지점에서만 셉니다. 회로에 막힌 tick까지 세면 "재빌드 없음"을 증명할 수 없습니다.
	if s.delta != nil {
		s.delta.fullBootstraps.Add(1)
	}
	retained := s.searchBytes.Load()
	var mine int64
	e.read(s, func(es *entrySnapshot) { mine = es.searchBytesOf() })
	retainedBudget := s.cfg.MaxSearchIndexBytes - (retained - mine)
	if per := s.perResourceSearchBudget(); retainedBudget > per {
		retainedBudget = per
	}
	peakAllowance := s.MaxSearchPeakBytes() - retained
	if retainedBudget <= 0 || peakAllowance <= 0 {
		e.noteBudgetRejection(s, estimate, estimate)
		return searchIndexResult{state: SearchUnavailable, reason: reasonBudget, needed: estimate}
	}
	// 부트스트랩이 잡을 임시 바이트를 **먼저 예약**합니다. 예약이 거절되면
	// 아무것도 할당하지 않고 명시적 unavailable로 끝납니다.
	pm := measurePersistentInput(index, normalizeToken(desc.Kind), desc.Namespaced)
	wantRetained, wantPeak := persistentSearchCost(pm)
	if !s.budget.reserveTransient(wantPeak - wantRetained) {
		e.noteBudgetRejection(s, estimate, wantPeak)
		return searchIndexResult{state: SearchUnavailable, reason: reasonBudget, needed: wantPeak}
	}

	r := buildSearchIndex(index, desc.Kind, desc.Namespaced, retainedBudget, peakAllowance)
	// **임시 예약을 먼저 되돌린 뒤** 지문을 남깁니다. defer로 미루면 지문이
	// "임시분만큼 좁아진" 가용 용량을 적게 되고, 다음 tick에는 그 임시분이 이미
	// 풀려 있어 언제나 "용량이 늘었다"로 읽힙니다 — 매 tick 전체 재빌드입니다.
	s.budget.releaseTransient(wantPeak - wantRetained)
	if r.state != SearchReady {
		e.noteBudgetRejection(s, estimate, r.needed)
		return r
	}
	e.clearBudgetRejection()
	return r
}

// bootstrapRetryEligible은 지금 전체 빌드를 **다시 시도할 자격이 있는지**입니다.
//
// 두 가지 중 하나여야 합니다.
//
//	① 입력이 **충분히** 줄었다        estimate <= prev - prev/bootstrapShrinkDivisor
//	② 지금 용량이 **필요했던 만큼**이다  availableRetained >= lastNeeded
//
// ①에 여유(1/8)를 두는 이유: 행 하나가 지워졌다고 100k 인덱스를 다시 만들면
// 삭제가 있을 때마다 전체 재구성이 됩니다. ②가 "avail > lastAvail"이 아닌 이유:
// 1바이트만 풀려도 열리면 그것이 바로 회전입니다. 기준은 **필요했던 양**입니다.
func (s *Service) bootstrapRetryEligible(e *resourceEntry, estimate, prev int64) bool {
	if estimate <= prev-prev/bootstrapShrinkDivisor {
		return true // 입력이 눈에 띄게 줄었습니다.
	}
	if need := e.lastNeeded.Load(); need > 0 && s.availableRetained(e) >= need {
		return true // 지난번에 필요했던 만큼을 지금 감당할 수 있습니다.
	}
	return false
}

// bootstrapShrinkDivisor는 "충분히 줄었다"의 기준입니다(1/8 = 12.5%).
const bootstrapShrinkDivisor = 8

// noteBudgetRejection은 예산 거절의 **입력·가용 용량 지문**을 남깁니다.
//
// 다음 tick은 입력이 줄거나 **실제 가용 용량이 늘었을 때만** 다시 만듭니다.
// 최종 게시에서 거절된 경우에도 이 지문을 남겨야 같은 입력으로 전체 인덱스를
// 반복해서 만드는 회전이 생기지 않습니다.
//
// 지문에 설정 상한(max)을 적으면, 다른 GVR이 놓아서 자리가 생긴 경우를 영영
// 놓칩니다 — 설정은 아무것도 바뀌지 않았기 때문입니다.
func (e *resourceEntry) noteBudgetRejection(s *Service, estimate, needed int64) {
	e.lastInputEstimate.Store(estimate)
	e.lastNeeded.Store(needed)
	e.lastAvail.Store(s.availableRetained(e))
}

// clearBudgetRejection은 회로를 닫습니다(부트스트랩이 실제로 성공했을 때).
func (e *resourceEntry) clearBudgetRejection() {
	e.lastInputEstimate.Store(0)
	e.lastNeeded.Store(0)
	e.lastAvail.Store(0)
}

// searchInputEstimate는 보유 바이트를 좌우하는 **단조 증가 요인 전부**를 담은 추정치입니다.
// 행 수·namespace 수·이름/UID/label 바이트·행별 토큰 수가 모두 들어갑니다.
func searchInputEstimate(index *indexSnapshot, kindTok string, namespaced bool) int64 {
	if index == nil {
		return 0
	}
	m := measureSearchInput(index, kindTok, namespaced)
	var idBytes int64
	for i := range index.rows {
		row := &index.rows[i]
		idBytes += int64(len(row.name)) + int64(len(rowUID(row)))
	}
	retained, _ := searchCost(m)
	return retained + idBytes + int64(m.namespaces)*64
}

// observePeak은 관측된 재구성 정점의 최대치를 기록합니다.
func (s *Service) observePeak(peak int64) {
	for {
		prev := s.searchPeak.Load()
		if peak <= prev || s.searchPeak.CompareAndSwap(prev, peak) {
			return
		}
	}
}

/* ── 조회 ────────────────────────────────────────────────────────────────
   여기부터는 요청 경로입니다. Kubernetes API를 호출하지 않습니다. */

// Catalog는 현재 카탈로그 snapshot입니다.
func (s *Service) Catalog() Snapshot {
	if !s.Available() {
		return Snapshot{}
	}
	disc := s.disc.Load()
	out := Snapshot{Descriptors: make([]Descriptor, 0, len(s.order))}
	if disc != nil {
		out.RefreshedAt = disc.refreshedAt
		out.Failure = disc.failure
	}
	for _, gvr := range s.order {
		entry, _ := disc.get(gvr)
		e := s.entries[gvr]
		status := e.currentStatus()
		d := Descriptor{
			Group:            gvr.Group,
			Version:          gvr.Version,
			Resource:         gvr.Resource,
			Kind:             entry.kind,
			Namespaced:       entry.namespaced,
			Verbs:            entry.verbs,
			PreferredVersion: entry.preferred,
			State:            status.state,
			Reason:           status.reason,
		}
		d.DryRun = s.dryRunCapable(gvr, d)
		// 목록 스냅숏만 원자적으로 집습니다. 검색 세대를 붙잡지 않고, 서비스 전역
		// 잠금도 잡지 않습니다.
		if idx := e.baselineIndex(); idx != nil {
			d.Count = len(idx.rows)
		}
		out.Descriptors = append(out.Descriptors, d)
	}
	return out
}

// Describe는 allowlist 한 항목의 discovery 사실과 상태입니다.
func (s *Service) Describe(gvr schema.GroupVersionResource) (Descriptor, error) {
	if !s.Available() {
		return Descriptor{}, ErrUnavailable
	}
	if _, ok := s.entries[gvr]; !ok {
		return Descriptor{}, ErrNotAllowlisted
	}
	// discovery·상태·목록 스냅숏 모두 원자적으로 읽습니다. 서비스 전역 잠금을
	// 잡지 않으므로 카탈로그 조회가 게시를 밀지 않습니다. (Round 7 §P1-3/P1-6)
	return s.describeBaseline(gvr)
}

// describeBaseline은 **목록 스냅숏만** 보고 카탈로그 한 줄을 만듭니다.
// 검색 세대를 붙잡지 않습니다.
func (s *Service) describeBaseline(gvr schema.GroupVersionResource) (Descriptor, error) {
	e, ok := s.entries[gvr]
	if !ok {
		return Descriptor{}, ErrNotAllowlisted
	}
	entry, _ := s.disc.Load().get(gvr)
	status := e.currentStatus()
	d := Descriptor{
		Group: gvr.Group, Version: gvr.Version, Resource: gvr.Resource,
		Kind: entry.kind, Namespaced: entry.namespaced, Verbs: entry.verbs,
		PreferredVersion: entry.preferred, State: status.state, Reason: status.reason,
	}
	d.DryRun = s.dryRunCapable(gvr, d)
	if idx := e.baselineIndex(); idx != nil {
		d.Count = len(idx.rows)
	}
	return d, nil
}

// describeSnapshot은 **이미 집어 둔 스냅숏**으로 카탈로그 한 줄을 만듭니다.
// 잠금을 잡지 않으므로 검색처럼 긴 경로가 여러 GVR을 한 번에 볼 때 씁니다.
func (s *Service) describeSnapshot(gvr schema.GroupVersionResource, es *entrySnapshot) (Descriptor, error) {
	var idx *indexSnapshot
	if es != nil {
		idx = es.index
	}
	return s.describeWithIndex(gvr, idx)
}

// describeWithIndex는 **이미 집어 둔 목록 스냅숏**으로 카탈로그 한 줄을 만듭니다.
// 검색 세대를 보지 않으므로 범위 제한 경로가 이것만으로 끝납니다.
func (s *Service) describeWithIndex(gvr schema.GroupVersionResource, idx *indexSnapshot) (Descriptor, error) {
	e, ok := s.entries[gvr]
	if !ok {
		return Descriptor{}, ErrNotAllowlisted
	}
	entry, _ := s.disc.Load().get(gvr)
	status := e.currentStatus()
	d := Descriptor{
		Group: gvr.Group, Version: gvr.Version, Resource: gvr.Resource,
		Kind: entry.kind, Namespaced: entry.namespaced, Verbs: entry.verbs,
		PreferredVersion: entry.preferred, State: status.state, Reason: status.reason,
	}
	d.DryRun = s.dryRunCapable(gvr, d)
	if idx != nil {
		d.Count = len(idx.rows)
	}
	return d, nil
}

// List는 로컬 인덱스에서 유계 페이지 하나를 만듭니다.
//
// Kubernetes API를 호출하지 않습니다. 상태가 ready가 아니면 빈 목록이 아니라
// 그 상태에 맞는 오류를 돌려줍니다.
func (s *Service) List(req ListRequest) (ListPage, Descriptor, error) {
	if !s.Available() {
		return ListPage{}, Descriptor{}, ErrUnavailable
	}
	gvr := schema.GroupVersionResource{Group: req.Group, Version: req.Version, Resource: req.Resource}
	desc, err := s.Describe(gvr)
	if err != nil {
		return ListPage{}, Descriptor{}, err
	}
	if err := stateError(desc.State); err != nil {
		return ListPage{}, desc, err
	}
	resolved, err := s.resolve(req, gvr, desc)
	if err != nil {
		return ListPage{}, desc, err
	}
	// 페이지 생성은 **목록 스냅숏 포인터 하나**로 끝냅니다.
	//
	// 스냅숏은 불변이므로 한 번 집으면 그 요청 동안 일관됩니다. 서비스 전역 읽기
	// 잠금을 쥐면 목록 요청 하나가 **다른 GVR의 게시까지** 밀고, 100ms 델타 게시가
	// 도는 지금은 그 영향이 큽니다. 검색 세대도 함께 붙잡지 않습니다.
	index := s.entries[gvr].baselineIndex()
	if index == nil {
		return ListPage{}, desc, ErrSyncing
	}
	return index.page(resolved), desc, nil
}

func stateError(state State) error {
	switch state {
	case StateReady:
		return nil
	case StateSyncing:
		return ErrSyncing
	case StateUnsupported:
		return ErrUnsupported
	case StateForbidden:
		return ErrForbidden
	default:
		return ErrNotServed
	}
}

// resolve는 요청 파라미터를 검증·정규화합니다. 상한을 넘는 값은 잘라내지 않고 거절합니다 —
// 조용히 자르면 사용자는 전체를 본 줄 압니다.
func (s *Service) resolve(req ListRequest, gvr schema.GroupVersionResource, desc Descriptor) (resolvedRequest, error) {
	// 프로덕션 응답 예산은 언제나 1MiB입니다. page()가 다시 한 번 조이므로
	// 이 값이 어떻게 바뀌어도 상한을 넘을 수 없습니다.
	out := resolvedRequest{limit: req.Limit, descending: req.Descending, maxBytes: MaxResponseBytes}
	if out.limit <= 0 {
		out.limit = DefaultPageSize
	}
	if out.limit > MaxPageSize {
		return resolvedRequest{}, ErrInvalidFilter
	}
	if len(req.NamePrefix) > MaxNameFilterLen {
		return resolvedRequest{}, ErrInvalidFilter
	}
	if !safeCursorSegment(req.NamePrefix) {
		return resolvedRequest{}, ErrInvalidFilter
	}
	out.namePrefix = req.NamePrefix
	if req.LabelSelector != "" {
		if len(req.LabelSelector) > MaxSelectorLen {
			return resolvedRequest{}, ErrInvalidFilter
		}
		selector, err := labels.Parse(req.LabelSelector)
		if err != nil {
			return resolvedRequest{}, ErrInvalidFilter
		}
		if reqs, _ := selector.Requirements(); len(reqs) > MaxSelectorRequirements {
			return resolvedRequest{}, ErrInvalidFilter
		}
		out.selector = selector
	}
	// 클러스터 범위 리소스는 namespace 구간이 없습니다. namespaced 리소스에서
	// Scope가 비어 있으면 **전체가 아니라 아무 구간도 없는 것**입니다.
	if !desc.Namespaced {
		out.spanAll = true
	} else {
		out.spanAll = req.Namespaces.All
		out.namespaces = sortedUnique(req.Namespaces.List)
	}
	out.fingerprint = fingerprint(FormatGVR(gvr), req, out.namespaces, out.spanAll)
	if req.Cursor != "" {
		key, err := decodeCursor(req.Cursor, out.fingerprint)
		if err != nil {
			return resolvedRequest{}, err
		}
		out.cursor, out.hasCursor = key, true
	}
	return out, nil
}

func sortedUnique(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	out := append([]string(nil), in...)
	sort.Strings(out)
	j := 0
	for i := 1; i < len(out); i++ {
		if out[i] != out[j] {
			j++
			out[j] = out[i]
		}
	}
	return out[:j+1]
}
