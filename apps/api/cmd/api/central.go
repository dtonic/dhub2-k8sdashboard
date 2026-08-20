package main

import (
	"context"
	cryptorand "crypto/rand"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"sync"
	"time"

	"github.com/xenx96/k8s-dashboard/apps/api/internal/cache"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/clusterstate"
	v1 "github.com/xenx96/k8s-dashboard/apps/api/internal/clusterstate/protocol/v1"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/clusterstate/transport"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/config"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/contract"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/datasource"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/datasource/alertmanager"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/httpapi"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/observability"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/querycatalog"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/queryprotect"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/stream"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	healthv1 "google.golang.org/grpc/health/grpc_health_v1"
)

// centralRuntime owns every background connection used by one API replica.
// It never creates a Kubernetes client; all state comes from the registry.
type centralRuntime struct {
	registry *clusterstate.GRPCRegistry
	catalog  *clusterstate.RemoteCatalog
	usage    *clusterstate.UsageStore
	ctx      context.Context
	cancel   context.CancelFunc
	conn     *grpc.ClientConn
	errCh    chan error
	done     chan struct{}
	wg       sync.WaitGroup
	once     sync.Once
}

func randomAlertInitialDelay(max time.Duration) time.Duration {
	if max <= 0 {
		return 0
	}
	n, err := cryptorand.Int(cryptorand.Reader, big.NewInt(int64(max)+1))
	if err != nil {
		return max / 2
	}
	return time.Duration(n.Int64())
}

func runCentral(ctx context.Context, logger *slog.Logger, cfg config.Config, configuredSources ...*alertmanager.Source) error {
	var configuredAlerts *alertmanager.Source
	if len(configuredSources) > 0 {
		configuredAlerts = configuredSources[0]
	}
	if cfg.Alertmanager.Enabled && configuredAlerts == nil {
		return errors.New("Alertmanager is enabled but not initialized")
	}
	queries, err := querycatalog.LoadPath(cfg.QueryCatalogDir)
	if err != nil {
		return err
	}
	dashboardAPI, err := openDashboardStore(ctx, cfg.DashboardBuilder)
	if err != nil {
		return err
	}
	if closer, ok := dashboardAPI.(interface{ Close() error }); ok {
		defer closer.Close()
	}
	resolver, err := buildResolver(ctx, logger, cfg)
	if err != nil {
		return err
	}
	if closer, ok := resolver.(interface{ Close() error }); ok {
		defer closer.Close()
	}
	streamMetrics := stream.NewMetrics()
	hub, err := stream.New(stream.Config{
		RingSize:          cfg.StreamReplayEvents,
		SubscriberBuffer:  cfg.StreamSubBuffer,
		MaxConnections:    cfg.StreamMaxConnections,
		MaxPerSubject:     cfg.StreamMaxPerSubject,
		ClusterIDs:        cfg.ClusterState.Clusters,
		MaxClusters:       cfg.ClusterState.MaxClusters,
		MaxRetainedEvents: stream.MaxRingSize,
	}, streamMetrics)
	if err != nil {
		return err
	}
	defer hub.Close()
	runtime, err := newCentralRuntime(ctx, cfg.ClusterState, hub)
	if err != nil {
		return err
	}
	defer runtime.Close()

	platformMetrics := observability.New()
	platformMetrics.ConfigureLogging(logger, cfg.QuerySlowThreshold)
	if configuredAlerts != nil {
		if err := configuredAlerts.BindCatalog(runtime.catalog); err != nil {
			return err
		}
		configuredAlerts.SetObserver(platformMetrics)
	}
	metrics, logs, alerts, topo := sourcesObserved(logger, cfg, runtime.catalog, queries, platformMetrics, configuredAlerts)
	if cfg.Alertmanager.Enabled {
		stopAlertPollers := startCentralAlertPollers(runtime.ctx, cfg, runtime.catalog, alerts, hub, logger)
		defer stopAlertPollers()
	}
	poller := &clusterstate.UsagePoller{Metrics: metrics, Catalog: runtime.catalog, Store: runtime.usage}
	usageErr := make(chan error, 1)
	usageDone := make(chan struct{})
	go func() {
		defer close(usageDone)
		if err := poller.Run(runtime.ctx, cfg.ClusterState.Clusters); err != nil && runtime.ctx.Err() == nil {
			usageErr <- err
		}
	}()
	defer func() {
		_ = runtime.Close()
		select {
		case <-usageDone:
		case <-time.After(5 * time.Second):
		}
	}()

	protectionMetrics := queryprotect.NewMetrics()
	responseCache := cache.New(cache.Config{DefaultTTL: cfg.CacheTTL, MaxEntries: cfg.CacheMaxEntries, MaxValueBytes: cfg.CacheMaxValueBytes, MaxLocalBytes: cfg.CacheMaxLocalBytes, RedisAddr: cfg.RedisAddr, RedisOpTimeout: cfg.RedisOpTimeout, RedisCooldown: cfg.RedisCooldown, LockTTL: cfg.QueryTimeout + time.Second, LockWait: cfg.QueryTimeout, Observer: protectionMetrics})
	defer responseCache.Close()
	guardCfg := queryprotect.DefaultConfig()
	guardCfg.UserRate, guardCfg.DashboardRate = cfg.QueryUserRate, cfg.QueryDashboardRate
	guardCfg.UserBurst, guardCfg.DashboardBurst = cfg.QueryUserBurst, cfg.QueryDashboardBurst
	guardCfg.UserConcurrent, guardCfg.DashboardConcurrent = cfg.QueryUserConcurrent, cfg.QueryDashboardConcurrent
	guardCfg.QueryTimeout, guardCfg.SlowThreshold = cfg.QueryTimeout, cfg.QuerySlowThreshold
	srv := httpapi.NewServer(httpapi.Deps{
		ProviderRegistry: runtime.registry, ScopeNamespaces: runtime.catalog,
		Metrics: metrics, Logs: logs, Alerts: alerts, Topology: topo,
		Resolver: resolver, Cache: responseCache, Guard: queryprotect.New(guardCfg, protectionMetrics),
		ProtectionMetrics: protectionMetrics, Observability: platformMetrics,
		PlannedQueryRefs: panelQueryRefs(queries),
		CacheTTL:         cache.TTLPolicy{State: cfg.CacheTTL, Short: cfg.CacheShortTTL, Historical: cfg.CacheHistoricalTTL, HistoricalSafety: cfg.CacheHistoricalSafety},
		Logger:           logger, Stream: hub, StreamMetrics: streamMetrics,
		StreamOptions:  httpapi.StreamOptions{Heartbeat: cfg.StreamHeartbeat, WriteIdleTimeout: cfg.StreamWriteIdle},
		DashboardStore: dashboardAPI, DashboardQueryRefs: queries.Refs(), AllowedOrigin: cfg.AllowedOrigin,
		Version: contract.VersionInfo{Version: version, Commit: commit, BuildDate: buildDate},
	})
	httpServer := &http.Server{Addr: cfg.Addr, Handler: srv, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: cfg.ReadTimeout, WriteTimeout: cfg.WriteTimeout}
	return serveCentralHTTP(ctx, httpServer, runtime.errCh, usageErr, hub)
}

func startCentralAlertPollers(parent context.Context, cfg config.Config, catalog *clusterstate.RemoteCatalog, alerts datasource.Alerts, hub *stream.Hub, logger *slog.Logger) func() {
	if !cfg.Alertmanager.Enabled {
		return func() {}
	}
	ctx, cancel := context.WithCancel(parent)
	var wg sync.WaitGroup
	seen := make(map[string]struct{}, len(cfg.ClusterState.Clusters))
	for _, clusterID := range cfg.ClusterState.Clusters {
		if _, exists := seen[clusterID]; exists {
			continue
		}
		seen[clusterID] = struct{}{}
		clusterID := clusterID
		poller := stream.NewAlertPoller(stream.AlertPollerConfig{
			ClusterID: clusterID, Interval: cfg.AlertPollInterval, MaxBackoff: cfg.AlertPollMaxBackoff,
			MaxAlerts: cfg.AlertSnapshotMax, Jitter: true, InitialDelay: randomAlertInitialDelay,
			TrustedNamespaces: func() map[string]string { return catalog.StreamEntityNamespaces(clusterID) },
		}, alerts, hub, logger)
		wg.Add(1)
		go func() { defer wg.Done(); poller.Run(ctx) }()
	}
	return func() {
		cancel()
		done := make(chan struct{})
		go func() { wg.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			logger.Error("Alertmanager poller shutdown timed out")
		}
	}
}

func serveCentralHTTP(ctx context.Context, server *http.Server, watchErr, usageErr <-chan error, hub *stream.Hub) error {
	httpErr := make(chan error, 1)
	go func() {
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			httpErr <- err
		}
	}()
	defer func() { _ = shutdownCentralHTTP(server) }()
	select {
	case err := <-httpErr:
		return err
	case err := <-watchErr:
		return err
	case err := <-usageErr:
		return err
	case <-ctx.Done():
		if hub != nil {
			hub.Close()
		}
		return shutdownCentralHTTP(server)
	}
}

func shutdownCentralHTTP(server *http.Server) error {
	if server == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := server.Shutdown(ctx); err != nil {
		_ = server.Close()
		return err
	}
	return nil
}

func newCentralRuntime(parent context.Context, cfg config.ClusterStateConfig, hub *stream.Hub) (*centralRuntime, error) {
	if hub == nil {
		return nil, fmt.Errorf("central stream hub is required")
	}
	catalog, err := clusterstate.NewRemoteCatalog(cfg.Clusters, cfg.MaxResources)
	if err != nil {
		return nil, err
	}
	usage, err := clusterstate.NewUsageStore(cfg.Clusters, cfg.MaxResources)
	if err != nil {
		return nil, err
	}
	files := transport.TLSFiles{CertFile: cfg.CertFile, KeyFile: cfg.KeyFile, CAFile: cfg.CAFile, TrustDomain: cfg.TrustDomain}
	dial := func(context.Context) (v1.ClusterStateClient, io.Closer, error) {
		tlsConfig, err := transport.ClientTLS(files, cfg.RegistryServerName)
		if err != nil {
			return nil, nil, err
		}
		conn, err := grpc.NewClient(cfg.RegistryEndpoint,
			grpc.WithTransportCredentials(credentials.NewTLS(tlsConfig)),
			grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(cfg.MaxMessageBytes), grpc.MaxCallSendMsgSize(cfg.MaxMessageBytes)))
		if err != nil {
			return nil, nil, err
		}
		conn.Connect()
		return v1.NewClusterStateClient(conn), conn, nil
	}
	client, closer, err := dial(parent)
	if err != nil {
		return nil, err
	}
	conn, ok := closer.(*grpc.ClientConn)
	if !ok {
		_ = closer.Close()
		return nil, fmt.Errorf("central registry connection unavailable")
	}
	ctx, cancel := context.WithCancel(parent)
	r := &centralRuntime{catalog: catalog, usage: usage, ctx: ctx, cancel: cancel, conn: conn, errCh: make(chan error, 1), done: make(chan struct{})}
	r.registry = &clusterstate.GRPCRegistry{
		Client:        client,
		Health:        healthv1.NewHealthClient(conn),
		MaxReplyBytes: cfg.MaxMessageBytes,
		Catalog:       catalog,
		Usage:         usage,
		WatchDial:     dial,
	}
	r.wg.Add(2)
	go func() {
		defer r.wg.Done()
		r.registry.StartHealth(ctx, time.Second, 500*time.Millisecond)
	}()
	go func() {
		defer r.wg.Done()
		if err := r.registry.StartWatch(ctx, cfg.Clusters, catalog, hub.PublishWatchFrame); err != nil && ctx.Err() == nil {
			select {
			case r.errCh <- err:
			default:
			}
		}
	}()
	go func() {
		r.wg.Wait()
		close(r.done)
	}()
	return r, nil
}

func (r *centralRuntime) Close() error {
	if r == nil {
		return nil
	}
	r.once.Do(func() {
		r.cancel()
		_ = r.conn.Close()
	})
	select {
	case <-r.done:
		return nil
	case <-time.After(5 * time.Second):
		return fmt.Errorf("central watch shutdown timed out")
	}
}
