package resourcecatalog

// 증분 델타 경로 — Round 6 v4 / v4.1
// --------------------------------------------------------------------------
// informer 콜백은 **키만** 큐에 넣습니다. 실제 상태는 적용 직전에 같은 세대의
// informer Store.GetByKey(캐시 전용, Kubernetes 요청 아님)로 다시 읽습니다.
// 그래서 지연된 옛 콜백이 부트스트랩·목록 스냅숏을 되돌릴 수 없고, tombstone·
// UID 교체·이름 변경이 전부 한 번의 해석으로 정리됩니다.
//
// 잠금 순서는 **snapMu → queueMu**이고 역순은 없습니다. flush는 둘을 겹쳐 잡지
// 않습니다(§5.4). queueMu는 큐와 stale 마커의 유일한 소유자입니다.
//
// 예산은 세 불변식으로 나뉩니다.
//
//	I-A  서비스 보유       <= Max
//	I-B  GVR별 보유        <= Max/2
//	I-C  보유+대기+진행+회수 <= 3*Max
//
// I-C는 원자 CAS 하나로 예약하고, 종단 경로에서 **정확히 한 번** 해제합니다.

import (
	"context"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/tools/cache"
)

const (
	// DeltaTickInterval은 watch 이벤트 **합치기 창**입니다. 전체 재구성 SLA가 아닙니다.
	DeltaTickInterval = 100 * time.Millisecond

	// maxPendingPerResource는 GVR 하나가 큐에 담을 수 있는 키 수입니다.
	maxPendingPerResource = 8192
	// maxBatchEvents는 한 tick이 전체에서 빼내는 키 수입니다.
	maxBatchEvents = 4096
	// maxBatchPerResource는 한 tick이 GVR 하나에서 빼내는 키 수입니다(굶김 방지).
	maxBatchPerResource = 512
	// maxStaleTracked는 회수 대상 namespace 추적 상한입니다. 넘치면 GVR 비트로 승급합니다.
	maxStaleTracked = 1024
	// maxHoldQueue는 회수 중인 파티션을 목표로 하는 보류 키 상한입니다.
	maxHoldQueue = 2048

	// recoveryChunkRows / recoverySliceBudget은 회수 한 조각의 상한입니다.
	recoveryChunkRows   = 4096
	recoverySliceBudget = 8 * time.Millisecond
	// recoveryCooldown / recoveryBackoffMin / recoveryBackoffMax는 폭풍 방지 장치입니다.
	recoveryCooldown   = 5 * time.Second
	recoveryBackoffMin = time.Second
	recoveryBackoffMax = 60 * time.Second

	// deltaEventFixedBytes는 큐 항목 하나의 고정 비용입니다.
	deltaEventFixedBytes = 112
)

/* ── 고정 용량 문자열 집합 ────────────────────────────────────────────────── */

// strSet은 고정 슬롯 개방 주소 집합입니다. Go map을 상한 근거로 쓰지 않기 위한 장치입니다.
//
// marks는 슬롯마다 하나씩 붙는 **마커 저장소**입니다. 별도의 Go map으로 두면
// 마커 저장소만 입력에 따라 자라고, 그 성장은 어떤 예약에도 잡히지 않습니다.
// 슬롯과 같은 고정 용량으로 묶어 두면 마커는 구조 고정 비용의 일부가 되고,
// 남는 동적 부분은 **담긴 문자열의 바이트뿐**입니다(dynBytes).
type strSet struct {
	slots []string
	used  []bool
	marks []uint64
	count int
	limit int
	// dyn은 지금 담고 있는 문자열 바이트의 합입니다. 슬롯 헤더는 고정 비용이지만
	// 문자열 본문은 이 집합이 **붙잡고 있는 동적 메모리**이므로 따로 셉니다.
	dyn int64
	// scratch는 remove/values가 쓰는 **재사용 버퍼**입니다.
	//
	// 매번 새로 잡으면 그 몫이 어떤 예약에도 잡히지 않습니다(최대 1024개 헤더).
	// 생성 시 한 번 잡아 고정 비용에 포함시키고, 이후에는 길이만 0으로 되돌려
	// 다시 씁니다. **delta.mu 아래에서만** 쓰이므로 재사용이 안전합니다.
	scratch []string
	// markScratch는 remove가 마커를 함께 옮길 때 쓰는 짝 버퍼입니다.
	markScratch []uint64
}

func strSetSlots(limit int) int {
	slots := 16
	for slots < limit*2 {
		slots <<= 1
	}
	return slots
}

// strSetFixedBytesFor는 **할당하기 전에** 알 수 있는 고정 비용입니다.
// queueFor가 예약을 먼저 받고 그 뒤에 할당하기 위해 필요합니다.
func strSetFixedBytesFor(limit int) int64 {
	slots := int64(strSetSlots(limit))
	return slots*(stringHeaderBytes+1+8) + // slots + used + marks
		int64(limit)*(stringHeaderBytes+8) + // scratch + markScratch
		48
}

func newStrSet(limit int) *strSet {
	slots := strSetSlots(limit)
	return &strSet{
		slots:       make([]string, slots),
		used:        make([]bool, slots),
		marks:       make([]uint64, slots),
		scratch:     make([]string, 0, limit),
		markScratch: make([]uint64, 0, limit),
		limit:       limit,
	}
}

// fixedBytes는 이 집합의 **고정 구조** 비용입니다(문자열 본문 제외).
func (s *strSet) fixedBytes() int64 { return strSetFixedBytesFor(s.limit) }

// at은 k번째(사용 중인 슬롯 순서) 값입니다. **할당하지 않습니다.**
//
// 회수 선택이 values()로 전체를 정렬해 첫 항목만 쓰면, 고를 때마다 1024개
// 헤더를 새로 잡습니다. 슬롯 순서는 집합이 같으면 같으므로 회전에 충분합니다.
func (s *strSet) at(k int) (string, bool) {
	if s.count == 0 {
		return "", false
	}
	k %= s.count
	for i, used := range s.used {
		if !used {
			continue
		}
		if k == 0 {
			return s.slots[i], true
		}
		k--
	}
	return "", false
}

// dynBytes는 지금 붙잡고 있는 문자열 본문 바이트입니다.
func (s *strSet) dynBytes() int64 { return s.dyn }

// setMark는 값의 마커를 올립니다(단조). 값이 없으면 아무 일도 하지 않습니다.
func (s *strSet) setMark(v string, seq uint64) {
	mask := uint32(len(s.slots) - 1)
	h := strSetHash(v) & mask
	for probe := uint32(0); probe <= mask; probe++ {
		at := (h + probe) & mask
		if !s.used[at] {
			return
		}
		if s.slots[at] == v {
			if seq > s.marks[at] {
				s.marks[at] = seq
			}
			return
		}
	}
}

// mark는 값의 마커입니다. 없으면 0입니다.
func (s *strSet) mark(v string) uint64 {
	mask := uint32(len(s.slots) - 1)
	h := strSetHash(v) & mask
	for probe := uint32(0); probe <= mask; probe++ {
		at := (h + probe) & mask
		if !s.used[at] {
			return 0
		}
		if s.slots[at] == v {
			return s.marks[at]
		}
	}
	return 0
}

func strSetHash(s string) uint32 {
	h := uint32(2166136261)
	for i := 0; i < len(s); i++ {
		h ^= uint32(s[i])
		h *= 16777619
	}
	return h
}

// add는 값을 넣습니다. 상한을 넘으면 false이며 호출자가 승급해야 합니다.
func (s *strSet) add(v string) bool {
	if s.count >= s.limit {
		return s.has(v)
	}
	mask := uint32(len(s.slots) - 1)
	h := strSetHash(v) & mask
	for probe := uint32(0); probe <= mask; probe++ {
		at := (h + probe) & mask
		if !s.used[at] {
			s.used[at], s.slots[at], s.marks[at] = true, v, 0
			s.count++
			s.dyn += int64(len(v))
			return true
		}
		if s.slots[at] == v {
			return true
		}
	}
	return false
}

func (s *strSet) has(v string) bool {
	mask := uint32(len(s.slots) - 1)
	h := strSetHash(v) & mask
	for probe := uint32(0); probe <= mask; probe++ {
		at := (h + probe) & mask
		if !s.used[at] {
			return false
		}
		if s.slots[at] == v {
			return true
		}
	}
	return false
}

// remove는 값 하나를 지웁니다.
//
// 선형 탐사 집합은 자리에 구멍을 내면 뒤따르는 탐사가 끊기므로, 남길 값만 모아
// 다시 채웁니다. 상한이 1024로 묶여 있어 비용이 유계입니다.
func (s *strSet) remove(v string) {
	if !s.has(v) {
		return
	}
	// **재사용 버퍼**를 씁니다. 매번 새로 잡으면 그 몫이 예약 밖입니다.
	s.scratch = s.scratch[:0]
	s.markScratch = s.markScratch[:0]
	for i, ok := range s.used {
		if ok && s.slots[i] != v {
			s.scratch = append(s.scratch, s.slots[i])
			s.markScratch = append(s.markScratch, s.marks[i])
		}
	}
	kept := s.scratch
	keptMarks := s.markScratch
	s.reset()
	for i, k := range kept {
		s.add(k)
		s.setMark(k, keptMarks[i])
	}
	s.scratch = s.scratch[:0]
	s.markScratch = s.markScratch[:0]
}

func (s *strSet) values() []string {
	out := make([]string, 0, s.count)
	for i, ok := range s.used {
		if ok {
			out = append(out, s.slots[i])
		}
	}
	sort.Strings(out)
	return out
}

func (s *strSet) reset() {
	for i := range s.used {
		s.used[i] = false
		s.slots[i] = ""
		s.marks[i] = 0
	}
	s.count = 0
	s.dyn = 0
}

func (s *strSet) bytes() int64 { return s.fixedBytes() + s.dyn }

/* ── 예약 회계 ──────────────────────────────────────────────────────────── */

// searchBudget은 세 불변식(I-A/I-B/I-C)의 **유일한 소유권 원장**입니다.
//
// 모든 변경은 mu 아래에서 일어납니다. "검사한 뒤 더한다"를 두 단계로 나누면 그
// 사이에 다른 게시가 끼어들어 상한을 넘긴 채로 둘 다 통과합니다. 그래서 승인은
// **검사와 적립이 한 임계 구역**입니다. 값 자체는 atomic이라 계측·판정 읽기는
// 잠금 없이 됩니다.
//
// 계상 분해:
//
//	retained  게시된 세대 + 아직 독자가 붙잡은 은퇴 세대
//	queued    대기 키 + 키 색인/stale 집합 등 큐 부속 + 보류(held)
//	inflight  드레인된 키 + 적용 임시(정렬·COW 경로 사본)
//	recovery  회수가 연장하는 원본 수명 + 측면 빌더 + 조각 scratch
type searchBudget struct {
	mu sync.Mutex

	max      atomic.Int64
	retained atomic.Int64
	queued   atomic.Int64
	inflight atomic.Int64
	recovery atomic.Int64
	live     atomic.Int64

	// rejected는 예약이 거절된 횟수입니다(계측).
	rejected atomic.Int64
}

func (b *searchBudget) limit() int64     { return b.max.Load() }
func (b *searchBudget) peakLimit() int64 { return searchPeakMultiplier * b.max.Load() }

// reserveLive는 I-C 상한 안에서 바이트를 예약합니다. 실패하면 아무것도 늘리지 않습니다.
func (b *searchBudget) reserveLive(n int64) bool {
	if n <= 0 {
		return true
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.reserveLiveLocked(n)
}

func (b *searchBudget) reserveLiveLocked(n int64) bool {
	if n <= 0 {
		return true
	}
	if b.live.Load()+n > b.peakLimit() {
		b.rejected.Add(1)
		return false
	}
	b.live.Add(n)
	return true
}

func (b *searchBudget) releaseLive(n int64) {
	if n <= 0 {
		return
	}
	b.mu.Lock()
	b.live.Add(-n)
	b.mu.Unlock()
}

// admitRetained는 게시 한 번의 **소유권 승인**입니다.
//
// 세 불변식을 한 임계 구역에서 함께 봅니다.
//
//	I-A  서비스 보유 합       + delta <= max
//	I-B  이 GVR의 보유        + delta <= max/2
//	I-C  살아 있는 전체       + delta <= 3*max
//
// 통과하면 그 자리에서 retained·live에 적립합니다. 검사와 적립 사이에 다른
// 게시가 끼어들 틈이 없습니다. delta가 0 이하면 언제나 통과하고 적립만 합니다.
func (b *searchBudget) admitRetained(delta, entryRetained, perGVR int64) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if delta > 0 {
		if b.retained.Load()+delta > b.limit() ||
			entryRetained+delta > perGVR ||
			b.live.Load()+delta > b.peakLimit() {
			b.rejected.Add(1)
			return false
		}
	}
	b.retained.Add(delta)
	b.live.Add(delta)
	return true
}

// availableRetained는 지금 이 GVR이 **실제로 더 받을 수 있는** 보유 바이트입니다.
//
// 세 불변식의 여유 중 최솟값입니다. 예산 거절 지문이 설정 상한이 아니라 이 값을
// 적어야, 설정은 그대로인 채 다른 GVR이 놓아서 자리가 생긴 경우를 재시도 조건으로
// 잡을 수 있습니다. 같은 임계 구역에서 세 값을 함께 읽어 서로 어긋나지 않습니다.
func (b *searchBudget) availableRetained(perGVR, entryRetained int64) int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	avail := b.limit() - b.retained.Load()
	if v := perGVR - entryRetained; v < avail {
		avail = v
	}
	if v := b.peakLimit() - b.live.Load(); v < avail {
		avail = v
	}
	if avail < 0 {
		return 0
	}
	return avail
}

// releaseRetained는 보유 바이트를 되돌립니다(은퇴 세대의 마지막 독자가 놓을 때).
func (b *searchBudget) releaseRetained(n int64) {
	if n <= 0 {
		return
	}
	b.mu.Lock()
	b.retained.Add(-n)
	b.live.Add(-n)
	b.mu.Unlock()
}

// reserveQueued/releaseQueued는 큐 대기 바이트입니다(I-C 안에서만 움직입니다).
// 대기 키뿐 아니라 키 색인·stale 집합·보류(held) 같은 큐 부속도 이 항에 듭니다.
func (b *searchBudget) reserveQueued(n int64) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.reserveLiveLocked(n) {
		return false
	}
	b.queued.Add(n)
	return true
}

// reserveRecovery는 회수 티켓이 붙잡을 바이트를 I-C 안에서 예약합니다.
// 실패는 **예산 거절**이며 버전 충돌과 다르게 처리됩니다.
func (b *searchBudget) reserveRecovery(n int64) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.reserveLiveLocked(n) {
		return false
	}
	b.recovery.Add(n)
	return true
}

// releaseRecovery는 회수 예약을 되돌립니다.
func (b *searchBudget) releaseRecovery(n int64) {
	if n <= 0 {
		return
	}
	b.mu.Lock()
	b.recovery.Add(-n)
	b.live.Add(-n)
	b.mu.Unlock()
}

// reserveTransient는 델타 적용이 쓰는 임시 바이트를 예약합니다.
// 게시·되돌림 어느 쪽이든 정확히 한 번 해제합니다.
func (b *searchBudget) reserveTransient(n int64) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.reserveLiveLocked(n) {
		return false
	}
	b.inflight.Add(n)
	return true
}

func (b *searchBudget) releaseTransient(n int64) {
	if n <= 0 {
		return
	}
	b.mu.Lock()
	b.inflight.Add(-n)
	b.live.Add(-n)
	b.mu.Unlock()
}

// transferQueuedToInflight는 drain이 예약 소유권을 옮기는 지점입니다. 해제가 아닙니다.
func (b *searchBudget) transferQueuedToInflight(n int64) {
	if n <= 0 {
		return
	}
	b.mu.Lock()
	b.queued.Add(-n)
	b.inflight.Add(n)
	b.mu.Unlock()
}

func (b *searchBudget) transferInflightToQueued(n int64) {
	if n <= 0 {
		return
	}
	b.mu.Lock()
	b.inflight.Add(-n)
	b.queued.Add(n)
	b.mu.Unlock()
}

// releaseInflight는 종단 경로(게시·폐기·밀려남·requeue 실패)에서 정확히 한 번입니다.
func (b *searchBudget) releaseInflight(n int64) {
	if n <= 0 {
		return
	}
	b.mu.Lock()
	b.inflight.Add(-n)
	b.live.Add(-n)
	b.mu.Unlock()
}

func (b *searchBudget) releaseQueued(n int64) {
	if b == nil || n <= 0 {
		return
	}
	b.mu.Lock()
	b.queued.Add(-n)
	b.live.Add(-n)
	b.mu.Unlock()
}

// admitStructural은 큐·회로 **구조 메모리를 할당하기 전에** 승인받습니다.
//
// 예전에는 이 몫을 "거절할 수 없는 계상"으로 그냥 실었습니다. 그러면 live가
// peakLimit을 **넘은 채로** 통과하고, 그 순간 I-C는 상한이 아니게 됩니다.
// 구조라고 예외를 두면 상한이 상한이 아닙니다 — 통과하지 못하면 **구조를 만들지
// 않습니다.** 호출자는 그 자원을 증분 경로에서 제외하고 전체 재구성으로 되돌립니다.
func (b *searchBudget) admitStructural(n int64) bool {
	if b == nil {
		return false
	}
	if n <= 0 {
		return true
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.live.Load()+n > b.peakLimit() {
		b.rejected.Add(1)
		return false
	}
	b.queued.Add(n)
	b.live.Add(n)
	return true
}

/* ── 큐 ─────────────────────────────────────────────────────────────────── */

// deltaEvent는 큐에 담긴 **키 하나**입니다. 라벨 페이로드를 담지 않습니다.
type deltaEvent struct {
	namespace string
	name      string
	seq       uint64
	gen       uint64
	reserved  int64
}

func (e deltaEvent) key() string { return e.namespace + "\x00" + e.name }

// deltaEventBytes는 큐 항목 하나가 **실제로 붙잡는 전부**입니다.
//
// 이벤트 본체 + 복사된 namespace/name + 키 색인 항목 + 색인 키 문자열까지 셉니다.
// 색인 몫을 예약에서 빼면 pendingBytes()가 budget.queued를 넘습니다 — 원장이
// 실제보다 적게 말하는 순간 I-C는 상한이 아니게 됩니다.
func deltaEventBytes(namespace, name string) int64 {
	body := int64(len(namespace) + len(name))
	return deltaEventFixedBytes + body + // 이벤트 본체 + 복사된 문자열
		deltaIndexEntryBytes + body + 1 // 색인 항목 + 색인 키("ns\x00name") 사본
}

// deltaQueue는 GVR 하나의 대기 키입니다. **queueMu가 유일한 소유자**입니다.
type deltaQueue struct {
	events []deltaEvent
	// index는 key → events 위치입니다. 같은 키를 선형 탐색하면 큐가 가득 찼을 때
	// 이벤트마다 8천 번을 비교하게 됩니다. **항목 수가 maxPendingPerResource로
	// 강제되어 있으므로** 이 맵의 크기도 그 상한 안에 묶여 있고, 회계는
	// deltaIndexEntryBytes로 보수적으로 계상합니다.
	index map[string]int
	// staleNS는 드롭 때문에 회수가 필요한 namespace입니다. 상한을 넘으면 gvrStale로 승급합니다.
	staleNS *strSet
	// staleEpoch는 마커 세대입니다. 회수는 **캡처한 epoch만** ack합니다.
	staleEpoch uint64
	gvrStale   bool
	// markerSeq는 가장 최근 드롭의 eventSeq입니다. 회수 게이트가 이 값을 봅니다.
	//
	// **namespace별** 마커는 staleNS 슬롯에 함께 삽니다(strSet.marks). 별도의 Go map을
	// 두면 마커 저장소만 입력에 따라 자라고 그 성장은 어떤 예약에도 잡히지 않습니다.
	// 슬롯과 같은 고정 용량으로 묶으면 마커는 구조 고정 비용의 일부가 됩니다.
	markerSeq uint64
	// hold는 회수 중인 파티션을 목표로 하는 보류 키입니다.
	hold   []deltaEvent
	holdNS string
	// dropped는 예산·상한으로 버린 이벤트 수입니다(계측).
	dropped int64
	// batches/batchNanos는 적용 배치 계측입니다.
	batches    int64
	batchNanos int64
	// fixed는 큐를 만드는 순간 이미 잡힌 **고정** 구조 바이트입니다.
	// 이벤트가 하나도 없어도 살아 있는 몫이라 **할당보다 먼저** 승인받습니다.
	fixed int64
	// dynamic은 staleNS가 붙잡고 있는 **namespace 문자열 본문** 바이트입니다.
	// 고정 슬롯과 달리 입력에 따라 자라므로 자랄 때마다 승인받습니다.
	dynamic int64
	// capCharged는 **도달 가능한 저장 용량**에 대한 계상입니다.
	//
	// len(events)만 세면 큐를 채웠다 비운 뒤에도 cap(events) 배열과 index 버킷이
	// 그대로 살아 있는데 원장은 0이라고 말합니다. 그래서 용량이 자랄 때 승인하고,
	// 압축할 때만 되돌립니다. index 맵은 줄지 않으므로 **고수위**로 셉니다.
	capCharged int64
	// eventCap/holdCap/indexCharged는 지금 계상된 용량입니다(고수위).
	eventCap     int
	holdCap      int
	indexCharged int

	// fallback은 이 GVR **전용**의 내장 회로입니다.
	//
	// 동적 회로 맵조차 승인받지 못하는 상황에서도, 아직 낡은 대상에게는 **지속되는
	// 백오프**가 있어야 합니다. 없으면 회로가 nil이라 allows가 언제나 참이 되고,
	// 쿨다운마다 같은 회수를 다시 시도하는 폭풍이 됩니다.
	//
	// 큐 구조체에 **값으로 내장**되어 있으므로 deltaQueueStructBytes에 이미
	// 계상되어 있고(선계상), GVR마다 하나씩이라 **가변 상태를 공유하지 않습니다.**
	// 의미는 GVR 전체 회로입니다 — 대상별 지문을 흉내 내지 않습니다.
	fallback recoveryCircuit
}

// newDeltaQueue는 **이미 승인된** 고정 몫으로 큐를 만듭니다.
// 호출자가 deltaQueueFixedBytes를 먼저 승인받았다는 것이 전제입니다.
func newDeltaQueue() *deltaQueue {
	q := &deltaQueue{
		staleNS:      newStrSet(maxStaleTracked),
		index:        make(map[string]int, deltaIndexSeedEntries),
		fixed:        deltaQueueFixedBytes,
		indexCharged: deltaIndexSeedEntries,
	}
	q.fallback.backoff = recoveryBackoffMin
	return q
}

const (
	// deltaIndexEntryBytes는 키 색인 한 항목의 보수적 비용입니다(맵 오버헤드 포함).
	deltaIndexEntryBytes = 96
	// deltaIndexSeedEntries는 newDeltaQueue가 미리 잡는 맵 용량입니다.
	deltaIndexSeedEntries = 64
	// deltaQueueStructBytes는 deltaQueue 본체 + 슬라이스/맵 헤더 + **내장 fallback
	// 회로**의 보수적 비용입니다. fallback을 값으로 품고 있으므로 큐를 승인하는
	// 순간 그 몫도 함께 선계상됩니다(별도 승인 경로가 필요 없습니다).
	deltaQueueStructBytes = 768
	// recoveryCircuitBytes는 회로 항목 하나(키 + 값 + 맵 오버헤드)의 보수적 비용입니다.
	recoveryCircuitBytes = 256
	// circuitsMapSeedBytes는 회로 맵 자체의 씨앗 버킷 비용입니다.
	circuitsMapSeedBytes = 16 * recoveryCircuitBytes
	// recoveryTicketBytes는 회수 티켓 구조체 하나의 보수적 비용입니다.
	recoveryTicketBytes = 512
	// deltaEventStructBytes는 events/hold 슬라이스 항목 하나의 크기입니다.
	deltaEventStructBytes = deltaEventFixedBytes
	// deltaQueueSeedStorageBytes는 압축이 새로 잡는 씨앗 저장소입니다.
	deltaQueueSeedStorageBytes = deltaIndexSeedEntries * deltaIndexEntryBytes
	// deltaStaleScratchBytes는 배치마다 낡음 목록을 담는 버퍼입니다.
	deltaStaleScratchBytes = maxStaleTracked*stringHeaderBytes + 48
)

// deltaQueueFixedBytes는 큐 하나의 고정 구조 비용입니다.
//
// **상수입니다.** 할당하기 전에 알아야 승인을 먼저 받을 수 있습니다.
// 구조체 + staleNS(슬롯·사용비트·마커·재사용 버퍼) + 색인 씨앗 버킷.
const deltaQueueFixedBytes = deltaQueueStructBytes +
	(2048*(stringHeaderBytes+1+8) + maxStaleTracked*(stringHeaderBytes+8) + 48) +
	deltaIndexSeedEntries*deltaIndexEntryBytes

// pendingBytes는 이 큐가 붙잡고 있는 **queued 항의 전부**입니다.
//
// 고정 구조 + 대기 이벤트 예약 + 보류(held) 예약입니다. 색인 몫은 이벤트 예약에
// 이미 들어 있으므로 여기서 다시 세지 않습니다 — 두 번 세면 원장이 실제보다
// 많다고 말하고, 안 세면 적다고 말합니다. 둘 다 상한을 무너뜨립니다.
func (q *deltaQueue) pendingBytes() int64 {
	total := q.fixed + q.dynamic + q.capCharged
	for _, e := range q.events {
		total += e.reserved
	}
	for _, e := range q.hold {
		total += e.reserved
	}
	return total
}

/* ── 도달 가능한 저장 용량의 소유권 ─────────────────────────────────────── */

// nextSliceCap은 need를 담을 다음 용량입니다(상한에서 잘립니다).
func nextSliceCap(cur, need, max int) int {
	c := cur
	if c == 0 {
		c = 8
	}
	for c < need {
		c *= 2
	}
	if c > max {
		c = max
	}
	if c < need {
		c = need
	}
	return c
}

// reserveEventSlotLocked는 이벤트 하나를 넣기 **전에** 용량을 확보합니다.
//
// append에 맡기면 배열이 먼저 커지고 회계는 나중에 따라옵니다. 여기서는
// **새 용량을 먼저 승인**하고, 배열을 바꾼 뒤에 옛 용량을 되돌립니다 —
// 겹치는 순간까지 원장이 실제를 덮습니다.
func (d *deltaState) reserveEventSlotLocked(q *deltaQueue) bool {
	return d.growEventsLocked(q, len(q.events)+1)
}

// growEventsLocked는 events 용량을 need 이상으로 키웁니다.
//
// **새 용량을 먼저 승인**하고 배열을 옮긴 뒤에 옛 용량을 되돌립니다 — 두 배열이
// 함께 사는 순간까지 원장이 실제를 덮습니다.
func (d *deltaState) growEventsLocked(q *deltaQueue, need int) bool {
	if need <= 0 {
		return true
	}
	if q.eventCap >= need && cap(q.events) >= need {
		return true
	}
	next := nextSliceCap(q.eventCap, need, maxPendingPerResource)
	if next < need {
		return false // 상한을 넘는 요구입니다.
	}
	newBytes := int64(next) * deltaEventStructBytes
	if !d.budget.admitStructural(newBytes) {
		return false
	}
	grown := make([]deltaEvent, len(q.events), next)
	copy(grown, q.events)
	oldBytes := int64(q.eventCap) * deltaEventStructBytes
	q.events = grown
	q.eventCap = next
	q.capCharged += newBytes - oldBytes
	d.budget.releaseQueued(oldBytes)
	return true
}

// reserveHoldSlotLocked는 보류 슬롯 하나를 같은 규칙으로 확보합니다.
func (d *deltaState) reserveHoldSlotLocked(q *deltaQueue) bool {
	need := len(q.hold) + 1
	if q.holdCap >= need && cap(q.hold) >= need {
		return true
	}
	next := nextSliceCap(q.holdCap, need, maxHoldQueue)
	newBytes := int64(next) * deltaEventStructBytes
	if !d.budget.admitStructural(newBytes) {
		return false
	}
	grown := make([]deltaEvent, len(q.hold), next)
	copy(grown, q.hold)
	oldBytes := int64(q.holdCap) * deltaEventStructBytes
	q.hold = grown
	q.holdCap = next
	q.capCharged += newBytes - oldBytes
	d.budget.releaseQueued(oldBytes)
	return true
}

// takeHoldLocked는 보류 배열을 큐에서 **분리**하고 그 용량 계상을 되돌립니다.
//
// 분리된 배열은 호출자가 settle까지 들고 있다가 버립니다 — 큐에는 더 이상
// 도달 가능한 보류 저장소가 없으므로 계상도 큐에 남으면 안 됩니다.
// 살아 있는 항목의 몫은 이벤트별 예약(deltaEventBytes >= deltaEventStructBytes)이
// 계속 덮으므로, 이 반환은 **쓰이지 않는 여유 용량만** 되돌립니다.
func (d *deltaState) takeHoldLocked(q *deltaQueue) []deltaEvent {
	if q == nil {
		return nil
	}
	held := q.hold
	q.hold = nil
	d.releaseHoldCapLocked(q)
	return held
}

// releaseHoldCapLocked는 보류 배열 용량 계상을 **정확히 한 번** 되돌립니다.
func (d *deltaState) releaseHoldCapLocked(q *deltaQueue) {
	if q == nil || q.holdCap == 0 {
		return
	}
	bytes := int64(q.holdCap) * deltaEventStructBytes
	q.holdCap = 0
	q.capCharged -= bytes
	d.budget.releaseQueued(bytes)
}

// reserveIndexSlotLocked는 색인 항목 하나가 **버킷을 늘리기 전에** 승인받습니다.
// Go 맵은 줄지 않으므로 계상은 고수위를 따릅니다.
func (d *deltaState) reserveIndexSlotLocked(q *deltaQueue) bool {
	return d.reserveIndexCapLocked(q, len(q.index)+1)
}

// reserveIndexCapLocked는 색인 고수위를 need까지 올립니다(부족분만 승인).
func (d *deltaState) reserveIndexCapLocked(q *deltaQueue, need int) bool {
	if need <= q.indexCharged {
		return true
	}
	delta := int64(need-q.indexCharged) * deltaIndexEntryBytes
	if !d.budget.admitStructural(delta) {
		return false
	}
	q.indexCharged = need
	q.capCharged += delta
	return true
}

// compactQueueLocked는 완전히 빈 큐의 **도달 가능한 저장 용량**을 되돌립니다.
//
// 비었는데도 cap(events) 배열과 커진 색인 버킷이 그대로 살아 있으면, 원장이
// 0이라고 말하는 동안 실제 메모리는 고수위에 머뭅니다. 압축은 새 씨앗 저장소를
// **먼저 승인**하고 옛 저장소를 그 뒤에 놓습니다(겹치는 구간까지 계상).
func (d *deltaState) compactQueueLocked(q *deltaQueue) {
	if q == nil || q.capCharged == 0 || len(q.events) != 0 || len(q.hold) != 0 {
		return
	}
	if !d.budget.admitStructural(deltaQueueSeedStorageBytes) {
		return // 지금은 압축할 여유가 없습니다. 다음 기회에 다시 봅니다.
	}
	q.events = nil
	q.hold = nil
	q.index = make(map[string]int, deltaIndexSeedEntries)
	freed := q.capCharged + deltaQueueSeedStorageBytes
	q.capCharged = 0
	q.eventCap, q.holdCap = 0, 0
	q.indexCharged = deltaIndexSeedEntries
	d.budget.releaseQueued(freed)
}

// syncStaleDynamicLocked는 staleNS가 붙잡은 **문자열 본문**을 원장과 맞춥니다.
// 줄어드는 쪽만 여기서 처리합니다 — 자라는 쪽은 addStaleLocked가 **담기 전에**
// 승인받습니다(할당 전 승인).
func (d *deltaState) syncStaleDynamicLocked(q *deltaQueue) {
	want := q.staleNS.dynBytes()
	if want >= q.dynamic {
		q.dynamic = want
		return
	}
	d.budget.releaseQueued(q.dynamic - want)
	q.dynamic = want
}

// clearStaleLocked는 낡음 집합을 비우고 **그 동적 몫을 정확히 한 번** 되돌립니다.
func (d *deltaState) clearStaleLocked(q *deltaQueue) {
	q.staleNS.reset()
	if q.dynamic != 0 {
		d.budget.releaseQueued(q.dynamic)
		q.dynamic = 0
	}
}

// addStale은 namespace를 낡음 집합에 넣고 **그 namespace의 입력 지문**을 함께 새깁니다.
// 새 드롭·새 tombstone처럼 입력이 실제로 달라진 지점에서만 부릅니다.
func (d *deltaState) addStaleLocked(q *deltaQueue, namespace string, seq uint64) {
	if !d.stakeStaleLocked(q, namespace) {
		return
	}
	q.staleNS.setMark(namespace, seq)
}

// stakeStaleLocked는 namespace를 낡음 집합에 **넣기 전에 그 문자열 몫을 승인**합니다.
//
// 먼저 담고 나중에 계상하면, 승인이 실패하는 순간에는 이미 백킹을 붙잡은 뒤입니다.
// 순서를 뒤집어야 "예약 안에서만 붙잡는다"가 성립합니다. 승인에 실패하거나 추적
// 상한을 넘으면 그 GVR을 통째로 낡음으로 승급합니다(유계 비트 하나).
func (d *deltaState) stakeStaleLocked(q *deltaQueue, namespace string) bool {
	if q.gvrStale {
		return false // 이미 통째로 낡았습니다. namespace별 추적이 의미 없습니다.
	}
	if q.staleNS.has(namespace) {
		return true // 이미 붙잡고 있습니다. 새 백킹이 생기지 않습니다.
	}
	if q.staleNS.count >= q.staleNS.limit {
		d.clearStaleLocked(q)
		q.gvrStale = true // 추적 상한 초과 → 유계 GVR 비트로 승급
		return false
	}
	if !d.budget.admitStructural(int64(len(namespace))) {
		d.clearStaleLocked(q)
		q.gvrStale = true
		return false
	}
	if !q.staleNS.add(namespace) {
		d.budget.releaseQueued(int64(len(namespace)))
		d.clearStaleLocked(q)
		q.gvrStale = true
		return false
	}
	q.dynamic += int64(len(namespace))
	return true
}

// restakeLocked는 회수 실패로 대상을 다시 낡음으로 되돌립니다.
//
// **입력 지문은 건드리지 않습니다.** 실패는 입력 변화가 아닙니다. 여기서 지문이
// 움직이면 회로가 스스로 닫히고, 같은 입력으로 백오프 없이 재시도합니다.
func (d *deltaState) restakeLocked(q *deltaQueue, namespace string) {
	d.stakeStaleLocked(q, namespace)
}

// removeStaleLocked는 회수가 성공한 namespace를 집합에서 뺍니다(마커 포함).
func (d *deltaState) removeStaleLocked(q *deltaQueue, namespace string) {
	q.staleNS.remove(namespace)
	d.syncStaleDynamicLocked(q)
}

// maxMarker는 마커를 **단조 증가**로만 갱신합니다.
//
// 직접 대입하면 늦게 도착한 옛 이벤트(seq 10)가 이미 기록된 새 마커(seq 11)를
// 되돌립니다. 그 뒤 covers 10짜리 ack이 오면, 아직 덮이지 않은 11을 깨끗하다고
// 선언해 **회수가 통째로 사라집니다.** 마커는 언제나 지금까지 본 가장 큰 값입니다.
func maxMarker(cur, next uint64) uint64 {
	if next > cur {
		return next
	}
	return cur
}

// markerFor는 그 대상의 입력 지문입니다.
// 전체 대상은 GVR 마커를, namespace 대상은 자기 마커를 봅니다.
func (q *deltaQueue) markerFor(target recoveryTarget) uint64 {
	if q == nil {
		return 0
	}
	if target.whole {
		return q.markerSeq
	}
	return q.staleNS.mark(target.namespace)
}

// reindex는 events 위치 색인을 다시 만듭니다. 드레인·되돌림 뒤에 한 번씩 부릅니다.
func (q *deltaQueue) reindex() {
	if q.index == nil {
		q.index = make(map[string]int, len(q.events))
	}
	for k := range q.index {
		delete(q.index, k)
	}
	for i, ev := range q.events {
		q.index[ev.key()] = i
	}
}

/* ── 회수 티켓 ──────────────────────────────────────────────────────────── */

// recoveryPhase는 회수 진행 상태입니다.
type recoveryPhase uint8

const (
	recoveryIdle recoveryPhase = iota
	recoveryWaitingCover
	recoveryBuilding
)

// recoveryTicket은 파티션(또는 GVR) 하나의 회수 작업입니다.
//
// 서비스 전체에서 **동시에 하나만** 살아 있습니다. 쿨다운·백오프·회로가 지속
// 오버플로에서 재구성이 반복되는 것을 막습니다.
// recoveryTicket의 **모든 mutable field는 delta.mu 아래에서만** 읽고 씁니다.
//
// 작업자(deltaLoop)는 잠금 아래에서 불변 입력과 `step`을 캡처하고, 계산은 잠금
// 밖에서 지역 사본으로만 한 뒤, 다시 잠금을 잡고 `step`이 그대로일 때만 결과를
// 넘깁니다. 그래서 discard/purge가 티켓을 걷어가도 작업자가 쓰던 메모리를
// 건드리지 않고, 예약 소유권도 언제나 한 곳에 있습니다.
//
//	예약 소유자   티켓. delta.mu 아래에서만 옮기고, dropTicketLocked가 유일한 해제자.
//	취소 소유자   작업자. ctx가 끝나면 커밋을 포기할 뿐 아무것도 해제하지 않습니다.
type recoveryTicket struct {
	gvr       schema.GroupVersionResource
	namespace string
	wholeGVR  bool
	// step은 이 티켓의 상태 세대입니다. 커밋마다 올라가며, 작업자가 캡처한 값과
	// 다르면 그 작업 결과는 버려집니다(오래된 조각이 새 상태를 덮지 못합니다).
	step uint64
	// dead는 이 티켓이 이미 걷혔다는 표시입니다. 되살아나지 않습니다.
	dead bool
	// workers는 잠금 밖에서 이 티켓의 지역 사본(src/side/store/held)을 들고 있는
	// 작업자 수입니다. **0이 되기 전에는 예약을 풀지 않습니다** — 아직 살아 있는
	// 메모리를 회계에서 먼저 빼면 상한이 실제보다 작게 보입니다.
	workers int
	// holdActive는 이 티켓이 대상 이벤트를 보류 중인지입니다.
	//
	// **cluster-scoped 회수(namespace "")를 위해 반드시 필요한 플래그입니다.**
	// 빈 문자열을 "보류 없음"으로 쓰면 cluster-scoped 파티션은 보류되지 않고,
	// 회수 도중 델타가 그 파티션을 바꿔 게시가 조용히 어긋납니다.
	holdActive bool

	// epoch는 이 티켓이 ack할 마커 세대입니다. 이후 epoch의 마커는 살아남습니다.
	epoch uint64
	// markerSeq는 이 티켓이 덮어야 하는 최소 eventSeq입니다.
	markerSeq uint64
	// markerCaptured는 markerSeq가 **그 대상의 마커로 캡처된** 값인지입니다.
	//
	// pickRecoveryLocked가 만든 티켓은 언제나 캡처합니다. 캡처된 티켓의 ack은
	// **그 namespace의 마커**로만 판정하므로, 옆 namespace의 이벤트가 ack을
	// 무효로 만들지 않습니다.
	//
	// 캡처되지 않은 티켓(직접 만든 경로)은 지문이 없으므로 전역 epoch 규칙으로
	// 되돌아갑니다. 그 규칙은 **더 보수적**입니다 — 무관한 이벤트에도 ack을
	// 보류할 뿐, 아직 낡은 것을 깨끗하다고 말하지는 않습니다.
	markerCaptured bool
	phase          recoveryPhase

	// ── 티켓 수명 내내 **고정**되는 원본 ───────────────────────────────────
	// 중간에 목록 스냅숏이 바뀌어도 이 티켓은 처음 고정한 원본만 씁니다.
	// 그래야 조각들이 서로 다른 시점을 섞지 않습니다.
	token buildToken
	// src는 **깊게 소유한** 검색 전용 행입니다(원본 객체를 붙잡지 않습니다).
	src          []recoveryRow
	srcCovers    uint64
	srcIndexVer  uint64
	srcSearchVer uint64
	partVersion  uint64
	store        cache.Store

	// side는 조각마다 조금씩 쌓는 측면 인덱스입니다. 게시 시 다시 훑지 않습니다.
	side    *searchIndex
	nextRow int
	hi      int
	// reserved는 이 티켓이 예약한 바이트입니다(고정 원본의 연장 수명 + 측면 빌더).
	reserved int64
	// chunkReserved는 조각 하나가 쓸 scratch·경로 복사로 이미 확보한 몫입니다.
	// 다음 조각이 더 필요하면 **차이만** 추가로 예약합니다(중복 예약 없음).
	chunkReserved int64
	// heldReserved는 **보류분 적용**으로 확보한 몫입니다. 조각과 성격이 다르므로
	// 항을 분리합니다 — 같은 항에 섞으면 max 규칙에 가려 실제 필요분이 빠집니다.
	heldReserved int64
	// structCharged는 티켓 구조체 자체의 계상 여부입니다.
	// 티켓도 지속되는 제어 객체이므로 만들기 전에 승인받고, 죽을 때 한 번 되돌립니다.
	structCharged bool

	attempts int
	// lastNeeded는 예산 실패 시 필요했던 바이트입니다. 줄지 않으면 재시도하지 않습니다(회로).
	lastNeeded int64
	notBefore  time.Time
	backoff    time.Duration
}

// holds는 이 티켓이 그 이벤트를 보류해야 하는지입니다. delta.mu 아래에서만 부릅니다.
func (t *recoveryTicket) holds(gvr schema.GroupVersionResource, namespace string) bool {
	if t == nil || t.dead || !t.holdActive || t.gvr != gvr {
		return false
	}
	return t.wholeGVR || t.namespace == namespace
}

// recoveryStep은 조각 하나가 쓸 **불변 입력과 신원**입니다.
// 잠금 아래에서 만들어지고, 잠금 밖에서는 읽기만 합니다.
type recoveryStep struct {
	src    []recoveryRow
	side   *searchIndex
	lo, hi int
}

// recoveryTarget은 회로가 기억하는 회수 대상입니다.
type recoveryTarget struct {
	gvr       schema.GroupVersionResource
	namespace string
	whole     bool
}

func (t *recoveryTicket) target() recoveryTarget {
	return recoveryTarget{gvr: t.gvr, namespace: t.namespace, whole: t.wholeGVR}
}

// recoveryCircuit은 대상 하나의 **지속되는** 재시도 상태입니다.
//
// 입력도 예산도 그대로면 다시 시도하지 않습니다. 다음 중 하나가 실제로 바뀌었을
// 때만 회로가 닫힙니다.
//
//	markerSeq   새 드롭·새 tombstone이 생겼습니다(입력이 달라졌습니다).
//	rows        원본 행 수가 줄었습니다(같은 예산에 들어갈 여지가 생겼습니다).
//	avail       **실제 가용 용량**이 지난번에 필요했던 만큼으로 늘었습니다.
//
// 설정 상한(budgetMax)만 보면 "다른 GVR이 놓아서 자리가 생긴" 경우를 영영 놓칩니다.
// 반대로 1바이트라도 늘면 열어 주면 폭풍이 됩니다 — 기준은 **지난번에 필요했던
// 바이트(lastNeeded)를 지금 감당할 수 있는가**입니다.
type recoveryCircuit struct {
	attempts   int
	lastNeeded int64
	notBefore  time.Time
	backoff    time.Duration
	// lastMarker/lastRows/lastMax/lastAvail은 마지막 실패 시점의 입력·용량 지문입니다.
	lastMarker uint64
	lastRows   int
	lastMax    int64
	lastAvail  int64
	// open이면 입력·용량이 바뀌기 전에는 잡지 않습니다.
	open bool
}

// maxCircuits는 회로 맵의 하드 상한입니다.
// 대상은 (GVR 수 × staleNS 상한)으로 유계이지만, 회로가 무한히 쌓이지 않도록
// 여기서 명시적으로 끊고 **GVR 전체 회로로 승급**시킵니다.
const maxCircuits = maxStaleTracked * 2

// circuitFor는 대상의 회로를 돌려줍니다(없으면 만듭니다). delta.mu 아래에서만 부릅니다.
//
// 상한에 닿으면 **임의 항목을 버리지 않습니다.** 열린(=아직 낡은) 회로를 버리면
// 그 대상은 지문을 잃고 다음 tick에 처음처럼 다시 시도합니다 — 정확히 우리가
// 막으려던 폭풍입니다. 대신 그 GVR을 **GVR 전체 회로 하나로 승급**시키고
// 그 GVR의 namespace별 회로를 회수합니다. 회로 수는 GVR 수만큼으로 접힙니다.
// **공유 넘침 회로는 없습니다.** 서로 다른 대상이 하나의 가변 회로를 나눠 쓰면
// 대상별 marker/needed/backoff가 보존되지 않습니다 — 옆 대상의 다른 마커가
// 그 회로를 reset해 버리고, 그 순간 이쪽은 백오프 없는 재시도 루프로 들어갑니다.
// 대신 **GVR 전체 회로로 결정적으로 승급**합니다. 그 회로의 지문은 그 GVR의
// 전체 마커이므로 대상별 의미가 유지됩니다.
// circuitForQ는 **언제나 지속되는 회로**를 돌려줍니다.
//
// 동적 회로 맵을 승인받지 못하면 큐에 내장된 선계상 회로로 되돌아갑니다.
// 그 회로의 의미는 **GVR 전체**이므로 대상도 함께 승급해 돌려줍니다 — 호출자가
// namespace 마커로 지문을 비교하면 승급된 회로의 의미와 어긋나기 때문입니다.
//
// 큐가 없으면(구조 거절) 회로도 없습니다. 그 경우는 증분 경로 자체를 포기하고
// 전체 재구성으로 되돌아간 상태라 백오프를 기억할 대상이 없습니다.
func (d *deltaState) circuitForQ(q *deltaQueue, target recoveryTarget) (*recoveryCircuit, recoveryTarget) {
	if c, eff := d.circuitFor(target); c != nil {
		return c, eff
	}
	if q == nil {
		return nil, target
	}
	// 내장 회로는 **GVR 전체 회로**입니다. 대상도 함께 승급해야 이후의 행 수·마커·
	// 티켓 필드·보류·ack이 전부 같은 의미로 움직입니다. 지문만 whole로 바꾸고
	// 티켓은 namespace로 두면, 전체 회로에 namespace 행 수와 마커가 달라붙습니다.
	return &q.fallback, recoveryTarget{gvr: target.gvr, whole: true}
}

// circuitFor는 회로와 **실제로 적용된 대상**을 함께 돌려줍니다.
// 자기 GVR 접기로 승급되면 두 번째 값이 전체 대상입니다.
func (d *deltaState) circuitFor(target recoveryTarget) (*recoveryCircuit, recoveryTarget) {
	c, eff := d.circuitForLocked(target)
	if c == nil {
		// **A는 성공하고 B는 실패한 사슬의 롤백입니다.**
		//
		// 맵 씨앗은 승인받았는데 정작 항목을 만들지 못하면, 비어 있는 맵과 그
		// 계상만 남습니다. 거절된 승인은 원장을 기준선 그대로 두어야 하므로
		// 여기서 정확히 한 번 되돌립니다(맵이 비어 있을 때만).
		d.releaseCircuitsSeedLocked()
	}
	return c, eff
}

// circuitForLocked는 회로와 **실제로 적용된 대상**을 돌려줍니다.
//
// 승급이 일어나면 두 번째 값이 전체 대상입니다. 호출자는 반드시 이 값을 써야
// 합니다 — 승급된 회로에 namespace 마커와 namespace 행 수를 붙이면, 전체 회로가
// 자기 것이 아닌 지문으로 열리고 닫힙니다.
func (d *deltaState) circuitForLocked(target recoveryTarget) (*recoveryCircuit, recoveryTarget) {
	if d.circuits == nil {
		if !d.budget.admitStructural(circuitsMapSeedBytes) {
			// 맵조차 만들 자리가 없습니다. 승인 없이 할당하지 않습니다.
			// 호출자는 nil을 "회로 없음"으로 다루고 보수적으로 넘어갑니다.
			return nil, target
		}
		d.circuits = make(map[recoveryTarget]*recoveryCircuit, 16)
		d.circuitsSeed = circuitsMapSeedBytes
	}
	if c, ok := d.circuits[target]; ok {
		return c, target
	}
	if len(d.circuits) >= maxCircuits {
		// ① 이 GVR을 접습니다. 접히면 자리가 생기고 지문은 전체 회로가 물려받습니다.
		d.escalateCircuitsLocked(target.gvr)
		// 승급 뒤 이 GVR은 통째로만 회수합니다.
		if !target.whole {
			target = recoveryTarget{gvr: target.gvr, whole: true}
		}
		if c, ok := d.circuits[target]; ok {
			return c, target
		}
		// ② 접을 것이 없었으면(처음 보는 GVR) **결정적으로 고른 다른 GVR**을 접습니다.
		// 회로 수는 2048이고 GVR은 64개 이하이므로, 가득 찬 맵에는 반드시
		// namespace 회로를 둘 이상 가진 GVR이 있습니다 — 접으면 자리가 생깁니다.
		if len(d.circuits) >= maxCircuits {
			if victim, ok := d.escalationVictimLocked(target.gvr); ok {
				d.escalateCircuitsLocked(victim)
			}
		}
		if len(d.circuits) >= maxCircuits {
			// 여기까지 왔다면 접을 것이 정말로 없습니다(전부 전체 회로).
			// 새 항목을 만들지 않고 nil을 돌려줍니다 — 상한을 넘기지도,
			// 남의 회로를 빌려 쓰지도 않습니다.
			return nil, target
		}
	}
	if !d.budget.admitStructural(recoveryCircuitBytes) {
		return nil, target // 승인 없이 만들지 않습니다.
	}
	c := &recoveryCircuit{backoff: recoveryBackoffMin}
	d.circuits[target] = c
	return c, target
}

// foldableCountLocked는 그 GVR의 namespace 회로 수입니다(할당 없음).
func (d *deltaState) foldableCountLocked(gvr schema.GroupVersionResource) int {
	n := 0
	for k := range d.circuits {
		if k.gvr == gvr && !k.whole {
			n++
		}
	}
	return n
}

// escalationVictimLocked는 접을 GVR을 **결정적으로** 고릅니다.
//
// **namespace 회로를 둘 이상 가진 GVR만** 고릅니다. 하나뿐인 GVR을 접으면
// 1개가 1개(전체 회로)로 바뀔 뿐이라 자리가 생기지 않고, 다음 tick에 같은 일을
// 반복합니다. 맵을 새로 만들지 않고, 동점 판정은 GVR 필드 비교로 합니다.
func (d *deltaState) escalationVictimLocked(except schema.GroupVersionResource) (schema.GroupVersionResource, bool) {
	// 후보는 GVR 수만큼이고 allowlist가 유계이므로 고정 배열로 충분합니다.
	var seen [maxFoldCandidates]schema.GroupVersionResource
	var counts [maxFoldCandidates]int
	n := 0
	for k := range d.circuits {
		if k.whole || k.gvr == except {
			continue
		}
		at := -1
		for i := 0; i < n; i++ {
			if seen[i] == k.gvr {
				at = i
				break
			}
		}
		if at < 0 {
			if n == maxFoldCandidates {
				continue // 후보가 넘칩니다. 이미 모은 것 중에서 고릅니다.
			}
			seen[n], counts[n] = k.gvr, 0
			at, n = n, n+1
		}
		counts[at]++
	}
	var best schema.GroupVersionResource
	found := false
	for i := 0; i < n; i++ {
		if counts[i] < 2 {
			continue // 접어도 자리가 생기지 않습니다.
		}
		if !found || gvrLess(seen[i], best) {
			best, found = seen[i], true
		}
	}
	return best, found
}

// maxFoldCandidates는 접기 후보 GVR 수의 상한입니다(allowlist 상한과 같은 자릿수).
const maxFoldCandidates = 128

// gvrLess는 GVR의 결정적 순서입니다(문자열을 만들지 않습니다).
func gvrLess(a, b schema.GroupVersionResource) bool {
	if a.Group != b.Group {
		return a.Group < b.Group
	}
	if a.Version != b.Version {
		return a.Version < b.Version
	}
	return a.Resource < b.Resource
}

// escalateCircuitsLocked는 GVR 하나의 namespace별 회로를 **전체 회로 하나로** 접습니다.
//
// 승급된 회로는 접힌 회로들 중 **가장 보수적인** 지문을 물려받습니다(가장 늦은
// notBefore, 가장 큰 backoff, 열림 상태 유지). 그래야 접는 행위 자체가 재시도를
// 앞당기지 않습니다.
func (d *deltaState) escalateCircuitsLocked(gvr schema.GroupVersionResource) {
	whole := recoveryTarget{gvr: gvr, whole: true}
	merged := d.circuits[whole]
	if merged == nil {
		// **접을 것이 없는데 새 항목만 만들면 상한을 넘깁니다.**
		// 처음 보는 GVR이 가득 찬 회로 맵에 도착하는 경우가 정확히 그 상황입니다.
		if d.foldableCountLocked(gvr) == 0 && len(d.circuits) >= maxCircuits {
			return
		}
		// **접히는 회로 중 하나를 그대로 재사용합니다.** 새 객체를 만들면 접기가
		// 자리를 만들기는커녕 예약 밖 할당을 하나 더 늘립니다.
		for k, c := range d.circuits {
			if k.gvr == gvr && !k.whole {
				merged = c
				delete(d.circuits, k)
				break
			}
		}
		if merged == nil {
			// 접을 것이 없지만 자리는 있습니다(맵이 가득 차지 않음).
			if !d.budget.admitStructural(recoveryCircuitBytes) {
				return
			}
			merged = &recoveryCircuit{backoff: recoveryBackoffMin}
		}
	}
	var freed int64
	for k, c := range d.circuits {
		if k.gvr != gvr || k.whole {
			continue
		}
		if c.open {
			merged.open = true
		}
		if c.notBefore.After(merged.notBefore) {
			merged.notBefore = c.notBefore
		}
		if c.backoff > merged.backoff {
			merged.backoff = c.backoff
		}
		if c.attempts > merged.attempts {
			merged.attempts = c.attempts
		}
		if c.lastNeeded > merged.lastNeeded {
			merged.lastNeeded = c.lastNeeded
		}
		if c.lastMarker > merged.lastMarker {
			merged.lastMarker = c.lastMarker
		}
		if c.lastMax > merged.lastMax {
			merged.lastMax = c.lastMax
		}
		if c.lastRows > merged.lastRows {
			merged.lastRows = c.lastRows
		}
		// 가용 용량 지문은 **가장 낮은 쪽**이 보수적입니다. 높은 쪽을 물려받으면
		// 접기만으로 "용량이 늘었다"고 판단해 재시도가 앞당겨집니다.
		if merged.lastAvail == 0 || (c.lastAvail > 0 && c.lastAvail < merged.lastAvail) {
			merged.lastAvail = c.lastAvail
		}
		delete(d.circuits, k)
		freed += recoveryCircuitBytes
	}
	// merged는 **이미 계상된 객체**입니다(기존 whole 회로이거나 접힌 것 중 하나를
	// 재사용했거나, 자리가 있어 새로 승인받은 것). 그러므로 여기서 다시 계상하지
	// 않고, 지운 나머지 회로의 몫만 되돌립니다.
	d.circuits[whole] = merged
	d.budget.releaseQueued(freed)
	// 이 GVR은 이제 통째로만 회수합니다. 큐 마커도 같은 결정을 따라야
	// pickRecovery가 사라진 namespace 대상을 다시 만들지 않습니다.
	if q := d.queues[gvr]; q != nil {
		d.clearStaleLocked(q)
		q.gvrStale = true
	}
}

// dropCircuitLocked는 회수가 성공한 대상의 회로를 지웁니다(더 이상 낡지 않았습니다).
func (d *deltaState) dropCircuitLocked(target recoveryTarget) {
	if _, ok := d.circuits[target]; !ok {
		return
	}
	delete(d.circuits, target)
	d.budget.releaseQueued(recoveryCircuitBytes)
	d.releaseCircuitsSeedLocked()
}

// releaseCircuitsSeedLocked는 회로가 하나도 남지 않으면 맵 자체를 되돌립니다.
// 멈춘 서비스가 0으로 수렴하려면 이 몫도 사라져야 합니다.
func (d *deltaState) releaseCircuitsSeedLocked() {
	if len(d.circuits) != 0 || d.circuitsSeed == 0 {
		return
	}
	d.circuits = nil
	d.budget.releaseQueued(d.circuitsSeed)
	d.circuitsSeed = 0
}

// dropCircuitsForLocked는 멈추는 GVR의 회로를 **전부** 걷어내고 그 몫을 되돌립니다.
// 회로는 큐와 같은 수명이라 종단 경로에서 함께 사라져야 합니다.
func (d *deltaState) dropCircuitsForLocked(gvr schema.GroupVersionResource) {
	var freed int64
	for k := range d.circuits {
		if k.gvr != gvr {
			continue
		}
		delete(d.circuits, k)
		freed += recoveryCircuitBytes
	}
	d.budget.releaseQueued(freed)
	d.releaseCircuitsSeedLocked()
}

// allows는 지금 이 대상을 잡아도 되는지입니다.
//
// markerSeq는 **그 대상의** 마커여야 합니다. GVR 전체 마커를 넘기면, 옆 namespace가
// 하나 드롭될 때마다 아무 상관 없는 대상의 회로가 함께 닫혀 재시도 폭풍이 됩니다.
// avail은 **지금 이 대상이 실제로 쓸 수 있는** 바이트입니다. 설정 상한이 아니라
// 이 값이 회로를 여는 기준입니다 — 상한이 그대로여도 다른 GVR이 놓거나 은퇴 세대가
// 빠지면 자리가 생기고, 그때가 재시도할 자격이 생기는 순간입니다.
func (c *recoveryCircuit) allows(now time.Time, markerSeq uint64, budgetMax, avail int64) bool {
	if c == nil {
		return true // 회로가 없으면 막을 근거도 없습니다.
	}
	if now.Before(c.notBefore) {
		return false
	}
	if !c.open {
		return true
	}
	// 회로가 열려 있습니다. **실제로** 달라졌을 때만 닫습니다.
	//
	//	입력이 달라졌다        markerSeq != lastMarker
	//	설정 상한이 커졌다      budgetMax > lastMax
	//	지난번에 필요했던 만큼을 지금 감당할 수 있다  avail >= lastNeeded
	//
	// 세 번째가 핵심입니다. "avail > lastAvail"로 두면 1바이트만 풀려도 전체
	// 재구성을 다시 돌려 폭풍이 됩니다. 기준은 **필요했던 양**이어야 합니다.
	if markerSeq != c.lastMarker || budgetMax > c.lastMax || c.capacitySuffices(avail) {
		c.reset()
		return true
	}
	return false
}

// capacitySuffices는 지금 용량이 지난번에 필요했던 만큼인지입니다.
// lastNeeded를 모르면(0) 용량만으로는 열지 않습니다 — 근거 없는 재시도는 폭풍입니다.
func (c *recoveryCircuit) capacitySuffices(avail int64) bool {
	return c.lastNeeded > 0 && avail >= c.lastNeeded
}

// noteRows는 원본 크기가 줄었으면 회로를 닫습니다(예산 여지가 생겼습니다).
func (c *recoveryCircuit) noteRows(rows int) {
	if c == nil {
		return
	}
	if c.open && c.lastRows > 0 && rows < c.lastRows {
		c.reset()
	}
}

// backoffOr는 회로가 없을 때 기본 쿨다운으로 되돌아갑니다.
func (c *recoveryCircuit) backoffOr(def time.Duration) time.Duration {
	if c == nil || c.backoff <= 0 {
		return def
	}
	return c.backoff
}

func (c *recoveryCircuit) reset() {
	c.attempts, c.lastNeeded, c.open = 0, 0, false
	c.backoff = recoveryBackoffMin
	c.notBefore = time.Time{}
	c.lastAvail = 0
}

// fail은 실패를 기록하고 지수 백오프를 늘립니다.
func (c *recoveryCircuit) fail(now time.Time, markerSeq uint64, rows int, needed, budgetMax, avail int64, openCircuit bool) {
	if c == nil {
		return
	}
	c.attempts++
	c.backoff *= 2
	if c.backoff < recoveryBackoffMin {
		c.backoff = recoveryBackoffMin
	}
	if c.backoff > recoveryBackoffMax {
		c.backoff = recoveryBackoffMax
	}
	c.notBefore = now.Add(c.backoff)
	c.lastMarker, c.lastRows, c.lastMax, c.lastAvail = markerSeq, rows, budgetMax, avail
	if needed > 0 {
		c.lastNeeded = needed
	}
	if openCircuit {
		c.open = true
	}
}

// recoveryReserveBytes는 회수 티켓 하나가 **살아 있는 동안 붙잡는 전부**입니다.
//
//	① 고정 원본의 연장 수명   rows × (행 레코드 + 이름/UID + 입력)
//	② 완성될 측면 인덱스      rows × (posting/행/트라이 보유 몫)
//	③ 조각 하나의 scratch·COW  recoveryChunkRows 만큼의 입력 + 경로 복사
//
// ②를 빼고 첫 조각만 예약하면 회수가 진행될수록 예약 밖 메모리가 쌓입니다.
// ③을 빼면 조각마다 예약 없이 경로 복사를 합니다. 셋 다 **할당 전에** 잡습니다.
func recoveryReserveBytes(rows int) int64 {
	if rows < 0 {
		rows = 0
	}
	pinned := int64(rows) * (rowRecordFixedBytes + 2*stringHeaderBytes + bootstrapRowInputBytes)
	// 측면 인덱스의 보유 몫: 행마다 posting 항목(최대 maxRowTokens) + 행 디렉터리
	// 항목 + 트라이 슬롯. 리프 용량은 postLeafSplit 채움을 기준으로 환산합니다.
	side := int64(rows) * (maxRowTokens*postEntryBytes*postLeafMax/postLeafSplit +
		rowEntryBytes*rowLeafMax/rowLeafSplit + trieBytesPerSlot + rowRecordFixedBytes)
	side += nsPartFixedBytes
	chunk := recoveryChunkReserveBytes(recoveryChunkRows)
	return recoveryTicketBytes + pinned + side + chunk
}

// recoveryChunkReserveBytes는 조각 하나가 잡는 입력 + 경로 복사입니다.
// 최초 예약과 growTicketReservation이 **같은 식**을 써야 중복 예약이 없습니다.
//
// 조각은 핀 단계에서 이미 정규화된 토큰을 **재사용**하므로 토큰 사본 몫이 없습니다.
func recoveryChunkReserveBytes(rows int) int64 {
	if rows < 0 {
		rows = 0
	}
	return int64(rows)*(bootstrapRowInputBytes+bootstrapPartOpBytes) + deltaCOWPerKeyBytes
}

// heldApplyReserveBytes는 **보류분 적용이 잡는 상한 전부**입니다.
//
// 보류 키는 델타 배치와 똑같은 일을 합니다 — 키마다 정규화 토큰을 새로 만들고,
// seen/byNS 맵과 슬라이스를 잡고, **여러 파티션에 걸친** 경로 복사를 하고,
// 원본 구간에 없던 **새 행·새 파티션**까지 만듭니다. 그래서 조각 식이 아니라
// 델타 배치 식을 씁니다. 게다가 게시 순간에는 옛 측면 루트와 새 루트가
// **함께 살아 있으므로** 그 공존 몫을 더합니다.
func heldApplyReserveBytes(n int) int64 {
	if n < 0 {
		n = 0
	}
	return deltaTransientBytes(n) + sideRootCoexistBytes
}

// sideRootCoexistBytes는 게시 순간 옛/새 측면 루트가 함께 사는 몫입니다.
const sideRootCoexistBytes = 2 * (nsPartStructBytes + nsDirPageCopyBytes + trieSpineBytes)

// recoveryRow는 회수가 **깊게 소유하는** 검색 전용 행입니다.
//
// indexRow를 얕게 복사하면 *PartialObjectMetadata가 함께 살아남습니다. 그러면
// 검색이 쓰지도 않는 annotations·ownerReferences·finalizers·managedFields까지
// 티켓 수명 내내 도달 가능한 채로 남고, 회계는 그것을 세지 않습니다.
// 회수가 실제로 필요한 것은 이 다섯 가지뿐입니다.
type recoveryRow struct {
	namespace string
	name      string
	uid       string
	// labels는 **정규화가 끝난** 토큰입니다. 조각 계산이 다시 정규화하지 않습니다.
	labels    []string
	truncated bool
}

// recoveryRowStructBytes는 소유 행 하나의 구조체 크기입니다(문자열 3 + 슬라이스 + bool).
const recoveryRowStructBytes = 3*stringHeaderBytes + 24 + 8

// copyRecoveryRows는 [lo, hi) 구간을 **깊은 소유 사본**으로 뜹니다.
//
// 예약이 통과한 뒤에만 부릅니다(2차 패스). 원본 객체 포인터를 남기지 않으므로,
// 이 사본이 사는 동안 informer 캐시의 큰 객체가 회수 때문에 붙잡히지 않습니다.
func copyRecoveryRows(idx *indexSnapshot, lo, hi int) []recoveryRow {
	if idx == nil || lo < 0 || hi > len(idx.rows) || lo >= hi {
		return nil
	}
	out := make([]recoveryRow, 0, hi-lo)
	var keyBuf, tokBuf []string
	for i := lo; i < hi; i++ {
		r := &idx.rows[i]
		var truncated bool
		tokBuf, truncated, keyBuf = labelTokensOf(r, keyBuf, tokBuf)
		labels := make([]string, len(tokBuf))
		copy(labels, tokBuf)
		out = append(out, recoveryRow{
			namespace: r.namespace,
			name:      r.name,
			uid:       rowUID(r),
			labels:    labels,
			truncated: truncated,
		})
	}
	return out
}

// recoverySpanCost는 [lo, hi) 구간이 **실제로** 붙잡는 바이트를 잽니다.
//
// 행 수만으로는 부족합니다 — 이름·UID·label 문자열이 그대로 소유 사본과 측면
// 인덱스(정규화 토큰·fence)로 들어가기 때문입니다. 예전 식은 이것을 전부 빼고
// 행 수만 곱했고, 그래서 긴 이름·포화 label 대상에서 예약이 실제보다 작았습니다.
//
//	owned  이 티켓이 소유할 행 사본(부모 백킹을 붙잡지 않기 위한 복사)
//	side   측면 인덱스가 완성되었을 때의 보유 몫
//
// **1차 패스입니다.** 아무것도 붙잡지 않고 크기만 잽니다 — 정규화 토큰 바이트는
// 원본 label 바이트를 넘지 않으므로(잘라내기만 함) raw 합이 안전한 상한입니다.
func recoverySpanCost(idx *indexSnapshot, lo, hi int) (owned, side int64) {
	if idx == nil || lo < 0 || hi > len(idx.rows) || lo >= hi {
		return 0, 0
	}
	n := int64(hi - lo)
	var text int64
	for i := lo; i < hi; i++ {
		r := &idx.rows[i]
		text += int64(len(r.namespace) + len(r.name))
		if r.obj == nil {
			continue
		}
		text += int64(len(r.obj.UID))
		for k, v := range r.obj.Labels {
			text += int64(len(k) + len(v))
		}
	}
	// 소유 사본: 구조체 + 토큰 슬라이스 헤더 + 복사된 문자열 본문.
	owned = n*(recoveryRowStructBytes+maxRowTokens*stringHeaderBytes) + text
	side = n*(maxRowTokens*postEntryBytes*postLeafMax/postLeafSplit+
		rowEntryBytes*rowLeafMax/rowLeafSplit+trieBytesPerSlot+rowRecordFixedBytes) +
		2*text + // 정규화 토큰 blob + fence 문자열
		nsPartFixedBytes
	return owned, side
}

// availableFor는 그 GVR이 지금 실제로 더 받을 수 있는 보유 바이트입니다.
// 회로가 "설정 상한"이 아니라 이 값을 보게 하는 진입점입니다.
func (s *Service) availableFor(gvr schema.GroupVersionResource) int64 {
	e, ok := s.entries[gvr]
	if !ok || e == nil {
		return 0
	}
	return s.availableRetained(e)
}

// ticketAliveLocked는 지금 티켓이 t이고 상태 세대가 그대로일 때만 참입니다.
// 커밋 직전에 반드시 통과해야 하는 관문입니다.
func (s *Service) ticketAliveLocked(t *recoveryTicket, step uint64) bool {
	return s.delta.ticket == t && !t.dead && t.step == step
}

/* ── 서비스 델타 상태 ────────────────────────────────────────────────────── */

// deltaState는 Service가 드는 증분 경로 상태입니다.
type deltaState struct {
	mu     sync.Mutex // queueMu — 큐·마커의 유일한 소유자
	queues map[schema.GroupVersionResource]*deltaQueue
	// budget은 큐·회로 **고정 구조를 계상할 원장**입니다.
	// nil이면 계상을 건너뜁니다(원장 없는 단위 테스트 하네스).
	budget *searchBudget

	// rr는 GVR 라운드로빈 커서입니다. 한 GVR이 tick을 독점하지 못하게 합니다.
	rr int
	// recoveryRR는 **회수 선택**의 라운드로빈 커서입니다.
	//
	// 델타 커서와 분리되어 있습니다. 언제나 order[0]부터 훑으면 앞선 GVR이 계속
	// 낡아 있는 동안 뒤쪽 GVR은 영영 회수되지 않습니다.
	recoveryRR int

	// ticket은 지금 살아 있는 유일한 회수 작업입니다.
	ticket *recoveryTicket
	// cooldownUntil은 다음 회수를 시작할 수 있는 가장 이른 시각입니다.
	// 성공·오버플로·예산 거절 모두 여기를 지나야 다시 잡힙니다(폭풍 방지).
	cooldownUntil time.Time
	// circuits는 **티켓보다 오래 사는** 회수 회로 상태입니다.
	//
	// 백오프·시도 횟수·필요 바이트를 티켓 안에만 두면, 티켓이 버려질 때 함께
	// 사라져 다음 tick이 처음처럼 다시 시작합니다. 그러면 입력도 예산도 그대로인데
	// 쿨다운마다 전체 재구성을 반복하는 폭풍이 됩니다.
	circuits map[recoveryTarget]*recoveryCircuit
	// circuitsSeed는 회로 맵 자체(씨앗 버킷)의 계상입니다.
	// 맵도 지속되는 제어 구조이므로 승인 대상이고, 비면 되돌립니다.
	circuitsSeed int64

	// copyBarrier는 **초기 소유 사본 구간**을 결정적으로 붙잡기 위한 테스트 seam입니다.
	// 프로덕션에서는 nil이며 env·설정 노브가 아닙니다. delta.mu 아래에서 읽습니다.
	copyBarrier func()

	// 계측
	recoveryAttempts atomic.Int64
	recoveryFailures atomic.Int64
	fullBootstraps   atomic.Int64
	partitionResyncs atomic.Int64
	// fullRecoveries는 **예외적** GVR 전체 회수 횟수입니다.
	// 정상 델타 flush에서는 절대 늘지 않습니다.
	fullRecoveries atomic.Int64
	// publishBudgetRejects는 최종 게시 잠금에서 예산으로 거절된 횟수입니다.
	// 되돌림 루프가 아니라 stale + 쿨다운으로 전환된 지점입니다.
	publishBudgetRejects atomic.Int64
	storeListCalls       atomic.Int64
	deltaFullBuilds      atomic.Int64
	directoryCopies      atomic.Int64
	visitedRows          atomic.Int64
	identityLookups      atomic.Int64
	postingsChanged      atomic.Int64
	// nodesCopied/sepBytes는 경로 복사가 실제로 만든 **후보 live 바이트**를
	// 되짚기 위한 계측입니다. 예약이 실제 할당을 덮는지 확인하는 근거입니다.
	nodesCopied atomic.Int64
	sepBytes    atomic.Int64
}

func newDeltaState() *deltaState {
	// coalescer(고정 8192 슬롯 strSet)는 아무도 읽지 않으면서 140KB를 원장 밖에서
	// 붙잡고 있었습니다. 쓰지 않는 구조는 계상 대상이 아니라 **삭제 대상**입니다.
	return &deltaState{queues: make(map[schema.GroupVersionResource]*deltaQueue, 64)}
}

// queuedBytesLocked는 **queued 항의 구조적 분해**입니다. delta.mu 아래에서만 부릅니다.
//
//	Σ 큐(고정 구조 + 대기 이벤트 + 보류) + 회로 항목
//
// 원장의 queued는 언제나 이 값과 같아야 합니다. 갈라지는 순간, 예약한 것과
// 실제로 살아 있는 것이 다르다는 뜻이고 상한은 더 이상 상한이 아닙니다.
// 64개 GVR이 전부 포화일 때 이 합이 곧 하한 증명입니다.
func (d *deltaState) queuedBytesLocked() int64 {
	var total int64
	for _, q := range d.queues {
		total += q.pendingBytes()
	}
	total += int64(len(d.circuits))*recoveryCircuitBytes + d.circuitsSeed
	if d.ticket != nil && d.ticket.structCharged {
		total += recoveryTicketBytes
	}
	return total
}

// queueFor는 GVR의 큐를 돌려줍니다. delta.mu 아래에서만 부릅니다.
//
// **구조를 만들기 전에 승인받습니다.** 이벤트가 하나도 없어도 staleNS 슬롯과
// 색인 맵은 이미 살아 있는 메모리이고, 그것을 승인 없이 실으면 live가 peakLimit을
// 넘은 채로 통과합니다 — 그 순간 I-C는 상한이 아닙니다.
//
// 승인에 실패하면 **큐를 만들지 않고 nil을 돌려줍니다.** 호출자는 그 GVR을 증분
// 경로에서 빼고 전체 재구성으로 되돌립니다(정확성은 유지, 비용만 늘어남).
func (d *deltaState) queueFor(gvr schema.GroupVersionResource) *deltaQueue {
	if q, ok := d.queues[gvr]; ok {
		return q
	}
	// **승인이 할당보다 먼저입니다.** deltaQueueFixedBytes는 상수라 슬롯·버킷을
	// 잡기 전에 알 수 있습니다. 예전처럼 먼저 만들고 나중에 승인하면, 거절되는
	// 큐가 이미 2천 개 슬롯을 잡은 뒤입니다.
	if !d.budget.admitStructural(deltaQueueFixedBytes) {
		return nil
	}
	q := newDeltaQueue()
	d.queues[gvr] = q
	return q
}

// queueOf는 **이미 있는** 큐만 돌려줍니다(만들지 않습니다).
// 읽기·정리 경로가 관측만으로 구조를 만들지 않게 하는 접근자입니다.
func (d *deltaState) queueOf(gvr schema.GroupVersionResource) *deltaQueue {
	return d.queues[gvr]
}

// forceRebuild는 증분 경로를 쓸 수 없을 때 그 GVR을 **전체 재구성으로** 되돌립니다.
//
// 큐 구조조차 상한 안에 만들 수 없는 상황에서, 조용히 낡은 채로 두지 않기 위한
// 마지막 경로입니다. 부트스트랩 표시를 내려 다음 목록 tick이 인덱스를 처음부터
// 다시 세우게 합니다 — 비싸지만 정확합니다.
func (s *Service) forceRebuild(gvr schema.GroupVersionResource) {
	e, ok := s.entries[gvr]
	if !ok || e == nil {
		return
	}
	e.bootstrapped.Store(false)
	e.dirty.Store(true)
}

/* ── 콜백: 키만 넣습니다 ─────────────────────────────────────────────────── */

// enqueueKey는 informer 콜백이 부르는 유일한 경로입니다.
//
// 라벨을 복사하기 **전에**(=복사 자체를 하지 않고) 정확한 바이트를 예약합니다.
// 예약이 실패하거나 큐가 가득 차면 그 namespace를 stale로 표시하고 드롭합니다 —
// 조용히 낡은 채로 두지 않습니다.
func (s *Service) enqueueKey(b *handlerBinding, namespace, name string) {
	if s == nil || s.delta == nil || b == nil || b.entry == nil ||
		!s.cfg.SearchEnabled || !s.cfg.SearchIncremental {
		return
	}
	e := b.entry
	// **고정된 신원**과 지금 살아 있는 신원이 같을 때만 담습니다. 재시작 뒤 도착한
	// 옛 informer의 콜백은 여기서 조용히 사라집니다(새 세대는 부트스트랩이 덮습니다).
	if e.tokenPacked.Load() != b.packed {
		return
	}
	gvr := e.gvr
	seq := s.eventSeq.Add(1)
	need := deltaEventBytes(namespace, name)

	s.delta.mu.Lock()
	defer s.delta.mu.Unlock()
	if e.tokenPacked.Load() != b.packed {
		return // 잠금을 기다리는 사이에 세대가 바뀌었습니다.
	}
	q := s.delta.queueFor(gvr)
	if q == nil {
		// 큐 구조조차 상한 안에 들어가지 않습니다. 증분 경로를 포기하고
		// 전체 재구성으로 되돌립니다 — 조용히 낡은 채로 두지 않습니다.
		s.forceRebuild(gvr)
		return
	}

	if len(q.events) >= maxPendingPerResource || !s.budget.reserveQueued(need) {
		q.dropped++
		q.markerSeq = maxMarker(q.markerSeq, seq)
		q.staleEpoch++
		s.delta.addStaleLocked(q, namespace, seq)
		return
	}
	// 같은 키가 이미 있으면 최신 seq로 갱신합니다(last-event-wins).
	ev := deltaEvent{namespace: namespace, name: name, seq: seq, gen: b.packed, reserved: need}
	if at, dup := q.index[ev.key()]; dup && at < len(q.events) {
		q.events[at].seq, q.events[at].gen = seq, b.packed
		s.budget.releaseQueued(need)
		return
	}
	// **저장 용량을 먼저 확보합니다.** append에 맡기면 배열·버킷이 먼저 커지고
	// 회계는 뒤따라옵니다. 확보하지 못하면 담지 않고 명시적 낡음으로 바꿉니다.
	if !s.delta.reserveEventSlotLocked(q) || !s.delta.reserveIndexSlotLocked(q) {
		s.budget.releaseQueued(need)
		q.dropped++
		q.markerSeq = maxMarker(q.markerSeq, seq)
		q.staleEpoch++
		s.delta.addStaleLocked(q, namespace, seq)
		return
	}
	q.index[ev.key()] = len(q.events)
	q.events = append(q.events, ev)
}

// metaKeyOf는 이벤트 객체의 (namespace, name)입니다. 알 수 없으면 빈 이름입니다.
func metaKeyOf(obj any, namespaced bool) (namespace, name string) {
	switch v := obj.(type) {
	case *metav1.PartialObjectMetadata:
		if v == nil {
			return "", ""
		}
		return v.Namespace, v.Name
	case cache.DeletedFinalStateUnknown:
		if m, ok := v.Obj.(*metav1.PartialObjectMetadata); ok && m != nil {
			return m.Namespace, m.Name
		}
		ns, n, ok := splitMetaKey(v.Key, namespaced)
		if !ok {
			return "", ""
		}
		return ns, n
	default:
		return "", ""
	}
}

// splitMetaKey는 informer 키를 리소스 성질에 맞춰 **엄격하게** 나눕니다.
//
// key-only tombstone은 문자열 하나뿐이라 여기서 틀리면 조용히 다른 객체를 지웁니다.
// 그래서 형식을 정확히 요구합니다.
//
//	namespaced   "ns/name" — 슬래시 정확히 하나, 양쪽 모두 비어 있지 않음
//	cluster      "name"    — 슬래시 없음, 비어 있지 않음
//
// 어긋나면 ns=""로 뭉개지 않고 실패를 돌려줍니다(호출자가 GVR stale로 승급합니다).
func splitMetaKey(key string, namespaced bool) (namespace, name string, ok bool) {
	if key == "" {
		return "", "", false
	}
	i := strings.IndexByte(key, '/')
	if !namespaced {
		if i >= 0 {
			return "", "", false // 클러스터 범위인데 슬래시가 있습니다.
		}
		return "", key, true
	}
	if i <= 0 || i == len(key)-1 {
		return "", "", false // 슬래시가 없거나, 앞뒤 한쪽이 비었습니다.
	}
	ns, n := key[:i], key[i+1:]
	if strings.IndexByte(n, '/') >= 0 {
		return "", "", false // 슬래시가 둘 이상입니다.
	}
	return ns, n, true
}

// enqueueObject는 이벤트 객체에서 키를 뽑아 큐에 넣습니다.
// tombstone(DeletedFinalStateUnknown)의 키만 있는 경우도 여기서 처리합니다.
func (s *Service) enqueueObject(b *handlerBinding, obj any) {
	if b == nil || b.entry == nil {
		return
	}
	switch v := obj.(type) {
	case *metav1.PartialObjectMetadata:
		if v == nil {
			return
		}
		s.enqueueKey(b, v.Namespace, v.Name)
	case cache.DeletedFinalStateUnknown:
		if m, ok := v.Obj.(*metav1.PartialObjectMetadata); ok && m != nil {
			s.enqueueKey(b, m.Namespace, m.Name)
			return
		}
		ns, name, ok := splitMetaKey(v.Key, b.namespaced)
		if !ok {
			// 형식이 어긋난 키입니다. ns=""로 뭉개면 엉뚱한(또는 클러스터 범위)
			// 객체를 지우게 되므로, 유계 GVR 비트로 승급해 회수에 맡깁니다.
			s.markGVRStale(b.entry.gvr)
			return
		}
		s.enqueueKey(b, ns, name)
	default:
		// 알 수 없는 형태입니다. 조용히 낡는 대신 GVR을 stale로 승급합니다.
		s.markGVRStale(b.entry.gvr)
	}
}

// ackCoveredLocked는 목록 스냅숏이 **덮는** 대기 키와 stale 마커를 지웁니다.
//
// snapMu 쓰기 잠금 안에서 호출됩니다(잠금 순서 snapMu → queueMu 그대로).
// covers보다 뒤의 이벤트·마커는 그대로 남습니다 — 그것들은 아직 반영되지 않았습니다.
func (s *Service) ackCoveredLocked(gvr schema.GroupVersionResource, covers uint64) {
	if s.delta == nil {
		return
	}
	s.delta.mu.Lock()
	defer s.delta.mu.Unlock()
	q, ok := s.delta.queues[gvr]
	if !ok {
		return
	}
	keep := q.events[:0]
	for _, ev := range q.events {
		if ev.seq <= covers {
			s.budget.releaseQueued(ev.reserved)
			continue
		}
		keep = append(keep, ev)
	}
	q.events = keep
	q.reindex()
	// 보류분도 같은 규칙입니다. 회수 티켓이 살아 있으면 그쪽이 소유하므로 건드리지 않습니다.
	if s.delta.ticket == nil && len(q.hold) > 0 {
		held := q.hold[:0]
		for _, ev := range q.hold {
			if ev.seq <= covers {
				s.budget.releaseQueued(ev.reserved)
				continue
			}
			held = append(held, ev)
		}
		q.hold = held
		if len(q.hold) == 0 {
			// 배열이 비었습니다. 붙잡아 둘 이유가 없으므로 용량 계상을 되돌립니다.
			q.hold = nil
			s.delta.releaseHoldCapLocked(q)
		}
	}
	if q.markerSeq != 0 && q.markerSeq <= covers {
		// 목록이 마커를 덮었습니다 — namespace별 마커도 **함께** 사라져야 합니다.
		// 집합만 비우고 마커를 남기면 다음 드롭이 옛 지문과 비교되어, 실제로는
		// 입력이 바뀌었는데 회로가 "그대로"라고 판단합니다.
		s.delta.clearStaleLocked(q)
		q.gvrStale = false
		q.markerSeq = 0
		q.staleEpoch++
	}
}

func (s *Service) markGVRStale(gvr schema.GroupVersionResource) {
	if s == nil || s.delta == nil {
		return
	}
	seq := s.eventSeq.Add(1)
	s.delta.mu.Lock()
	defer s.delta.mu.Unlock()
	q := s.delta.queueFor(gvr)
	if q == nil {
		s.forceRebuild(gvr)
		return
	}
	q.gvrStale = true
	q.markerSeq = maxMarker(q.markerSeq, seq)
	q.staleEpoch++
}

/* ── 100ms 루프 ─────────────────────────────────────────────────────────── */

// tickerFactory는 테스트 seam입니다. env·Helm 노브가 아닙니다.
type tickerFactory func(time.Duration) (<-chan time.Time, func())

func defaultTickerFactory(d time.Duration) (<-chan time.Time, func()) {
	t := time.NewTicker(d)
	return t.C, t.Stop
}

// runDeltaLoop는 100ms마다 큐를 비우고 회수를 한 조각씩 진행합니다.
//
// 목록 2s 경로와 **다른 고루틴**이라 서로 굶기지 않고, ctx가 끝나면 즉시 멈춥니다.
func (s *Service) runDeltaLoop(ctx context.Context) {
	factory := s.cfg.NewTicker
	if factory == nil {
		factory = defaultTickerFactory
	}
	c, stop := factory(DeltaTickInterval)
	defer stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-c:
			s.deltaTick(ctx)
		}
	}
}

// nowOrDefault는 시계가 주입되지 않은 서비스(테스트가 직접 만든 경우)에서도
// 안전한 현재 시각입니다.
func (s *Service) nowOrDefault() time.Time {
	if s.cfg.Now != nil {
		return s.cfg.Now()
	}
	return time.Now()
}

// deltaTick은 한 번의 합치기 창을 처리합니다.
func (s *Service) deltaTick(ctx context.Context) {
	if s == nil || s.delta == nil || ctx.Err() != nil {
		return
	}
	budgetLeft := maxBatchEvents
	order := s.order
	if len(order) == 0 {
		return
	}
	s.delta.mu.Lock()
	start := s.delta.rr % len(order)
	s.delta.mu.Unlock()

	// 커서는 **마지막으로 시도한 GVR 다음**으로 옮깁니다. 매 tick 하나씩만 밀면
	// 64개가 전부 포화일 때 한 바퀴에 64 tick이 걸려 뒤쪽 GVR이 굶습니다.
	attempted := 0
	for i := 0; i < len(order) && budgetLeft > 0; i++ {
		if ctx.Err() != nil {
			break
		}
		gvr := order[(start+i)%len(order)]
		used := s.flushSearchDeltas(ctx, gvr, min(budgetLeft, maxBatchPerResource))
		budgetLeft -= used
		attempted++
	}
	if attempted == 0 {
		attempted = 1
	}
	s.delta.mu.Lock()
	s.delta.rr = (start + attempted) % len(order)
	s.delta.mu.Unlock()
	s.advanceRecovery(ctx)
}

/* ── flush ──────────────────────────────────────────────────────────────── */

// deltaTransientBytes는 배치 하나가 잡을 **임시 바이트 전부**의 상한입니다.
//
// 항목마다 드는 것을 빠짐없이 셉니다.
//
//	deltaEventFixedBytes   drained 슬라이스 항목
//	partOpBytes            byNS 슬라이스 항목
//	rowInputBytes          신원·라벨 사본 헤더
//	maxRowTokens*(...)     라벨 토큰 문자열 사본
//	maxRowTokens*2*postOpBytes  applyOps 정렬 scratch(추가/삭제 양쪽)
//
// **경로 복사(COW)가 이 항의 대부분입니다.** 입력·토큰·정렬 scratch만 세면
// 실제 사용량의 10분의 1도 되지 않습니다 — posting 리프, 상위 노드, fence 문자열,
// 행 디렉터리 리프·노드, 트라이 리프·척추, namespace 디렉터리 페이지가 전부
// 적용 도중 **새로 할당**되기 때문입니다. 하나라도 빼면 예약이 실제보다 적어지고
// "예약 안에서만 할당한다"는 약속이 깨집니다.
func deltaTransientBytes(n int) int64 {
	return int64(n)*(deltaScratchPerKeyBytes+deltaCOWPerKeyBytes) + deltaStaleScratchBytes
}

const (
	// partOpBytes는 partOp 하나(이름 헤더 + 입력 포인터)의 보수적 크기입니다.
	partOpBytes = 32
	// rowInputBytes는 rowInput 하나(이름·UID 헤더 + 슬라이스 헤더 + 플래그)의 크기입니다.
	rowInputBytes = 64
	// deltaMapEntryBytes는 byNS 맵 항목 하나(키 헤더 + 슬라이스 헤더 + 버킷 몫)입니다.
	deltaMapEntryBytes = 64
	// maxTokenCopyBytes는 복사되는 토큰 하나의 바이트 상한입니다.
	// 색인에 담기는 토큰은 tokenPrefixBytes에서 잘립니다.
	maxTokenCopyBytes = tokenPrefixBytes

	// deltaScratchPerKeyBytes는 키 하나의 **입력·토큰·정렬 scratch**입니다.
	deltaScratchPerKeyBytes = deltaEventFixedBytes + partOpBytes + rowInputBytes + deltaMapEntryBytes +
		maxRowTokens*(stringHeaderBytes+maxTokenCopyBytes) + // 라벨 토큰 사본
		maxRowTokens*2*postOpBytes // 정렬 scratch(추가/삭제 양쪽)

	// cowMaxDepth는 경로 복사가 지나는 최대 깊이입니다.
	// 파티션 하나의 트리는 fanout 32 · 리프 256이므로 4단계면 2^24 항목을 덮습니다.
	cowMaxDepth = 4
	// postInternalBytes/rowInternalBytes는 내부 노드 **하나**의 크기입니다.
	// 리프당 몫(postNodePerLeafBytes)은 treeNodeMin으로 나눈 값이므로 되돌려 곱합니다.
	postInternalBytes = postNodePerLeafBytes * treeNodeMin
	rowInternalBytes  = rowNodePerLeafBytes * treeNodeMin

	// deltaCOWPerKeyBytes는 키 하나가 일으키는 경로 복사의 상한입니다.
	//
	// 키 하나는 최대 maxRowTokens개의 posting을 건드립니다. 리프는 최대 그 수만큼
	// 복사되고, 각 리프 위로 **내부 노드 하나를 통째로** 잡습니다 — 리프당 몫
	// (postNodePerLeafBytes)은 공유를 가정한 값이라 공유가 없는 배치에서 과소
	// 계상됩니다. 여기에 가장 깊은 경로 한 벌을 더합니다.
	deltaCOWPerKeyBytes = maxRowTokens*(postLeafCapBytes+postInternalBytes) + // posting 리프 + 노드
		cowMaxDepth*postInternalBytes + // 가장 깊은 posting 경로 한 벌
		rowLeafCapBytes + rowInternalBytes + cowMaxDepth*rowInternalBytes + // 행 디렉터리
		trieLeafBytes + trieSpineBytes + // 트라이 리프 + 척추
		nsPartStructBytes + nsDirPageCopyBytes + // 파티션 구조 + ns 디렉터리 페이지
		maxRowTokens*(tokenPrefixBytes+stringHeaderBytes) + // 분할이 만드는 fence 문자열
		cowFreeListBytes

	// nsDirPageCopyBytes는 namespace 디렉터리 페이지 하나의 복사 비용입니다.
	nsDirPageCopyBytes = 24*2 + nsAggBytes + nsPageMax*(stringHeaderBytes+8)
	// cowFreeListBytes는 free-list 노드와 pendingFree 슬라이스 몫입니다.
	cowFreeListBytes = 4 << 10
)

// rejectQueuedLocked는 임시 예약이 거절됐을 때 대기 이벤트를 **명시적 낡음**으로 바꿉니다.
//
// 되돌림 루프 대신 stale + 쿨다운입니다. 드레인 전에 부르므로 inflight로 옮긴
// 적이 없고, 예약은 queued에서 정확히 한 번 풉니다.
func (s *Service) rejectQueuedLocked(q *deltaQueue, want int) int {
	if want > len(q.events) {
		want = len(q.events)
	}
	if want <= 0 {
		return 0
	}
	for _, ev := range q.events[:want] {
		s.budget.releaseQueued(ev.reserved)
		q.dropped++
		q.markerSeq = maxMarker(q.markerSeq, ev.seq)
		s.delta.addStaleLocked(q, ev.namespace, ev.seq)
	}
	q.events = append(q.events[:0], q.events[want:]...)
	q.reindex()
	q.staleEpoch++
	return want
}

// flushSearchDeltas는 GVR 하나의 대기 키를 한 번 적용합니다. 테스트 seam이기도 합니다.
//
// 순서(§5.4): snapMu에서 신원·기준 버전을 집고 **놓은 뒤** queueMu에서 드레인하고,
// 무잠금으로 적용한 다음 snapMu에서 CAS 게시합니다. 두 잠금을 겹쳐 잡지 않습니다.
func (s *Service) flushSearchDeltas(ctx context.Context, gvr schema.GroupVersionResource, limit int) int {
	if s == nil || s.delta == nil || !s.cfg.SearchEnabled || !s.cfg.SearchIncremental {
		return 0
	}
	e, ok := s.entries[gvr]
	if !ok {
		return 0
	}
	// ① snapMu: 신원·기준 버전·informer·인덱스를 집고 곧바로 놓습니다.
	// 델타 적용도 sindex를 훑습니다(COW 경로 복사). 그동안 그 세대가 회계에서
	// 먼저 빠지지 않도록 **명시적으로 빌립니다.** 작업이 끝나야 놓습니다.
	s.snapMu.RLock()
	informer := e.informer
	token := buildToken{lifecycle: e.lifecycle, generation: e.generation}
	lease := e.leasePtr.Load()
	var snap *entrySnapshot
	if lease != nil {
		lease.refs.Add(1)
		snap = lease.snapshot()
	} else {
		snap = e.snapPtr.Load()
	}
	s.snapMu.RUnlock()
	defer s.releaseSearch(lease)
	if informer == nil || snap == nil || snap.sindex == nil {
		return 0
	}
	baseVersion := snap.searchVer
	desc, err := s.describeSnapshot(gvr, snap)
	if err != nil || desc.State != StateReady {
		return 0
	}

	// ② queueMu: 드레인. 회수 중인 파티션을 목표로 하는 키는 보류합니다.
	//
	// **임시 예약이 드레인보다 먼저입니다.** drained 슬라이스·byNS 맵·라벨 사본·
	// applyOps의 정렬 scratch와 경로 복사는 전부 이 예약 안에서만 잡힙니다.
	// 예약보다 먼저 할당하면, 거절되는 배치가 이미 메모리를 다 먹은 뒤입니다.
	s.delta.mu.Lock()
	q := s.delta.queueFor(gvr)
	if q == nil {
		// 큐 구조가 상한 안에 들어가지 않습니다. 증분을 포기하고 전체 재구성으로.
		s.forceRebuild(gvr)
		s.delta.mu.Unlock()
		return 0
	}
	want := len(q.events)
	if want > limit {
		want = limit
	}
	if want == 0 {
		s.delta.mu.Unlock()
		return 0
	}
	// 한 번에 다 못 잡으면 **배치를 줄여서라도 진행합니다.** 예산이 좁다는 이유로
	// 통째로 거절하면 큐는 계속 자라고 결국 드롭으로 이어집니다. 한 건도 못 잡을
	// 때만 진짜 거절입니다.
	var transient int64
	for {
		transient = deltaTransientBytes(want)
		if s.budget.reserveTransient(transient) {
			break
		}
		if want == 1 {
			// 예산 거절입니다. 무한 되돌림 대신 명시적 stale + 쿨다운으로 바꿉니다.
			// 아직 아무것도 드레인하지 않았으므로 큐에서 바로 떼어냅니다.
			n := s.rejectQueuedLocked(q, 1)
			s.delta.cooldownUntil = s.nowOrDefault().Add(recoveryCooldown)
			s.delta.mu.Unlock()
			return n
		}
		want /= 2
	}
	defer s.budget.releaseTransient(transient)
	ticket := s.delta.ticket
	drained := make([]deltaEvent, 0, want)
	keep := q.events[:0]
	for _, ev := range q.events {
		if len(drained) >= want {
			keep = append(keep, ev)
			continue
		}
		// 죽은 세대의 이벤트는 여기서 버립니다(부트스트랩이 그 상태를 이미 덮습니다).
		if ev.gen != 0 && ev.gen != e.tokenPacked.Load() {
			s.budget.releaseQueued(ev.reserved)
			continue
		}
		if ticket.holds(gvr, ev.namespace) {
			// 보류 슬롯도 **넣기 전에** 용량을 확보합니다.
			if len(q.hold) < maxHoldQueue && s.delta.reserveHoldSlotLocked(q) {
				q.hold = append(q.hold, ev)
				continue
			}
			// 보류 상한 초과: 측면 인덱스를 버리고 티켓을 최신 seq로 전진시킨 뒤,
			// 이 GVR의 보류를 풀어 정상 드레인으로 되돌립니다.
			s.abandonRecoveryLocked(q, ev.seq)
			ticket = nil
		}
		drained = append(drained, ev)
	}
	q.events = keep
	q.reindex()
	for _, ev := range drained {
		s.budget.transferQueuedToInflight(ev.reserved)
	}
	// 완전히 비었으면 **도달 가능한 저장 용량**을 되돌립니다. 비었는데도 고수위
	// 배열·버킷이 살아 있으면 원장이 실제보다 적게 말합니다.
	s.delta.compactQueueLocked(q)
	s.delta.mu.Unlock()

	if len(drained) == 0 {
		return 0
	}
	started := time.Now()

	// ③ 무잠금 적용. 키는 **같은 세대의 Store**에서 다시 해석합니다.
	byNS := make(map[string][]partOp, 8)
	store := informer.GetStore()
	var keyBuf []string
	var tokBuf []string
	for i, ev := range drained {
		if i%64 == 0 && ctx.Err() != nil {
			s.requeueDrained(gvr, drained)
			return len(drained)
		}
		key := ev.name
		if ev.namespace != "" {
			key = ev.namespace + "/" + ev.name
		}
		obj, exists, getErr := store.GetByKey(key)
		s.delta.identityLookups.Add(1)
		if getErr != nil || !exists {
			byNS[ev.namespace] = append(byNS[ev.namespace], partOp{name: ev.name})
			continue
		}
		m, castOK := obj.(*metav1.PartialObjectMetadata)
		if !castOK || m == nil {
			byNS[ev.namespace] = append(byNS[ev.namespace], partOp{name: ev.name})
			continue
		}
		row := indexRow{namespace: m.Namespace, name: m.Name, obj: m}
		var truncated bool
		tokBuf, truncated, keyBuf = labelTokensOf(&row, keyBuf, tokBuf)
		labels := make([]string, len(tokBuf))
		copy(labels, tokBuf)
		byNS[ev.namespace] = append(byNS[ev.namespace], partOp{
			name: ev.name,
			input: &rowInput{
				name: m.Name, uid: string(m.UID),
				labels: labels, keysTruncated: truncated,
			},
		})
	}

	var st applyStats
	st.coalescedKeyCount = int64(len(drained))
	next := snap.sindex.applyOps(s.nowOrDefault(), byNS, &st)
	// 회수 대기 상태를 인덱스에도 새깁니다. Get·Recent의 "신선한 파티션" 판정이
	// 인덱스만 보고도 정확해야 하기 때문입니다.
	// 낡음 목록 버퍼는 이 배치의 임시 예약(deltaStaleScratchBytes) 안입니다.
	staleBuf := make([]string, 0, maxStaleTracked)
	if staleNS, gvrStale := s.staleListInto(gvr, staleBuf); len(staleNS) > 0 || gvrStale {
		for _, ns := range staleNS {
			next = next.markStale(ns, s.nowOrDefault())
		}
		if gvrStale {
			marked := *next
			marked.gvrStale = true
			next = &marked
		}
	}
	s.delta.directoryCopies.Add(st.directoryCopies)
	s.delta.visitedRows.Add(st.visitedRows)
	s.delta.postingsChanged.Add(st.postingsChanged)
	s.delta.nodesCopied.Add(st.nodesCopied)
	s.delta.sepBytes.Add(st.sepBytes)

	// ④ snapMu: CAS 게시. I-A/I-B/I-C를 여기서 다시 봅니다.
	//
	// **버전 충돌과 예산 거절을 다르게 다룹니다.** 충돌은 다음 tick에 다시 시도하면
	// 되므로 되돌리지만, 예산 거절은 되돌려 봐야 같은 결과가 100ms마다 반복될 뿐입니다.
	// 그래서 정확히 그 namespace를 stale로 남기고 예약을 풀고 쿨다운에 들어갑니다.
	switch s.publishSearchIndex(e, token, baseVersion, next) {
	case recoveryVersionConflict:
		s.requeueDrained(gvr, drained)
		return len(drained)
	case recoveryBudgetRejected:
		s.delta.mu.Lock()
		rq := s.delta.queueOf(gvr)
		for _, ev := range drained {
			s.budget.releaseInflight(ev.reserved)
			if rq == nil {
				continue
			}
			rq.dropped++
			rq.markerSeq = maxMarker(rq.markerSeq, ev.seq)
			s.delta.addStaleLocked(rq, ev.namespace, ev.seq)
		}
		if rq != nil {
			rq.staleEpoch++
		} else {
			// 큐가 사라졌습니다(폐기). 낡음을 기록할 곳이 없으니 전체 재구성으로.
			s.forceRebuild(gvr)
		}
		s.delta.cooldownUntil = s.nowOrDefault().Add(recoveryCooldown)
		s.delta.mu.Unlock()
		s.delta.publishBudgetRejects.Add(1)
		return len(drained)
	}
	for _, ev := range drained {
		s.budget.releaseInflight(ev.reserved)
	}
	s.delta.mu.Lock()
	if q = s.delta.queueOf(gvr); q != nil {
		q.batches++
		q.batchNanos += time.Since(started).Nanoseconds()
	}
	s.delta.mu.Unlock()
	if st.compactionRequired {
		// 슬롯 압축은 GVR 전체를 다시 세워야 합니다(cluster-scoped 파티션 하나가 아닙니다).
		s.requestGVRResync(gvr, s.eventSeq.Load())
	}
	return len(drained)
}

// requeueDrained는 충돌·취소로 게시하지 못한 키를 큐에 되돌립니다.
//
// **더 새로운 같은 키가 이미 큐에 있으면 되돌린 키를 버립니다** — 그러지 않으면
// 오래된 upsert가 새 delete를 되살립니다. 되돌린 항목은 예약을 그대로 유지하고,
// 버린 항목만 정확히 한 번 해제합니다.
func (s *Service) requeueDrained(gvr schema.GroupVersionResource, drained []deltaEvent) {
	s.delta.mu.Lock()
	defer s.delta.mu.Unlock()
	q := s.delta.queueOf(gvr)
	if q == nil {
		// 되돌릴 큐가 없습니다(폐기·구조 거절). 예약만 정확히 한 번 풀고
		// 전체 재구성으로 되돌립니다 — 예약을 흘리지 않습니다.
		for _, ev := range drained {
			s.budget.releaseInflight(ev.reserved)
		}
		s.forceRebuild(gvr)
		return
	}
	// restored는 이 배치의 임시 예약 안입니다(키당 deltaEventFixedBytes).
	restored := make([]deltaEvent, 0, len(drained))
	for _, ev := range drained {
		if _, newer := q.index[ev.key()]; newer {
			s.budget.releaseInflight(ev.reserved) // 더 새로운 이벤트가 이겼습니다.
			continue
		}
		if len(restored)+len(q.events) >= maxPendingPerResource {
			s.budget.releaseInflight(ev.reserved)
			q.dropped++
			q.markerSeq = maxMarker(q.markerSeq, ev.seq)
			q.staleEpoch++
			s.delta.addStaleLocked(q, ev.namespace, ev.seq)
			continue
		}
		s.budget.transferInflightToQueued(ev.reserved)
		restored = append(restored, ev)
	}
	if len(restored) == 0 {
		q.reindex()
		s.delta.compactQueueLocked(q)
		return
	}
	// **큐 저장 용량을 먼저 확보한 뒤** 자리를 옮깁니다. append로 새 배열을 만들면
	// 그 용량이 승인 밖에서 생깁니다. 확보하지 못하면 되돌린 것들을 명시적
	// 낡음으로 바꿉니다 — 예약을 흘리지 않습니다.
	total := len(restored) + len(q.events)
	if !s.delta.growEventsLocked(q, total) || !s.delta.reserveIndexCapLocked(q, total) {
		for _, ev := range restored {
			s.budget.releaseQueued(ev.reserved)
			q.dropped++
			q.markerSeq = maxMarker(q.markerSeq, ev.seq)
			s.delta.addStaleLocked(q, ev.namespace, ev.seq)
		}
		q.staleEpoch++
		q.reindex()
		return
	}
	buf := q.events[:total]
	copy(buf[len(restored):], q.events) // 기존 항목을 뒤로 밀고
	copy(buf[:len(restored)], restored) // 되돌린 항목을 앞에 둡니다
	q.events = buf
	q.reindex()
}

/* ── 회수 ───────────────────────────────────────────────────────────────── */

// requestResync는 **그 namespace 하나**의 회수를 예약합니다.
//
// 빈 문자열은 "전체"가 아니라 **cluster-scoped 파티션**입니다. 그 둘을 같은 값으로
// 쓰면 cluster-scoped 하나가 GVR 전체를 stale로 만들어 버립니다.
func (s *Service) requestResync(gvr schema.GroupVersionResource, ns string, seq uint64) {
	if s == nil || s.delta == nil {
		return
	}
	s.delta.mu.Lock()
	defer s.delta.mu.Unlock()
	q := s.delta.queueFor(gvr)
	if q == nil {
		s.forceRebuild(gvr)
		return
	}
	q.markerSeq = maxMarker(q.markerSeq, seq)
	q.staleEpoch++
	s.delta.addStaleLocked(q, ns, seq)
}

// requestGVRResync는 GVR 전체 회수를 예약합니다(형식 불명 tombstone·슬롯 압축 등).
func (s *Service) requestGVRResync(gvr schema.GroupVersionResource, seq uint64) {
	if s == nil || s.delta == nil {
		return
	}
	s.delta.mu.Lock()
	defer s.delta.mu.Unlock()
	q := s.delta.queueFor(gvr)
	if q == nil {
		s.forceRebuild(gvr)
		return
	}
	q.markerSeq = maxMarker(q.markerSeq, seq)
	q.staleEpoch++
	q.gvrStale = true
}

// abandonRecoveryLocked는 보류 상한 초과 시 티켓을 버립니다.
// 티켓을 **최신 seq로 전진**시키고, 다음 목록 스냅숏이 그 seq를 덮을 때까지 기다립니다.
//
// **소유권 확인**: 지금 살아 있는 티켓이 그 티켓일 때만 상태를 건드립니다.
func (s *Service) abandonRecoveryLocked(q *deltaQueue, seq uint64) {
	t := s.delta.ticket
	if t == nil {
		return
	}
	// 보류분을 버리지 않고 큐로 되돌립니다. 상한을 넘긴 것만 stale이 됩니다.
	// 배열은 큐에서 떠나므로 그 용량 계상도 함께 되돌립니다.
	held := s.delta.takeHoldLocked(q)
	s.mergeHeldLocked(q, held)
	q.markerSeq = maxMarker(q.markerSeq, seq)
	q.staleEpoch++
	if t.wholeGVR {
		q.gvrStale = true
	} else {
		// 보류 상한 초과는 **새 입력**입니다(그 사이 이벤트가 실제로 더 왔습니다).
		// 지문을 전진시켜 회로가 다시 시도할 자격을 갖게 합니다.
		s.delta.addStaleLocked(q, t.namespace, seq)
	}
	s.dropTicketLocked(t)
	s.delta.recoveryFailures.Add(1)
}

// releaseHeldLocked는 보류분을 버리고 예약을 정확히 한 번 해제합니다.
func (s *Service) releaseHeldLocked(q *deltaQueue) {
	if q == nil {
		return
	}
	for _, ev := range q.hold {
		s.budget.releaseQueued(ev.reserved)
	}
	// 배열까지 버리므로 **그 용량 계상도 함께** 되돌립니다.
	q.hold = nil
	s.delta.releaseHoldCapLocked(q)
}

// dropTicketLocked는 티켓을 버리고 그 예약을 **정확히 한 번** 해제합니다.
//
// 작업자가 들고 있을지 모르는 side/src/store는 건드리지 않습니다 — 작업자는
// 자기 지역 사본만 쓰고, 커밋 때 step 대조에 걸려 결과를 버립니다. 여기서
// 그 포인터들을 nil로 만들면 작업 중인 메모리를 밑에서 빼는 셈이 됩니다.
func (s *Service) dropTicketLocked(t *recoveryTicket) {
	if t == nil || t.dead {
		if t != nil && s.delta.ticket == t {
			s.delta.ticket = nil
		}
		return
	}
	t.dead = true
	// 티켓 구조체의 계상은 **여기서 정확히 한 번** 되돌립니다. 예약(reserved)과
	// 달리 작업자와 무관합니다 — 구조체는 지역 사본이 아니라 티켓 자신입니다.
	if t.structCharged {
		s.budget.releaseQueued(recoveryTicketBytes)
		t.structCharged = false
	}
	// 작업자가 아직 지역 사본을 들고 있으면 예약 해제를 미룹니다. 마지막 작업자가
	// 빠질 때 workerDoneLocked가 정확히 한 번 풉니다.
	s.maybeReleaseTicketLocked(t)
	t.step++
	t.holdActive = false
	t.phase = recoveryIdle
	if s.delta.ticket == t {
		s.delta.ticket = nil
	}
}

// maybeReleaseTicketLocked는 **작업자가 모두 빠졌을 때만** 예약을 풉니다.
func (s *Service) maybeReleaseTicketLocked(t *recoveryTicket) {
	if t == nil || t.workers > 0 || t.reserved <= 0 {
		return
	}
	s.budget.releaseRecovery(t.reserved)
	t.reserved = 0
	// 조각·보류 예약도 함께 사라집니다. 남겨 두면 되살아난 티켓이 "이미 확보했다"고
	// 착각해 예약 없이 할당합니다.
	t.chunkReserved, t.heldReserved = 0, 0
}

// workerDoneLocked는 작업자 하나가 지역 사본을 놓았음을 기록합니다.
func (s *Service) workerDoneLocked(t *recoveryTicket) {
	if t == nil {
		return
	}
	if t.workers > 0 {
		t.workers--
	}
	if t.dead {
		s.maybeReleaseTicketLocked(t)
	}
}

// mergeHeldLocked는 분리해 둔 보류분을 현재 큐와 **키별 최신 seq 우선**으로 합칩니다.
//
// 회수가 실패했을 때만 부릅니다. 진 쪽의 예약은 정확히 한 번 해제하고, 큐 상한을
// 넘으면 조용히 버리지 않고 stale + 쿨다운으로 바꿉니다.
func (s *Service) mergeHeldLocked(q *deltaQueue, held []deltaEvent) {
	if len(held) == 0 {
		return
	}
	for _, ev := range held {
		key := ev.key()
		if at, dup := q.index[key]; dup && at < len(q.events) {
			cur := q.events[at]
			if cur.seq >= ev.seq {
				s.budget.releaseQueued(ev.reserved) // 큐 쪽이 더 새롭습니다.
				continue
			}
			s.budget.releaseQueued(cur.reserved) // 보류 쪽이 더 새롭습니다.
			q.events[at] = ev
			continue
		}
		if len(q.events) >= maxPendingPerResource ||
			!s.delta.reserveEventSlotLocked(q) || !s.delta.reserveIndexSlotLocked(q) {
			s.budget.releaseQueued(ev.reserved)
			q.dropped++
			q.markerSeq = maxMarker(q.markerSeq, ev.seq)
			q.staleEpoch++
			s.delta.addStaleLocked(q, ev.namespace, ev.seq)
			s.delta.cooldownUntil = s.nowOrDefault().Add(recoveryCooldown)
			continue
		}
		q.index[key] = len(q.events)
		q.events = append(q.events, ev)
	}
	q.reindex()
	s.delta.compactQueueLocked(q)
}

// advanceRecovery는 회수를 한 조각 진행합니다. 서비스 전체에서 동시에 하나뿐입니다.
//
// 티켓 하나는 처음 고정한 **원본 하나**만 씁니다. 목록 스냅숏이 중간에 바뀌어도
// 조각들이 서로 다른 시점을 섞지 않습니다.
func (s *Service) advanceRecovery(ctx context.Context) {
	if s == nil || s.delta == nil || !s.cfg.SearchEnabled || !s.cfg.SearchIncremental {
		return
	}
	now := s.nowOrDefault()

	// ── 캡처 ①: 티켓 신원과 지금 필요한 단계를 잠금 아래에서 집습니다. ─────────
	s.delta.mu.Lock()
	t := s.delta.ticket
	if t == nil {
		t = s.pickRecoveryLocked(now)
		s.delta.ticket = t
	}
	if t == nil || t.dead || now.Before(t.notBefore) {
		s.delta.mu.Unlock()
		return
	}
	// 정숙 조건: 이 GVR의 대기가 상한의 25% 미만일 때만 시작·진행합니다.
	if tq := s.delta.queueOf(t.gvr); tq != nil && len(tq.events) > maxPendingPerResource/4 {
		s.delta.mu.Unlock()
		return
	}
	step := t.step
	phase := t.phase
	gvr, namespace, whole, markerSeq := t.gvr, t.namespace, t.wholeGVR, t.markerSeq
	var work recoveryStep
	workerHeld := false
	if phase == recoveryBuilding {
		work = recoveryStep{src: t.src, side: t.side, lo: t.nextRow, hi: t.hi}
		// 이 시점부터 잠금 밖에서 티켓의 지역 사본을 듭니다. 폐기가 끼어들어도
		// **예약은 우리가 놓을 때까지** 살아 있어야 합니다.
		t.workers++
		workerHeld = true
	}
	s.delta.mu.Unlock()

	defer func() {
		if !workerHeld {
			return
		}
		s.delta.mu.Lock()
		s.workerDoneLocked(t)
		s.delta.mu.Unlock()
	}()

	e, ok := s.entries[gvr]
	if !ok {
		s.abortRecovery(t, step)
		return
	}

	// ── ① 원본 고정 (한 번만) ──────────────────────────────────────────────
	if phase != recoveryBuilding {
		// **스냅숏을 잠금 밖에서 쓰려면 소유권이 있어야 합니다.**
		//
		// 예전에는 RLock 안에서 포인터만 집고 놓은 뒤, 그 스냅숏의 index·sindex를
		// 계속 읽었습니다. 그 사이 게시가 세대를 은퇴시키면 회계는 먼저 빠지고
		// 우리는 회수 입력을 죽은 세대에서 떠 오게 됩니다. 여기서 **지금 게시된
		// 세대를 빌리고** 이 단계가 끝날 때 정확히 한 번 놓습니다.
		s.snapMu.RLock()
		token := buildToken{lifecycle: e.lifecycle, generation: e.generation}
		informer := e.informer
		lease := e.leasePtr.Load()
		var snap *entrySnapshot
		if lease != nil {
			lease.refs.Add(1)
			snap = lease.snapshot()
		} else {
			snap = e.snapPtr.Load()
		}
		s.snapMu.RUnlock()
		defer s.releaseSearch(lease)
		if snap == nil || snap.index == nil || snap.sindex == nil || informer == nil {
			s.abortRecovery(t, step)
			return
		}
		if snap.coversThroughSeq < markerSeq {
			// 게이트: 목록 스냅숏이 마커 seq를 **덮어야** 합니다(>=).
			s.delta.mu.Lock()
			if s.ticketAliveLocked(t, step) {
				t.phase = recoveryWaitingCover
			}
			s.delta.mu.Unlock()
			return
		}
		desc, err := s.describeSnapshot(gvr, snap)
		if err != nil || desc.State != StateReady {
			return
		}
		lo, hi := 0, len(snap.index.rows)
		if !whole {
			sp := snap.index.namespaceSpan(namespace)
			lo, hi = sp.lo, sp.hi
		}
		partVersion := uint64(0)
		if !whole {
			if part := snap.sindex.dir.find(namespace); part != nil {
				partVersion = part.partVersion
			}
		}
		// **대상 데이터에서 직접** 예약을 계산합니다. 행 수만 곱하면 이름·UID·label
		// 바이트가 빠져 긴 이름·포화 label 대상에서 예약이 실제보다 작아집니다.
		//
		//	owned  이 티켓이 소유할 행 사본 — snap.index 전체 백킹을 붙잡지 않습니다.
		//	side   완성될 측면 인덱스의 보유 몫
		//	chunk  조각 하나의 scratch/COW
		//
		// namespace 대상이 t.src = snap.index를 그대로 들면 **GVR 전체 백킹**이
		// 티켓 수명 내내 살아 있습니다. 그래서 구간만 소유 사본으로 떠서 씁니다.
		// ── 1차: **재지 않고는 예약할 수 없습니다.** ─────────────────────────
		// 정규화 토큰까지 담은 소유 사본의 크기를 먼저 잽니다. 이 패스는 아무것도
		// 붙잡지 않습니다(재사용 버퍼만 씁니다).
		ownedBytes, sideBytes := recoverySpanCost(snap.index, lo, hi)
		chunkRows := hi - lo
		if chunkRows > recoveryChunkRows {
			chunkRows = recoveryChunkRows
		}
		chunkBytes := recoveryChunkReserveBytes(chunkRows)
		need := ownedBytes + sideBytes + chunkBytes
		store := informer.GetStore()

		s.delta.mu.Lock()
		if !s.ticketAliveLocked(t, step) {
			s.delta.mu.Unlock()
			return // 폐기·재시작이 걷어갔습니다. 아무것도 예약하지 않았습니다.
		}
		// 원본 크기가 줄었으면 회로를 먼저 닫습니다(같은 예산에 들어갈 여지가 생겼습니다).
		pinCircuit, _ := s.delta.circuitForQ(s.delta.queueOf(gvr), t.target())
		pinCircuit.noteRows(hi - lo)
		if !s.budget.reserveRecovery(need) {
			s.markBudgetRejectedLocked(t, need, hi-lo)
			s.delta.mu.Unlock()
			return
		}
		t.reserved = need
		// 최초 예약에 조각 하나가 이미 들어 있습니다. 같은 값을 chunkReserved에
		// 실어 두어야 growTicketReservation이 같은 몫을 두 번 잡지 않습니다.
		t.chunkReserved = chunkBytes
		t.token, t.store = token, store
		t.srcCovers, t.srcIndexVer, t.srcSearchVer = snap.coversThroughSeq, snap.indexVer, snap.searchVer
		t.partVersion = partVersion
		t.phase = recoveryBuilding
		t.holdActive = true // cluster-scoped(namespace "")도 이 플래그로 보류됩니다.
		// **초기 복사도 작업자입니다.**
		//
		// 잠금을 놓고 사본을 뜨는 동안 폐기가 끼어들면, workers가 0이라
		// maybeReleaseTicketLocked가 예약을 먼저 풀어 버립니다. 우리는 그 예약
		// 안에서 계속 할당하고 있는데 원장은 이미 놓았다고 말하는 상태입니다.
		// 기존 지연 해제 경로(workerDoneLocked)를 그대로 재사용합니다.
		t.workers++
		workerHeld = true
		barrier := s.delta.copyBarrier
		s.delta.mu.Unlock()

		// ── 2차: **예약이 통과한 뒤에** 깊은 소유 사본과 측면 빌더를 만듭니다. ──
		//
		// indexRow를 얕게 복사하면 *PartialObjectMetadata가 그대로 살아남아
		// annotations·ownerReferences·finalizers까지 티켓 수명 내내 붙잡힙니다.
		// 회수에 필요한 것은 이름·UID·정규화 토큰·절단 여부뿐이므로 그것만 뜹니다.
		if barrier != nil {
			barrier() // 테스트 seam: 이 구간을 결정적으로 붙잡습니다.
		}
		owned := copyRecoveryRows(snap.index, lo, hi)
		side := newSearchIndex(desc.Kind, desc.Namespaced)

		s.delta.mu.Lock()
		if !s.ticketAliveLocked(t, step) {
			// 복사하는 사이에 걷혔습니다. 예약은 우리가 작업자에서 빠질 때
			// (지연 해제 경로에서) 정확히 한 번 풀립니다 — 되살리지 않습니다.
			s.delta.mu.Unlock()
			return
		}
		t.src = owned
		t.side = side
		t.nextRow, t.hi = 0, len(owned)
		t.step++
		s.delta.mu.Unlock()
		s.delta.recoveryAttempts.Add(1)
		return // 다음 tick에서 첫 조각을 쌓습니다(핀과 계산을 한 tick에 섞지 않습니다).
	}

	// ── ② 조각 하나를 **지역 사본으로** 계산합니다 ─────────────────────────
	if work.src == nil || work.side == nil {
		// 핀은 끝났지만 **깊은 소유 사본이 아직 채워지지 않았습니다**(예약 뒤,
		// 잠금 밖 복사 중). 티켓을 버리지 않고 다음 tick에 이어갑니다 —
		// 여기서 abort하면 방금 예약을 받은 정상 티켓을 스스로 죽입니다.
		return
	}
	// **조각 하나가 잡을 입력·경로 복사를 할당 전에 확보합니다.**
	//
	// 티켓의 최초 예약에는 조각 하나 몫이 이미 들어 있지만, 이 조각이 그보다 크게
	// 잡을 여지가 있으면 **모자란 만큼 티켓 예약을 늘린 뒤에** 계산을 시작합니다.
	// 늘리지 못하면 예약 없이 할당하지 않고, 명시적 예산 거절로 끝냅니다.
	chunkRows := work.hi - work.lo
	if chunkRows > recoveryChunkRows {
		chunkRows = recoveryChunkRows
	}
	if ok := s.growTicketReservation(t, step, chunkRows); !ok {
		s.failRecovery(t, step, recoveryBudgetRejected)
		return
	}

	deadline := time.Now().Add(recoverySliceBudget)
	byNS := make(map[string][]partOp, 4)
	next := work.lo
	processed := 0
	for next < work.hi && processed < recoveryChunkRows {
		if ctx.Err() != nil {
			return // 취소는 아무것도 커밋하지 않습니다. 예약은 티켓이 계속 소유합니다.
		}
		if processed%256 == 0 && time.Now().After(deadline) {
			break
		}
		// 토큰은 핀 단계에서 이미 정규화되어 **티켓이 소유**하고 있습니다.
		// 조각마다 다시 정규화하지 않으므로 여기서 새로 잡는 것은 partOp/rowInput뿐입니다.
		row := &work.src[next]
		byNS[row.namespace] = append(byNS[row.namespace], partOp{
			name: row.name,
			input: &rowInput{
				name: row.name, uid: row.uid,
				labels: row.labels, keysTruncated: row.truncated,
			},
		})
		next++
		processed++
	}
	built := work.side
	if len(byNS) > 0 {
		var st applyStats
		built = work.side.applyOps(s.nowOrDefault(), byNS, &st)
		if st.slotExhausted {
			s.failRecovery(t, step, recoveryBudgetRejected)
			return
		}
	}
	if ctx.Err() != nil {
		return
	}

	// ── 커밋 ②: step이 그대로일 때만 넘깁니다. ─────────────────────────────
	s.delta.mu.Lock()
	if !s.ticketAliveLocked(t, step) {
		s.delta.mu.Unlock()
		return // 오래된 조각입니다. 버립니다.
	}
	t.side, t.nextRow = built, next
	t.step++
	done := next >= t.hi
	if !done {
		s.delta.mu.Unlock()
		return
	}
	// ── 캡처 ③: 보류분을 **원자적으로 분리**해 소유권을 가져옵니다. ─────────
	//
	// step은 **증가시킨 뒤의 값**을 들고 나가야 합니다. 증가 전 값을 들고 나가면
	// settle의 소유권 검사가 언제나 실패해 티켓이 정리되지 않고 남습니다.
	held := s.delta.takeHoldLocked(s.delta.queueOf(gvr))
	side := t.side
	ticketToken := t.token
	partVersion := t.partVersion
	srcSearchVer := t.srcSearchVer
	store := t.store
	t.step++
	step = t.step
	s.delta.mu.Unlock()

	// ── ③ 게시 ─────────────────────────────────────────────────────────────
	//
	// **보류분 적용도 할당입니다.** 게시 직전에 보류 키를 측면 인덱스에 얹는
	// 경로 복사가 일어나므로, 그 몫을 여기서 확보한 뒤에 들어갑니다.
	if len(held) > 0 {
		if ok := s.growHeldReservation(t, step, len(held)); !ok {
			// 예약을 못 받았습니다. 보류분은 큐로 되돌리고 명시적 stale로 끝냅니다.
			s.settleRecovery(t, step, recoveryBudgetRejected, held)
			return
		}
	}
	outcome := s.publishRecovery(e, recoveryPublishInput{
		token: ticketToken, whole: whole, namespace: namespace,
		side: side, partVersion: partVersion, srcSearchVer: srcSearchVer,
		store: store, held: held,
	})
	s.settleRecovery(t, step, outcome, held)
}

// growTicketReservation은 이번 조각이 필요로 하는 만큼 티켓 예약을 **미리** 늘립니다.
//
// 이미 확보한 몫으로 충분하면 아무것도 하지 않습니다. 모자라면 그 차이를
// I-C 안에서 예약하고 티켓 소유로 옮깁니다 — 실패하면 false이고, 호출자는
// 아무것도 할당하지 않은 채 명시적 예산 거절로 끝냅니다.
func (s *Service) growTicketReservation(t *recoveryTicket, step uint64, chunkRows int) bool {
	want := recoveryChunkReserveBytes(chunkRows)
	s.delta.mu.Lock()
	if !s.ticketAliveLocked(t, step) {
		s.delta.mu.Unlock()
		return true // 죽은 티켓입니다. 커밋에서 걸러집니다.
	}
	have := t.chunkReserved
	if have >= want {
		s.delta.mu.Unlock()
		return true
	}
	delta := want - have
	if !s.budget.reserveRecovery(delta) {
		needed := t.reserved + want
		s.markBudgetRejectedLocked(t, needed, t.hi-t.nextRow)
		s.delta.mu.Unlock()
		return false
	}
	t.reserved += delta
	t.chunkReserved = want
	s.delta.mu.Unlock()
	return true
}

// growHeldReservation은 **보류분 적용에 들어가기 전에** 그 몫을 확보합니다.
//
// 조각 예약(chunkReserved)과 **분리된 항**입니다. 보류 적용은 조각보다 훨씬 많은
// 것을 잡으므로(토큰 사본·맵·다중 파티션 COW·새 행) 같은 항에 섞으면 max 규칙에
// 가려 실제 필요분이 예약되지 않습니다. 늘린 몫은 t.reserved에 합쳐지므로
// 게시·거절·충돌·폐기·취소 어느 경로로 끝나든 **정확히 한 번** 풀립니다.
func (s *Service) growHeldReservation(t *recoveryTicket, step uint64, n int) bool {
	want := heldApplyReserveBytes(n)
	s.delta.mu.Lock()
	if !s.ticketAliveLocked(t, step) {
		s.delta.mu.Unlock()
		return true // 죽은 티켓입니다. 커밋에서 걸러집니다.
	}
	if t.heldReserved >= want {
		s.delta.mu.Unlock()
		return true
	}
	delta := want - t.heldReserved
	if !s.budget.reserveRecovery(delta) {
		s.markBudgetRejectedLocked(t, t.reserved+want, t.hi-t.nextRow)
		s.delta.mu.Unlock()
		return false
	}
	t.reserved += delta
	t.heldReserved = want
	s.delta.mu.Unlock()
	return true
}

// abortRecovery는 진행할 수 없는 티켓을 잠금 아래에서 걷어냅니다.
func (s *Service) abortRecovery(t *recoveryTicket, step uint64) {
	s.delta.mu.Lock()
	defer s.delta.mu.Unlock()
	if !s.ticketAliveLocked(t, step) {
		return
	}
	if q := s.delta.queueOf(t.gvr); q != nil {
		s.releaseHeldLocked(q)
	}
	s.dropTicketLocked(t)
}

// failRecovery는 게시 이전 단계의 실패를 처리합니다(보류분은 아직 큐가 소유합니다).
func (s *Service) failRecovery(t *recoveryTicket, step uint64, outcome recoveryOutcome) {
	s.delta.mu.Lock()
	defer s.delta.mu.Unlock()
	if !s.ticketAliveLocked(t, step) {
		return
	}
	held := s.delta.takeHoldLocked(s.delta.queueOf(t.gvr))
	s.settleLocked(t, outcome, held)
}

// markBudgetRejectedLocked는 예산 거절을 **명시적 stale + 쿨다운**으로 바꿉니다.
// 100ms마다 다시 시도하는 무한 되돌림이 되지 않게 하는 지점입니다.
func (s *Service) markBudgetRejectedLocked(t *recoveryTicket, needed int64, rows int) {
	q := s.delta.queueOf(t.gvr)
	target := t.target()
	switch {
	case q == nil:
		// 큐가 사라졌습니다. 낡음을 기록할 곳이 없으니 전체 재구성으로 되돌립니다.
		s.forceRebuild(t.gvr)
	case t.wholeGVR:
		q.gvrStale = true
	default:
		s.delta.restakeLocked(q, t.namespace)
	}
	now := s.nowOrDefault()
	// 회로에 기록합니다 — 티켓이 사라져도 백오프와 "같은 입력이면 재시도 없음"이 남습니다.
	// 지문은 **그 대상의** 마커입니다. GVR 전체 마커를 쓰면 옆 namespace의 드롭이
	// 이 회로를 대신 닫아 버립니다.
	// 동적 회로를 못 만들어도 **내장 fallback**이 백오프를 기억합니다.
	// 회로가 nil이면 fail이 no-op이 되어 같은 회수를 쿨다운마다 반복합니다.
	c, eff := s.delta.circuitForQ(q, target)
	c.fail(now, q.markerFor(eff), rows, needed, s.budget.limit(), s.availableFor(t.gvr), true)
	t.lastNeeded = needed
	s.dropTicketLocked(t)
	s.delta.recoveryFailures.Add(1)
	s.delta.cooldownUntil = now.Add(c.backoffOr(recoveryCooldown))
}

// pickRecoveryLocked는 다음 회수 대상을 고릅니다. queueMu를 잡고 있어야 합니다.
func (s *Service) pickRecoveryLocked(now time.Time) *recoveryTicket {
	if now.Before(s.delta.cooldownUntil) {
		return nil
	}
	budgetMax := s.budget.limit()
	n := len(s.order)
	if n == 0 {
		return nil
	}
	// **라운드로빈으로 시작점을 옮깁니다.** 언제나 order[0]부터 훑으면, 앞선 GVR이
	// 계속 낡아 있는 동안 뒤쪽 GVR은 영영 잡히지 않습니다(굶음).
	start := s.delta.recoveryRR % n
	for off := 0; off < n; off++ {
		idx := (start + off) % n
		gvr := s.order[idx]
		q, ok := s.delta.queues[gvr]
		if !ok {
			continue
		}
		// 후보는 최대 둘입니다. **슬라이스를 새로 잡지 않습니다** — 고르는 일만으로
		// 예약 밖 메모리를 만들지 않기 위해서입니다.
		var candidates [2]recoveryTarget
		nc := 0
		if q.gvrStale {
			candidates[nc] = recoveryTarget{gvr: gvr, whole: true}
			nc++
		}
		// **GVR이 통째로 낡았으면 namespace 후보를 내지 않습니다.**
		//
		// 전체 재구성이 그 namespace까지 덮으므로 따로 회수해도 gvrStale은 남습니다 —
		// 낭비일 뿐 아니라, 전체 대상이 백오프 중일 때 namespace 대상으로 우회해
		// 같은 일을 반복하는 통로가 됩니다.
		if !q.gvrStale && q.staleNS.count > 0 {
			// namespace 대상도 회전시킵니다. 하나만 계속 고르면 같은 GVR의 다른
			// namespace가 굶습니다. at()은 정렬·할당 없이 k번째 슬롯을 집습니다.
			if pick, ok := q.staleNS.at(s.delta.recoveryRR); ok {
				candidates[nc] = recoveryTarget{gvr: gvr, namespace: pick}
				nc++
			}
		}
		for ci := 0; ci < nc; ci++ {
			target := candidates[ci]
			// **회로는 티켓보다 오래 삽니다.** 입력도 예산도 그대로면 잡지 않습니다.
			// 동적 회로를 못 만들면 큐 내장 fallback이 대신 백오프를 기억합니다 —
			// 회로가 nil이면 allows가 언제나 참이 되어 폭풍이 됩니다.
			// 회로가 승급되면 **대상 자체가 전체 대상이 됩니다.** 이후의 행 수·마커·
			// 티켓 필드·보류·ack이 전부 그 의미를 따라야 합니다.
			c, eff := s.delta.circuitForQ(q, target)
			// 선택자에 걸리기 **전에** 입력 축소를 관찰합니다. 행 수가 줄었으면
			// 같은 예산에 들어갈 여지가 생긴 것이므로 회로를 닫아야 합니다.
			// 전체 스캔이 아니라 정렬 인덱스의 **namespace 구간 두 번의 이분 탐색**입니다.
			c.noteRows(s.targetRowSpan(eff))
			if !c.allows(now, q.markerFor(eff), budgetMax, s.availableFor(gvr)) {
				continue
			}
			// 티켓 구조도 **만들기 전에** 승인받습니다.
			if !s.budget.admitStructural(recoveryTicketBytes) {
				return nil
			}
			s.delta.recoveryRR = idx + 1
			// **적용된 대상의 마커**를 캡처합니다. 전체 대상이면 전역 마커,
			// namespace 대상이면 그 namespace의 마커입니다.
			return &recoveryTicket{
				gvr: eff.gvr, namespace: eff.namespace, wholeGVR: eff.whole,
				epoch: q.staleEpoch, markerSeq: q.markerFor(eff), markerCaptured: true,
				notBefore: now, structCharged: true,
			}
		}
	}
	// 아무것도 잡지 못했어도 커서는 전진시킵니다. 그래야 다음 tick이 다른
	// 시작점에서 훑고, 막힌 GVR이 커서를 붙잡아 두지 않습니다.
	s.delta.recoveryRR = start + 1
	return nil
}

// targetRowSpan은 대상이 덮는 행 수입니다. **유계 메타데이터만** 읽습니다.
//
// namespace 대상은 정렬 인덱스의 구간(이분 탐색 두 번), GVR 전체 대상은 행 수입니다.
// 여기서 전체 스캔을 하면 회로를 확인하는 것만으로 tick 예산을 다 씁니다.
func (s *Service) targetRowSpan(target recoveryTarget) int {
	e := s.entries[target.gvr]
	if e == nil {
		return 0
	}
	idx := e.baselineIndex()
	if idx == nil {
		return 0
	}
	if target.whole {
		return len(idx.rows)
	}
	sp := idx.namespaceSpan(target.namespace)
	return sp.hi - sp.lo
}

// settleRecovery는 게시 결과를 정리합니다. **분리해 둔 보류분의 소유자는 호출자**이고,
// 여기서 정확히 한 번 처리합니다.
//
// 티켓이 이미 걷혔더라도 보류분은 반드시 정리해야 합니다 — 그러지 않으면 예약이
// 영원히 남습니다. 그래서 소유권 검사는 **티켓 상태 변경**에만 걸고, 보류분은
// 언제나 처리합니다.
func (s *Service) settleRecovery(t *recoveryTicket, step uint64, outcome recoveryOutcome, held []deltaEvent) {
	s.delta.mu.Lock()
	defer s.delta.mu.Unlock()
	if !s.ticketAliveLocked(t, step) {
		// 폐기·재시작이 걷어갔습니다. 티켓은 되살리지 않되, 우리가 들고 있던
		// 보류분의 예약은 여기서 정확히 한 번 풉니다.
		for _, ev := range held {
			s.budget.releaseQueued(ev.reserved)
		}
		return
	}
	s.settleLocked(t, outcome, held)
}

// finishRecovery는 보류분을 잠금 아래에서 분리해 정리까지 한 번에 합니다.
// 게시를 거치지 않는 단순 경로(테스트·즉시 실패)에서 씁니다.
func (s *Service) finishRecovery(t *recoveryTicket, outcome recoveryOutcome) {
	s.delta.mu.Lock()
	defer s.delta.mu.Unlock()
	if s.delta.ticket != t || t.dead {
		return // 이미 걷혔습니다. 되살리지 않습니다.
	}
	held := s.delta.takeHoldLocked(s.delta.queueOf(t.gvr))
	s.settleLocked(t, outcome, held)
}

// settleLocked는 성공·실패 공통 정리입니다. delta.mu를 잡고 있어야 합니다.
//
// 성공 처리는
//   - namespace 회수: 그 namespace 마커 **하나만** 지웁니다.
//   - GVR 전체 회수: 그때만 전부 지웁니다.
//
// 로 나뉩니다. 하나가 나머지를 지우면 아직 회수하지 않은 namespace가 조용히
// "깨끗함"으로 바뀝니다.
// namespaceAckAllowedLocked는 namespace 회수의 ack이 성립하는지입니다.
//
// 캡처된 티켓은 **그 namespace의 마커**만 봅니다 — 옆 namespace에서 이벤트가
// 떨어져도 이쪽 ack은 그대로여야 하고(대상 지역성), 같은 namespace의 더 새로운
// 이벤트는 ack을 막아야 합니다(유실 금지).
//
// 지문을 캡처하지 않은 티켓은 비교할 기준이 없으므로 전역 epoch로 되돌아갑니다.
// 그쪽이 **더 보수적**입니다: 무관한 이벤트에도 ack을 보류할 뿐, 아직 낡은 것을
// 깨끗하다고 말하지는 않습니다.
func (s *Service) namespaceAckAllowedLocked(q *deltaQueue, t *recoveryTicket) bool {
	if t.markerCaptured {
		return q.markerFor(t.target()) == t.markerSeq
	}
	return q.staleEpoch == t.epoch
}

func (s *Service) settleLocked(t *recoveryTicket, outcome recoveryOutcome, held []deltaEvent) {
	q := s.delta.queueOf(t.gvr)
	if q == nil {
		// 큐가 사라졌습니다(폐기·구조 거절). 보류분의 예약만 정확히 한 번 풀고,
		// 티켓을 걷어낸 뒤 전체 재구성으로 되돌립니다.
		for _, ev := range held {
			s.budget.releaseQueued(ev.reserved)
		}
		s.forceRebuild(t.gvr)
		s.dropTicketLocked(t)
		return
	}
	if outcome == recoveryPublished {
		// **ack 조건은 대상별입니다.**
		//
		//	전체 대상    : 전역 epoch가 그대로일 때만(그때만 전부 지웁니다)
		//	namespace 대상: **그 namespace의 마커**가 그대로일 때만
		//
		// namespace 대상에 전역 epoch를 쓰면, 회수 도중 옆 namespace에서 이벤트가
		// 하나만 떨어져도 epoch가 올라 이쪽 ack이 통째로 무효가 됩니다. 반대로
		// **같은 namespace의 더 새로운 이벤트**는 그 마커를 올리므로 살아남습니다.
		if t.wholeGVR {
			if q.staleEpoch == t.epoch {
				s.delta.clearStaleLocked(q)
				q.gvrStale = false
				q.markerSeq = 0
			}
		} else if s.namespaceAckAllowedLocked(q, t) {
			s.delta.removeStaleLocked(q, t.namespace)
			if q.staleNS.count == 0 && !q.gvrStale {
				q.markerSeq = 0
			}
		}
		// **보류분은 이미 게시에 반영되었습니다.** 다시 큐에 넣으면 같은 키를 두 번
		// 적용하게 되므로, 예약만 정확히 한 번 풀고 버립니다.
		for _, ev := range held {
			s.budget.releaseQueued(ev.reserved)
		}
		if t.wholeGVR {
			s.delta.fullRecoveries.Add(1)
		} else {
			s.delta.partitionResyncs.Add(1)
		}
		// 성공했으므로 이 대상의 회로를 아예 걷어냅니다.
		// 더 이상 낡지 않은 대상의 회로를 남겨 두면 회로 상한만 갉아먹습니다.
		s.delta.dropCircuitLocked(t.target())
		// 동적 회로가 없어 내장 fallback으로 돌던 경우도 여기서 닫습니다.
		// 성공했는데 백오프가 남으면 다음 낡음이 이유 없이 늦춰집니다.
		if t.wholeGVR {
			q.fallback.reset()
		}
		// 보류가 사라졌으므로 큐가 완전히 비었을 수 있습니다. 그렇다면 **도달 가능한
		// 저장 용량**을 되돌립니다 — 회수 중에는 보류 때문에 압축이 미뤄집니다.
		s.delta.compactQueueLocked(q)
		s.dropTicketLocked(t)
		s.delta.cooldownUntil = s.nowOrDefault().Add(recoveryCooldown)
		return
	}

	s.delta.recoveryFailures.Add(1)
	// 실패했으므로 보류분은 큐로 돌려보냅니다. 같은 키가 큐에도 있으면
	// **seq가 큰 쪽만** 남기고 진 쪽 예약은 정확히 한 번 풉니다.
	s.mergeHeldLocked(q, held)

	now := s.nowOrDefault()
	// **회로는 티켓이 사라져도 남습니다.** 백오프·시도 횟수·필요 바이트가 여기 쌓여
	// 같은 입력·같은 예산에서 반복 재구성이 일어나지 않습니다.
	target := t.target()
	c, eff := s.delta.circuitForQ(q, target)
	c.fail(now, q.markerFor(eff), t.hi-t.nextRow, t.lastNeeded, s.budget.limit(),
		s.availableFor(t.gvr), outcome == recoveryBudgetRejected)
	if outcome == recoveryBudgetRejected {
		// 예산 거절은 충돌과 다릅니다. 명시적 stale로 남기고 회로를 엽니다.
		// 지문은 건드리지 않습니다 — 실패는 입력 변화가 아닙니다.
		if t.wholeGVR {
			q.gvrStale = true
		} else {
			s.delta.restakeLocked(q, t.namespace)
		}
		s.delta.cooldownUntil = now.Add(c.backoffOr(recoveryCooldown))
	} else {
		s.delta.cooldownUntil = now.Add(recoveryCooldown)
	}
	s.dropTicketLocked(t)
}

/* ── 계측 ───────────────────────────────────────────────────────────────── */

// deltaMetricSample은 GVR 하나의 델타 계측 표본입니다.
type deltaMetricSample struct {
	resource   string
	pending    int
	pendingB   int64
	dropped    int64
	batches    int64
	batchNanos int64
	staleParts int
	gvrStale   int
}

// staleSummary는 이 GVR에 남아 있는 회수 대기 상태입니다.
//
// 질의가 이것을 직접 보는 이유는, 마커가 큐에 있고 인덱스에는 아직 반영되지 않은
// 창이 존재하기 때문입니다. 그 창에서 "완전한 결과"라고 말하지 않으려면 응답이
// 큐의 사실을 읽어야 합니다. 잠금 순서는 **snapMu → queueMu** 그대로입니다
// (Search가 snapMu 읽기 잠금을 든 채 여기서 queueMu를 잠깐 잡습니다).
func (s *Service) staleSummary(gvr schema.GroupVersionResource) (partitions int, gvrStale bool) {
	if s == nil || s.delta == nil {
		return 0, false
	}
	s.delta.mu.Lock()
	defer s.delta.mu.Unlock()
	q, ok := s.delta.queues[gvr]
	if !ok {
		return 0, false
	}
	return q.staleNS.count, q.gvrStale
}

// namespaceStale은 **그 namespace 하나**가 회수 대기인지입니다.
//
// GVR 전체를 막으면, 사용자가 볼 수 없는 namespace 하나가 stale이 됐다는 이유로
// 허용된 참조의 해석 방식이 바뀝니다 — 숨겨진 데이터의 상태가 응답에 비칩니다.
// cluster-scoped는 namespace ""로 그대로 조회합니다(빈 값이 "전체"가 아닙니다).
func (s *Service) namespaceStale(gvr schema.GroupVersionResource, namespace string) bool {
	if s == nil || s.delta == nil {
		return false
	}
	s.delta.mu.Lock()
	defer s.delta.mu.Unlock()
	q, ok := s.delta.queues[gvr]
	if !ok {
		return false
	}
	return q.gvrStale || q.staleNS.has(namespace)
}

// staleList는 회수 대기 namespace 목록입니다(<= maxStaleTracked).
// 게시 직전에 인덱스에도 같은 사실을 새겨 넣기 위해 씁니다.
func (s *Service) staleList(gvr schema.GroupVersionResource) ([]string, bool) {
	return s.staleListInto(gvr, make([]string, 0, maxStaleTracked))
}

// staleListInto는 같은 목록을 **호출자가 준 버퍼**에 채웁니다.
//
// 매 배치마다 새 슬라이스를 잡으면 그 몫(최대 1024개 헤더)이 어떤 예약에도
// 잡히지 않습니다. flush는 이 배치의 임시 예약(deltaStaleScratchBytes) 안에서
// 버퍼를 잡아 넘깁니다.
func (s *Service) staleListInto(gvr schema.GroupVersionResource, dst []string) ([]string, bool) {
	if s == nil || s.delta == nil {
		return dst[:0], false
	}
	s.delta.mu.Lock()
	defer s.delta.mu.Unlock()
	q, ok := s.delta.queues[gvr]
	if !ok || (q.staleNS.count == 0 && !q.gvrStale) {
		return dst[:0], false
	}
	dst = dst[:0]
	for i, used := range q.staleNS.used {
		if used {
			dst = append(dst, q.staleNS.slots[i])
		}
	}
	sort.Strings(dst)
	return dst, q.gvrStale
}

// sampleDeltaMetrics는 큐 계측을 queueMu 아래에서 한 번에 뜹니다.
// snapMu와 겹쳐 잡지 않습니다.
func (s *Service) sampleDeltaMetrics() []deltaMetricSample {
	if s == nil || s.delta == nil {
		return nil
	}
	out := make([]deltaMetricSample, 0, len(s.order))
	s.delta.mu.Lock()
	defer s.delta.mu.Unlock()
	for _, gvr := range s.order {
		q, ok := s.delta.queues[gvr]
		sample := deltaMetricSample{resource: FormatGVR(gvr)}
		if ok {
			sample.pending = len(q.events)
			sample.pendingB = q.pendingBytes()
			sample.dropped = q.dropped
			sample.batches = q.batches
			sample.batchNanos = q.batchNanos
			sample.staleParts = q.staleNS.count
			if q.gvrStale {
				sample.gvrStale = 1
			}
		}
		out = append(out, sample)
	}
	return out
}
