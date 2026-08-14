package stream

// Alert 변경원 — 이슈 #12.
//
// Alertmanager 실클라이언트(#17 잔여)가 아직 없으므로, 기존 datasource.Alerts
// 추상화 위에서 **유계 스냅숏 diff**로 변경만 뽑아냅니다. 어댑터가 실클라이언트로
// 바뀌어도 이 파일은 그대로입니다.
//
// 규칙:
//   - 최초 스냅숏은 변경이 아닙니다. 아무것도 내보내지 않습니다.
//   - 스냅숏 크기는 상한이 있습니다. 초과 poll은 diff 전체를 보류하고 이전 complete
//     snapshot을 유지해 입력 순서 변화가 false add/delete를 만들지 않게 합니다.
//   - 조회 실패는 지수 backoff로 물러납니다. 소스가 없어도(Unavailable) 스핀하거나
//     메모리를 늘리지 않습니다.
//   - 봉투에는 알림 신원(entity·namespace)만 싣습니다. annotation·label 원문은
//     싣지 않습니다 — 본문은 알림 화면의 HTTP 조회가 담당합니다.

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"hash"
	"io"
	"log/slog"
	"strconv"
	"time"

	"github.com/xenx96/k8s-dashboard/apps/api/internal/contract"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/datasource"
)

const (
	maxAlertIDLen   = 512
	maxEntityUIDLen = 128
)

// AlertPollerConfig는 폴링 주기·상한입니다. 0 이하는 기본값으로 대체됩니다.
type AlertPollerConfig struct {
	ClusterID string
	// Interval은 정상 폴링 주기입니다.
	Interval time.Duration
	// MaxBackoff는 연속 실패 시 물러나는 최대 대기입니다.
	MaxBackoff time.Duration
	// MaxAlerts는 스냅숏에 유지하는 알림 수 상한입니다.
	MaxAlerts int
	// Window는 List에 넘기는 조회 구간 폭입니다.
	Window time.Duration
	Now    func() time.Time
	// TrustedNamespaces returns a server-owned informer catalog keyed by
	// "pod:<uid>" or "workload:<uid>". Upstream alert namespaces are ignored.
	TrustedNamespaces func() map[string]string
}

func (c *AlertPollerConfig) setDefaults() {
	if c.ClusterID == "" {
		c.ClusterID = "default"
	}
	if c.Interval <= 0 {
		c.Interval = 30 * time.Second
	}
	if c.MaxBackoff < c.Interval {
		c.MaxBackoff = 5 * time.Minute
	}
	if c.MaxAlerts <= 0 {
		c.MaxAlerts = 2000
	}
	if c.Window <= 0 {
		c.Window = time.Hour
	}
	if c.Now == nil {
		c.Now = time.Now
	}
}

// alertState는 diff에 필요한 최소 상태입니다. annotation·label 원문은 두지 않습니다.
type alertState struct {
	status    string
	namespace string
	identity  string
	visible   [sha256.Size]byte
	entity    *contract.EntityRef
}

// AlertPoller는 datasource.Alerts 스냅숏의 변경분을 허브로 내보냅니다.
type AlertPoller struct {
	cfg    AlertPollerConfig
	src    datasource.Alerts
	hub    *Hub
	logger *slog.Logger

	primed    bool
	snapshot  map[string]alertState
	truncated bool
}

func NewAlertPoller(cfg AlertPollerConfig, src datasource.Alerts, hub *Hub, logger *slog.Logger) *AlertPoller {
	cfg.setDefaults()
	if logger == nil {
		logger = slog.Default()
	}
	return &AlertPoller{cfg: cfg, src: src, hub: hub, logger: logger, snapshot: map[string]alertState{}}
}

// Run은 ctx가 끝날 때까지 폴링합니다. 실패는 backoff, 성공은 기본 주기로 돌아갑니다.
func (p *AlertPoller) Run(ctx context.Context) {
	delay := p.cfg.Interval
	for {
		if err := p.PollOnce(ctx); err != nil {
			if delay *= 2; delay > p.cfg.MaxBackoff {
				delay = p.cfg.MaxBackoff
			}
		} else {
			delay = p.cfg.Interval
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

// PollOnce는 한 번 조회해 이전 스냅숏과 diff합니다. 테스트가 직접 부릅니다.
func (p *AlertPoller) PollOnce(ctx context.Context) error {
	now := p.cfg.Now()
	ctx, cancel := context.WithTimeout(ctx, p.cfg.Interval)
	defer cancel()

	res, err := p.src.List(ctx, datasource.AlertQuery{
		Target: datasource.Target{ClusterID: p.cfg.ClusterID},
		Window: datasource.Window{From: now.Add(-p.cfg.Window), To: now},
	})
	if err != nil {
		// 소스가 없거나 죽었어도 스냅숏은 그대로 둡니다 — 복구되면 그때 diff합니다.
		return err
	}

	current := make(map[string]alertState, min(len(p.snapshot), p.cfg.MaxAlerts))
	trusted := map[string]string(nil)
	if p.cfg.TrustedNamespaces != nil {
		trusted = p.cfg.TrustedNamespaces()
	}
	over := 0
	collect := func(list []contract.AlertInstance, status string) {
		for i := range list {
			if len(current) >= p.cfg.MaxAlerts {
				over++
				continue
			}
			a := &list[i]
			if a.ID == "" || len(a.ID) > maxAlertIDLen {
				continue
			}
			namespace, identity, entity := trustedAlertEntity(p.cfg.ClusterID, a.Entity, trusted)
			current[a.ID] = alertState{status: status, namespace: namespace, identity: identity, visible: alertVisibleFingerprint(a), entity: entity}
		}
	}
	collect(res.Firing, "firing")
	collect(res.Resolved, "resolved")
	if over > 0 && !p.truncated {
		// 상한 초과는 한 번만 알립니다 — 폴링마다 찍으면 로그가 도배됩니다.
		p.logger.Warn("알림 스냅숏 상한 초과 · 이번 poll diff를 보류하고 이전 complete snapshot을 유지합니다",
			"max", p.cfg.MaxAlerts, "over", over)
	}
	p.truncated = over > 0
	if over > 0 {
		// Retain the previous complete snapshot. Diffing two arbitrary prefixes
		// would manufacture additions/deletions when upstream order changes.
		return nil
	}

	if !p.primed {
		// 최초 스냅숏은 변경이 아니라 현재 상태입니다. 내보내지 않습니다.
		p.snapshot, p.primed = current, true
		return nil
	}

	for id, cur := range current {
		prev, existed := p.snapshot[id]
		switch {
		case !existed:
			p.publish(contract.StreamActionAdded, cur)
		case prev.namespace != cur.namespace || prev.identity != cur.identity:
			// Invalidate both authorization domains. Publishing only the new
			// namespace would leave the old scoped screen stale.
			p.publish(contract.StreamActionDeleted, prev)
			p.publish(contract.StreamActionUpdated, cur)
		case prev.status != cur.status || prev.visible != cur.visible:
			p.publish(contract.StreamActionUpdated, cur)
		}
	}
	for id, prev := range p.snapshot {
		if _, still := current[id]; !still {
			p.publish(contract.StreamActionDeleted, prev)
		}
	}
	p.snapshot = current
	return nil
}

// alertVisibleFingerprint folds every HTTP UI-visible field into a fixed-size
// digest. Sensitive labels/annotations are never retained in the snapshot.
func alertVisibleFingerprint(a *contract.AlertInstance) [sha256.Size]byte {
	h := sha256.New()
	for _, value := range []string{a.Name, a.Severity, a.Status, a.StartsAt, a.EndsAt, a.EntityName, a.SourceURL, a.Source, strconv.Itoa(a.GroupSize), a.GroupKey} {
		writeHashString(h, value)
	}
	writeHashMap(h, a.Labels)
	writeHashMap(h, a.Annotations)
	if a.Entity == nil {
		writeHashString(h, "")
	} else {
		e := a.Entity
		for _, value := range []string{e.ClusterID, e.Namespace, e.WorkloadKind, e.WorkloadName, e.WorkloadUID, e.PodName, e.PodUID, e.ContainerName, e.ServiceName, e.ServiceNamespace, e.ServiceVersion} {
			writeHashString(h, value)
		}
	}
	var out [sha256.Size]byte
	copy(out[:], h.Sum(nil))
	return out
}

func writeHashString(h hash.Hash, value string) {
	var size [8]byte
	binary.BigEndian.PutUint64(size[:], uint64(len(value)))
	_, _ = h.Write(size[:])
	_, _ = io.WriteString(h, value)
}

// writeHashMap is independent of Go map iteration order. Each length-prefixed
// entry is hashed separately, then fixed-size digests are commutatively folded.
func writeHashMap(h hash.Hash, values map[string]string) {
	var folded [sha256.Size]byte
	for key, value := range values {
		entry := sha256.New()
		writeHashString(entry, key)
		writeHashString(entry, value)
		sum := entry.Sum(nil)
		for i := range folded {
			folded[i] ^= sum[i]
		}
	}
	var count [8]byte
	binary.BigEndian.PutUint64(count[:], uint64(len(values)))
	_, _ = h.Write(count[:])
	_, _ = h.Write(folded[:])
}

func (p *AlertPoller) publish(action contract.StreamEventAction, st alertState) {
	p.hub.Publish(contract.EventEnvelope{
		Kind:      contract.StreamKindAlert,
		Action:    action,
		ClusterID: p.cfg.ClusterID,
		Namespace: st.namespace,
		Entity:    st.entity,
	})
}

// trustedAlertEntity는 UID를 informer-owned catalog로 확인해 namespace를 정합니다.
// upstream namespace/labels는 신뢰하지 않으며, 검증 불가는 cluster-wide(All-only)입니다.
func trustedAlertEntity(clusterID string, claimed *contract.EntityRef, trusted map[string]string) (string, string, *contract.EntityRef) {
	if claimed == nil {
		return "", "", nil
	}
	if claimed.PodUID != "" && len(claimed.PodUID) <= maxEntityUIDLen {
		if ns, ok := trusted["pod:"+claimed.PodUID]; ok {
			identity := "pod:" + claimed.PodUID
			return ns, identity, &contract.EntityRef{ClusterID: clusterID, Namespace: ns, PodUID: claimed.PodUID}
		}
	}
	if claimed.WorkloadUID != "" && len(claimed.WorkloadUID) <= maxEntityUIDLen {
		if ns, ok := trusted["workload:"+claimed.WorkloadUID]; ok {
			identity := "workload:" + claimed.WorkloadUID
			return ns, identity, &contract.EntityRef{ClusterID: clusterID, Namespace: ns, WorkloadUID: claimed.WorkloadUID}
		}
	}
	// Unverifiable alerts are cluster-wide and therefore reach All scope only.
	return "", "", nil
}
