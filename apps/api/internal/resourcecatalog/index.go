package resourcecatalog

import (
	"sort"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
)

// 목록 응답의 유계 상한입니다. 브라우저가 더 큰 값을 보내도 넘지 못합니다. (ADR 0018 결정 4)
const (
	// DefaultPageSize는 limit을 지정하지 않았을 때의 페이지 크기입니다.
	DefaultPageSize = 50
	// MaxPageSize는 한 페이지의 절대 상한입니다.
	MaxPageSize = 200
	// MaxResponseBytes는 목록 응답 본문의 상한입니다.
	MaxResponseBytes = 1 << 20
	// MaxNameFilterLen은 name prefix 필터 길이 상한입니다(DNS 이름 상한과 같습니다).
	MaxNameFilterLen = 253
	// MaxSelectorLen은 label selector 문자열 길이 상한입니다.
	MaxSelectorLen = 512
	// MaxSelectorRequirements는 label selector 표현식 개수 상한입니다.
	MaxSelectorRequirements = 8
	// maxScanRows는 필터가 걸린 요청 하나가 훑을 수 있는 행 수 상한입니다.
	// 상한에 걸리면 지금까지 검사한 위치를 cursor로 돌려주므로 중복·누락이 없습니다.
	maxScanRows = 5000
	// scanFactor는 limit 대비 최소 scan 예산 배수입니다.
	scanFactor = 20
	// rowOverheadBytes는 목록 한 행의 JSON 고정 비용 추정치입니다.
	rowOverheadBytes = 96
)

// indexRow는 정렬 인덱스의 한 행입니다.
//
// namespace/name을 행에 직접 담는 이유는 정렬·이분탐색이 포인터를 따라가지 않게
// 하기 위해서입니다. obj는 informer 캐시가 소유한 읽기 전용 객체입니다.
type indexRow struct {
	namespace string
	name      string
	obj       *metav1.PartialObjectMetadata
}

// indexSnapshot은 (namespace, name)으로 정렬된 불변 인덱스입니다.
//
// 요청은 이 snapshot에 대해 이분탐색 + 페이지 크기만큼의 순회만 합니다.
// **요청마다 전체 정렬·복사·순회를 하지 않습니다.**
type indexSnapshot struct {
	rows    []indexRow
	builtAt time.Time
}

func lessRow(a, b indexRow) bool {
	if a.namespace != b.namespace {
		return a.namespace < b.namespace
	}
	return a.name < b.name
}

func buildIndexSnapshot(objs []any, at time.Time) *indexSnapshot {
	rows := make([]indexRow, 0, len(objs))
	for _, o := range objs {
		m, ok := o.(*metav1.PartialObjectMetadata)
		if !ok || m == nil {
			continue
		}
		rows = append(rows, indexRow{namespace: m.Namespace, name: m.Name, obj: m})
	}
	sort.Slice(rows, func(i, j int) bool { return lessRow(rows[i], rows[j]) })
	return &indexSnapshot{rows: rows, builtAt: at}
}

// lookup은 정렬 인덱스에서 (namespace, name) 행 하나를 찾습니다.
//
// 상세 조회가 "목록에서 실제로 본 행"인지 확인하는 경계입니다. 전체 순회가 아니라
// 이분탐색이므로 100k 인덱스에서도 비용은 O(log n)입니다.
func (s *indexSnapshot) lookup(namespace, name string) (*indexRow, bool) {
	want := indexRow{namespace: namespace, name: name}
	i := sort.Search(len(s.rows), func(i int) bool { return !lessRow(s.rows[i], want) })
	if i < len(s.rows) && s.rows[i].namespace == namespace && s.rows[i].name == name {
		return &s.rows[i], true
	}
	return nil, false
}

// namespaceSpan은 정렬 인덱스에서 한 namespace가 차지하는 [lo, hi) 구간입니다.
func (s *indexSnapshot) namespaceSpan(ns string) span {
	lo := sort.Search(len(s.rows), func(i int) bool { return s.rows[i].namespace >= ns })
	hi := sort.Search(len(s.rows), func(i int) bool { return s.rows[i].namespace > ns })
	return span{lo: lo, hi: hi}
}

type span struct{ lo, hi int }

// NamespaceFilter는 서버가 이미 Scope와 교차시킨 namespace 집합입니다.
// All이면 전체, 아니면 List에 있는 것만 봅니다. 핸들러가 아니라 Scope가 채웁니다.
type NamespaceFilter struct {
	All  bool
	List []string
}

// ListRequest는 한 번의 목록 조회입니다. 필터·정렬·페이지는 전부 유계입니다.
type ListRequest struct {
	Group      string
	Version    string
	Resource   string
	Namespaces NamespaceFilter
	// NamePrefix는 이름 prefix 필터입니다. 빈 값이면 적용하지 않습니다.
	NamePrefix string
	// LabelSelector는 Kubernetes label selector 문자열입니다.
	LabelSelector string
	Limit         int
	Cursor        string
	// Descending은 (namespace, name) 역순 정렬입니다. 정렬 키는 이 하나뿐입니다 —
	// 인덱스 없는 정렬을 허용하면 100k 리소스에서 요청마다 전체 정렬이 됩니다.
	Descending bool
}

// Row는 목록 한 줄입니다. 신원은 이름이 아니라 UID입니다. (README §5)
type Row struct {
	Namespace string
	Name      string
	UID       string
	CreatedAt string
}

// ListPage는 유계 페이지 하나입니다.
type ListPage struct {
	Items      []Row
	NextCursor string
	// Truncated는 페이지·byte·scan 예산 때문에 조기 종료했음을 뜻합니다.
	Truncated bool
	// Total은 이 GVR 인덱스의 전체 행 수입니다(필터 전).
	Total      int
	ObservedAt time.Time
}

// resolvedRequest는 검증·정규화가 끝난 조회입니다.
type resolvedRequest struct {
	limit      int
	descending bool
	namePrefix string
	selector   labels.Selector
	// spanAll이 true면 인덱스 전체가 순회 대상입니다(전체 namespace 권한 또는 클러스터 범위 리소스).
	// false면 namespaces에 있는 구간만 봅니다. **비어 있으면 볼 수 있는 것이 없다는 뜻이지
	// 전체가 아닙니다.**
	spanAll     bool
	namespaces  []string
	cursor      cursorKey
	hasCursor   bool
	fingerprint string
	// maxBytes는 이 응답의 byte 예산입니다. 0이거나 MaxResponseBytes보다 크면
	// page()가 MaxResponseBytes로 조입니다 — 프로덕션 상한은 어떤 값을 넣어도 1MiB입니다.
	// 실제 Kubernetes 이름 길이로는 200행 상한이 이미 1MiB를 보장하므로, 예산 경로 자체를
	// 검증하려면 테스트가 이 값을 작게 잡습니다.
	maxBytes int
}

// responseBudget은 이 요청에 허용된 byte 예산입니다. 상한은 언제나 MaxResponseBytes입니다.
func (r resolvedRequest) responseBudget() int {
	if r.maxBytes <= 0 || r.maxBytes > MaxResponseBytes {
		return MaxResponseBytes
	}
	return r.maxBytes
}

// page는 snapshot 위에서 keyset 페이지 하나를 만듭니다.
//
// 중복·누락이 없어야 합니다. cursor는 **마지막으로 검사한 행의 키**이므로
// 필터에 걸린 행을 다시 훑지 않고, 방출한 행을 다시 방출하지도 않습니다.
func (s *indexSnapshot) page(req resolvedRequest) ListPage {
	page := ListPage{Items: make([]Row, 0, req.limit), Total: len(s.rows), ObservedAt: s.builtAt}
	w := s.newWalker(req)
	budget := req.responseBudget()
	scanBudget := req.limit * scanFactor
	if scanBudget > maxScanRows {
		scanBudget = maxScanRows
	}
	if scanBudget < req.limit {
		scanBudget = req.limit
	}
	examined := 0
	var lastKey cursorKey
	haveKey := false

	for {
		row, ok := w.next()
		if !ok {
			break
		}
		examined++
		if matches(row, req) {
			cost := rowOverheadBytes + len(row.namespace) + len(row.name)
			if len(page.Items) > 0 && cost > budget {
				// 이 행은 방출하지 않습니다. cursor는 직전 행에 머물러 다음 페이지가 여기서 이어집니다.
				page.Truncated = true
				if haveKey {
					page.NextCursor = encodeCursor(lastKey, req.fingerprint)
				}
				return page
			}
			budget -= cost
			page.Items = append(page.Items, toRow(row))
		}
		lastKey, haveKey = cursorKey{namespace: row.namespace, name: row.name}, true
		if len(page.Items) >= req.limit || examined >= scanBudget {
			if w.hasNext() {
				page.Truncated = true
				page.NextCursor = encodeCursor(lastKey, req.fingerprint)
			}
			return page
		}
	}
	return page
}

func toRow(row *indexRow) Row {
	out := Row{Namespace: row.namespace, Name: row.name}
	if row.obj != nil {
		out.UID = string(row.obj.UID)
		if !row.obj.CreationTimestamp.IsZero() {
			out.CreatedAt = row.obj.CreationTimestamp.UTC().Format(time.RFC3339)
		}
	}
	return out
}

func matches(row *indexRow, req resolvedRequest) bool {
	if req.namePrefix != "" && !strings.HasPrefix(row.name, req.namePrefix) {
		return false
	}
	if req.selector != nil && !req.selector.Empty() {
		if row.obj == nil {
			return false
		}
		if !req.selector.Matches(labels.Set(row.obj.Labels)) {
			return false
		}
	}
	return true
}

/* ── 순회 ─────────────────────────────────────────────────────────────────
   Scope가 namespace 목록으로 좁혀져 있으면 namespace마다 이분탐색으로 구간을
   잡습니다. 전체 접근이면 구간은 하나입니다. 어느 쪽이든 요청 비용은
   O(구간 수 × log n + 페이지 크기)이고 전체 순회가 아닙니다. */

type walker struct {
	rows  []indexRow
	spans []span
	desc  bool
	s     int
	i     int
	done  bool
}

func (s *indexSnapshot) newWalker(req resolvedRequest) *walker {
	var spans []span
	if req.spanAll {
		spans = []span{{lo: 0, hi: len(s.rows)}}
	} else {
		spans = make([]span, 0, len(req.namespaces))
		for _, ns := range req.namespaces {
			sp := s.namespaceSpan(ns)
			if sp.lo < sp.hi {
				spans = append(spans, sp)
			}
		}
	}
	if req.hasCursor {
		spans = clipSpans(s.rows, spans, req.cursor, req.descending)
	}
	if req.descending {
		for i, j := 0, len(spans)-1; i < j; i, j = i+1, j-1 {
			spans[i], spans[j] = spans[j], spans[i]
		}
	}
	w := &walker{rows: s.rows, spans: spans, desc: req.descending}
	w.reset()
	return w
}

// clipSpans는 cursor 위치를 기준으로 이미 지나간 구간을 잘라냅니다.
func clipSpans(rows []indexRow, spans []span, cursor cursorKey, desc bool) []span {
	out := spans[:0]
	for _, sp := range spans {
		if desc {
			// cursor보다 작은 행만 남깁니다.
			hi := sp.lo + sort.Search(sp.hi-sp.lo, func(i int) bool {
				r := rows[sp.lo+i]
				return !lessKey(cursorKey{r.namespace, r.name}, cursor)
			})
			if hi > sp.lo {
				out = append(out, span{lo: sp.lo, hi: hi})
			}
			continue
		}
		// cursor보다 큰 행만 남깁니다.
		lo := sp.lo + sort.Search(sp.hi-sp.lo, func(i int) bool {
			r := rows[sp.lo+i]
			return lessKey(cursor, cursorKey{r.namespace, r.name})
		})
		if lo < sp.hi {
			out = append(out, span{lo: lo, hi: sp.hi})
		}
	}
	return out
}

func lessKey(a, b cursorKey) bool {
	if a.namespace != b.namespace {
		return a.namespace < b.namespace
	}
	return a.name < b.name
}

func (w *walker) reset() {
	w.s = 0
	if len(w.spans) == 0 {
		w.done = true
		return
	}
	if w.desc {
		w.i = w.spans[0].hi - 1
		return
	}
	w.i = w.spans[0].lo
}

func (w *walker) hasNext() bool {
	if w.done || w.s >= len(w.spans) {
		return false
	}
	if w.desc {
		return w.i >= w.spans[w.s].lo || w.s+1 < len(w.spans)
	}
	return w.i < w.spans[w.s].hi || w.s+1 < len(w.spans)
}

func (w *walker) next() (*indexRow, bool) {
	for !w.done && w.s < len(w.spans) {
		sp := w.spans[w.s]
		if w.desc {
			if w.i < sp.lo {
				w.s++
				if w.s < len(w.spans) {
					w.i = w.spans[w.s].hi - 1
				}
				continue
			}
			row := &w.rows[w.i]
			w.i--
			return row, true
		}
		if w.i >= sp.hi {
			w.s++
			if w.s < len(w.spans) {
				w.i = w.spans[w.s].lo
			}
			continue
		}
		row := &w.rows[w.i]
		w.i++
		return row, true
	}
	w.done = true
	return nil, false
}
