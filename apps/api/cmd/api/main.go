// Command api는 Observability API/BFF입니다.
//
// 기동 순서가 중요합니다.
//  1. informer를 만들고 watch를 시작합니다.
//  2. **최초 동기화가 끝날 때까지 기다립니다.** 이 전에 트래픽을 받으면
//     "Pod 0개"처럼 틀린 값이 정상처럼 보입니다.
//  3. 그다음 HTTP 리스너를 엽니다.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"sync"
	"syscall"
	"time"

	"github.com/xenx96/k8s-dashboard/apps/api/internal/auth"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/cache"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/clusterstate"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/config"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/contract"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/dashboard"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/datasource"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/datasource/alertmanager"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/datasource/demo"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/datasource/greptime"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/datasource/quickwit"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/datasource/upstream"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/httpapi"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/observability"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/querycatalog"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/queryprotect"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/scope"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/stream"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/topologylayout"
)

// 빌드 정보 — GET /version이 그대로 돌려줍니다. 릴리스 빌드는 ldflags로 덮어씁니다:
//
//	go build -ldflags "-X main.version=v1.2.3 -X main.commit=abc1234 -X main.buildDate=2026-08-14T00:00:00Z"
var (
	version   = "dev"
	commit    = "unknown"
	buildDate = "unknown"
)

func main() {
	if len(os.Args) == 2 && os.Args[1] == "--version" {
		fmt.Println(version)
		return
	}
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	if err := run(logger); err != nil {
		logger.Error("서버를 시작하지 못했습니다", "err", err)
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	cfg := config.Load()
	// 필수 설정 오류는 Kubernetes 클라이언트·informer를 만들기 전에 멈춥니다. (#5)
	if err := cfg.Validate(); err != nil {
		return fmt.Errorf("설정 오류: %w", err)
	}
	var configuredAlerts *alertmanager.Source
	if cfg.Alertmanager.Enabled {
		var err error
		configuredAlerts, err = alertmanager.NewUnbound(cfg.Alertmanager)
		if err != nil {
			return fmt.Errorf("Alertmanager 초기화 실패: %w", err)
		}
		defer configuredAlerts.Close()
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if cfg.ClusterState.Mode == "central" {
		return runCentral(ctx, logger, cfg, configuredAlerts)
	}

	restCfg, err := clusterstate.RestConfig(clusterstate.ClientOptions{
		Kubeconfig:      cfg.Kubeconfig,
		QPS:             cfg.QPS,
		Burst:           cfg.Burst,
		UserAgent:       "k8s-dashboard-api",
		DisableProtobuf: cfg.DisableProtobuf,
	})
	if err != nil {
		return err
	}
	clients, err := clusterstate.NewClients(restCfg)
	if err != nil {
		return err
	}
	store, err := clusterstate.New(clients, clusterstate.Options{
		Resync:             cfg.Resync,
		EventFieldSelector: cfg.EventFieldSelector,
		ClusterID:          cfg.ClusterID,
		ClusterName:        cfg.ClusterName,
	})
	if err != nil {
		return err
	}

	// 상태 변경 SSE 허브 (#12). informer가 watch를 시작하기 전에 관찰자를 등록해
	// 어떤 변경도 놓치지 않습니다. 최초 LIST는 observe.go가 억제합니다.
	streamMetrics := stream.NewMetrics()
	hub, err := stream.New(stream.Config{
		RingSize:         cfg.StreamReplayEvents,
		SubscriberBuffer: cfg.StreamSubBuffer,
		MaxConnections:   cfg.StreamMaxConnections,
		MaxPerSubject:    cfg.StreamMaxPerSubject,
	}, streamMetrics)
	if err != nil {
		return err
	}
	defer hub.Close()
	if err := store.OnChange(func(c clusterstate.Change) {
		hub.Publish(stream.EnvelopeFromChange(cfg.ClusterID, c))
	}); err != nil {
		return err
	}

	logger.Info("informer 시작",
		"resync", cfg.Resync.String(),
		"eventFieldSelector", cfg.EventFieldSelector,
		"protobuf", !cfg.DisableProtobuf)

	syncCtx, cancelSync := context.WithTimeout(ctx, 2*time.Minute)
	defer cancelSync()
	if err := store.StartAndWait(ctx, syncCtx); err != nil {
		return err
	}
	logger.Info("informer 캐시 동기화 완료")

	// 쿼리 카탈로그 — 오류는 여기서(시작 단계) 멈춥니다. 잘못된 카탈로그로
	// 뜬 서버는 빈 화면을 정상처럼 보여주게 됩니다. (#9)
	queries, err := querycatalog.LoadPath(cfg.QueryCatalogDir)
	if err != nil {
		return err
	}
	logger.Info("쿼리 카탈로그 로드", "refs", len(queries.Refs()), "panels", len(queries.Panels()))

	dashboardStore, err := openDashboardStore(ctx, cfg.DashboardBuilder)
	if err != nil {
		return err
	}
	if dashboardStore != nil {
		defer dashboardStore.Close()
		logger.Info("dashboard builder metadata store ready")
	}
	var dashboardAPI dashboard.Store
	if dashboardStore != nil {
		dashboardAPI = dashboardStore
	}

	resolver, err := buildResolver(ctx, logger, cfg)
	if err != nil {
		return err
	}
	if closer, ok := resolver.(interface{ Close() error }); ok {
		defer closer.Close()
	}

	platformMetrics := observability.New()
	platformMetrics.ConfigureLogging(logger, cfg.QuerySlowThreshold)
	if configuredAlerts != nil {
		if err := configuredAlerts.BindCatalog(store); err != nil {
			return err
		}
		configuredAlerts.SetObserver(platformMetrics)
	}
	// nil *alertmanager.Source를 인터페이스에 그대로 담으면 non-nil 인터페이스가 되어
	// demo/Unavailable 분기 대신 nil 리시버 호출로 panic합니다. 비활성이면 untyped nil을 넘깁니다.
	var alertSource datasource.Alerts
	if configuredAlerts != nil {
		alertSource = configuredAlerts
	}
	metrics, logs, alerts, topo := sourcesObserved(logger, cfg, store, queries, platformMetrics, alertSource)

	// 사용량은 메트릭 데이터소스에서 옵니다. 주기적으로 스냅숏만 갱신합니다 —
	// 요청마다 조회하면 화면 하나가 데이터소스에 수십 번 나갑니다.
	go refreshUsage(ctx, logger, store, metrics, cfg.ClusterID)

	// 알림 변경은 read-only Alertmanager 스냅숏 diff로 만듭니다. (#12)
	// 소스가 Unavailable이어도 백오프로 물러날 뿐 스핀하지 않습니다.
	if cfg.Alertmanager.Enabled || cfg.UseDemoData {
		alertPoller := stream.NewAlertPoller(stream.AlertPollerConfig{
			ClusterID:         cfg.ClusterID,
			Interval:          cfg.AlertPollInterval,
			MaxBackoff:        cfg.AlertPollMaxBackoff,
			MaxAlerts:         cfg.AlertSnapshotMax,
			TrustedNamespaces: store.StreamEntityNamespaces,
		}, alerts, hub, logger)
		stopAlertPoller := startDirectAlertPoller(ctx, alertPoller, logger)
		defer stopAlertPoller()
	}

	protectionMetrics := queryprotect.NewMetrics()
	responseCache := cache.New(cache.Config{DefaultTTL: cfg.CacheTTL, MaxEntries: cfg.CacheMaxEntries, MaxValueBytes: cfg.CacheMaxValueBytes, MaxLocalBytes: cfg.CacheMaxLocalBytes, RedisAddr: cfg.RedisAddr, RedisOpTimeout: cfg.RedisOpTimeout, RedisCooldown: cfg.RedisCooldown, LockTTL: cfg.QueryTimeout + time.Second, LockWait: cfg.QueryTimeout, Observer: protectionMetrics})
	defer responseCache.Close()
	guardCfg := queryprotect.DefaultConfig()
	guardCfg.UserRate = cfg.QueryUserRate
	guardCfg.DashboardRate = cfg.QueryDashboardRate
	guardCfg.UserBurst = cfg.QueryUserBurst
	guardCfg.DashboardBurst = cfg.QueryDashboardBurst
	guardCfg.UserConcurrent = cfg.QueryUserConcurrent
	guardCfg.DashboardConcurrent = cfg.QueryDashboardConcurrent
	guardCfg.QueryTimeout = cfg.QueryTimeout
	guardCfg.SlowThreshold = cfg.QuerySlowThreshold
	topoLayout := topologylayout.New(topologylayout.Config{RedisAddr: cfg.RedisAddr, OpTimeout: cfg.RedisOpTimeout, Logger: logger})
	defer func() { _ = topoLayout.Close() }()
	srv := httpapi.NewServer(httpapi.Deps{
		Store:              store,
		Metrics:            metrics,
		Logs:               logs,
		Alerts:             alerts,
		Topology:           topo,
		Resolver:           resolver,
		Cache:              responseCache,
		Guard:              queryprotect.New(guardCfg, protectionMetrics),
		ProtectionMetrics:  protectionMetrics,
		Observability:      platformMetrics,
		PlannedQueryRefs:   panelQueryRefs(queries),
		CacheTTL:           cache.TTLPolicy{State: cfg.CacheTTL, Short: cfg.CacheShortTTL, Historical: cfg.CacheHistoricalTTL, HistoricalSafety: cfg.CacheHistoricalSafety},
		Logger:             logger,
		Stream:             hub,
		StreamMetrics:      streamMetrics,
		StreamOptions:      httpapi.StreamOptions{Heartbeat: cfg.StreamHeartbeat, WriteIdleTimeout: cfg.StreamWriteIdle},
		DashboardStore:     dashboardAPI,
		DashboardQueryRefs: queries.Refs(),
		TopologyLayout:     topoLayout,
		// 관리(ADR 0014)는 direct 모드에서만 — clientset을 직접 씁니다. central은 nil.
		KubeClient: clients.Typed,
		AllowedOrigin:      cfg.AllowedOrigin,
		Version:            contract.VersionInfo{Version: version, Commit: commit, BuildDate: buildDate},
	})

	httpSrv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           srv,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       cfg.ReadTimeout,
		WriteTimeout:      cfg.WriteTimeout,
	}

	errCh := make(chan error, 1)
	go func() {
		logger.Info("HTTP 리스너 시작", "addr", cfg.Addr)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		logger.Info("종료 신호 수신 · 진행 중인 요청을 정리합니다")
		// SSE 핸들러는 구독 채널이 닫혀야 빠져나옵니다. Shutdown보다 먼저
		// 허브를 닫지 않으면 스트림 연결 수만큼 Shutdown이 타임아웃까지 기다립니다.
		hub.Close()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return httpSrv.Shutdown(shutdownCtx)
	}
}

func startDirectAlertPoller(parent context.Context, poller *stream.AlertPoller, logger *slog.Logger) func() {
	ctx, cancel := context.WithCancel(parent)
	done := make(chan struct{})
	go func() {
		defer close(done)
		poller.Run(ctx)
	}()
	return func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			logger.Error("Alertmanager direct poller shutdown timed out")
		}
	}
}

func openDashboardStore(ctx context.Context, cfg config.DashboardBuilderConfig) (*dashboard.Postgres, error) {
	if !cfg.Enabled {
		return nil, nil
	}
	store, err := dashboard.Open(ctx, cfg.DatabaseURL, []byte(cfg.CursorKey), int32(cfg.MaxConns), cfg.ConnectTimeout, cfg.RequireTLS)
	if err != nil {
		return nil, fmt.Errorf("dashboard metadata store: %w", err)
	}
	return store, nil
}

func panelQueryRefs(c querycatalog.Catalog) []string {
	set := map[string]struct{}{}
	for _, panel := range c.Panels() {
		for _, series := range panel.Series {
			set[series.QueryRef] = struct{}{}
		}
	}
	refs := make([]string, 0, len(set))
	for ref := range set {
		refs = append(refs, ref)
	}
	sort.Strings(refs)
	return refs
}

// buildResolver는 AUTH_MODE에 따라 Scope 해석기를 만듭니다. (#10)
//
// 어느 모드든 핸들러는 scope.Resolver 뒤만 봅니다 — 인증 방식이 바뀌어도
// 화면 코드와 Scope 강제 규칙은 같습니다.
func buildResolver(ctx context.Context, logger *slog.Logger, cfg config.Config) (scope.Resolver, error) {
	switch cfg.Auth.Mode {
	case "", "none":
		logger.Info("인증: 없음 (SCOPE_NAMESPACES 정적 Scope) — 개발·데모 전용")
		return scope.Static{S: cfg.Scope()}, nil

	case "oidc":
		clusters := make([]scope.Cluster, 0, len(cfg.ClusterState.Clusters))
		if cfg.ClusterState.Mode == "central" {
			for _, id := range cfg.ClusterState.Clusters {
				clusters = append(clusters, scope.Cluster{ID: id, Name: id})
			}
		}
		r, err := auth.NewResolver(ctx, auth.Config{
			IssuerURL:      cfg.Auth.Issuer,
			Audience:       cfg.Auth.Audience,
			RolesClaim:     cfg.Auth.RolesClaim,
			Leeway:         cfg.Auth.Leeway,
			JWKSMinRefresh: cfg.Auth.JWKSMinRefresh,
			ClusterID:      cfg.ClusterID,
			ClusterName:    cfg.ClusterName,
			Clusters:       clusters,
			Central:        cfg.ClusterState.Mode == "central",
			Session: auth.SessionConfig{
				Enabled: cfg.Auth.SessionEnabled, PublicOrigin: cfg.Auth.PublicOrigin,
				RedirectURI: cfg.Auth.RedirectURI, ClientID: cfg.Auth.ClientID,
				ClientSecret: cfg.Auth.ClientSecret, EncryptionKey: cfg.Auth.SessionKey,
				RedisAddr: cfg.RedisAddr, RedisTimeout: cfg.RedisOpTimeout,
				TransactionTTL: cfg.Auth.LoginTransactionTTL, IdleTTL: cfg.Auth.SessionIdleTTL,
				AbsoluteTTL: cfg.Auth.SessionAbsoluteTTL, RefreshSkew: cfg.Auth.RefreshSkew, MaxSessions: cfg.Auth.SessionMaxSessions,
			},
		}, logger)
		if err != nil {
			// 인증이 깨진 채로 뜨면 전부 401이 되어 장애처럼 보입니다. 여기서 멈춥니다.
			return nil, err
		}
		logger.Info("인증: OIDC", "issuer", cfg.Auth.Issuer, "rolesClaim", cfg.Auth.RolesClaim)
		return r, nil

	case "mock":
		audience := cfg.Auth.Audience
		if audience == "" {
			audience = auth.DefaultMockAudience
		}
		idp, err := auth.StartMockIDP(cfg.Auth.MockAddr, audience, nil)
		if err != nil {
			return nil, err
		}
		owned := false
		defer func() {
			if !owned {
				_ = idp.Close()
			}
		}()
		r, err := auth.NewResolver(ctx, auth.Config{
			IssuerURL:   idp.Issuer,
			Audience:    audience,
			ClusterID:   cfg.ClusterID,
			ClusterName: cfg.ClusterName,
		}, logger)
		if err != nil {
			return nil, err
		}
		managed := &managedMockResolver{Resolver: r, idp: idp}
		owned = true
		// 개발 편의 — 토큰 발급 방법만 알려주고 토큰 자체는 로그에 남기지 않습니다.
		logger.Warn("인증: mock IdP — 운영 금지. 누구나 토큰을 만들 수 있습니다",
			"issuer", idp.Issuer,
			"tokenHint", "curl -X POST '"+idp.Issuer+"/token?sub=dev&roles=platform.admin'")
		return managed, nil
	}
	return nil, fmt.Errorf("알 수 없는 AUTH_MODE %q (none|oidc|mock)", cfg.Auth.Mode)
}

type managedMockResolver struct {
	*auth.Resolver
	idp       *auth.MockIDP
	closeOnce sync.Once
	closeErr  error
}

func (r *managedMockResolver) Close() error {
	if r == nil {
		return nil
	}
	r.closeOnce.Do(func() {
		if r.Resolver != nil {
			r.closeErr = r.Resolver.Close()
		}
		if r.idp != nil {
			if err := r.idp.Close(); r.closeErr == nil {
				r.closeErr = err
			}
		}
	})
	return r.closeErr
}

// sources는 데이터소스 어댑터를 고릅니다.
//
// 우선순위는 **실주소 > 데모 > Unavailable**입니다. GREPTIME_URL·QUICKWIT_URL을
// 적으면 USE_DEMO_DATA와 무관하게 실제 어댑터를 씁니다 — 주소를 적은 것이
// 의도이기 때문입니다. 운영 Alertmanager는 시작 단계에서 검증한 source를
// sourcesObserved에 주입합니다. 토폴로지는 데모가 아니면 Unavailable입니다.
func sources(logger *slog.Logger, cfg config.Config, store *clusterstate.Store, queries querycatalog.Catalog) (
	datasource.Metrics, datasource.Logs, datasource.Alerts, datasource.Topology,
) {
	return sourcesObserved(logger, cfg, store, queries, nil, nil)
}

func sourcesObserved(logger *slog.Logger, cfg config.Config, catalog datasource.PodCatalog, queries querycatalog.Catalog, observer upstream.Observer, configuredAlerts datasource.Alerts) (
	datasource.Metrics, datasource.Logs, datasource.Alerts, datasource.Topology,
) {
	var d *demo.Source
	if cfg.UseDemoData {
		store, ok := catalog.(*clusterstate.Store)
		if ok {
			d = demo.New(store)
		}
	}

	var metrics datasource.Metrics
	switch {
	case cfg.Greptime.URL != "":
		g, err := greptime.New(greptime.Config{
			BaseURL:       cfg.Greptime.URL,
			DB:            cfg.Greptime.DB,
			Username:      cfg.Greptime.Username,
			Password:      cfg.Greptime.Password,
			Timeout:       cfg.Greptime.Timeout,
			MaxDataPoints: cfg.Greptime.MaxDataPoints,
			ClusterScoped: cfg.ClusterState.Mode == "central",
			Observer:      observer,
		}, catalog, queries)
		if err != nil {
			logger.Error("GreptimeDB 설정이 잘못되었습니다 · 메트릭 섹션은 degraded로 내려갑니다", "err", err)
			metrics = datasource.Unavailable{Reason: "GreptimeDB 설정 오류"}
			break
		}
		logger.Info("메트릭 데이터소스: GreptimeDB", "db", cfg.Greptime.DB)
		metrics = g
	case d != nil:
		metrics = d
	default:
		metrics = datasource.Unavailable{Reason: "메트릭 데이터소스가 설정되지 않았습니다"}
	}

	var logs datasource.Logs
	switch {
	case cfg.Quickwit.URL != "":
		qcfg := quickwit.Config{
			BaseURL:       cfg.Quickwit.URL,
			Index:         cfg.Quickwit.Index,
			Username:      cfg.Quickwit.Username,
			Password:      cfg.Quickwit.Password,
			Timeout:       cfg.Quickwit.Timeout,
			MaxPageSize:   cfg.Quickwit.MaxPageSize,
			MaxLines:      cfg.Quickwit.MaxLines,
			Fields:        quickwitFields(cfg.Quickwit.Fields),
			ClusterScoped: cfg.ClusterState.Mode == "central",
			Observer:      observer,
		}
		// 로그 조회 한계는 Git의 쿼리 카탈로그가 선언한 값이 환경변수보다 우선입니다.
		// 한계의 진실은 배포 설정이 아니라 카탈로그 한 곳이어야 합니다. (#9)
		if l := queries.Logs().Search; l != (querycatalog.Limits{}) {
			if l.Timeout > 0 {
				qcfg.Timeout = l.Timeout
			}
			if l.MaxPageSize > 0 {
				qcfg.MaxPageSize = l.MaxPageSize
			}
			if l.MaxLines > 0 {
				qcfg.MaxLines = l.MaxLines
			}
		}
		q, err := quickwit.New(qcfg, catalog)
		if err != nil {
			logger.Error("Quickwit 설정이 잘못되었습니다 · 로그 섹션은 degraded로 내려갑니다", "err", err)
			logs = datasource.Unavailable{Reason: "Quickwit 설정 오류"}
			break
		}
		logger.Info("로그 데이터소스: Quickwit", "index", cfg.Quickwit.Index)
		logs = q
	case d != nil:
		logs = d
	default:
		logs = datasource.Unavailable{Reason: "로그 데이터소스가 설정되지 않았습니다"}
	}

	var alerts datasource.Alerts = datasource.Unavailable{Reason: "알림 데이터소스가 설정되지 않았습니다"}
	var topo datasource.Topology = datasource.Unavailable{Reason: "토폴로지 데이터소스가 아직 연결되지 않았습니다"}
	if configuredAlerts != nil {
		alerts = configuredAlerts
		logger.Info("알림 데이터소스: Alertmanager")
	} else if d != nil {
		alerts, topo = d, d
	}
	if d != nil {
		topo = d
	}
	return metrics, logs, alerts, topo
}

// quickwitFields는 "message=body.message" 재정의를 FieldMap에 적용합니다.
// 모르는 키는 조용히 무시하지 않고 기동 로그에 남길 수도 있지만,
// 오타가 곧 빈 화면이므로 여기서는 알려진 키만 받습니다.
func quickwitFields(m map[string]string) quickwit.FieldMap {
	var f quickwit.FieldMap
	set := map[string]*string{
		"timestamp": &f.Timestamp, "level": &f.Level, "message": &f.Message,
		"namespace": &f.Namespace, "pod_name": &f.PodName, "pod_uid": &f.PodUID,
		"container": &f.Container, "workload_kind": &f.WorkloadKind,
		"workload_name": &f.WorkloadName, "node": &f.Node,
		"trace_id": &f.TraceID, "span_id": &f.SpanID, "event_id": &f.EventID,
		"cluster": &f.Cluster,
	}
	for k, v := range m {
		if p, ok := set[k]; ok {
			*p = v
		}
	}
	return f
}

const usageRefreshInterval = 30 * time.Second

func refreshUsage(ctx context.Context, logger *slog.Logger, store *clusterstate.Store, m datasource.Metrics, clusterID string) {
	apply := func() {
		usage, err := m.Usage(ctx, clusterID)
		if err != nil {
			logger.Warn("사용량 갱신 실패 · request/limit만 표시됩니다", "err", err)
			return
		}
		store.SetUsage(func(uid string) (contract.ContainerUsage, bool) {
			v, ok := usage[uid]
			return v, ok
		})
	}
	apply()

	t := time.NewTicker(usageRefreshInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			apply()
		}
	}
}
