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
	"github.com/xenx96/k8s-dashboard/apps/api/internal/httpapi"
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

	metrics, logs, alerts, topo := sources(cfg, store)

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
// 아직 GreptimeDB/Quickwit/Alertmanager 클라이언트가 없으므로,
// demo가 아니면 **연결 실패로 취급**해 화면이 degraded를 그리게 합니다.
func sources(cfg config.Config, store *clusterstate.Store) (
	datasource.Metrics, datasource.Logs, datasource.Alerts, datasource.Topology,
) {
	if cfg.UseDemoData {
		d := demo.New(store)
		return d, d, d, d
	}
	u := datasource.Unavailable{Reason: "데이터소스가 아직 연결되지 않았습니다"}
	return u, u, u, u
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
