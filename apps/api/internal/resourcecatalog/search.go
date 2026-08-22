package resourcecatalog

// 전역 리소스 검색 (ADR 0023)
// --------------------------------------------------------------------------
// ADR 0018의 metadata 인덱스 **위에** 얹는 조회 전용 접두사 검색입니다. 새 watch도,
// 새 수집 경로도, 별도 배경 루프도 만들지 않습니다.
//
// 조회 경로가 **두 가지**이고, 어느 쪽을 쓰는지는 오직 Scope가 정합니다.
//
//	① 클러스터 전체 접근(Scope.All)
//	   GVR별 전역 접두사 색인(postings)을 토큰 순서로 병합합니다. 결과 전순서는
//	   (token, namespace, name, gvr, uid)입니다. 이 사용자에게는 숨겨진 데이터가
//	   없으므로 색인이 예산에 들어가지 못하면 그 사실을 그대로 알립니다.
//
//	② 범위 제한 접근(Scope가 namespace 목록)
//	   **색인 상태와 무관하게 언제나** 목록 스냅숏의 허용 namespace 구간만 행 순서로
//	   순회합니다. 결과 전순서는 (gvr, namespace, name, uid)입니다. 색인이 준비됐는지
//	   여부가 결과·cursor·진단을 바꾸지 않아야 하기 때문입니다 — 색인이 예산을 넘기는
//	   이유는 대개 이 사용자가 볼 수 없는 namespace의 규모이고, 그것이 응답에 비치면
//	   숨겨진 데이터의 크기가 새어 나갑니다.
//	   이 경로는 **observedAt을 채우지 않습니다.** 목록 스냅숏의 builtAt은 숨겨진
//	   namespace의 변경으로도 갱신되므로, 그 값을 실으면 볼 수 없는 데이터의 변경
//	   시각이 새어 나갑니다. 계약에서 선택 필드입니다.
//	   누락 사유(잘린 label, 건너뛴 비-ready 리소스)는 cursor의 고정 비트로 이어져,
//	   1페이지에서만 만난 누락이 2페이지에서 사라지지 않습니다.
//
// 두 경로가 공유하는 불변식은 네 가지입니다.
//
//   - **Scope 밖 행은 어떤 경로에서도 만나지 않습니다.** ①은 namespace major postings
//     구간을, ②는 목록 스냅숏의 namespace 구간을 씁니다. 숨겨진 namespace는 결과·
//     scan 예산·cursor·truncated·degraded·observedAt 어디에도 참여하지 않습니다.
//   - **label 판정 규칙이 하나입니다.** 행 단위 MaxLabelKeysPerObject와 namespace 단위
//     MaxLabelTokensPerNamespace를 색인 빌드와 순회가 똑같이 적용하고, 잘리면 같은
//     사유로 알립니다. ②는 namespace 상한 판정을 페이지 사이에서 이어가려고 지금까지
//     센 label 토큰 수를 cursor에 싣습니다.
//   - **메모리 상한은 할당 전에 강제합니다.** 크기를 먼저 재고(할당 0) 보유·정점 예산을
//     둘 다 만족할 때만 배열을 잡습니다. 사전은 Go map이 아니라 preflight가 크기를
//     계산한 **고정 용량 개방 주소 테이블**입니다.
//   - **독자는 세대를 넘겨 붙잡지 못합니다.** 스냅숏 사용은 Service.snapMu 아래에서만
//     일어납니다. (catalog.go의 resourceEntry 참조)

import (
	"fmt"
	"io"
	"slices"
	"sort"
	"strings"
	"time"
)

// 질의·페이지 상한입니다. 브라우저가 더 큰 값을 보내도 넘지 못합니다. (ADR 0023 결정 3·5)
const (
	// MinQueryLen은 질의 최소 길이입니다. 1자 접두사는 사실상 전체 순회입니다.
	MinQueryLen = 2
	// MaxQueryLen은 질의 최대 길이입니다.
	MaxQueryLen = 64
	// tokenPrefixBytes는 색인에 담는 토큰의 최대 길이입니다.
	//
	// 질의가 MaxQueryLen을 넘지 못하므로 토큰을 이 길이로 잘라도 접두사 일치
	// 결과는 원본과 완전히 같습니다. 인덱스와 cursor 크기가 이 상수 하나로 묶입니다.
	tokenPrefixBytes = MaxQueryLen

	// DefaultSearchPageSize / MaxSearchPageSize는 검색 페이지 크기입니다.
	DefaultSearchPageSize = 20
	MaxSearchPageSize     = 50

	// MaxSearchResponseBytes는 검색 응답 본문의 상한입니다.
	MaxSearchResponseBytes = 256 << 10

	// maxSearchScan은 색인 경로가 검사할 수 있는 후보 수 상한입니다.
	maxSearchScan = 20_000
	// searchScanFactor는 limit 대비 최소 scan 예산 배수입니다.
	searchScanFactor = 40
	// maxScopedScanRows는 범위 제한 순회가 한 요청에서 훑는 **허용 행** 수 상한입니다.
	//
	// 창이 닫히면 결과를 조용히 줄이지 않고, 마지막으로 훑은 위치를 cursor에 담아
	// 다음 페이지가 거기서 이어가게 합니다. 그래서 20k행·4096건을 넘는 결과도
	// 페이지를 넘기면 전부 닿습니다.
	maxScopedScanRows = 20_000
	// searchRowOverheadBytes는 검색 결과 한 행의 JSON 고정 비용 추정치입니다.
	searchRowOverheadBytes = 160

	// MaxSearchStreams는 색인 경로가 동시에 여는 (GVR, namespace) 스트림 수 상한입니다.
	MaxSearchStreams = 16384

	// MaxLabelKeysPerObject는 객체 하나에서 색인·순회할 label 키 수입니다.
	// **행 단위 상수**이므로 다른 객체가 이 판단에 개입하지 않습니다.
	MaxLabelKeysPerObject = 16
	// MaxLabelTokensPerNamespace는 namespace 하나가 만들 수 있는 label 토큰 수입니다.
	// **namespace 단위 상수**이므로 다른 namespace가 이 판단에 개입하지 않습니다.
	MaxLabelTokensPerNamespace = 1 << 18
)

// 인덱스 보유·정점 바이트 상한입니다.
//
// **GVR별 상한만 두면 상한이 아닙니다** — allowlist 64개가 각자 상한까지 쓰면
// 곱해집니다. 그래서 서비스 전체 보유량을 1차 상한으로 두고, GVR별 몫은 한
// 리소스가 전체를 독점하지 못하게 막는 2차 장치로만 씁니다.
const (
	// DefaultMaxSearchIndexBytes는 모든 GVR이 **동시에 보유하는** 검색 인덱스 합의 기본 상한입니다.
	DefaultMaxSearchIndexBytes = 64 << 20
	// MinMaxSearchIndexBytes / MaxMaxSearchIndexBytes는 설정 허용 범위입니다.
	MinMaxSearchIndexBytes = 16 << 20
	MaxMaxSearchIndexBytes = 512 << 20
	// searchPerResourceDivisor는 GVR 하나의 보유 몫입니다(전체의 1/2).
	searchPerResourceDivisor = 2
	// searchPeakMultiplier는 재구성 정점의 상한 배수입니다.
	searchPeakMultiplier = 3
)

// 바이트 회계 상수입니다. ret*는 스냅숏이 계속 붙잡는 바이트, tra*는 빌드 중에만
// 사는 바이트입니다. 두 값을 모두 예산에 넣어야 정점이 실제로 묶입니다.
//
// **여기 없는 할당은 빌더에 없어야 합니다.** Go map처럼 용량 증설 시점을 코드가
// 보장할 수 없는 자료구조는 쓰지 않습니다 — 상한이라고 말하려면 코드가 강제해야 합니다.
//
// 이 회계가 세는 것은 **페이로드와 슬라이스 용량, 그리고 고정 여유(fixedSnapshotBytes)**
// 입니다. Go 런타임의 실제 할당 크기(size class 반올림, 힙 단편화)나 프로세스 RSS와
// 같지 않으며, 그것을 보장하지도 않습니다. 문자열 본문은 informer 객체와 공유될 수
// 있어 실제 추가 사용량보다 크게 잡히는 쪽입니다 — 상한을 지키는 방향입니다.
const (
	stringHeaderBytes = 16 // string 헤더(포인터 + 길이)
	postingBytes      = 8  // posting{token, row uint32}
	csrBytes          = 4  // ids 한 항목
	uint32Bytes       = 4
	// fixedSnapshotBytes는 슬라이스 헤더·구조체 자체의 고정 비용입니다.
	fixedSnapshotBytes = 256

	// 보유: tokens 헤더 + 최종 ids + postings.
	retTokenBytes = stringHeaderBytes
	retPostBytes  = csrBytes + postingBytes
	retRowBytes   = uint32Bytes                         // off
	retNsBytes    = stringHeaderBytes + uint32Bytes + 1 // nsNames + nsPostings + nsLabelIncomplete

	// 작업용(등장 횟수마다): 작업용 ids + dst + counts 몫.
	traOccBytes = csrBytes + postingBytes + uint32Bytes
	// tokenSortKeyBytes는 정렬 키 하나의 크기입니다(uint64 + uint32 + 패딩).
	tokenSortKeyBytes = 16
	// 작업용(distinct 토큰마다, 상한은 등장 횟수):
	// 사전 문자열 + 정렬 키 + 기수 정렬 임시 배열 + remap.
	traDistinctBytes = stringHeaderBytes + 2*tokenSortKeyBytes + uint32Bytes
	traRowBytes      = uint32Bytes // rowNS
	traNsBytes       = 16          // nsRowStart(int) + 여유
)

// SearchState는 GVR 하나가 **색인 경로**에 참여할 수 있는지입니다.
// 범위 제한 순회는 이 값을 보지 않습니다.
type SearchState string

const (
	// SearchReady는 검색 인덱스가 준비된 상태입니다.
	SearchReady SearchState = "ready"
	// SearchDisabled는 배포에서 검색을 끈 상태입니다.
	SearchDisabled SearchState = "disabled"
	// SearchSyncing은 목록 인덱스가 아직 없어 검색 인덱스도 없는 상태입니다.
	SearchSyncing SearchState = "syncing"
	// SearchUnavailable은 이 리소스가 예산에 들어가지 않아 색인되지 않은 상태입니다.
	SearchUnavailable SearchState = "unavailable"
)

// matchedField 값입니다. CSR 항목의 하위 3비트에 담깁니다.
const (
	fieldName      uint32 = 0
	fieldNamespace uint32 = 1
	fieldKind      uint32 = 2
	fieldLabel     uint32 = 3
	fieldMask      uint32 = 7
	fieldShift            = 3
	// maxTokenID는 fieldShift만큼 밀고도 uint32에 남는 토큰 ID 상한입니다.
	maxTokenID = (1 << (32 - fieldShift)) - 1
)

var matchedFieldNames = [...]string{"name", "namespace", "kind", "label"}

// MatchedFieldNames는 계약이 노출하는 matchedField 어휘입니다. 순서가 계약입니다.
func MatchedFieldNames() []string { return matchedFieldNames[:] }

// degraded 사유는 짧은 고정 문구입니다 — 내부 주소·질의·스택트레이스를 담지 않습니다. (ADR 0018)
const (
	reasonBudget      = "색인 예산을 초과해 일부 리소스가 검색에서 제외되었습니다"
	reasonScopedScan  = "훑기 창이 닫혔습니다. 다음 페이지로 이어보세요"
	reasonLabelNs     = "일부 namespace의 label 색인이 상한으로 잘렸습니다"
	reasonSyncing     = "일부 리소스가 아직 동기화 중입니다"
	reasonUnsupported = "일부 리소스는 metadata 조회를 지원하지 않아 검색에서 제외됩니다"
	reasonForbidden   = "일부 리소스는 서버 권한이 없어 검색에서 제외됩니다"
	reasonMissing     = "일부 리소스는 클러스터가 제공하지 않습니다"
	// reasonSearchStale은 드롭된 이벤트 때문에 일부 파티션을 믿을 수 없다는 뜻입니다.
	// 회수가 끝날 때까지 응답은 완전하다고 주장하지 않습니다. (Round 6)
	reasonSearchStale = "일부 namespace의 검색 색인이 갱신되지 못했습니다"
)

// 순회 모드에서 **페이지를 넘어 이어지는** 누락 사유 비트입니다.
//
// 순회는 cursor 뒤부터 다시 훑으므로, 1페이지에서만 만난 누락(키가 잘린 행,
// 건너뛴 비-ready 리소스)이 2페이지에서 사라집니다. 그러면 degraded가 참에서
// 거짓으로 뒤집혀 "완전한 결과"라고 말하게 됩니다. 그래서 **누락 사유만** 고정
// 비트로 cursor에 싣습니다.
//
// 비트 위치가 곧 계약입니다 — 순서를 바꾸면 진행 중인 cursor의 의미가 달라집니다.
// 창이 닫혔다는 reasonScopedScan은 그 페이지의 사실이므로 싣지 않습니다.
const (
	scanFlagLabelNs uint32 = 1 << iota
	scanFlagSyncing
	scanFlagUnsupported
	scanFlagForbidden
	scanFlagMissing
	scanFlagLimit
	// scanFlagAll은 유효 비트 전체입니다. cursor 검증이 이 밖의 비트를 거절합니다.
	scanFlagAll = scanFlagLimit - 1
)

// scanFlagReasons는 비트 → 사유의 고정 순서 표입니다.
var scanFlagReasons = [...]struct {
	bit    uint32
	reason string
}{
	{scanFlagLabelNs, reasonLabelNs},
	{scanFlagSyncing, reasonSyncing},
	{scanFlagUnsupported, reasonUnsupported},
	{scanFlagForbidden, reasonForbidden},
	{scanFlagMissing, reasonMissing},
}

// scanStateFlag는 준비되지 않은 informer 상태의 누락 비트입니다.
func scanStateFlag(state State) uint32 {
	switch state {
	case StateSyncing:
		return scanFlagSyncing
	case StateUnsupported:
		return scanFlagUnsupported
	case StateForbidden:
		return scanFlagForbidden
	default:
		return scanFlagMissing
	}
}

// posting은 (토큰, 행) 한 쌍입니다. (namespace 구간, 토큰, 행) 순으로 놓입니다.
type posting struct {
	token uint32
	row   uint32
}

// searchSnapshot은 GVR 하나의 불변 접두사 인덱스입니다. 클러스터 전체 접근 경로만 씁니다.
//
// base.rows가 (namespace, name)으로 정렬되어 있으므로 행 번호 순서 = (namespace, name)
// 순서이고, postings를 (namespace, token, row)로 정렬해 두면
//
//	① namespace 구간을 이분탐색으로 잘라낼 수 있고
//	② 구간 안에서는 (token, name) 순서라 질의 시점 정렬이 아예 없으며
//	③ CSR(off/ids)로 "이 행이 이 접두사에서 갖는 최소 토큰"을 O(log k)에 알 수 있어
//	   한 행이 여러 필드에 걸려도 페이지 경계와 무관하게 정확히 한 번만 나갑니다.
//
// 보유 슬라이스는 전부 **정확한 크기**로 잡습니다(append 여유 없음).
type searchSnapshot struct {
	base *indexSnapshot
	kind string

	tokens     []string
	postings   []posting
	nsNames    []string
	nsPostings []uint32
	// nsLabelIncomplete[i]는 nsNames[i]의 label 색인이 **그 namespace 자신의** 상한으로
	// 잘렸는지입니다.
	nsLabelIncomplete []bool

	off []uint32
	ids []uint32

	bytes int64
}

/* ── 토큰 정규화 ────────────────────────────────────────────────────────── */

// normalizeToken은 소문자화 + 길이 절단입니다. 접두사 일치에 필요한 만큼만 남깁니다.
func normalizeToken(s string) string {
	s = strings.ToLower(s)
	if len(s) > tokenPrefixBytes {
		s = s[:tokenPrefixBytes]
	}
	return s
}

// tokenBytesOf는 할당 없이 정규화 토큰의 길이를 셉니다.
// Kubernetes가 이름·namespace·label 키/값을 ASCII로 검증하므로 소문자화가 길이를
// 바꾸지 않고, 상한 절단만 반영하면 됩니다.
func tokenBytesOf(s string) int64 { return int64(min(len(s), tokenPrefixBytes)) }

// safeToken은 cursor에 그대로 실을 수 있는 토큰인지입니다.
func safeToken(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
		case c == '-', c == '.', c == '_', c == '/', c == ':':
		default:
			return false
		}
	}
	return true
}

// sortedLabelKeys는 행 하나에서 색인·순회할 label 키를 사전순으로 돌려줍니다.
// buf는 재사용 버퍼이며, 상한을 넘으면 앞에서부터 잘라 결정적으로 고정합니다.
func sortedLabelKeys(row *indexRow, buf []string) ([]string, bool) {
	buf = buf[:0]
	for k := range row.obj.Labels {
		buf = append(buf, k)
	}
	slices.Sort(buf)
	if len(buf) > MaxLabelKeysPerObject {
		return buf[:MaxLabelKeysPerObject], true
	}
	return buf, false
}

// labelTokenCount는 이 행이 만들 label 토큰 수입니다.
// 색인 빌드와 범위 제한 순회가 **같은 값**을 봐야 namespace 상한 판정이 일치합니다.
func labelTokenCount(row *indexRow, keys []string) int64 {
	var count int64
	for _, k := range keys {
		count++
		if row.obj.Labels[k] != "" {
			count++
		}
	}
	return count
}

func rowUID(row *indexRow) string {
	if row.obj == nil {
		return ""
	}
	return string(row.obj.UID)
}

/* ── 고정 용량 사전 (P1-Perf) ─────────────────────────────────────────────
   등장 토큰 전체를 문자열로 정렬하면 O(P log P) 문자열 비교가 되어 100k 재구성이
   무너집니다. 개방 주소 테이블로 기대 O(P)에 intern하고, **distinct만** 정렬합니다. */

// tokenDict는 preflight가 크기를 계산한 고정 용량 개방 주소 사전입니다.
//
// Go map을 쓰지 않는 이유는 증설 시점과 버킷 크기를 코드가 보장할 수 없기 때문입니다.
// 여기서는 슬롯 수를 등장 횟수의 2배 이상으로 잡으므로 부하율이 0.5를 넘지 않고,
// 선형 탐사 길이도 그만큼 묶여 있습니다.
type tokenDict struct {
	slots  []uint32 // 0 = 빈 슬롯, 그 외에는 id+1
	mask   uint32
	tokens []string // 등장 순서의 distinct 토큰
}

func newTokenDict(slots int64, maxDistinct int) *tokenDict {
	return &tokenDict{
		slots:  make([]uint32, slots),
		mask:   uint32(slots - 1),
		tokens: make([]string, 0, maxDistinct),
	}
}

// hashSafeToken은 안전성 검사와 해시를 **한 번의 순회로** 끝냅니다.
//
// 빌드는 토큰마다 소문자화·안전성 검사·해시를 하는데, 각각 문자열을 다시 훑으면
// 10만 객체 재구성에서 그 차이가 그대로 드러납니다. 해시는 시드가 없어 같은
// 입력이면 언제나 같은 자리에 들어가고, 그래서 빌드 결과가 재현됩니다.
//
// 시드가 없다는 것은 뒤집으면, 같은 슬롯으로 몰리도록 이름을 고른 객체를 대량으로
// 만들면 선형 탐사가 길어질 수 있다는 뜻입니다. 부하율이 0.5로 묶여 있어 평균
// 탐사는 짧고 빌드는 배경 경로이지만, 클러스터에 객체를 만들 수 있는 주체가
// 재구성 시간을 늘릴 여지는 남습니다. 프로세스마다 무작위 시드를 쓰면 막을 수
// 있으나 빌드 재현성(같은 입력 → 같은 사전 순서) 검증이 어려워지므로, 지금은
// 남은 위험으로 기록해 둡니다.
func hashSafeToken(s string) (uint32, bool) {
	if s == "" {
		return 0, false
	}
	h := uint32(2166136261)
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
		case c == '-', c == '.', c == '_', c == '/', c == ':':
		default:
			return 0, false
		}
		h ^= uint32(c)
		h *= 16777619
	}
	return h, true
}

// intern은 토큰의 id를 돌려줍니다. 해시는 호출자가 이미 계산해 넘깁니다.
// 두 번째 반환값이 false면 테이블이 가득 찼다는 뜻이며, 슬롯이 등장 횟수의
// 2배이므로 도달하지 않습니다.
func (d *tokenDict) intern(tok string, hash uint32) (uint32, bool) {
	h := hash & d.mask
	for probe := uint32(0); probe <= d.mask; probe++ {
		at := (h + probe) & d.mask
		slot := d.slots[at]
		if slot == 0 {
			if len(d.tokens) >= cap(d.tokens) || len(d.tokens) >= maxTokenID {
				return 0, false
			}
			id := uint32(len(d.tokens))
			d.slots[at] = id + 1
			d.tokens = append(d.tokens, tok)
			return id, true
		}
		if d.tokens[slot-1] == tok {
			return slot - 1, true
		}
	}
	return 0, false
}

// tokenSortKey는 distinct 토큰 정렬용 키입니다.
// head는 토큰의 앞 8바이트를 big-endian으로 채운 값이라, 짧은 토큰이 같은 접두사의
// 긴 토큰보다 앞서는 바이트 순서가 그대로 보존됩니다.
type tokenSortKey struct {
	head uint64
	id   uint32
}

func tokenHead(s string) uint64 {
	var h uint64
	for i := 0; i < 8; i++ {
		h <<= 8
		if i < len(s) {
			h |= uint64(s[i])
		}
	}
	return h
}

// sortTokenKeys는 head 기준 LSD 기수 정렬 뒤, head가 같은 묶음만 문자열로 정렬합니다.
//
// 비교 정렬은 D log D회의 비교를 요구하고, 이름·label 값이 거의 다 달라 distinct가
// 등장 횟수에 육박하는 클러스터에서는 그 비용이 재구성 시간을 지배합니다. 기수 정렬은
// head 8바이트에 대해 O(8D)이고 비교가 없습니다. head가 같은 토큰끼리만 남는 묶음은
// 작으므로 그때만 문자열을 봅니다 — 최종 순서는 사전순 그대로입니다.
//
// 자리값이 전부 같은 패스는 건너뛰므로, 짧은 토큰이 많으면 패스 수가 줄어듭니다.
// 그래서 결과가 keys에 있을지 scratch에 있을지가 입력에 따라 달라지고, 어느 쪽인지를
// 반환값으로 알립니다.
func sortTokenKeys(keys, scratch []tokenSortKey, tokens []string) []tokenSortKey {
	if len(keys) < 2 {
		return keys
	}
	var counts [256]int
	for shift := 0; shift < 64; shift += 8 {
		for i := range counts {
			counts[i] = 0
		}
		for _, k := range keys {
			counts[byte(k.head>>uint(shift))]++
		}
		if counts[byte(keys[0].head>>uint(shift))] == len(keys) {
			continue // 이 자리 값이 전부 같습니다. 옮길 이유가 없습니다.
		}
		sum := 0
		for i := 0; i < 256; i++ {
			c := counts[i]
			counts[i] = sum
			sum += c
		}
		for _, k := range keys {
			b := byte(k.head >> uint(shift))
			scratch[counts[b]] = k
			counts[b]++
		}
		keys, scratch = scratch, keys
	}
	// head가 같은 구간만 사전순으로 다시 정렬합니다.
	for i := 0; i < len(keys); {
		j := i + 1
		for j < len(keys) && keys[j].head == keys[i].head {
			j++
		}
		if j-i > 1 {
			group := keys[i:j]
			slices.SortFunc(group, func(a, b tokenSortKey) int {
				return strings.Compare(tokens[a.id], tokens[b.id])
			})
		}
		i = j
	}
	return keys
}

// dictSlots는 등장 횟수 tokens를 부하율 0.5 이하로 담는 2의 거듭제곱 슬롯 수입니다.
func dictSlots(tokens int64) int64 {
	slots := int64(16)
	for slots < tokens*2 {
		slots <<= 1
	}
	return slots
}

/* ── 1단계: 크기 측정 (할당 0) ──────────────────────────────────────────── */

type searchMeasurement struct {
	rows       int
	namespaces int
	// tokens는 색인할 토큰(=posting) 등장 횟수입니다. 행·namespace 상한을 이미 반영했습니다.
	tokens int64
	// tokenBytes는 그 토큰들의 바이트 합입니다(중복 제거 전이므로 상한).
	tokenBytes   int64
	maxLabelKeys int
	nsNameBytes  int64
}

func measureSearchInput(index *indexSnapshot, kindTok string, namespaced bool) searchMeasurement {
	m := searchMeasurement{rows: len(index.rows)}
	kindOK := safeToken(kindTok)
	kindBytes := tokenBytesOf(kindTok)
	var nsLabelTokens, nsLabelBytes int64
	flush := func() {
		if nsLabelTokens > MaxLabelTokensPerNamespace {
			m.tokens += MaxLabelTokensPerNamespace
			m.tokenBytes += MaxLabelTokensPerNamespace * tokenPrefixBytes
		} else {
			m.tokens += nsLabelTokens
			m.tokenBytes += nsLabelBytes
		}
		nsLabelTokens, nsLabelBytes = 0, 0
	}
	for i := range index.rows {
		row := &index.rows[i]
		if i == 0 || row.namespace != index.rows[i-1].namespace {
			if i > 0 {
				flush()
			}
			m.namespaces++
			m.nsNameBytes += int64(len(row.namespace))
		}
		m.tokens++
		m.tokenBytes += tokenBytesOf(row.name)
		if namespaced && row.namespace != "" {
			m.tokens++
			m.tokenBytes += tokenBytesOf(row.namespace)
		}
		if kindOK {
			m.tokens++
			m.tokenBytes += kindBytes
		}
		if row.obj == nil || len(row.obj.Labels) == 0 {
			continue
		}
		keys := len(row.obj.Labels)
		if keys > m.maxLabelKeys {
			m.maxLabelKeys = keys
		}
		if keys <= MaxLabelKeysPerObject {
			for k, v := range row.obj.Labels {
				nsLabelTokens++
				nsLabelBytes += tokenBytesOf(k)
				if v != "" {
					nsLabelTokens++
					nsLabelBytes += tokenBytesOf(v)
				}
			}
			continue
		}
		nsLabelTokens += int64(MaxLabelKeysPerObject) * 2
		nsLabelBytes += int64(MaxLabelKeysPerObject) * 2 * tokenPrefixBytes
	}
	if m.rows > 0 {
		flush()
	}
	return m
}

// searchCost는 (보유, 보유+작업용) 바이트 상한입니다.
//
// 빌더가 잡는 모든 backing의 용량이 여기 항으로 들어 있습니다 — 사전 슬롯, 사전
// 문자열, order, remap, 작업용 ids, dst, counts, rowNS, nsRowStart, labelBuf까지입니다.
// distinct 수는 등장 횟수 이하이므로 그 상한으로 계산합니다.
func searchCost(m searchMeasurement) (retained, peak int64) {
	retained = fixedSnapshotBytes +
		int64(m.rows+1)*retRowBytes +
		int64(m.namespaces+1)*retNsBytes + m.nsNameBytes +
		m.tokens*(retPostBytes+retTokenBytes) + m.tokenBytes
	transient := m.tokens*(traOccBytes+traDistinctBytes) +
		dictSlots(m.tokens)*uint32Bytes +
		int64(m.rows)*traRowBytes +
		int64(m.namespaces+1)*traNsBytes +
		int64(m.maxLabelKeys)*stringHeaderBytes
	return retained, retained + transient
}

/* ── 2단계: 빌드 ───────────────────────────────────────────────────────── */

// searchBuildResult는 빌드 결과와 그 결과를 만든 근거입니다.
type searchBuildResult struct {
	snapshot *searchSnapshot
	state    SearchState
	reason   string
	// peak는 이 빌드가 예약한 (보유 + 작업용) 바이트입니다. 정점 회계에 씁니다.
	peak int64
}

// buildSearchSnapshot은 이미 만들어진 목록 인덱스 위에 접두사 인덱스를 세웁니다.
//
// retainedBudget은 이 스냅숏이 계속 붙잡을 수 있는 바이트, peakAllowance는 빌드 중
// 작업용 배열까지 포함해 쓸 수 있는 바이트입니다. **둘 다 큰 할당 전에 판정합니다.**
//
// 판정은 한 번뿐이고 label만 따로 빼는 경로가 없습니다.
func buildSearchSnapshot(index *indexSnapshot, kind string, namespaced bool, retainedBudget, peakAllowance int64) searchBuildResult {
	kindTok := normalizeToken(kind)
	n := len(index.rows)
	if n == 0 {
		snap := &searchSnapshot{base: index, kind: kindTok, off: []uint32{0}, nsNames: []string{}, nsPostings: []uint32{0}}
		snap.bytes = snap.retainedBytes()
		return searchBuildResult{snapshot: snap, state: SearchReady, peak: snap.bytes}
	}

	m := measureSearchInput(index, kindTok, namespaced)
	if n > maxTokenID || m.tokens > maxTokenID {
		return searchBuildResult{state: SearchUnavailable, reason: reasonBudget}
	}
	retained, peak := searchCost(m)
	if retained > retainedBudget || peak > peakAllowance {
		// 아직 아무것도 할당하지 않았습니다.
		return searchBuildResult{state: SearchUnavailable, reason: reasonBudget}
	}

	// ── 여기부터 할당합니다. 모든 backing 용량이 위 계산에 들어 있습니다.
	capacity := int(m.tokens)
	dict := newTokenDict(dictSlots(m.tokens), capacity)
	ids := make([]uint32, 0, capacity)
	off := make([]uint32, n+1)
	nsNames := make([]string, 0, m.namespaces)
	nsRowStart := make([]int, 0, m.namespaces+1)
	nsLabelIncomplete := make([]bool, m.namespaces)
	labelBuf := make([]string, 0, m.maxLabelKeys)

	full := false
	emit := func(tok string, field uint32) {
		hash, safe := hashSafeToken(tok)
		if !safe || len(ids) >= capacity {
			return
		}
		id, ok := dict.intern(tok, hash)
		if !ok {
			full = true
			return
		}
		ids = append(ids, id<<fieldShift|field)
	}

	nsIdx := -1
	var nsLabelTokens int64
	for i := range index.rows {
		row := &index.rows[i]
		if i == 0 || row.namespace != index.rows[i-1].namespace {
			nsIdx++
			nsLabelTokens = 0
			nsNames = append(nsNames, row.namespace)
			nsRowStart = append(nsRowStart, i)
		}
		off[i] = uint32(len(ids))

		emit(normalizeToken(row.name), fieldName)
		if namespaced && row.namespace != "" {
			emit(normalizeToken(row.namespace), fieldNamespace)
		}
		if kindTok != "" {
			emit(kindTok, fieldKind)
		}
		if row.obj == nil || len(row.obj.Labels) == 0 {
			continue
		}
		keys, keysTruncated := sortedLabelKeys(row, labelBuf)
		labelBuf = keys[:0]
		if keysTruncated {
			nsLabelIncomplete[nsIdx] = true
		}
		rowTokens := labelTokenCount(row, keys)
		if nsLabelTokens+rowTokens > MaxLabelTokensPerNamespace {
			nsLabelIncomplete[nsIdx] = true
			continue
		}
		for _, k := range keys {
			emit(normalizeToken(k), fieldLabel)
			if v := row.obj.Labels[k]; v != "" {
				emit(normalizeToken(v), fieldLabel)
			}
		}
		nsLabelTokens += rowTokens
	}
	if full {
		// 슬롯이 등장 횟수의 2배이므로 도달하지 않습니다. 도달하면 반쪽 인덱스를 남기지 않습니다.
		return searchBuildResult{state: SearchUnavailable, reason: reasonBudget}
	}
	off[n] = uint32(len(ids))
	nsRowStart = append(nsRowStart, n)

	// ── distinct 토큰만 정렬해 최종 ID를 매기고 CSR을 다시 씁니다.
	//
	// 두 가지로 비용을 줄입니다. 첫째, 등장 횟수 전체가 아니라 distinct만 정렬합니다.
	// 둘째, 그 정렬을 토큰 앞 8바이트에 대한 기수 정렬로 합니다 — 비교가 없어
	// distinct가 등장 횟수에 육박하는 고카디널리티 클러스터에서도 선형입니다.
	// 앞 8바이트가 같은 묶음만 문자열로 다시 정렬하므로 최종 순서는 사전순 그대로입니다.
	distinct := len(dict.tokens)
	keys := make([]tokenSortKey, distinct)
	scratch := make([]tokenSortKey, distinct)
	for i := range keys {
		keys[i] = tokenSortKey{head: tokenHead(dict.tokens[i]), id: uint32(i)}
	}
	if distinct > 0 {
		keys = sortTokenKeys(keys, scratch, dict.tokens)
	}
	remap := make([]uint32, distinct)
	tokens := make([]string, distinct)
	for final, key := range keys {
		remap[key.id] = uint32(final)
		tokens[final] = dict.tokens[key.id]
	}
	for i := range ids {
		ids[i] = remap[ids[i]>>fieldShift]<<fieldShift | ids[i]&fieldMask
	}

	// ── 행마다 토큰을 정렬·중복 제거합니다. 같은 토큰이 여러 필드에서 나오면
	// name > namespace > kind > label 순으로 하나만 남습니다.
	//
	// 정렬은 할당 없는 삽입 정렬입니다 — 행당 토큰이 3 + 2×MaxLabelKeysPerObject로
	// 묶여 있고, sort.Slice의 리플렉션 swapper는 객체 수만큼 할당을 만듭니다.
	write := 0
	for i := 0; i < n; i++ {
		lo, hi := off[i], off[i+1]
		seg := ids[lo:hi]
		insertionSortUint32(seg)
		off[i] = uint32(write)
		var last uint32
		for j, v := range seg {
			if j > 0 && v>>fieldShift == last>>fieldShift {
				continue
			}
			ids[write] = v
			write++
			last = v
		}
	}
	off[n] = uint32(write)

	// ── postings를 (namespace, token, row)로 놓습니다. 계수 정렬 2회이며
	// namespace마다 토큰 전체를 훑지 않으므로 O(P + T + N)입니다.
	compact := make([]uint32, write)
	copy(compact, ids[:write])
	src := make([]posting, write)
	dst := make([]posting, write)
	at := 0
	for r := 0; r < n; r++ {
		for _, v := range compact[off[r]:off[r+1]] {
			src[at] = posting{token: v >> fieldShift, row: uint32(r)}
			at++
		}
	}
	counts := make([]uint32, max(distinct, len(nsNames))+1)
	for _, p := range src {
		counts[p.token]++
	}
	slot := uint32(0)
	for t := 0; t < distinct; t++ {
		c := counts[t]
		counts[t] = slot
		slot += c
	}
	for _, p := range src {
		dst[counts[p.token]] = p
		counts[p.token]++
	}
	rowNS := make([]uint32, n)
	for i := range nsNames {
		for r := nsRowStart[i]; r < nsRowStart[i+1]; r++ {
			rowNS[r] = uint32(i)
		}
	}
	for i := range counts {
		counts[i] = 0
	}
	for _, p := range dst {
		counts[rowNS[p.row]]++
	}
	nsPostings := make([]uint32, len(nsNames)+1)
	slot = 0
	for i := 0; i < len(nsNames); i++ {
		nsPostings[i] = slot
		c := counts[i]
		counts[i] = slot
		slot += c
	}
	nsPostings[len(nsNames)] = slot
	for _, p := range dst {
		ns := rowNS[p.row]
		src[counts[ns]] = p
		counts[ns]++
	}

	snap := &searchSnapshot{
		base: index, kind: kindTok,
		tokens: tokens, postings: src,
		nsNames: nsNames, nsPostings: nsPostings, nsLabelIncomplete: nsLabelIncomplete,
		off: off, ids: compact,
	}
	snap.bytes = snap.retainedBytes()
	if snap.bytes > retainedBudget {
		return searchBuildResult{state: SearchUnavailable, reason: reasonBudget}
	}
	return searchBuildResult{snapshot: snap, state: SearchReady, peak: peak}
}

// insertionSortUint32는 행 하나의 토큰 구간을 정렬합니다. 할당이 없습니다.
func insertionSortUint32(a []uint32) {
	for i := 1; i < len(a); i++ {
		v := a[i]
		j := i - 1
		for j >= 0 && a[j] > v {
			a[j+1] = a[j]
			j--
		}
		a[j+1] = v
	}
}

/* ── 보유 바이트 회계 ────────────────────────────────────────────────────── */

// retainedBytes는 이 스냅숏이 붙잡고 있는 **페이로드 + 슬라이스 용량 + 고정 여유**입니다.
//
// Go 런타임의 실제 할당량(size class 반올림)이나 RSS가 아닙니다. 보유 슬라이스는
// 전부 정확한 크기로 잡으므로 len과 backing이 같지만, cap을 세어 append 여유가
// 생기는 변경이 조용히 회계를 벗어나지 못하게 합니다.
func (s *searchSnapshot) retainedBytes() int64 {
	if s == nil {
		return 0
	}
	total := int64(fixedSnapshotBytes)
	for _, t := range s.tokens {
		total += int64(len(t)) + stringHeaderBytes
	}
	for _, ns := range s.nsNames {
		total += int64(len(ns)) + stringHeaderBytes
	}
	total += int64(cap(s.tokens)-len(s.tokens)) * stringHeaderBytes
	total += int64(cap(s.nsNames)-len(s.nsNames)) * stringHeaderBytes
	total += int64(cap(s.postings)) * postingBytes
	total += int64(cap(s.ids)) * csrBytes
	total += int64(cap(s.off)) * uint32Bytes
	total += int64(cap(s.nsPostings)) * uint32Bytes
	total += int64(cap(s.nsLabelIncomplete))
	return total
}

func (s *searchSnapshot) postingCount() int {
	if s == nil {
		return 0
	}
	return len(s.postings)
}

func (s *searchSnapshot) labelIncompleteIn(ns int) bool {
	return ns >= 0 && ns < len(s.nsLabelIncomplete) && s.nsLabelIncomplete[ns]
}

/* ── 질의 ────────────────────────────────────────────────────────────────
   요청 경로입니다. Kubernetes를 호출하지 않습니다. */

// SearchRequest는 전역 검색 한 번입니다. Namespaces는 **서버가 Scope에서 채웁니다.**
type SearchRequest struct {
	Query      string
	Limit      int
	Cursor     string
	Namespaces NamespaceFilter
}

// SearchItem은 검색 결과 한 줄입니다.
// status는 없습니다 — PartialObjectMetadata에 status가 없으므로 지어내지 않습니다.
type SearchItem struct {
	Group        string
	Version      string
	Resource     string
	Kind         string
	Namespaced   bool
	Namespace    string
	Name         string
	UID          string
	MatchedField string
}

// SearchPage는 유계 검색 페이지 하나입니다.
type SearchPage struct {
	// Query는 서버가 정규화한 질의어입니다.
	Query      string
	Items      []SearchItem
	NextCursor string
	Truncated  bool
	Degraded   bool
	Reason     string
	ObservedAt time.Time
}

// searchDiagnostics는 **Scope 안에서만** 모은 degraded 사유입니다.
//
// flags는 그중 페이지를 넘어 이어져야 하는 누락 사유입니다. 순회 모드에서만 쓰이며
// cursor에 실립니다.
type searchDiagnostics struct {
	reasons []string
	seen    map[string]bool
	flags   uint32
}

// noteOmission은 누락 사유를 남기면서 이어보기용 비트도 함께 세웁니다.
func (d *searchDiagnostics) noteOmission(bit uint32, reason string) {
	d.flags |= bit
	d.note(reason)
}

// restore는 앞 페이지들이 이미 만난 누락 사유를 되살립니다.
func (d *searchDiagnostics) restore(flags uint32) {
	for _, f := range scanFlagReasons {
		if flags&f.bit != 0 {
			d.noteOmission(f.bit, f.reason)
		}
	}
}

func (d *searchDiagnostics) note(reason string) {
	if reason == "" {
		return
	}
	if d.seen == nil {
		d.seen = map[string]bool{}
	}
	if d.seen[reason] {
		return
	}
	d.seen[reason] = true
	d.reasons = append(d.reasons, reason)
}

func (d *searchDiagnostics) apply(page *SearchPage) {
	if len(d.reasons) == 0 {
		return
	}
	sort.Strings(d.reasons)
	page.Degraded, page.Reason = true, strings.Join(d.reasons, " · ")
}

// Search는 Scope 안에서 접두사 일치 결과 한 페이지를 만듭니다.
//
// Scope가 전체면 색인 경로, 범위 제한이면 **언제나** 허용 구간 순회입니다.
// 시작할 때 스냅숏 read lock을 잡고 끝날 때 놓습니다.
func (s *Service) Search(req SearchRequest) (SearchPage, error) {
	if !s.Available() {
		return SearchPage{}, ErrUnavailable
	}
	if !s.cfg.SearchEnabled {
		return SearchPage{}, ErrSearchDisabled
	}
	// 원문 길이를 **다듬기 전에** 먼저 봅니다. 공백을 잔뜩 붙인 긴 질의를 받아
	// 다듬은 뒤에야 거절하면, 그 검사 전까지의 비용을 클라이언트가 정할 수 있습니다.
	if len(req.Query) > MaxQueryLen {
		return SearchPage{}, ErrInvalidQuery
	}
	raw := strings.TrimSpace(req.Query)
	if len(raw) > MaxQueryLen {
		return SearchPage{}, ErrInvalidQuery
	}
	query := normalizeToken(raw)
	if len(query) < MinQueryLen || !safeToken(query) {
		return SearchPage{}, ErrInvalidQuery
	}
	limit := req.Limit
	if limit <= 0 {
		limit = DefaultSearchPageSize
	}
	if limit > MaxSearchPageSize {
		return SearchPage{}, ErrInvalidFilter
	}
	allowed := sortedUnique(req.Namespaces.List)
	fingerprint := searchFingerprint(s.cfg.ClusterID, query, req.Namespaces.All, allowed)
	mode := cursorModeScan
	if req.Namespaces.All {
		mode = cursorModeIndex
	}
	var cursor searchCursorKey
	hasCursor := false
	if req.Cursor != "" {
		key, err := decodeSearchCursor(req.Cursor, fingerprint, mode)
		if err != nil {
			return SearchPage{}, err
		}
		cursor, hasCursor = key, true
	}

	// **잠금을 들고 훑지 않습니다.** 요청 시작 시점에 이 요청이 볼 자료를 한 번 고정합니다.
	//
	// 범위 제한 요청은 목록 스냅숏만 씁니다 — 검색 세대를 빌리지도, 들여다보지도
	// 않습니다. 빌리면 훑지도 않을 세대의 수명과 회계를 붙잡을 뿐입니다.
	//
	// 클러스터 전체 접근만 세대를 빌리고, 그때 **빌린 세대의 스냅숏까지 함께** 고정합니다.
	// 빌린 뒤에 다시 읽으면 빌리지 않은 세대를 훑으면서 회계는 엉뚱한 옛 세대를
	// 붙잡게 됩니다. (Round 8)
	page := SearchPage{Query: query, Items: make([]SearchItem, 0, limit)}
	var diag searchDiagnostics
	if !req.Namespaces.All {
		view := s.baselineView()
		return s.searchScoped(query, limit, allowed, cursor, hasCursor, fingerprint, &page, &diag, view)
	}
	view := s.acquireView()
	defer s.releaseView(view)
	// 증분 모드에서는 지속 구조 인덱스를, 롤백 모드에서는 기존 배열 색인을 씁니다.
	// 두 경로의 결과 전순서·cursor 형식·진단 어휘는 같습니다.
	if s.cfg.SearchIncremental {
		return s.searchPersistent(query, limit, cursor, hasCursor, fingerprint, &page, &diag, view)
	}
	return s.searchIndexed(query, limit, cursor, hasCursor, fingerprint, &page, &diag, view)
}

/* ── 범위 제한 순회 (P1-Scope) ────────────────────────────────────────────
   허용 namespace 구간만, 행 순서로, 스트리밍으로 훑습니다.
   후보를 모아 두지 않고 구간 크기에 비례하는 할당도 하지 않습니다. */

// searchScoped는 (gvr, namespace, name, uid) 순서로 허용 행만 훑습니다.
//
// 색인이 준비됐는지 **보지 않습니다.** 그것이 결과를 바꾸면 숨겨진 namespace의
// 규모가 이 사용자의 응답에 비치기 때문입니다.
//
// **ObservedAt을 채우지 않습니다.** 이 경로가 읽는 목록 스냅숏은 GVR 하나에 하나뿐이라
// 그 builtAt은 숨겨진 namespace의 변경으로도 갱신됩니다. 그 값을 응답에 실으면
// 볼 수 없는 데이터의 변경 시각이 새어 나가고, 뒤 페이지에서는 접미사만 보게 되어
// 값 자체도 흔들립니다. 계약에서 선택 필드이므로 그냥 비웁니다.
func (s *Service) searchScoped(query string, limit int, allowed []string,
	cursor searchCursorKey, hasCursor bool, fingerprint string,
	page *SearchPage, diag *searchDiagnostics, view *searchView) (SearchPage, error) {

	if hasCursor {
		// 앞 페이지들이 만난 누락 사유를 먼저 되살립니다. 그러지 않으면 2페이지에서
		// degraded가 조용히 거짓으로 뒤집힙니다.
		diag.restore(cursor.scanFlags)
	}
	budget := maxScopedScanRows
	byteBudget := MaxSearchResponseBytes
	labelBuf := make([]string, 0, MaxLabelKeysPerObject+1)

	// 마지막으로 **훑은** 위치입니다. 방출 여부와 무관하게 갱신해야 다음 페이지가
	// 검사한 곳 바로 뒤에서 이어집니다.
	var lastGVR, lastNS string
	var lastRow *indexRow
	var lastNsTokens int64
	exhausted := false

	stop := func() (SearchPage, error) {
		page.Truncated = true
		if lastRow != nil {
			// 누락 비트를 함께 실어 다음 페이지가 같은 사실을 이어 말하게 합니다.
			// 창이 닫혔다는 사실(reasonScopedScan)은 이 페이지의 것이라 싣지 않습니다.
			page.NextCursor = encodeSearchCursor(searchCursorKey{
				mode: cursorModeScan, namespace: lastNS, name: lastRow.name,
				gvr: lastGVR, uid: rowUID(lastRow), nsLabelTokens: lastNsTokens,
				scanFlags: diag.flags,
			}, fingerprint)
		}
		if exhausted {
			diag.note(reasonScopedScan)
		}
		diag.apply(page)
		return *page, nil
	}

	for i, gvr := range s.order {
		gvrKey := FormatGVR(gvr)
		if hasCursor && gvrKey < cursor.gvr {
			continue
		}
		// **요청 뷰의 목록 스냅숏만** 씁니다. 이 경로는 검색 인덱스를 훑지 않으므로
		// 세대를 빌리지도, 들여다보지도 않습니다.
		index := view.baseAt(i)
		desc, err := s.describeWithIndex(gvr, index)
		if err != nil || !desc.Namespaced {
			continue // allowlist 밖이거나, 범위 제한 사용자에게 보이지 않는 클러스터 범위입니다.
		}
		if desc.State != StateReady {
			diag.noteOmission(scanStateFlag(desc.State), searchStateReason(desc.State))
			continue
		}
		if index == nil {
			diag.noteOmission(scanFlagSyncing, reasonSyncing)
			continue
		}
		kindTok := normalizeToken(desc.Kind)

		for _, ns := range allowed {
			if hasCursor && gvrKey == cursor.gvr && ns < cursor.namespace {
				continue
			}
			sp := index.namespaceSpan(ns)
			if sp.lo >= sp.hi {
				continue // 이 리소스에는 그 namespace의 객체가 없습니다.
			}

			start := sp.lo
			nsTokens := int64(0)
			if hasCursor && gvrKey == cursor.gvr && ns == cursor.namespace {
				start = resumeAfter(index.rows, sp, cursor)
				nsTokens = cursor.nsLabelTokens
			}
			for i := start; i < sp.hi; i++ {
				if len(page.Items) >= limit {
					return stop()
				}
				if budget <= 0 {
					exhausted = true
					return stop()
				}
				budget--
				row := &index.rows[i]
				_, field, matched, incomplete, next := scopedMatch(row, kindTok, query, labelBuf, &nsTokens)
				labelBuf = next
				if incomplete {
					diag.noteOmission(scanFlagLabelNs, reasonLabelNs)
				}
				lastGVR, lastNS, lastRow, lastNsTokens = gvrKey, ns, row, nsTokens
				if !matched {
					continue
				}
				cost := searchRowOverheadBytes + len(ns) + len(row.name) + len(rowUID(row)) + len(gvrKey)
				if len(page.Items) > 0 && cost > byteBudget {
					return stop()
				}
				byteBudget -= cost
				page.Items = append(page.Items, SearchItem{
					Group: GroupSegment(gvr.Group), Version: gvr.Version, Resource: gvr.Resource,
					Kind: desc.Kind, Namespaced: true,
					Namespace: ns, Name: row.name, UID: rowUID(row),
					MatchedField: matchedFieldNames[field],
				})
			}
		}
	}
	diag.apply(page)
	return *page, nil
}

// resumeAfter는 cursor 바로 다음 행의 위치입니다.
// 구간 안에서 이름이 정렬되어 있으므로 이분탐색입니다.
func resumeAfter(rows []indexRow, sp span, cursor searchCursorKey) int {
	return sp.lo + sort.Search(sp.hi-sp.lo, func(k int) bool {
		row := &rows[sp.lo+k]
		if row.name != cursor.name {
			return row.name > cursor.name
		}
		return rowUID(row) > cursor.uid
	})
}

// scopedMatch는 행 하나를 **색인 빌드와 같은 규칙**으로 판정합니다.
//
// 행 단위 키 상한과 namespace 단위 토큰 상한을 빌드와 똑같이 적용하고, 잘리면
// incomplete를 세웁니다. nsTokens는 이 namespace를 여기까지 훑는 동안 센 label
// 토큰 수이며, 상한 판정을 페이지 사이에서 이어가려고 cursor에 실립니다.
func scopedMatch(row *indexRow, kindTok, query string, buf []string, nsTokens *int64) (
	token string, field uint32, matched bool, incomplete bool, next []string) {

	consider := func(rawTok string, f uint32) {
		tok := normalizeToken(rawTok)
		if !safeToken(tok) || !strings.HasPrefix(tok, query) {
			return
		}
		if !matched || tok < token {
			token = tok
		}
		if !matched || f < field {
			field = f
		}
		matched = true
	}
	consider(row.name, fieldName)
	if row.namespace != "" {
		consider(row.namespace, fieldNamespace)
	}
	if kindTok != "" {
		consider(kindTok, fieldKind)
	}
	next = buf
	if row.obj == nil || len(row.obj.Labels) == 0 {
		return token, field, matched, incomplete, next
	}
	keys, keysTruncated := sortedLabelKeys(row, buf)
	next = keys[:0]
	if keysTruncated {
		incomplete = true
	}
	rowTokens := labelTokenCount(row, keys)
	if *nsTokens+rowTokens > MaxLabelTokensPerNamespace {
		// 빌드와 같은 지점에서 자릅니다. 이 행의 label은 색인되지 않았을 것이므로
		// 순회에서도 보지 않습니다.
		return token, field, matched, true, next
	}
	for _, k := range keys {
		consider(k, fieldLabel)
		if v := row.obj.Labels[k]; v != "" {
			consider(v, fieldLabel)
		}
	}
	*nsTokens += rowTokens
	return token, field, matched, incomplete, next
}

/* ── 클러스터 전체 색인 경로 ──────────────────────────────────────────────── */

// searchStream은 (GVR, namespace) 하나의 postings 순회입니다.
type searchStream struct {
	gvrKey     string
	group      string
	version    string
	resource   string
	kind       string
	namespaced bool

	snap       *searchSnapshot
	ns         string
	loID, hiID uint32
	pos, end   int

	curToken string
	curName  string
	curUID   string
}

func (s *searchStream) load() bool {
	if s.pos >= s.end {
		return false
	}
	p := s.snap.postings[s.pos]
	row := &s.snap.base.rows[p.row]
	s.curToken = s.snap.tokens[p.token]
	s.curName = row.name
	s.curUID = rowUID(row)
	return true
}

// searchKeyLess는 색인 경로의 결과 전순서입니다: (token, namespace, name, gvr, uid).
func searchKeyLess(aToken, aNS, aName, aGVR, aUID, bToken, bNS, bName, bGVR, bUID string) bool {
	if aToken != bToken {
		return aToken < bToken
	}
	if aNS != bNS {
		return aNS < bNS
	}
	if aName != bName {
		return aName < bName
	}
	if aGVR != bGVR {
		return aGVR < bGVR
	}
	return aUID < bUID
}

func (s *searchStream) less(o *searchStream) bool {
	return searchKeyLess(s.curToken, s.ns, s.curName, s.gvrKey, s.curUID,
		o.curToken, o.ns, o.curName, o.gvrKey, o.curUID)
}

type streamHeap []*searchStream

func (h streamHeap) down(i int) {
	n := len(h)
	for {
		l, r, small := 2*i+1, 2*i+2, i
		if l < n && h[l].less(h[small]) {
			small = l
		}
		if r < n && h[r].less(h[small]) {
			small = r
		}
		if small == i {
			return
		}
		h[i], h[small] = h[small], h[i]
		i = small
	}
}

func (h streamHeap) init() {
	for i := len(h)/2 - 1; i >= 0; i-- {
		h.down(i)
	}
}

// searchIndexed는 클러스터 전체 접근의 색인 병합 경로입니다.
func (s *Service) searchIndexed(query string, limit int, cursor searchCursorKey, hasCursor bool,
	fingerprint string, page *SearchPage, diag *searchDiagnostics, view *searchView) (SearchPage, error) {

	streams, ok := s.openStreams(query, cursor, hasCursor, page, diag, view)
	if !ok {
		return SearchPage{}, ErrSearchTooBroad
	}
	diag.apply(page)
	if len(streams) == 0 {
		return *page, nil
	}
	streams.init()

	scanBudget := limit * searchScanFactor
	if scanBudget > maxSearchScan {
		scanBudget = maxSearchScan
	}
	if scanBudget < limit {
		scanBudget = limit
	}
	byteBudget := MaxSearchResponseBytes
	examined := 0
	var lastKey searchCursorKey
	haveKey := false
	stop := func() (SearchPage, error) {
		page.Truncated = true
		if haveKey {
			page.NextCursor = encodeSearchCursor(lastKey, fingerprint)
		}
		return *page, nil
	}

	for len(streams) > 0 {
		if len(page.Items) >= limit || examined >= scanBudget {
			return stop()
		}
		st := streams[0]
		p := st.snap.postings[st.pos]
		examined++
		// 한 행이 여러 필드에 걸려도 **가장 작은 토큰에서 한 번만** 나갑니다.
		if id, field, found := st.snap.matchInRange(p.row, st.loID, st.hiID); found && id == p.token {
			cost := searchRowOverheadBytes + len(st.ns) + len(st.curName) + len(st.curUID) + len(st.gvrKey)
			if len(page.Items) > 0 && cost > byteBudget {
				return stop()
			}
			byteBudget -= cost
			page.Items = append(page.Items, SearchItem{
				Group: st.group, Version: st.version, Resource: st.resource,
				Kind: st.kind, Namespaced: st.namespaced,
				Namespace: st.ns, Name: st.curName, UID: st.curUID,
				MatchedField: matchedFieldNames[field],
			})
		}
		lastKey = searchCursorKey{
			mode: cursorModeIndex, token: st.curToken, namespace: st.ns,
			name: st.curName, gvr: st.gvrKey, uid: st.curUID,
		}
		haveKey = true

		st.pos++
		if st.load() {
			streams.down(0)
			continue
		}
		last := len(streams) - 1
		streams[0], streams[last] = streams[last], nil
		streams = streams[:last]
		if len(streams) > 0 {
			streams.down(0)
		}
	}
	return *page, nil
}

// openStreams는 클러스터 전체 접근에서 모든 (GVR, namespace) 스트림을 엽니다.
// 스냅숏은 요청 뷰가 이미 고정해 둔 것을 그대로 씁니다 — 여기서 다시 읽지 않습니다.
func (s *Service) openStreams(query string, cursor searchCursorKey, hasCursor bool,
	page *SearchPage, diag *searchDiagnostics, view *searchView) (streamHeap, bool) {
	streams := make(streamHeap, 0, 32)

	for i, gvr := range s.order {
		// **요청 뷰에서만** 집습니다(빌린 세대와 훑는 세대를 일치시킵니다).
		// 카운트는 baseline, 검색 절반은 빌린 세대에서 옵니다.
		es := view.searchAt(i)
		desc, err := s.describeWithIndex(gvr, view.baseAt(i))
		if err != nil {
			continue
		}
		if desc.State != StateReady {
			diag.note(searchStateReason(desc.State))
			continue
		}
		if es == nil {
			diag.note(reasonSyncing)
			continue
		}
		// **상태를 먼저 봅니다.** 예산 초과로 색인하지 못한 리소스는 search가 nil이므로,
		// nil을 먼저 검사하면 "동기화 중"으로 잘못 보고됩니다.
		switch es.searchState {
		case SearchReady:
			if es.search == nil {
				diag.note(reasonSyncing)
				continue
			}
		case SearchUnavailable:
			if es.searchReason != "" {
				diag.note(es.searchReason)
			} else {
				diag.note(reasonBudget)
			}
			continue
		case SearchDisabled:
			continue
		default:
			diag.note(reasonSyncing)
			continue
		}
		snap := es.search
		loID, hiID, hasPrefix := snap.prefixRange(query)

		gvrKey := FormatGVR(gvr)
		participates := false
		for nsIdx := range snap.nsNames {
			participates = true
			if snap.labelIncompleteIn(nsIdx) {
				diag.note(reasonLabelNs)
			}
			if !hasPrefix {
				continue
			}
			lo, end, found := snap.postingRange(nsIdx, loID, hiID)
			if !found {
				continue
			}
			st := &searchStream{
				gvrKey: gvrKey, group: GroupSegment(gvr.Group), version: gvr.Version,
				resource: gvr.Resource, kind: desc.Kind, namespaced: desc.Namespaced,
				snap: snap, ns: snap.nsNames[nsIdx], loID: loID, hiID: hiID, pos: lo, end: end,
			}
			if hasCursor {
				st.pos = snap.seekAfter(st, cursor)
			}
			if !st.load() {
				continue
			}
			if len(streams) >= MaxSearchStreams {
				return nil, false
			}
			streams = append(streams, st)
		}
		if !participates {
			continue
		}
		// 여러 GVR이 서로 다른 tick에 재구성되므로 **가장 오래된** 시각을 말합니다.
		if page.ObservedAt.IsZero() || snap.base.builtAt.Before(page.ObservedAt) {
			page.ObservedAt = snap.base.builtAt
		}
	}
	return streams, true
}

// searchStateReason은 informer 상태를 고정 문구로 옮깁니다.
func searchStateReason(state State) string {
	switch state {
	case StateSyncing:
		return reasonSyncing
	case StateUnsupported:
		return reasonUnsupported
	case StateForbidden:
		return reasonForbidden
	default:
		return reasonMissing
	}
}

// prefixRange는 접두사에 해당하는 연속 토큰 ID 구간입니다.
func (s *searchSnapshot) prefixRange(q string) (uint32, uint32, bool) {
	lo := sort.SearchStrings(s.tokens, q)
	if lo == len(s.tokens) || !strings.HasPrefix(s.tokens[lo], q) {
		return 0, 0, false
	}
	hi := lo + sort.Search(len(s.tokens)-lo, func(i int) bool { return !strings.HasPrefix(s.tokens[lo+i], q) })
	return uint32(lo), uint32(hi), true
}

// postingRange는 namespace 구간 안에서 토큰 ID 구간에 해당하는 postings 범위입니다.
func (s *searchSnapshot) postingRange(ns int, loID, hiID uint32) (int, int, bool) {
	p0, p1 := int(s.nsPostings[ns]), int(s.nsPostings[ns+1])
	lo := p0 + sort.Search(p1-p0, func(k int) bool { return s.postings[p0+k].token >= loID })
	hi := p0 + sort.Search(p1-p0, func(k int) bool { return s.postings[p0+k].token >= hiID })
	if lo >= hi {
		return 0, 0, false
	}
	return lo, hi, true
}

// matchInRange는 행 하나가 이 접두사 구간에서 갖는 **최소 토큰**과 **가장 구체적인 필드**입니다.
func (s *searchSnapshot) matchInRange(row, loID, hiID uint32) (uint32, uint32, bool) {
	seg := s.ids[s.off[row]:s.off[row+1]]
	i := sort.Search(len(seg), func(k int) bool { return seg[k]>>fieldShift >= loID })
	if i == len(seg) || seg[i]>>fieldShift >= hiID {
		return 0, 0, false
	}
	first, best := seg[i]>>fieldShift, seg[i]&fieldMask
	for k := i + 1; k < len(seg) && seg[k]>>fieldShift < hiID; k++ {
		if f := seg[k] & fieldMask; f < best {
			best = f
		}
	}
	return first, best, true
}

// seekAfter는 cursor보다 뒤에 있는 첫 위치입니다. 값으로 찾으므로 인덱스가 다시
// 만들어져도 그대로 이어집니다.
func (s *searchSnapshot) seekAfter(st *searchStream, cursor searchCursorKey) int {
	lo, end := st.pos, st.end
	return lo + sort.Search(end-lo, func(k int) bool {
		p := s.postings[lo+k]
		row := &s.base.rows[p.row]
		return searchKeyLess(cursor.token, cursor.namespace, cursor.name, cursor.gvr, cursor.uid,
			s.tokens[p.token], st.ns, row.name, st.gvrKey, rowUID(row))
	})
}

/* ── 계측 ───────────────────────────────────────────────────────────────── */

// SearchIndexBytes는 지금 보유 중인 검색 인덱스 바이트 합입니다.
func (s *Service) SearchIndexBytes() int64 {
	if s == nil {
		return 0
	}
	return s.searchBytes.Load()
}

// SearchPeakBytes는 재구성 중 (기존 보유량 + 신규 + 작업용)이 동시에 살아 있던 최대치입니다.
func (s *Service) SearchPeakBytes() int64 {
	if s == nil {
		return 0
	}
	return s.searchPeak.Load()
}

// MaxSearchBytes는 이 서비스의 보유 상한입니다.
func (s *Service) MaxSearchBytes() int64 {
	if s == nil {
		return 0
	}
	return s.cfg.MaxSearchIndexBytes
}

// MaxSearchPeakBytes는 재구성 정점의 상한입니다.
func (s *Service) MaxSearchPeakBytes() int64 {
	if s == nil {
		return 0
	}
	return searchPeakMultiplier * s.cfg.MaxSearchIndexBytes
}

func (s *Service) perResourceSearchBudget() int64 {
	return s.cfg.MaxSearchIndexBytes / searchPerResourceDivisor
}

// searchMetricSample은 리소스 하나의 계측 표본입니다.
type searchMetricSample struct {
	resource string
	state    string
	postings int
	bytes    int64
	// labels는 **ready일 때만** 1입니다.
	labels int
	// partitions는 증분 인덱스의 (GVR, namespace) 파티션 수입니다.
	partitions int
}

// WriteSearchMetrics는 검색 인덱스 계측을 Prometheus 텍스트로 씁니다.
//
// 총합과 리소스별 표본을 **한 번의 read lock 안에서** 뜹니다. 실제 쓰기는 잠금을
// 놓은 뒤에 합니다.
func (s *Service) WriteSearchMetrics(w io.Writer) error {
	if !s.Available() {
		return nil
	}
	samples := make([]searchMetricSample, 0, len(s.order))
	// 계측도 sindex를 훑습니다(파티션마다 postEntries 합산). 그래서 요청 경로와
	// 똑같이 세대를 빌리고, 다 훑은 뒤에 놓습니다.
	view := s.acquireView()
	defer s.releaseView(view)
	total := s.searchBytes.Load()
	peak := s.searchPeak.Load()
	for i, gvr := range s.order {
		sample := searchMetricSample{resource: FormatGVR(gvr), state: string(SearchSyncing)}
		if es := view.searchAt(i); es != nil {
			sample.state = string(es.searchState)
			if es.searchState == SearchReady && es.search != nil {
				sample.postings, sample.bytes, sample.labels = es.search.postingCount(), es.search.bytes, 1
			}
			if es.searchState == SearchReady && es.sindex != nil {
				sample.bytes += es.sindex.bytes
				sample.labels = 1
				sample.partitions = es.sindex.partitionCount()
				es.sindex.dir.each(func(p *nsPart) bool {
					sample.postings += int(p.postEntries)
					return true
				})
			}
		}
		samples = append(samples, sample)
	}

	// 큐 계측은 queueMu에서 따로 뜹니다. **두 잠금을 겹쳐 잡지 않습니다.**
	deltaSamples := s.sampleDeltaMetrics()

	if _, err := fmt.Fprintf(w, "dashboard_resource_search_index_bytes %d\ndashboard_resource_search_index_peak_bytes %d\ndashboard_resource_search_index_max_bytes %d\ndashboard_resource_search_index_peak_max_bytes %d\n",
		total, peak, s.MaxSearchBytes(), s.MaxSearchPeakBytes()); err != nil {
		return err
	}
	for _, sample := range samples {
		if _, err := fmt.Fprintf(w,
			"dashboard_resource_search_resource_bytes{resource=%q} %d\n"+
				"dashboard_resource_search_resource_postings{resource=%q} %d\n"+
				"dashboard_resource_search_resource_labels_indexed{resource=%q} %d\n"+
				"dashboard_resource_search_resource_state{resource=%q,state=%q} 1\n"+
				"dashboard_resource_search_resource_partitions{resource=%q} %d\n",
			sample.resource, sample.bytes, sample.resource, sample.postings,
			sample.resource, sample.labels, sample.resource, sample.state,
			sample.resource, sample.partitions); err != nil {
			return err
		}
	}
	if s.delta == nil {
		return nil
	}
	// 증분 경로 계측. 라벨은 resource 하나뿐이라 카디널리티가 allowlist로 묶여 있습니다.
	if _, err := fmt.Fprintf(w,
		"dashboard_resource_search_live_bytes %d\n"+
			"dashboard_resource_search_retained_bytes %d\n"+
			"dashboard_resource_search_queued_bytes %d\n"+
			"dashboard_resource_search_inflight_bytes %d\n"+
			"dashboard_resource_search_reservation_rejected_total %d\n"+
			"dashboard_resource_search_recovery_attempts_total %d\n"+
			"dashboard_resource_search_recovery_failures_total %d\n"+
			"dashboard_resource_search_full_bootstrap_total %d\n"+
			"dashboard_resource_search_partition_resync_total %d\n"+
			"dashboard_resource_search_full_recovery_total %d\n"+
			"dashboard_resource_search_publish_budget_rejects_total %d\n"+
			"dashboard_resource_search_list_store_list_total %d\n"+
			"dashboard_resource_search_delta_full_build_total %d\n",
		s.budget.live.Load(), s.budget.retained.Load(), s.budget.queued.Load(),
		s.budget.inflight.Load(), s.budget.rejected.Load(),
		s.delta.recoveryAttempts.Load(), s.delta.recoveryFailures.Load(),
		s.delta.fullBootstraps.Load(), s.delta.partitionResyncs.Load(),
		s.delta.fullRecoveries.Load(), s.delta.publishBudgetRejects.Load(),
		s.delta.storeListCalls.Load(), s.delta.deltaFullBuilds.Load()); err != nil {
		return err
	}
	for _, d := range deltaSamples {
		if _, err := fmt.Fprintf(w,
			"dashboard_resource_search_pending_events{resource=%q} %d\n"+
				"dashboard_resource_search_pending_bytes{resource=%q} %d\n"+
				"dashboard_resource_search_dropped_events_total{resource=%q} %d\n"+
				"dashboard_resource_search_delta_batches_total{resource=%q} %d\n"+
				"dashboard_resource_search_delta_batch_seconds_sum{resource=%q} %f\n"+
				"dashboard_resource_search_stale_partitions{resource=%q} %d\n"+
				"dashboard_resource_search_gvr_stale{resource=%q} %d\n",
			d.resource, d.pending, d.resource, d.pendingB, d.resource, d.dropped,
			d.resource, d.batches, d.resource, float64(d.batchNanos)/1e9,
			d.resource, d.staleParts, d.resource, d.gvrStale); err != nil {
			return err
		}
	}
	return nil
}
