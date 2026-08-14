package observability

import (
	"context"
	"sort"
	"strings"
	"sync"
)

const maxQueryRefs = 32

type traceKey struct{}
type Trace struct {
	mu        sync.Mutex
	refs      []string
	overflow  bool
	requestID string
}

func (t *Trace) SetRequestID(v string) { t.mu.Lock(); t.requestID = v; t.mu.Unlock() }
func (t *Trace) RequestID() string     { t.mu.Lock(); defer t.mu.Unlock(); return t.requestID }

func WithTrace(ctx context.Context) (context.Context, *Trace) {
	t := &Trace{}
	return context.WithValue(ctx, traceKey{}, t), t
}
func TraceFrom(ctx context.Context) *Trace { t, _ := ctx.Value(traceKey{}).(*Trace); return t }
func RecordQueryRef(ctx context.Context, ref string) {
	t := TraceFrom(ctx)
	if t == nil || !safeRef(ref) {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, existing := range t.refs {
		if existing == ref {
			return
		}
	}
	if len(t.refs) == maxQueryRefs {
		t.overflow = true
		return
	}
	t.refs = append(t.refs, ref)
}
func (t *Trace) Summary() (string, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	refs := append([]string(nil), t.refs...)
	sort.Strings(refs)
	return strings.Join(refs, ","), t.overflow
}
func safeRef(ref string) bool {
	if ref == "" || len(ref) > 64 {
		return false
	}
	for _, c := range ref {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("._:-", c)) {
			return false
		}
	}
	return true
}
