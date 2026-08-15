package clusterstate

import (
	"context"
	"fmt"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/xenx96/k8s-dashboard/apps/api/internal/contract"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/datasource"
)

// UsagePoller refreshes each configured cluster independently. Metrics must
// enforce its own upstream cluster predicate; the catalog filter is a second
// fail-closed UID boundary before admission to UsageStore.
type UsagePoller struct {
	Metrics   datasource.Metrics
	Catalog   *RemoteCatalog
	Store     *UsageStore
	Interval  time.Duration
	Timeout   time.Duration
	RetryMin  time.Duration
	RetryMax  time.Duration
	Delay     func(time.Duration) time.Duration
	AfterPoll func(string, error)
}

func (p *UsagePoller) Run(ctx context.Context, clusterIDs []string) error {
	if p == nil || p.Metrics == nil || p.Catalog == nil || p.Store == nil || len(clusterIDs) == 0 || len(clusterIDs) > 64 {
		return fmt.Errorf("invalid usage poller")
	}
	normalized := *p
	if normalized.Interval == 0 {
		normalized.Interval = 30 * time.Second
	}
	if normalized.Timeout == 0 {
		normalized.Timeout = 10 * time.Second
	}
	if normalized.RetryMin == 0 {
		normalized.RetryMin = time.Second
	}
	if normalized.RetryMax == 0 {
		normalized.RetryMax = 30 * time.Second
	}
	if normalized.Interval < time.Millisecond || normalized.Interval > 24*time.Hour || normalized.Timeout < time.Millisecond || normalized.Timeout > time.Minute || normalized.RetryMin < time.Millisecond || normalized.RetryMin > time.Minute || normalized.RetryMax < normalized.RetryMin || normalized.RetryMax > 5*time.Minute {
		return fmt.Errorf("invalid usage poller timing")
	}
	seen := make(map[string]struct{}, len(clusterIDs))
	for _, id := range clusterIDs {
		if !p.Catalog.AllowsCluster(id) {
			return fmt.Errorf("unknown usage cluster")
		}
		if _, duplicate := seen[id]; duplicate {
			return fmt.Errorf("duplicate usage cluster")
		}
		seen[id] = struct{}{}
	}
	var wg sync.WaitGroup
	for _, clusterID := range clusterIDs {
		id := clusterID
		wg.Add(1)
		go func() {
			defer wg.Done()
			normalized.runCluster(ctx, id)
		}()
	}
	wg.Wait()
	return nil
}

func (p *UsagePoller) runCluster(ctx context.Context, clusterID string) {
	interval := p.Interval
	retryMin := p.RetryMin
	retryMax := p.RetryMax
	backoff := retryMin
	for ctx.Err() == nil {
		var err error
		if !p.Catalog.Available(clusterID) {
			err = p.Store.Update(clusterID, map[string]contract.ContainerUsage{})
		} else {
			pollCtx, cancel := context.WithTimeout(ctx, p.Timeout)
			var values map[string]contract.ContainerUsage
			values, err = p.Metrics.Usage(pollCtx, clusterID)
			cancel()
			if err == nil {
				filtered := make(map[string]contract.ContainerUsage, len(values))
				for uid, usage := range values {
					if p.Catalog.HasPodUID(clusterID, uid) {
						filtered[uid] = usage
					}
				}
				err = p.Store.Update(clusterID, filtered)
			}
		}
		if p.AfterPoll != nil {
			p.AfterPoll(clusterID, err)
		}
		delay := interval
		if err != nil {
			delay = backoff
			backoff *= 2
			if backoff > retryMax {
				backoff = retryMax
			}
		} else {
			backoff = retryMin
		}
		if p.Delay != nil {
			delay = p.Delay(delay)
		} else if err != nil {
			delay += time.Duration(rand.Int64N(max(1, int64(delay/5))))
		}
		if delay < time.Millisecond || delay > interval+retryMax {
			delay = interval
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return
		case <-timer.C:
		}
	}
}
