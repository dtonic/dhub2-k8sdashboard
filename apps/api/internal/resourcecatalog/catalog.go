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

type resourceEntry struct {
	gvr schema.GroupVersionResource

	mu       sync.Mutex
	informer cache.SharedIndexInformer
	stop     chan struct{}
	done     chan struct{}

	status atomic.Pointer[entryStatus]
	snap   atomic.Pointer[indexSnapshot]
	dirty  atomic.Bool
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

func (e *resourceEntry) running() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.informer != nil
}

// Service는 이 기능의 유일한 상태 소유자입니다.
type Service struct {
	cfg     Config
	clients Clients

	order   []schema.GroupVersionResource
	entries map[schema.GroupVersionResource]*resourceEntry

	disc atomic.Pointer[discoverySnapshot]

	guard *detailGuard

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
	}
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
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.informer != nil || ctx.Err() != nil {
		return
	}
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
	if _, err := informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    func(any) { e.dirty.Store(true) },
		UpdateFunc: func(any, any) { e.dirty.Store(true) },
		DeleteFunc: func(any) { e.dirty.Store(true) },
	}); err != nil {
		e.setStatus(StateUnsupported, "informer를 준비하지 못했습니다")
		return
	}
	stop := make(chan struct{})
	done := make(chan struct{})
	e.informer, e.stop, e.done = informer, stop, done
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

func (s *Service) stopEntry(e *resourceEntry) {
	if e == nil {
		return
	}
	e.mu.Lock()
	stop, done := e.stop, e.done
	e.informer, e.stop, e.done = nil, nil, nil
	e.mu.Unlock()
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
	e.snap.Store(nil)
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
			if e.running() {
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
		e.mu.Lock()
		informer := e.informer
		e.mu.Unlock()
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

// rebuildIndexes는 변경이 있었던 GVR의 정렬 인덱스를 다시 만듭니다.
// **요청 경로가 아니라 여기서** 정렬 비용을 지불합니다.
func (s *Service) rebuildIndexes(force bool) {
	now := s.cfg.Now()
	for _, gvr := range s.order {
		e := s.entries[gvr]
		e.mu.Lock()
		informer := e.informer
		e.mu.Unlock()
		if informer == nil {
			continue
		}
		dirty := e.dirty.Swap(false)
		if !dirty && !force && e.snap.Load() != nil {
			continue
		}
		if !informer.HasSynced() {
			// 아직 못 만들었으므로 변경 표시를 되돌려 다음 tick에서 다시 시도합니다.
			if dirty {
				e.dirty.Store(true)
			}
			continue
		}
		e.snap.Store(buildIndexSnapshot(informer.GetStore().List(), now))
		if st := e.currentStatus(); st.state == StateSyncing {
			e.setStatus(StateReady, "")
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
		if snap := e.snap.Load(); snap != nil {
			d.Count = len(snap.rows)
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
	if snap := e.snap.Load(); snap != nil {
		d.Count = len(snap.rows)
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
	e := s.entries[gvr]
	snap := e.snap.Load()
	if snap == nil {
		return ListPage{}, desc, ErrSyncing
	}
	resolved, err := s.resolve(req, gvr, desc)
	if err != nil {
		return ListPage{}, desc, err
	}
	return snap.page(resolved), desc, nil
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
