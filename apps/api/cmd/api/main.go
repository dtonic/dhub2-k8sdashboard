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
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/xenx96/k8s-dashboard/apps/api/internal/cache"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/clusterstate"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/config"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/contract"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/datasource"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/datasource/demo"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/datasource/greptime"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/datasource/quickwit"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/httpapi"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/querycatalog"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/scope"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	if err := run(logger); err != nil {
		logger.Error("서버를 시작하지 못했습니다", "err", err)
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	cfg := config.Load()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

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

	logger.Info("informer 시작",
		"resync", cfg.Resync.String(),
		"eventFieldSelector", cfg.EventFieldSelector,
		"protobuf", !cfg.DisableProtobuf)

	syncCtx, cancelSync := context.WithTimeout(ctx, 2*time.Minute)
	defer cancelSync()
	if err := store.Start(syncCtx); err != nil {
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

	metrics, logs, alerts, topo := sources(logger, cfg, store, queries)

	// 사용량은 메트릭 데이터소스에서 옵니다. 주기적으로 스냅숏만 갱신합니다 —
	// 요청마다 조회하면 화면 하나가 데이터소스에 수십 번 나갑니다.
	go refreshUsage(ctx, logger, store, metrics, cfg.ClusterID)

	srv := httpapi.NewServer(httpapi.Deps{
		Store:         store,
		Metrics:       metrics,
		Logs:          logs,
		Alerts:        alerts,
		Topology:      topo,
		Resolver:      scope.Static{S: cfg.Scope()},
		Cache:         cache.NewTTL(cfg.CacheTTL),
		Logger:        logger,
		AllowedOrigin: cfg.AllowedOrigin,
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
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return httpSrv.Shutdown(shutdownCtx)
	}
}

// sources는 데이터소스 어댑터를 고릅니다.
//
// 우선순위는 **실주소 > 데모 > Unavailable**입니다. GREPTIME_URL·QUICKWIT_URL을
// 적으면 USE_DEMO_DATA와 무관하게 실제 어댑터를 씁니다 — 주소를 적은 것이
// 의도이기 때문입니다. 알림·토폴로지는 아직 실클라이언트가 없습니다(#17 잔여) —
// 데모가 아니면 연결 실패로 취급해 화면이 degraded를 그리게 합니다.
func sources(logger *slog.Logger, cfg config.Config, store *clusterstate.Store, queries querycatalog.Catalog) (
	datasource.Metrics, datasource.Logs, datasource.Alerts, datasource.Topology,
) {
	var d *demo.Source
	if cfg.UseDemoData {
		d = demo.New(store)
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
		}, store, queries)
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
			BaseURL:     cfg.Quickwit.URL,
			Index:       cfg.Quickwit.Index,
			Username:    cfg.Quickwit.Username,
			Password:    cfg.Quickwit.Password,
			Timeout:     cfg.Quickwit.Timeout,
			MaxPageSize: cfg.Quickwit.MaxPageSize,
			MaxLines:    cfg.Quickwit.MaxLines,
			Fields:      quickwitFields(cfg.Quickwit.Fields),
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
		q, err := quickwit.New(qcfg, store)
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

	var alerts datasource.Alerts = datasource.Unavailable{Reason: "알림 데이터소스가 아직 연결되지 않았습니다"}
	var topo datasource.Topology = datasource.Unavailable{Reason: "토폴로지 데이터소스가 아직 연결되지 않았습니다"}
	if d != nil {
		alerts, topo = d, d
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
		"trace_id": &f.TraceID, "span_id": &f.SpanID,
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
