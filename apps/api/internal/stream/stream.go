// Package stream은 상태 변경 SSE의 **유계 프로세스 내 허브**입니다. (#12, ADR 0005)
//
// 규칙은 네 가지입니다.
//   - Publish는 논블로킹이고 활성 구독자 수 n에 대해 O(n)입니다. 느린 구독자는
//     끊습니다 — informer 콜백을 막거나 메모리를 늘리는 선택지는 없습니다.
//   - 재생 링과 구독자 채널은 크기가 고정입니다. 연결 수는 전역·주체별 상한이 있습니다.
//   - 모든 전달(라이브·재생)은 서버가 확정한 Scope로 필터링됩니다.
//   - 재생을 보장할 수 없으면(다른 인스턴스·보존 구간 밖·미래 ID) reset을 보냅니다.
//     완전 재생을 조용히 주장하지 않습니다.
package stream

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/xenx96/k8s-dashboard/apps/api/internal/contract"
)

// 구독 거절 사유입니다. 핸들러가 상태 코드로 바꿉니다.
var (
	// ErrCapacity — 전역 또는 주체별 연결 상한 초과. 429 + Retry-After가 됩니다.
	ErrCapacity = errors.New("stream connection capacity exceeded")
	// ErrClosed — 종료된 허브는 새 구독을 받을 수 없습니다. 용량 초과가 아니므로
	// HTTP 경계에서는 재시도 힌트 없는 503으로 매핑합니다.
	ErrClosed = errors.New("stream hub is closed")
	// ErrBadLastEventID — 형식이 틀리거나 상한을 넘는 Last-Event-ID. 구독 자원을
	// 배정하기 전에 실패합니다. 400이 됩니다.
	ErrBadLastEventID = errors.New("malformed Last-Event-ID")
)

// Observer는 /metrics로 나가는 스트림 관측 훅입니다. 라벨은 고정된
// kind/reason뿐입니다 — 주체·namespace·ID·질의를 넘기지 않습니다.
type Observer interface {
	StreamOpened()
	StreamClosed(reason string)
	StreamRejected(reason string)
	StreamDropped(reason string)
	StreamReplayed(n int)
	StreamReset()
	StreamPublished(kind string)
	StreamDelivered(d time.Duration)
}

// Config는 허브의 유계값입니다. 0이면 기본값을 씁니다.
type Config struct {
	// RingSize는 재생 링의 고정 크기입니다. 이 수를 넘는 과거는 reset으로 답합니다.
	RingSize int
	// SubscriberBuffer는 구독자별 고정 채널 크기입니다. 가득 차면 그 구독자를 끊습니다.
	SubscriberBuffer int
	// MaxConnections/MaxPerSubject는 전역·주체별 동시 연결 상한입니다.
	MaxConnections int
	MaxPerSubject  int
	Now            func() time.Time
}

// Absolute configuration caps bound eager ring allocation and the aggregate
// subscriber channel slots. They allow large installations while keeping an
// operator typo from allocating unbounded memory before the server starts.
const (
	MaxRingSize         = 65_536
	MaxConnections      = 4_096
	MaxSubscriberBuffer = 4_096
	MaxSubscriberSlots  = 262_144
)

// DefaultConfig는 안전한 기본값입니다. config 패키지의 env 기본값과 같습니다.
func DefaultConfig() Config {
	return Config{RingSize: 1024, SubscriberBuffer: 64, MaxConnections: 256, MaxPerSubject: 8, Now: time.Now}
}

func (c *Config) setDefaults() {
	d := DefaultConfig()
	if c.RingSize <= 0 {
		c.RingSize = d.RingSize
	}
	if c.SubscriberBuffer <= 0 {
		c.SubscriberBuffer = d.SubscriberBuffer
	}
	if c.MaxConnections <= 0 {
		c.MaxConnections = d.MaxConnections
	}
	if c.MaxPerSubject <= 0 {
		c.MaxPerSubject = d.MaxPerSubject
	}
	if c.Now == nil {
		c.Now = time.Now
	}
}

func validateConfig(c Config) error {
	if c.RingSize > MaxRingSize {
		return fmt.Errorf("stream ring size %d exceeds maximum %d", c.RingSize, MaxRingSize)
	}
	if c.MaxConnections > MaxConnections {
		return fmt.Errorf("stream connections %d exceeds maximum %d", c.MaxConnections, MaxConnections)
	}
	if c.SubscriberBuffer > MaxSubscriberBuffer {
		return fmt.Errorf("stream subscriber buffer %d exceeds maximum %d", c.SubscriberBuffer, MaxSubscriberBuffer)
	}
	if int64(c.MaxConnections)*int64(c.SubscriberBuffer) > MaxSubscriberSlots {
		return fmt.Errorf("stream subscriber slots %d exceed maximum %d", int64(c.MaxConnections)*int64(c.SubscriberBuffer), MaxSubscriberSlots)
	}
	return nil
}

// Filter는 구독자에게 서버가 확정한 Scope입니다. 요청 파라미터가 아니라
// scope.Resolver의 결과에서 만듭니다. (README §10)
type Filter struct {
	ClusterID  string
	All        bool
	Namespaces []string
}

// allows는 이 Scope가 envelope을 받아도 되는지입니다.
//   - reset은 제어 신호라 클러스터가 맞으면 누구에게나 갑니다.
//   - Namespace가 빈 클러스터 범위 신호는 전체(All) Scope에게만 갑니다.
func (f Filter) allows(env contract.EventEnvelope) bool {
	if env.ClusterID != f.ClusterID {
		return false
	}
	if env.Kind == contract.StreamKindReset {
		return true
	}
	if f.All {
		return true
	}
	if env.Namespace == "" {
		return false
	}
	for _, ns := range f.Namespaces {
		if ns == env.Namespace {
			return true
		}
	}
	return false
}

// Event는 전달 지연 측정을 위해 적재 시각을 함께 나릅니다.
type Event struct {
	Envelope contract.EventEnvelope
	// EnqueuedAt은 Publish 시각입니다. 핸들러가 write 시점에 지연을 관측합니다.
	EnqueuedAt time.Time
}

// Subscription은 구독 하나입니다. Replay()를 먼저 소비한 뒤 Events()를 읽습니다 —
// 두 구간은 구독 시점에 같은 잠금 아래에서 잘라 순서 보장·중복 없음이 성립합니다.
type Subscription struct {
	hub     *Hub
	subject [sha256.Size]byte
	filter  Filter
	replay  []Event
	ch      chan Event
}

// Replay는 Last-Event-ID 이후 놓친 이벤트(또는 reset 신호)입니다. Scope 필터가
// 이미 적용되어 있습니다.
func (s *Subscription) Replay() []Event { return s.replay }

// Events는 라이브 이벤트 채널입니다. 허브가 닫히거나 구독자가 느려서 끊기면 닫힙니다.
func (s *Subscription) Events() <-chan Event { return s.ch }

// Close는 구독을 해제합니다. 여러 번 불러도 안전합니다.
func (s *Subscription) Close() { s.hub.remove(s, "client") }

// Hub는 프로세스 로컬 재생 링과 구독자 집합입니다.
type Hub struct {
	cfg Config
	obs Observer

	mu       sync.Mutex
	closed   bool
	instance string
	seq      uint64
	// ring은 고정 크기 순환 버퍼입니다. ring[i]의 seq는 firstSeq()+i입니다.
	ring  []Event
	start int
	count int

	subs         map[*Subscription]struct{}
	subjectConns map[[sha256.Size]byte]int
}

// New는 허브를 만듭니다. 고루틴을 만들지 않습니다 — 수명 관리는 Close 하나입니다.
func New(cfg Config, obs Observer) (*Hub, error) {
	return newWithRandom(cfg, obs, rand.Reader)
}

// newWithRandom keeps entropy injection private to tests. A process must never
// start with an all-zero/reused instance ID because that could make replay after
// restart look valid and silently skip a required reset.
func newWithRandom(cfg Config, obs Observer, random io.Reader) (*Hub, error) {
	cfg.setDefaults()
	if err := validateConfig(cfg); err != nil {
		return nil, err
	}
	var b [16]byte
	if _, err := io.ReadFull(random, b[:]); err != nil {
		return nil, fmt.Errorf("stream instance ID entropy unavailable: %w", err)
	}
	return &Hub{
		cfg:          cfg,
		obs:          obs,
		instance:     hex.EncodeToString(b[:]),
		ring:         make([]Event, cfg.RingSize),
		subs:         map[*Subscription]struct{}{},
		subjectConns: map[[sha256.Size]byte]int{},
	}, nil
}

// InstanceID는 이 프로세스의 스트림 인스턴스 식별자입니다.
func (h *Hub) InstanceID() string { return h.instance }

// eventID는 "<instance>-<seq>"입니다. 불투명 문자열로 취급하되 서버는 형식을 압니다.
func (h *Hub) eventID(seq uint64) string {
	return h.instance + "-" + strconv.FormatUint(seq, 10)
}

// parseLastEventID는 인바운드 Last-Event-ID를 엄격하게 해석합니다.
// 상한 초과·형식 오류는 오류입니다 — 구독 자원 배정 전에 거절합니다.
func (h *Hub) parseLastEventID(id string) (instance string, seq uint64, err error) {
	if len(id) == 0 || len(id) > contract.MaxStreamEventIDLen {
		return "", 0, ErrBadLastEventID
	}
	inst, rest, ok := strings.Cut(id, "-")
	if !ok || len(inst) != len(h.instance) || len(rest) == 0 || len(rest) > 20 {
		return "", 0, ErrBadLastEventID
	}
	for i := 0; i < len(inst); i++ {
		c := inst[i]
		if !('0' <= c && c <= '9' || 'a' <= c && c <= 'f') {
			return "", 0, ErrBadLastEventID
		}
	}
	n, perr := strconv.ParseUint(rest, 10, 64)
	if perr != nil {
		return "", 0, ErrBadLastEventID
	}
	return inst, n, nil
}

func (h *Hub) firstSeq() uint64 { return h.seq - uint64(h.count) + 1 }

// Publish는 envelope에 단조 ID를 붙여 링에 적재하고 구독자에게 팬아웃합니다.
// **논블로킹**입니다. 채널이 가득 찬 구독자는 이 자리에서 끊습니다.
func (h *Hub) Publish(env contract.EventEnvelope) {
	now := h.cfg.Now()
	if _, err := time.Parse(time.RFC3339Nano, env.ObservedAt); err != nil {
		env.ObservedAt = now.UTC().Format(time.RFC3339Nano)
	}

	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return
	}
	h.seq++
	env.ID = h.eventID(h.seq)
	ev := Event{Envelope: env, EnqueuedAt: now}

	// 순환 버퍼 적재 — 가장 오래된 항목을 덮습니다. 메모리는 구조적으로 고정입니다.
	if h.count < len(h.ring) {
		h.ring[(h.start+h.count)%len(h.ring)] = ev
		h.count++
	} else {
		h.ring[h.start] = ev
		h.start = (h.start + 1) % len(h.ring)
	}

	var dropped []*Subscription
	for sub := range h.subs {
		if !sub.filter.allows(env) {
			continue
		}
		select {
		case sub.ch <- ev:
		default:
			// 느린 구독자 — 여기서 기다리면 informer 콜백이 막힙니다. 끊습니다.
			dropped = append(dropped, sub)
		}
	}
	for _, sub := range dropped {
		h.removeLocked(sub)
	}
	obs := h.obs
	h.mu.Unlock()

	if obs != nil {
		obs.StreamPublished(string(env.Kind))
		for range dropped {
			obs.StreamDropped("slow_subscriber")
			obs.StreamClosed("slow_subscriber")
		}
	}
}

// Subscribe는 상한 검사 → Last-Event-ID 해석 → (재생 or reset) → 구독 등록을
// 한 잠금 안에서 수행합니다. 재생 절단과 라이브 시작 사이에 Publish가 끼어들 수 없어
// 유실·중복이 없습니다.
func (h *Hub) Subscribe(subject string, f Filter, lastEventID string) (*Subscription, error) {
	// 형식 오류는 어떤 자원도 배정하기 전에 거절합니다.
	var wantSeq uint64
	var replayRequested, foreign bool
	if lastEventID != "" {
		inst, seq, err := h.parseLastEventID(lastEventID)
		if err != nil {
			if h.obs != nil {
				h.obs.StreamRejected("bad_last_event_id")
			}
			return nil, err
		}
		replayRequested = true
		wantSeq = seq
		// 다른 인스턴스의 ID는 재생이 불가능합니다. reset으로 전락합니다(아래).
		foreign = inst != h.instance
	}

	subjectKey := sha256.Sum256([]byte(subject))
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		if h.obs != nil {
			h.obs.StreamRejected("shutdown")
		}
		return nil, ErrClosed
	}
	if len(h.subs) >= h.cfg.MaxConnections {
		h.mu.Unlock()
		if h.obs != nil {
			h.obs.StreamRejected("capacity")
		}
		return nil, ErrCapacity
	}
	if h.subjectConns[subjectKey] >= h.cfg.MaxPerSubject {
		h.mu.Unlock()
		if h.obs != nil {
			h.obs.StreamRejected("subject_capacity")
		}
		return nil, ErrCapacity
	}

	sub := &Subscription{
		hub:     h,
		subject: subjectKey,
		filter:  f,
		ch:      make(chan Event, h.cfg.SubscriberBuffer),
	}

	var replayed int
	var reset bool
	if replayRequested {
		switch {
		case foreign, wantSeq > h.seq, h.count > 0 && wantSeq+1 < h.firstSeq():
			// 다른 인스턴스 · 미래 ID · 보존 구간 밖 — 완전 재생을 주장할 수 없습니다.
			// 화면이 HTTP로 현재 상태를 다시 가져오도록 reset을 보냅니다.
			reset = true
			sub.replay = []Event{{
				Envelope: contract.EventEnvelope{
					ID:         h.eventID(h.seq),
					Kind:       contract.StreamKindReset,
					Action:     contract.StreamActionReset,
					ClusterID:  f.ClusterID,
					ObservedAt: h.cfg.Now().UTC().Format(time.RFC3339),
				},
				EnqueuedAt: h.cfg.Now(),
			}}
		default:
			// 같은 인스턴스 · 보존 구간 안 — wantSeq 다음부터 순서대로 재생합니다.
			for s := wantSeq + 1; s <= h.seq; s++ {
				ev := h.ring[(h.start+int(s-h.firstSeq()))%len(h.ring)]
				if sub.filter.allows(ev.Envelope) {
					sub.replay = append(sub.replay, ev)
					replayed++
				}
			}
		}
	}

	h.subs[sub] = struct{}{}
	h.subjectConns[subjectKey]++
	obs := h.obs
	h.mu.Unlock()

	if obs != nil {
		obs.StreamOpened()
		if replayed > 0 {
			obs.StreamReplayed(replayed)
		}
		if reset {
			obs.StreamReset()
		}
	}
	return sub, nil
}

// remove는 구독을 해제하고 채널을 닫습니다. 잠금 아래에서 소속을 확인하므로
// Close()와 느린 구독자 강제 해제가 경합해도 채널은 한 번만 닫힙니다.
func (h *Hub) remove(sub *Subscription, reason string) {
	h.mu.Lock()
	removed := h.removeLocked(sub)
	obs := h.obs
	h.mu.Unlock()
	if removed && obs != nil {
		obs.StreamClosed(reason)
	}
}

func (h *Hub) removeLocked(sub *Subscription) bool {
	if _, ok := h.subs[sub]; !ok {
		return false
	}
	delete(h.subs, sub)
	h.subjectConns[sub.subject]--
	if h.subjectConns[sub.subject] <= 0 {
		delete(h.subjectConns, sub.subject)
	}
	close(sub.ch)
	return true
}

// Close는 모든 구독자 채널을 닫고 이후의 Publish/Subscribe를 무시·거절합니다.
// 서버 종료 시 핸들러들이 즉시 빠져나오게 합니다.
func (h *Hub) Close() {
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return
	}
	h.closed = true
	subs := make([]*Subscription, 0, len(h.subs))
	for sub := range h.subs {
		subs = append(subs, sub)
	}
	for _, sub := range subs {
		h.removeLocked(sub)
	}
	obs := h.obs
	h.mu.Unlock()
	if obs != nil {
		for range subs {
			obs.StreamClosed("shutdown")
		}
	}
}

// Stats는 테스트·진단용 현재 상태입니다.
func (h *Hub) Stats() (connections int, retained int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.subs), h.count
}
