// Package topologylayout은 Pod Topology 화면의 **공유 노드 배치** 저장소입니다. (#28)
//
// 관리자가 Edit 모드에서 저장한 좌표를 모든 사용자가 같은 화면에서 봅니다.
// 저장소는 Redis(REDIS_ADDR)이며 TTL 없이 보존합니다. Redis가 없거나 실패하면
// 프로세스 메모리로 동작합니다 — 단일 replica 개발 환경용이며 재시작 시 사라집니다.
// 배치는 표시 편의 데이터일 뿐이므로 저장 실패가 화면 조회를 막지 않아야 합니다.
package topologylayout

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/xenx96/k8s-dashboard/apps/api/internal/contract"
)

const (
	// MaxPositions는 저장할 수 있는 노드 좌표 수 상한입니다. 그래프 노드는 워크로드
	// 단위라 개수 상한이 없으므로(#3) 대형 클러스터 여유분을 두되, 임의 크기 본문이
	// 저장소를 채우지 못하게 막습니다.
	MaxPositions = 2000
	// MaxIDLen은 노드 ID(Pod UID 등) 길이 상한입니다.
	MaxIDLen = 253
	// coordLimit 밖의 좌표는 화면 밖 유실이므로 거부합니다.
	coordLimit = 1_000_000
)

// ErrInvalid는 요청 본문이 규격을 벗어났을 때 반환합니다. 400으로 매핑합니다.
var ErrInvalid = errors.New("invalid layout")

type Config struct {
	RedisAddr string
	OpTimeout time.Duration
	Logger    *slog.Logger
	// Now는 테스트에서 시간을 고정합니다.
	Now func() time.Time
}

type Store struct {
	mu     sync.RWMutex
	mem    map[string]contract.TopologyLayout
	rdb    *redis.Client
	op     time.Duration
	logger *slog.Logger
	now    func() time.Time
}

func New(cfg Config) *Store {
	s := &Store{
		mem:    map[string]contract.TopologyLayout{},
		op:     cfg.OpTimeout,
		logger: cfg.Logger,
		now:    cfg.Now,
	}
	if s.op <= 0 {
		s.op = 250 * time.Millisecond
	}
	if s.now == nil {
		s.now = time.Now
	}
	if s.logger == nil {
		s.logger = slog.Default()
	}
	if cfg.RedisAddr != "" {
		s.rdb = redis.NewClient(&redis.Options{Addr: cfg.RedisAddr})
	}
	return s
}

func key(clusterID string) string { return "topology:layout:v1:" + clusterID }

// Get은 저장된 배치를 돌려줍니다. 없으면 (nil, nil)입니다.
// Redis 오류는 메모리 사본으로 대체하고 오류를 삼킵니다 — 배치는 조회 화면을
// 실패시킬 만큼 중요하지 않습니다.
func (s *Store) Get(ctx context.Context, clusterID string) (*contract.TopologyLayout, error) {
	if s.rdb != nil {
		opCtx, cancel := context.WithTimeout(ctx, s.op)
		defer cancel()
		raw, err := s.rdb.Get(opCtx, key(clusterID)).Bytes()
		switch {
		case err == nil:
			var l contract.TopologyLayout
			if json.Unmarshal(raw, &l) == nil && len(l.Positions) > 0 {
				return &l, nil
			}
			return nil, nil
		case errors.Is(err, redis.Nil):
			return nil, nil
		default:
			s.logger.Warn("topology layout redis 조회 실패 · 메모리 사본 사용", "err", err)
		}
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if l, ok := s.mem[clusterID]; ok && len(l.Positions) > 0 {
		out := l
		return &out, nil
	}
	return nil, nil
}

// Put은 배치를 검증 후 저장합니다. positions가 비면 저장을 지우고 기본 배치로
// 돌아갑니다. 반환값은 저장된(또는 초기화된) 배치입니다.
func (s *Store) Put(ctx context.Context, clusterID string, positions []contract.TopologyNodePosition) (contract.TopologyLayout, error) {
	if err := validate(positions); err != nil {
		return contract.TopologyLayout{}, err
	}
	layout := contract.TopologyLayout{
		Positions: positions,
		UpdatedAt: s.now().UTC().Format(time.RFC3339),
	}
	if layout.Positions == nil {
		layout.Positions = []contract.TopologyNodePosition{}
	}

	s.mu.Lock()
	if len(layout.Positions) == 0 {
		delete(s.mem, clusterID)
	} else {
		s.mem[clusterID] = layout
	}
	s.mu.Unlock()

	if s.rdb != nil {
		opCtx, cancel := context.WithTimeout(ctx, s.op)
		defer cancel()
		var err error
		if len(layout.Positions) == 0 {
			err = s.rdb.Del(opCtx, key(clusterID)).Err()
		} else {
			raw, _ := json.Marshal(layout)
			err = s.rdb.Set(opCtx, key(clusterID), raw, 0).Err()
		}
		if err != nil {
			// 메모리에는 반영됐으므로 이 프로세스는 새 배치를 보여줍니다.
			// 다중 replica에서는 불일치가 생기므로 로그로 남깁니다.
			s.logger.Warn("topology layout redis 저장 실패 · 메모리에만 반영", "err", err)
		}
	}
	return layout, nil
}

func validate(positions []contract.TopologyNodePosition) error {
	if len(positions) > MaxPositions {
		return fmt.Errorf("%w: 좌표는 최대 %d개입니다", ErrInvalid, MaxPositions)
	}
	seen := make(map[string]struct{}, len(positions))
	for _, p := range positions {
		if p.ID == "" || len(p.ID) > MaxIDLen {
			return fmt.Errorf("%w: 노드 ID 길이는 1~%d입니다", ErrInvalid, MaxIDLen)
		}
		if _, dup := seen[p.ID]; dup {
			return fmt.Errorf("%w: 노드 ID가 중복되었습니다", ErrInvalid)
		}
		seen[p.ID] = struct{}{}
		for _, v := range []float64{p.X, p.Y} {
			if math.IsNaN(v) || math.IsInf(v, 0) || math.Abs(v) > coordLimit {
				return fmt.Errorf("%w: 좌표 값이 허용 범위를 벗어났습니다", ErrInvalid)
			}
		}
	}
	return nil
}

// Close는 Redis 연결을 정리합니다.
func (s *Store) Close() error {
	if s.rdb != nil {
		return s.rdb.Close()
	}
	return nil
}
