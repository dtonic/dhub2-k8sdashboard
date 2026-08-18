package topologylayout_test

import (
	"context"
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/xenx96/k8s-dashboard/apps/api/internal/contract"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/topologylayout"
)

func fixed() time.Time { return time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC) }

func newMemStore() *topologylayout.Store {
	return topologylayout.New(topologylayout.Config{Now: fixed})
}

// TestPutGetRoundtrip — 저장한 배치를 그대로 돌려받고, 빈 저장은 nil입니다.
func TestPutGetRoundtrip(t *testing.T) {
	s := newMemStore()
	ctx := context.Background()

	if l, err := s.Get(ctx, "lnode"); err != nil || l != nil {
		t.Fatalf("빈 저장소 Get = %v, %v", l, err)
	}

	saved, err := s.Put(ctx, "lnode", []contract.TopologyNodePosition{{ID: "pod-a", X: 10, Y: -20.5}})
	if err != nil {
		t.Fatalf("Put 실패: %v", err)
	}
	if saved.UpdatedAt != "2026-08-18T12:00:00Z" {
		t.Fatalf("UpdatedAt = %s", saved.UpdatedAt)
	}

	got, err := s.Get(ctx, "lnode")
	if err != nil || got == nil || len(got.Positions) != 1 || got.Positions[0].ID != "pod-a" {
		t.Fatalf("Get = %+v, %v", got, err)
	}
	// 다른 클러스터 키에는 영향이 없어야 합니다.
	if l, _ := s.Get(ctx, "other"); l != nil {
		t.Fatalf("다른 클러스터에 배치가 새었습니다: %+v", l)
	}
}

// TestPutEmptyResets — 빈 positions는 저장을 지우고 기본 배치로 돌아갑니다.
func TestPutEmptyResets(t *testing.T) {
	s := newMemStore()
	ctx := context.Background()
	if _, err := s.Put(ctx, "lnode", []contract.TopologyNodePosition{{ID: "a", X: 1, Y: 2}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Put(ctx, "lnode", nil); err != nil {
		t.Fatalf("빈 Put 실패: %v", err)
	}
	if l, _ := s.Get(ctx, "lnode"); l != nil {
		t.Fatalf("초기화 후에도 배치가 남았습니다: %+v", l)
	}
}

// TestValidation — 규격 밖 입력은 ErrInvalid로 거절합니다.
func TestValidation(t *testing.T) {
	s := newMemStore()
	ctx := context.Background()

	tooMany := make([]contract.TopologyNodePosition, topologylayout.MaxPositions+1)
	for i := range tooMany {
		tooMany[i] = contract.TopologyNodePosition{ID: strings.Repeat("a", 8) + string(rune('0'+i%10)) + string(rune('0'+i/10%10)) + string(rune('0'+i/100%10)), X: 0, Y: 0}
	}
	cases := map[string][]contract.TopologyNodePosition{
		"과다":     tooMany,
		"빈 ID":   {{ID: "", X: 0, Y: 0}},
		"긴 ID":   {{ID: strings.Repeat("x", topologylayout.MaxIDLen+1), X: 0, Y: 0}},
		"중복 ID":  {{ID: "a", X: 0, Y: 0}, {ID: "a", X: 1, Y: 1}},
		"NaN":    {{ID: "a", X: math.NaN(), Y: 0}},
		"Inf":    {{ID: "a", X: 0, Y: math.Inf(1)}},
		"범위 밖 X": {{ID: "a", X: 2_000_000, Y: 0}},
	}
	for name, positions := range cases {
		if _, err := s.Put(ctx, "lnode", positions); !errors.Is(err, topologylayout.ErrInvalid) {
			t.Fatalf("%s: ErrInvalid 기대, got %v", name, err)
		}
	}
	if l, _ := s.Get(ctx, "lnode"); l != nil {
		t.Fatalf("거절된 Put이 저장되었습니다: %+v", l)
	}
}
