package resourcecatalog

// 매니페스트 파싱과 구조 diff (ADR 0019 Phase 1)
// --------------------------------------------------------------------------
// 두 가지만 합니다 — 사용자가 보낸 문서를 **안전하게** 값으로 바꾸는 것,
// 그리고 정제된 두 객체의 차이를 **유계·결정적으로** 만드는 것.
//
// 파서 쪽 위협 모델은 명확합니다: 짧은 입력으로 CPU·메모리를 터뜨리는 문서
// (billion laughs 류 alias 폭발, 수만 겹 중첩, 거대 스칼라)와, 같은 키를 두 번
// 적어 검증을 통과한 뒤 다른 값이 적용되게 만드는 문서. 그래서 alias·anchor는
// 아예 거부하고, 깊이·노드 수·스칼라 길이에 상한을 두고, 중복 키를 거부합니다.

import (
	"encoding/json"
	"errors"
	"io"
	"sort"
	"strconv"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"gopkg.in/yaml.v3"

	"github.com/xenx96/k8s-dashboard/apps/api/internal/contract"
)

/* ── 매니페스트 파싱 ─────────────────────────────────────────────────────── */

// 파서 상한. 바이트 상한(cfg.MaxManifestBytes)을 이미 통과한 입력에 추가로 겁니다 —
// 256KiB 안에서도 중첩과 노드 수로 충분히 비싼 문서를 만들 수 있습니다.
const (
	// maxManifestDepth는 중첩 깊이 상한입니다. 실제 Kubernetes 객체는 10을 넘지 않습니다.
	maxManifestDepth = 32
	// maxManifestNodes는 문서 하나의 노드 수 상한입니다(키 노드 포함).
	maxManifestNodes = 20000
	// maxManifestScalarBytes는 스칼라 하나(키·값)의 바이트 상한입니다.
	maxManifestScalarBytes = 64 << 10
)

// decodeManifest는 YAML 또는 JSON **단일 문서**를 JSON 호환 map으로 바꿉니다.
//
// 거부하는 것: 문서 0개·2개 이상, 최상위가 mapping이 아닌 문서, 중복 mapping 키,
// anchor·alias·병합 키, 상한을 넘는 깊이·노드 수·스칼라, 해석할 수 없는 태그.
//
// 실패는 전부 ErrManifestInvalid 하나입니다. 어느 줄이 왜 틀렸는지를 되돌려주면
// 그 문자열에 매니페스트 조각이 실려 나갑니다.
func decodeManifest(raw string) (map[string]any, error) {
	dec := yaml.NewDecoder(strings.NewReader(raw))
	var root *yaml.Node
	for {
		var doc yaml.Node
		err := dec.Decode(&doc)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, ErrManifestInvalid
		}
		content := &doc
		if content.Kind == yaml.DocumentNode {
			// 내용이 없는 문서(`---`만 있는 구분자)는 문서로 세지 않습니다.
			if len(content.Content) == 0 {
				continue
			}
			if len(content.Content) != 1 {
				return nil, ErrManifestInvalid
			}
			content = content.Content[0]
		}
		if content.Kind == 0 {
			continue
		}
		if root != nil {
			// 두 번째 문서를 만나는 순간 끝냅니다. 나머지를 파싱할 이유가 없습니다.
			return nil, ErrManifestInvalid
		}
		root = content
	}
	if root == nil || root.Kind != yaml.MappingNode {
		return nil, ErrManifestInvalid
	}
	d := &manifestDecoder{}
	value, err := d.convert(root, 0)
	if err != nil {
		return nil, err
	}
	obj, ok := value.(map[string]any)
	if !ok {
		return nil, ErrManifestInvalid
	}
	return obj, nil
}

type manifestDecoder struct{ nodes int }

// convert는 YAML AST를 JSON 호환 값으로 바꾸면서 상한을 강제합니다.
//
// unstructured가 받아들이는 타입(string·bool·int64·float64·nil·[]any·map[string]any)만
// 만듭니다. 해석할 수 없는 태그를 조용히 문자열로 만들지 않는 것이 요점입니다 —
// 그러면 사용자가 적은 것과 서버가 보낸 것이 달라집니다.
func (d *manifestDecoder) convert(n *yaml.Node, depth int) (any, error) {
	if n == nil {
		return nil, ErrManifestInvalid
	}
	if depth > maxManifestDepth {
		return nil, ErrManifestInvalid
	}
	d.nodes++
	if d.nodes > maxManifestNodes {
		return nil, ErrManifestInvalid
	}
	// anchor를 남기면 어딘가에서 alias가 그것을 펼칠 수 있습니다. 둘 다 막습니다.
	if n.Anchor != "" || n.Kind == yaml.AliasNode {
		return nil, ErrManifestInvalid
	}
	switch n.Kind {
	case yaml.ScalarNode:
		return d.scalar(n)
	case yaml.SequenceNode:
		out := make([]any, 0, len(n.Content))
		for _, child := range n.Content {
			v, err := d.convert(child, depth+1)
			if err != nil {
				return nil, err
			}
			out = append(out, v)
		}
		return out, nil
	case yaml.MappingNode:
		if len(n.Content)%2 != 0 {
			return nil, ErrManifestInvalid
		}
		out := make(map[string]any, len(n.Content)/2)
		for i := 0; i < len(n.Content); i += 2 {
			key := n.Content[i]
			d.nodes++
			if d.nodes > maxManifestNodes {
				return nil, ErrManifestInvalid
			}
			// 복합 키·anchor 키·병합 키(<<)는 전부 거부합니다.
			if key.Kind != yaml.ScalarNode || key.Anchor != "" || key.Value == "<<" {
				return nil, ErrManifestInvalid
			}
			if len(key.Value) > maxManifestScalarBytes {
				return nil, ErrManifestInvalid
			}
			// **중복 키는 거부합니다.** 마지막 값이 조용히 이기면, 검증이 본 값과
			// 적용되는 값이 달라질 수 있습니다.
			if _, dup := out[key.Value]; dup {
				return nil, ErrManifestInvalid
			}
			v, err := d.convert(n.Content[i+1], depth+1)
			if err != nil {
				return nil, err
			}
			out[key.Value] = v
		}
		return out, nil
	default:
		return nil, ErrManifestInvalid
	}
}

func (d *manifestDecoder) scalar(n *yaml.Node) (any, error) {
	if len(n.Value) > maxManifestScalarBytes {
		return nil, ErrManifestInvalid
	}
	switch n.Tag {
	case "":
		// 태그를 해석하지 못한 노드입니다. 문자열로 봅니다.
		return n.Value, nil
	case "!!null":
		return nil, nil
	case "!!bool":
		v, err := strconv.ParseBool(n.Value)
		if err != nil {
			return nil, ErrManifestInvalid
		}
		return v, nil
	case "!!int":
		v, err := strconv.ParseInt(n.Value, 0, 64)
		if err != nil {
			return nil, ErrManifestInvalid
		}
		return v, nil
	case "!!float":
		v, err := strconv.ParseFloat(n.Value, 64)
		if err != nil {
			return nil, ErrManifestInvalid
		}
		return v, nil
	case "!!str", "!!timestamp":
		// timestamp는 Kubernetes JSON에서 문자열입니다. 따옴표 없이 쓴 날짜를
		// 거부하는 대신 같은 값의 문자열로 다룹니다.
		return n.Value, nil
	default:
		// !!binary·사용자 태그 등은 받아들이지 않습니다.
		return nil, ErrManifestInvalid
	}
}

/* ── 정제 ────────────────────────────────────────────────────────────────
   현재본과 dry-run 결과에 **같은 정책**을 적용합니다. 한쪽만 지우면 지운 쪽이
   전부 변경으로 보입니다. */

// volatileMetadata는 서버가 소유해 매 요청 달라지는 필드입니다.
// 이것들이 남아 있으면 모든 dry-run이 "변경 있음"이 됩니다.
var volatileMetadata = []string{
	"managedFields", "resourceVersion", "generation", "creationTimestamp",
	"uid", "selfLink", "deletionTimestamp", "deletionGracePeriodSeconds",
}

// stripVolatile은 status와 휘발성 metadata를 지우고 지운 경로를 돌려줍니다.
func stripVolatile(obj *unstructured.Unstructured) []string {
	var removed []string
	if _, had := obj.Object["status"]; had {
		delete(obj.Object, "status")
		removed = append(removed, "status")
	}
	meta, _ := obj.Object["metadata"].(map[string]any)
	if meta == nil {
		return removed
	}
	for _, field := range volatileMetadata {
		if _, had := meta[field]; had {
			delete(meta, field)
			removed = append(removed, joinDiffPath("metadata", field))
		}
	}
	return removed
}

// collectSensitive는 정제로 사라질 **값**을 미리 모읍니다.
//
// 정제 후에 비교하면 민감 필드의 변경이 통째로 사라져, 사용자는 아무것도 바뀌지
// 않은 줄 압니다. 그래서 지우기 전에 한 번 떠서 "바뀌었다는 사실"만 남기고
// 값은 버립니다.
// 경로는 diff와 **같은 escape 정책**으로 만듭니다. annotation 키는 임의 문자열이라
// 그대로 이어 붙이면 서로 다른 키가 같은 경로로 보일 수 있습니다.
func collectSensitive(obj *unstructured.Unstructured, gvr schema.GroupVersionResource) map[string]string {
	out := map[string]string{}
	if gvr == secretGVR {
		for _, field := range []string{"data", "stringData"} {
			if v, had := obj.Object[field]; had {
				rendered, _ := json.Marshal(v)
				out[joinDiffPath("", field)] = string(rendered)
			}
		}
	}
	for key, value := range obj.GetAnnotations() {
		if isSensitiveAnnotation(key) {
			out[joinDiffPath("metadata.annotations", key)] = value
		}
	}
	return out
}

/* ── 구조 diff ───────────────────────────────────────────────────────────── */

// maxDiffNodes는 비교가 방문할 노드 수 상한입니다. 두 객체가 각각 1MiB 이하이므로
// 정상 입력은 여기 닿지 않습니다. 닿으면 결과를 버리고 ErrTooLarge입니다.
//
// const가 아니라 var인 것은 **테스트 seam**입니다 — 소진 경로를 20만 노드짜리
// 객체를 만들지 않고 확인하기 위해서입니다. env·Helm 노브가 아닙니다.
var maxDiffNodes = 200000

// maxDiffDepth는 비교 깊이 상한입니다. 더 깊은 곳은 통째로 하나의 변경입니다.
const maxDiffDepth = 32

type reviewDiff struct {
	changes []contract.ResourceDryRunChange
	// total은 절단 **이전** 전체 변경 수이며 언제나 정확합니다. 순회를 끝내지
	// 못하면 근사치를 돌려주는 대신 오류를 냅니다.
	total     int
	truncated bool
	redacted  []string
}

// compareForReview는 정제 후의 두 객체를 비교합니다. 입력 객체는 건드리지 않습니다.
//
// 오류를 돌려주는 경우는 두 가지이고 둘 다 **표현할 수 없는 결과**입니다.
//   - 경로 하나가 상한(512바이트)을 넘음 — 애매하게 잘라서 다른 필드처럼 보이게
//     만드는 것보다 실패가 낫습니다.
//   - 순회 예산 소진 — 그때의 changeCount는 전체 수가 아니므로, 부분 diff를
//     완전한 diff로 내보내지 않습니다.
//
// 둘 다 ErrTooLarge입니다. 정상 입력(각 1MiB 이하)은 여기 닿지 않습니다.
func compareForReview(live, patched *unstructured.Unstructured, gvr schema.GroupVersionResource) (reviewDiff, error) {
	before := live.DeepCopy()
	after := patched.DeepCopy()

	// 값을 지우기 **전에** 민감 필드를 떠 둡니다.
	beforeSensitive := collectSensitive(before, gvr)
	afterSensitive := collectSensitive(after, gvr)

	removed := map[string]bool{}
	for path := range beforeSensitive {
		removed[path] = true
	}
	for path := range afterSensitive {
		removed[path] = true
	}
	// stripVolatile을 **먼저** 돌려 managedFields 등을 자기 표기로 보고하게 합니다.
	for _, path := range stripVolatile(before) {
		removed[path] = true
	}
	for _, path := range stripVolatile(after) {
		removed[path] = true
	}
	// sanitize는 **변형만** 쓰고 반환 경로는 버립니다. 상세 응답(ADR 0018)의 표기와
	// 이 경로의 escape 정책이 다르기 때문입니다 — 두 표기를 섞으면 같은 목록 안에
	// 서로 다른 규칙의 경로가 들어갑니다. 지워지는 대상 자체는 위 두 출처가 이미
	// 같은 정책으로 덮습니다.
	sanitize(before, gvr)
	sanitize(after, gvr)

	c := &diffCollector{}
	// 민감 경로의 변경은 값 없이 사실만 싣습니다.
	for _, path := range sortedKeysOf(beforeSensitive, afterSensitive) {
		bv, bok := beforeSensitive[path]
		av, aok := afterSensitive[path]
		switch {
		case bok && aok && bv != av:
			c.emit(contract.ResourceDryRunChange{Path: path, Op: contract.DryRunChangeChanged, ValueRedacted: true})
		case bok && !aok:
			c.emit(contract.ResourceDryRunChange{Path: path, Op: contract.DryRunChangeRemoved, ValueRedacted: true})
		case !bok && aok:
			c.emit(contract.ResourceDryRunChange{Path: path, Op: contract.DryRunChangeAdded, ValueRedacted: true})
		}
	}
	c.walk("", before.Object, after.Object, 0)
	if c.err != nil {
		return reviewDiff{}, c.err
	}

	return reviewDiff{
		changes: c.result(),
		total:   c.total,
		// 순회를 끝냈으므로 total은 정확합니다. truncated는 오직 "상한 때문에
		// 일부만 담았다"는 뜻이고, "세지 못했다"는 뜻이 아닙니다.
		truncated: c.total > len(c.changes),
		redacted:  boundedPaths(removed),
	}, nil
}

// boundedPaths는 정제 경로 목록을 정렬·유계화합니다.
func boundedPaths(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for path := range set {
		out = append(out, boundedText(path, dryRunTextBytes))
	}
	sort.Strings(out)
	if len(out) > contract.MaxDryRunRedacted {
		out = out[:contract.MaxDryRunRedacted]
	}
	return out
}

// sortedKeysOf는 두 map의 키 합집합을 정렬해 돌려줍니다. 순회 순서를 고정하는
// 유일한 장치이므로 diff의 결정성이 여기에 걸려 있습니다.
func sortedKeysOf[V any](a, b map[string]V) []string {
	seen := make(map[string]bool, len(a)+len(b))
	out := make([]string, 0, len(a)+len(b))
	for k := range a {
		if !seen[k] {
			seen[k] = true
			out = append(out, k)
		}
	}
	for k := range b {
		if !seen[k] {
			seen[k] = true
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// diffCollector는 **정렬된 상위 N개만** 들고 있으면서 전체 개수는 정확히 셉니다.
//
// 전부 모아 두고 나중에 자르면, 변경이 수만 개인 객체에서 메모리가 상한을 넘습니다.
// 삽입할 때마다 경로 순서를 유지하고 상한을 넘는 뒤쪽은 버리므로, 보유량은 언제나
// MaxDryRunChanges개이고 결과는 입력 순서와 무관하게 같습니다.
type diffCollector struct {
	changes []contract.ResourceDryRunChange
	total   int
	nodes   int
	// err이 서면 순회를 멈추고 결과 전체를 버립니다. 부분 결과를 성공으로
	// 내보내지 않는 것이 이 필드의 유일한 목적입니다.
	err error
}

// emit은 변경 하나를 담습니다. 경로가 상한을 넘으면 **잘라 담지 않고 실패**합니다 —
// 잘린 경로는 다른 필드를 가리키는 것처럼 보이고, 그 오해가 곧 잘못된 검토입니다.
func (c *diffCollector) emit(ch contract.ResourceDryRunChange) {
	if c.err != nil {
		return
	}
	if len(ch.Path) > dryRunTextBytes {
		c.err = ErrTooLarge
		return
	}
	c.total++
	n := len(c.changes)
	if n >= contract.MaxDryRunChanges {
		if ch.Path >= c.changes[n-1].Path {
			return
		}
		c.changes = c.changes[:n-1]
	}
	i := sort.Search(len(c.changes), func(i int) bool { return c.changes[i].Path >= ch.Path })
	c.changes = append(c.changes, contract.ResourceDryRunChange{})
	copy(c.changes[i+1:], c.changes[i:])
	c.changes[i] = ch
}

// result는 언제나 non-nil입니다 — 계약이 필수 배열입니다.
func (c *diffCollector) result() []contract.ResourceDryRunChange {
	if c.changes == nil {
		return []contract.ResourceDryRunChange{}
	}
	return c.changes
}

func (c *diffCollector) valueChange(path string, op contract.ResourceDryRunChangeOp, before, after any) {
	ch := contract.ResourceDryRunChange{Path: path, Op: op}
	if op != contract.DryRunChangeAdded {
		ch.Before, ch.ValueTruncated = renderDiffValue(before)
	}
	if op != contract.DryRunChangeRemoved {
		rendered, truncated := renderDiffValue(after)
		ch.After = rendered
		ch.ValueTruncated = ch.ValueTruncated || truncated
	}
	c.emit(ch)
}

// walk는 두 값을 leaf까지 비교합니다. 추가·삭제된 **하위 트리는 항목 하나**입니다 —
// 그 안의 leaf를 전부 펼치면 항목 수가 입력 크기만큼 늘어납니다.
func (c *diffCollector) walk(path string, before, after any, depth int) {
	if c.err != nil {
		return
	}
	c.nodes++
	if c.nodes > maxDiffNodes {
		// 여기서 멈추면 total이 전체 수가 아닙니다. 근사치를 내보내는 대신 실패합니다.
		c.err = ErrTooLarge
		return
	}
	if depth > maxDiffDepth {
		if !sameRendered(before, after) {
			c.valueChange(path, contract.DryRunChangeChanged, before, after)
		}
		return
	}
	switch b := before.(type) {
	case map[string]any:
		a, ok := after.(map[string]any)
		if !ok {
			c.valueChange(path, contract.DryRunChangeChanged, before, after)
			return
		}
		for _, key := range sortedKeysOf(b, a) {
			bv, bok := b[key]
			av, aok := a[key]
			child := joinDiffPath(path, key)
			switch {
			case bok && !aok:
				c.valueChange(child, contract.DryRunChangeRemoved, bv, nil)
			case !bok && aok:
				c.valueChange(child, contract.DryRunChangeAdded, nil, av)
			default:
				c.walk(child, bv, av, depth+1)
			}
			if c.err != nil {
				return
			}
		}
	case []any:
		a, ok := after.([]any)
		if !ok {
			c.valueChange(path, contract.DryRunChangeChanged, before, after)
			return
		}
		c.walkList(path, b, a, depth)
	default:
		if !scalarEqual(before, after) {
			c.valueChange(path, contract.DryRunChangeChanged, before, after)
		}
	}
}

// walkList는 목록을 비교합니다.
//
// 모든 원소가 고유한 name을 가진 객체면 **name으로 짝을 맞춥니다.** 컨테이너 하나를
// 앞에 끼워 넣었을 뿐인데 뒤쪽 전부가 변경으로 보이는 것을 막는 것이 목적입니다.
// 짝짓기는 map 두 개를 한 번씩 만드는 O(n)이고, 원소끼리 비교하는 O(n²)는 하지 않습니다.
func (c *diffCollector) walkList(path string, before, after []any, depth int) {
	if bn, an, ok := nameKeyedList(before, after); ok {
		for _, name := range sortedKeysOf(bn, an) {
			bv, bok := bn[name]
			av, aok := an[name]
			child := path + namePathSegment(name)
			switch {
			case bok && !aok:
				c.valueChange(child, contract.DryRunChangeRemoved, bv, nil)
			case !bok && aok:
				c.valueChange(child, contract.DryRunChangeAdded, nil, av)
			default:
				c.walk(child, bv, av, depth+1)
			}
			if c.err != nil {
				return
			}
		}
		return
	}
	longest := len(before)
	if len(after) > longest {
		longest = len(after)
	}
	for i := 0; i < longest; i++ {
		child := path + indexPathSegment(i)
		switch {
		case i >= len(after):
			c.valueChange(child, contract.DryRunChangeRemoved, before[i], nil)
		case i >= len(before):
			c.valueChange(child, contract.DryRunChangeAdded, nil, after[i])
		default:
			c.walk(child, before[i], after[i], depth+1)
		}
		if c.err != nil {
			return
		}
	}
}

// nameKeyedList는 두 목록이 모두 "고유한 name을 가진 객체 목록"인지 보고,
// 맞으면 name → 원소 map 두 개를 돌려줍니다. 한 번씩만 훑습니다.
func nameKeyedList(before, after []any) (map[string]any, map[string]any, bool) {
	if len(before) == 0 || len(after) == 0 {
		return nil, nil, false
	}
	bn, ok := indexByName(before)
	if !ok {
		return nil, nil, false
	}
	an, ok := indexByName(after)
	if !ok {
		return nil, nil, false
	}
	return bn, an, true
}

func indexByName(items []any) (map[string]any, bool) {
	out := make(map[string]any, len(items))
	for _, item := range items {
		obj, ok := item.(map[string]any)
		if !ok {
			return nil, false
		}
		name, ok := obj["name"].(string)
		if !ok || name == "" {
			return nil, false
		}
		if _, dup := out[name]; dup {
			return nil, false // 이름이 겹치면 짝을 맞출 수 없습니다. 인덱스로 돌아갑니다.
		}
		out[name] = item
	}
	return out, true
}

/* ── canonical 경로 ───────────────────────────────────────────────────────

   경로는 **단사(injective)**여야 합니다. 서로 다른 키 계열이 같은 문자열이 되면
   사용자는 다른 필드의 변경을 보고 승인하게 됩니다. map 키는 임의 문자열이라
   (annotation·label·ConfigMap data가 대표적입니다) `.`·`[`·`]`·따옴표·제어문자가
   그대로 들어올 수 있고, 그것을 점 경로에 이어 붙이면 곧바로 충돌합니다 —
   키 `a.b` 하나와 중첩된 `a`→`b`가 둘 다 `a.b`가 됩니다.

   규칙은 세 가지뿐입니다.
     - 단순 키(`[A-Za-z0-9_-]+`)  →  `parent.key`      (기존 읽기 쉬운 표기 유지)
     - 목록 인덱스               →  `parent[3]`
     - 그 밖의 키·목록 이름       →  `parent["따옴표 표현"]`
   숫자로만 된 목록 이름도 따옴표를 씌웁니다. 그래야 이름 "0"과 인덱스 0이
   구분됩니다. */

// simplePathKey는 점 경로에 그대로 쓸 수 있는 키인지입니다.
// 여기서 `.`을 제외하는 것이 핵심입니다 — 구분자와 같은 문자를 허용하면 단사가 깨집니다.
func simplePathKey(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '_', c == '-':
		default:
			return false
		}
	}
	return true
}

func allDigitPathKey(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// quotedPathSegment는 임의 키를 유계·단사 표현으로 감쌉니다.
//
// strconv.Quote가 따옴표·역슬래시·제어문자를 이미 이스케이프하므로, 남는 구멍은
// 대괄호뿐입니다. Quote 출력에 남은 `[`·`]`는 **반드시 입력에 있던 것**이므로
// (Quote가 스스로 만들지 않습니다) 그것만 \x 형태로 바꾸면 세그먼트 경계가
// 모호해지지 않습니다. 역슬래시는 이미 이스케이프되어 있어 `\x5d`가 미리 존재할 수도 없습니다.
func quotedPathSegment(key string) string {
	q := strconv.Quote(key)
	q = strings.ReplaceAll(q, "]", `\x5d`)
	q = strings.ReplaceAll(q, "[", `\x5b`)
	return "[" + q + "]"
}

// joinDiffPath는 map 키 하나를 경로에 붙입니다. 최상위는 접두사가 없습니다.
func joinDiffPath(parent, key string) string {
	if !simplePathKey(key) {
		return parent + quotedPathSegment(key)
	}
	if parent == "" {
		return key
	}
	return parent + "." + key
}

// indexPathSegment는 위치로 짝지은 목록 원소입니다. 언제나 숫자뿐입니다.
func indexPathSegment(i int) string { return "[" + strconv.Itoa(i) + "]" }

// namePathSegment는 name으로 짝지은 목록 원소입니다.
// 숫자만으로 된 이름은 인덱스와 구분하기 위해 따옴표를 씌웁니다.
func namePathSegment(name string) string {
	if simplePathKey(name) && !allDigitPathKey(name) {
		return "[" + name + "]"
	}
	return quotedPathSegment(name)
}

// scalarEqual은 JSON 호환 스칼라만 비교합니다.
//
// `==`를 interface에 그대로 쓰면 map·slice가 들어왔을 때 런타임 panic입니다.
// 비교할 수 없는 타입은 "다르다"로 봅니다 — 그 경우는 상위 case가 이미 걸러냅니다.
func scalarEqual(a, b any) bool {
	switch av := a.(type) {
	case nil:
		return b == nil
	case string:
		bv, ok := b.(string)
		return ok && av == bv
	case bool:
		bv, ok := b.(bool)
		return ok && av == bv
	case int64:
		bv, ok := b.(int64)
		return ok && av == bv
	case float64:
		bv, ok := b.(float64)
		return ok && av == bv
	default:
		return false
	}
}

// sameRendered는 깊이 상한을 넘은 하위 트리를 직렬화해 비교합니다.
// 상한 아래에서만 쓰이므로 비용이 입력 크기에 비례해 커지지 않습니다.
func sameRendered(a, b any) bool {
	left, errA := json.Marshal(a)
	right, errB := json.Marshal(b)
	if errA != nil || errB != nil {
		return false
	}
	return string(left) == string(right)
}

// renderDiffValue는 값을 compact JSON으로 만들고 UTF-8 경계에서 자릅니다.
// 두 번째 반환값은 잘렸는지입니다.
func renderDiffValue(v any) (string, bool) {
	raw, err := json.Marshal(v)
	if err != nil {
		return "", false
	}
	s := string(raw)
	if len(s) <= contract.MaxDryRunValueBytes {
		return s, false
	}
	return truncateUTF8(s, contract.MaxDryRunValueBytes), true
}
