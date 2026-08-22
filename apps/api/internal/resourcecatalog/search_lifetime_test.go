package resourcecatalog

// 스냅숏 수명·회계 프로토콜 검증입니다. (P1-C)
//
// 세대(generation) + 서비스 단위 RWMutex 하나로 다음 다섯 가지를 증명합니다.
//
//   - 멈춤은 진행 중인 빌드를 무효화한다.
//   - 멈춘 뒤에는 어떤 빌드도 다시 게시되지 않는다.
//   - 보유 바이트는 **게시되어 살아 있는 인덱스의 합과 정확히 같다.**
//   - 멈추면 informer가 이미 없었더라도 인덱스와 회계가 함께 사라진다.
//   - **스냅숏을 쥔 요청이 살아 있는 동안에는 게시가 그 세대를 교체하지 못한다.**
//     이것이 없으면 옛 세대가 회계 밖에서 무한정 살아남을 수 있습니다.
//
// 이 파일의 테스트는 클라이언트를 만들지 않습니다 — 프로토콜 자체가 검증 대상이라
// fake 클러스터를 끼우면 오히려 경합이 가려집니다.

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

// lifetimeService는 수명·회계 프로토콜 검증용 서비스 한 벌입니다.
//
// 예산 상한을 반드시 채워야 합니다. 0으로 두면 게시가 **예산 거절**로 바뀌어
// 검색 절반이 조용히 빠지고, 이 파일의 회계 단언이 전부 엉뚱한 것을 재게 됩니다.
// 프로덕션 서비스는 New가 검증한 설정으로만 만들어지므로, 여기서도 같은 최소값을
// 갖춰 두는 것이 실제 경로와 어긋나지 않는 방법입니다.
func lifetimeService() *Service {
	s := &Service{cfg: Config{
		SearchEnabled:       true,
		MaxSearchIndexBytes: DefaultMaxSearchIndexBytes,
	}}
	s.budget.max.Store(DefaultMaxSearchIndexBytes)
	return s
}

func lifetimeBuild(t *testing.T, names ...string) (*indexSnapshot, searchBuildResult) {
	t.Helper()
	rows := make([]any, 0, len(names))
	for i, name := range names {
		rows = append(rows, metaRow("payments", name, fmt.Sprintf("uid-%d", i), nil))
	}
	index := buildIndexSnapshot(rows, indexBase)
	result := buildSearchSnapshot(index, "Service", true, hugeBudget, hugeBudget)
	if result.snapshot == nil {
		t.Fatalf("빌드 실패: %s", result.state)
	}
	return index, result
}

// entrySearchBytes는 지금 게시되어 있는 인덱스의 보유 바이트입니다.
func entrySearchBytes(s *Service, e *resourceEntry) int64 {
	var bytes int64
	e.read(s, func(es *entrySnapshot) { bytes = es.searchBytesOf() })
	return bytes
}

func publishedSnapshot(s *Service, e *resourceEntry) *entrySnapshot {
	var got *entrySnapshot
	e.read(s, func(es *entrySnapshot) { got = es })
	return got
}

func TestPublishAndDiscardKeepAccountingExact(t *testing.T) {
	s := lifetimeService()
	e := &resourceEntry{}
	index, first := lifetimeBuild(t, "payments-a")

	_, token, _ := e.beginBuild(s)
	if !e.publish(s, token, index, first) {
		t.Fatal("첫 게시가 거절되었습니다")
	}
	if got := s.searchBytes.Load(); got != first.snapshot.bytes {
		t.Fatalf("게시 후 보유 바이트 %d != %d", got, first.snapshot.bytes)
	}
	if got := entrySearchBytes(s, e); got != first.snapshot.bytes {
		t.Fatalf("항목 보유 바이트 %d != %d", got, first.snapshot.bytes)
	}

	// 같은 신원의 재게시는 **차이만** 반영해야 합니다. 더하기만 하면 누수입니다.
	index2, second := lifetimeBuild(t, "payments-a", "payments-b", "payments-c")
	_, token, _ = e.beginBuild(s)
	if !e.publish(s, token, index2, second) {
		t.Fatal("재게시가 거절되었습니다")
	}
	if got := s.searchBytes.Load(); got != second.snapshot.bytes {
		t.Fatalf("재게시 후 보유 바이트 %d != %d (이전 세대가 남았습니다)", got, second.snapshot.bytes)
	}

	// 멈추면 인덱스와 회계가 함께 사라집니다.
	e.discard(s)
	if got := s.searchBytes.Load(); got != 0 {
		t.Fatalf("멈춘 뒤 보유 바이트가 %d 남았습니다", got)
	}
	if publishedSnapshot(s, e) != nil {
		t.Fatal("멈춘 뒤에도 스냅숏이 남았습니다")
	}
}

func TestDiscardClearsIndexEvenWithoutRunningInformer(t *testing.T) {
	// informer가 이미 없는 항목입니다. 예전에는 여기서 곧바로 돌아가면서
	// 게시된 인덱스와 그 바이트가 영원히 남았습니다.
	s := lifetimeService()
	e := &resourceEntry{}
	index, built := lifetimeBuild(t, "payments-a")
	_, token, _ := e.beginBuild(s)
	if !e.publish(s, token, index, built) {
		t.Fatal("게시가 거절되었습니다")
	}
	if stop, done := e.discard(s); stop != nil || done != nil {
		t.Fatal("돌고 있지 않은 informer에서 채널이 나왔습니다")
	}
	if publishedSnapshot(s, e) != nil || s.searchBytes.Load() != 0 {
		t.Fatalf("informer 없이 멈췄더니 인덱스가 남았습니다: bytes=%d", s.searchBytes.Load())
	}
}

func TestPublishIsRejectedAfterStopChangesGeneration(t *testing.T) {
	s := lifetimeService()
	e := &resourceEntry{}
	index, built := lifetimeBuild(t, "payments-a")

	// 빌드가 시작된 시점의 신원을 들고 있습니다.
	_, token, _ := e.beginBuild(s)
	// 빌드가 도는 사이에 항목이 멈췄습니다.
	e.discard(s)

	if e.publish(s, token, index, built) {
		t.Fatal("멈춘 항목에 낡은 신원의 빌드가 게시되었습니다")
	}
	if publishedSnapshot(s, e) != nil {
		t.Fatal("게시가 거절되었는데 스냅숏이 붙었습니다")
	}
	if got := s.searchBytes.Load(); got != 0 {
		t.Fatalf("거절된 게시가 보유 바이트를 %d 늘렸습니다", got)
	}

	// 다시 시작하면 새 신원으로 정상 게시됩니다.
	_, next, _ := e.beginBuild(s)
	if next == token {
		t.Fatal("멈춤이 수명 신원을 바꾸지 않았습니다")
	}
	if !e.publish(s, next, index, built) {
		t.Fatal("새 신원의 게시가 거절되었습니다")
	}
	if s.searchBytes.Load() != built.snapshot.bytes {
		t.Fatal("재시작 후 회계가 어긋났습니다")
	}
}

// TestPublishRejectsInterleavedLifecycleAndGeneration — **P1-1의 강제 인터리빙입니다.**
//
// 예전에는 informer를 e.mu에서, 세대를 snapMu에서 **따로** 읽었습니다. 그 사이에
// discard가 끼어들면 (옛 informer + 새 세대) 조합이 잡히고, 멈춘 캐시로 만든 결과가
// 유효한 자격으로 게시됩니다. 지금은 신원을 한 잠금에서 함께 집고 게시가 두 값을
// 모두 확인하므로, 그런 조합을 **직접 만들어 넣어도** 거절되어야 합니다.
func TestPublishRejectsInterleavedLifecycleAndGeneration(t *testing.T) {
	s := lifetimeService()
	e := &resourceEntry{}
	index, built := lifetimeBuild(t, "payments-a")

	// informer 인스턴스 하나를 설치합니다(테스트는 채널·informer 값을 쓰지 않습니다).
	if !e.install(s, nil, nil, nil) {
		t.Fatal("설치가 거절되었습니다")
	}
	_, before, _ := e.beginBuild(s)
	e.discard(s)
	_, after, _ := e.beginBuild(s)

	if before.lifecycle == after.lifecycle {
		t.Fatal("멈춤이 인스턴스 번호를 올리지 않았습니다 — 사라진 인스턴스의 신원을 다시 주장할 수 있습니다")
	}
	if before.generation == after.generation {
		t.Fatal("멈춤이 정지 세대를 올리지 않았습니다")
	}

	for _, forged := range []buildToken{
		{lifecycle: before.lifecycle, generation: after.generation}, // 옛 informer + 새 세대
		{lifecycle: after.lifecycle, generation: before.generation}, // 새 informer + 옛 세대
		before,
	} {
		if e.publish(s, forged, index, built) {
			t.Fatalf("인터리빙 신원 %+v로 게시되었습니다", forged)
		}
	}
	if publishedSnapshot(s, e) != nil || s.searchBytes.Load() != 0 {
		t.Fatalf("거절된 게시가 상태를 남겼습니다: bytes=%d", s.searchBytes.Load())
	}

	// 지금의 신원으로는 정상 게시됩니다.
	if !e.publish(s, after, index, built) {
		t.Fatal("현재 신원의 게시가 거절되었습니다")
	}
}

// TestPublishWaitsForInFlightReader — **P1-C의 핵심입니다.**
//
// 요청이 스냅숏을 쥐고 있는 동안에는 게시가 그 세대를 교체하지 못해야 합니다.
// 교체할 수 있으면 옛 세대가 회계 밖에서 요청 수만큼 쌓일 수 있고, 그러면
// "살아 있는 세대는 둘뿐"이라는 정점 모형이 성립하지 않습니다.
func TestPublishWaitsForInFlightReader(t *testing.T) {
	s := lifetimeService()
	e := &resourceEntry{}
	index, first := lifetimeBuild(t, "payments-a")
	_, token, _ := e.beginBuild(s)
	if !e.publish(s, token, index, first) {
		t.Fatal("첫 게시가 거절되었습니다")
	}

	entered := make(chan struct{})
	release := make(chan struct{})
	observed := make(chan *entrySnapshot, 1)
	go func() {
		e.read(s, func(es *entrySnapshot) {
			close(entered)
			<-release
			observed <- es
		})
	}()
	<-entered

	index2, second := lifetimeBuild(t, "payments-a", "payments-b")
	published := make(chan bool, 1)
	go func() {
		_, next, _ := e.beginBuild(s)
		published <- e.publish(s, next, index2, second)
	}()

	select {
	case <-published:
		t.Fatal("독자가 스냅숏을 쥐고 있는데 게시가 끼어들었습니다")
	case <-time.After(50 * time.Millisecond):
	}

	close(release)
	if got := <-observed; got.searchBytesOf() != first.snapshot.bytes {
		t.Fatal("독자가 보던 세대가 도중에 바뀌었습니다")
	}
	if !<-published {
		t.Fatal("독자가 놓아준 뒤에도 게시가 거절되었습니다")
	}
	if got := s.searchBytes.Load(); got != second.snapshot.bytes {
		t.Fatalf("게시 후 보유 바이트 %d != %d", got, second.snapshot.bytes)
	}
}

// TestConcurrentPublishAndDiscardKeepAccountingExact — race 검출기와 함께 도는 것이 목적입니다.
// 어떤 순서로 끝나든 보유 바이트는 **지금 게시되어 있는 것**과 정확히 같아야 합니다.
func TestConcurrentPublishAndDiscardKeepAccountingExact(t *testing.T) {
	s := lifetimeService()
	e := &resourceEntry{}
	index, built := lifetimeBuild(t, "payments-a", "payments-b")

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				_, token, _ := e.beginBuild(s)
				e.publish(s, token, index, built)
			}
		}()
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				e.discard(s)
			}
		}()
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				// 독자는 게시·폐기와 함께 돌면서도 반쪽 상태를 보면 안 됩니다.
				e.read(s, func(es *entrySnapshot) {
					if es != nil && es.index == nil {
						t.Error("index 없는 스냅숏이 게시되었습니다")
					}
				})
			}
		}()
	}
	wg.Wait()

	if got, want := s.searchBytes.Load(), entrySearchBytes(s, e); got != want {
		t.Fatalf("보유 바이트 %d가 게시된 인덱스 %d와 다릅니다", got, want)
	}
	// 마지막으로 한 번 더 멈추면 언제나 0으로 수렴해야 합니다.
	e.discard(s)
	if got := s.searchBytes.Load(); got != 0 {
		t.Fatalf("모두 멈춘 뒤 보유 바이트가 %d 남았습니다", got)
	}
}

// TestUnavailableEntryIsNotResurrectedAsReady — 예산 초과로 색인하지 못한 항목의 수명입니다.
//
// 세 가지를 함께 봅니다.
//   - unavailable도 **게시된 한 벌**입니다. 스냅숏 자체는 존재하고 검색 인덱스만 없습니다.
//   - 재구성 루프가 보는 신호(published)가 참이어야 dirty가 아닐 때 다시 만들지 않습니다.
//   - 같은 결과를 다시 게시해도 상태와 회계가 그대로여야 합니다 — ready로 되살아나면 안 됩니다.
func TestUnavailableEntryIsNotResurrectedAsReady(t *testing.T) {
	s := lifetimeService()
	e := &resourceEntry{}
	index, _ := lifetimeBuild(t, "payments-a")
	unavailable := searchBuildResult{state: SearchUnavailable, reason: reasonBudget}

	_, token, published := e.beginBuild(s)
	if published {
		t.Fatal("아직 게시된 것이 없는데 published입니다")
	}
	if !e.publish(s, token, index, unavailable) {
		t.Fatal("게시가 거절되었습니다")
	}

	got := publishedSnapshot(s, e)
	if got == nil || got.searchState != SearchUnavailable || got.search != nil {
		t.Fatalf("unavailable 상태가 보존되지 않았습니다: %+v", got)
	}
	if got.searchReason == "" {
		t.Fatal("제외 사유가 비어 있습니다 — 계측·응답이 이유를 말할 수 없습니다")
	}
	if s.searchBytes.Load() != 0 {
		t.Fatalf("색인이 없는데 %d바이트를 붙잡고 있습니다", s.searchBytes.Load())
	}

	// 재구성 루프는 이 신호로 "이미 게시됨"을 판단합니다. 거짓이면 매 tick마다 다시 빌드합니다.
	if _, _, published = e.beginBuild(s); !published {
		t.Fatal("unavailable 항목이 미게시로 보입니다")
	}

	// dirty 전이가 있어 다시 만들더라도, 같은 예산이면 결과는 여전히 unavailable입니다.
	_, token, _ = e.beginBuild(s)
	if !e.publish(s, token, index, unavailable) {
		t.Fatal("재게시가 거절되었습니다")
	}
	got = publishedSnapshot(s, e)
	if got.searchState != SearchUnavailable || got.search != nil {
		t.Fatalf("재구성이 unavailable을 ready로 되살렸습니다: state=%s", got.searchState)
	}
	if s.searchBytes.Load() != 0 {
		t.Fatalf("재구성 뒤 보유 바이트가 %d 남았습니다", s.searchBytes.Load())
	}
}

func TestRepeatedRebuildDoesNotAccumulateBytes(t *testing.T) {
	s := lifetimeService()
	e := &resourceEntry{}
	index, built := lifetimeBuild(t, "payments-a", "payments-b", "payments-c")
	for i := 0; i < 50; i++ {
		_, token, _ := e.beginBuild(s)
		if !e.publish(s, token, index, built) {
			t.Fatalf("%d번째 게시가 거절되었습니다", i)
		}
	}
	if got := s.searchBytes.Load(); got != built.snapshot.bytes {
		t.Fatalf("50회 재구성 후 보유 바이트 %d != %d — 세대가 누적되었습니다", got, built.snapshot.bytes)
	}
}
