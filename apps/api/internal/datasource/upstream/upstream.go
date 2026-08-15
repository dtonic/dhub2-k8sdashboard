// Package upstream은 데이터소스 HTTP 클라이언트의 공통 바닥입니다.
//
// GreptimeDB와 Quickwit 어댑터가 같은 규칙을 공유합니다 —
//   - 오류 분류: 연결 실패·타임아웃·5xx는 datasource.ErrUnavailable로,
//     4xx는 ErrBadQuery로 접습니다. 에러 문자열에 **주소·질의를 담지 않습니다.**
//     섹션 degraded 사유로 그대로 노출될 수 있기 때문입니다. (README §10)
//   - 제한적 retry: 멱등한 조회에 한해 일시 오류(연결 실패·502·503·504)를 1회만
//     재시도합니다. 데이터소스가 이미 힘들 때 재시도 폭풍을 만들지 않습니다.
//   - 요청 취소 전파: 컨텍스트가 죽으면 데이터소스 요청도 함께 끊습니다. (README §11)
package upstream

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/xenx96/k8s-dashboard/apps/api/internal/datasource"
)

// ErrBadQuery는 서버가 만든 질의를 upstream이 거절한 경우입니다.
// 사용자 입력이 아니라 **우리 쿼리 카탈로그의 버그**이므로 로그로만 확인합니다.
var ErrBadQuery = errors.New("upstream이 질의를 거절했습니다")
var errCircuitOpen = errors.New("upstream circuit open")

// maxBodyBytes는 upstream 응답 크기 상한입니다. 대량 응답이 그대로
// 메모리에 올라오지 않게 합니다. (README §11)
const maxBodyBytes = 32 << 20 // 32 MiB

// retryBackoff는 1회 재시도 전 대기 시간입니다.
const retryBackoff = 200 * time.Millisecond

// Client는 인증·타임아웃·재시도 규칙이 적용된 HTTP 클라이언트입니다.
type Client struct {
	base    *url.URL
	http    *http.Client
	user    string
	pass    string
	headers map[string]string
	// what은 오류 메시지에 실을 데이터소스 이름입니다. 주소가 아니라 이름만 노출합니다.
	what             string
	timeout          time.Duration
	now              func() time.Time
	breakerMu        sync.Mutex
	breakerFailures  int
	breakerOpenUntil time.Time
	halfOpen         bool
	breakerThreshold int
	breakerCooldown  time.Duration
	observer         Observer
	upstream         Upstream
	circuitState     CircuitState
	circuitGen       uint64
}

// Observer receives bounded logical-call and circuit state events.
type Observer interface {
	ObserveUpstream(ctx context.Context, upstream Upstream, outcome Outcome, duration time.Duration)
	SetCircuit(upstream Upstream, state CircuitState, generation uint64)
}

type Upstream uint8

const (
	UpstreamOther Upstream = iota
	UpstreamGreptime
	UpstreamQuickwit
	UpstreamAlertmanager
)

func (u Upstream) String() string {
	names := [...]string{"other", "greptime", "quickwit", "alertmanager"}
	if int(u) < 0 || int(u) >= len(names) {
		return names[0]
	}
	return names[u]
}

type Outcome uint8

const (
	OutcomeSuccess Outcome = iota
	OutcomeTimeout
	OutcomeCanceled
	OutcomeBadQuery
	OutcomeUnavailable
	OutcomeCircuitOpen
)

func (o Outcome) String() string {
	names := [...]string{"success", "timeout", "canceled", "bad_query", "unavailable", "circuit_open"}
	if int(o) < 0 || int(o) >= len(names) {
		return names[4]
	}
	return names[o]
}

type CircuitState int

const (
	CircuitClosed CircuitState = iota
	CircuitOpen
	CircuitHalfOpen
)

// Config는 클라이언트 구성입니다.
type Config struct {
	// BaseURL은 데이터소스 주소입니다. 예: http://greptimedb:4000
	BaseURL string
	// What은 오류 메시지용 이름입니다. 예: "GreptimeDB"
	What     string
	Username string
	Password string
	// Timeout은 요청 1건의 상한입니다. 0이면 10초입니다.
	Timeout time.Duration
	// Headers는 모든 요청에 붙는 고정 헤더입니다. 예: X-Greptime-DB-Name
	Headers          map[string]string
	CircuitThreshold int
	CircuitCooldown  time.Duration
	Now              func() time.Time
	Observer         Observer
	// Upstream is a fixed observability identity: greptime, quickwit, or other.
	Upstream Upstream
}

// New는 클라이언트를 만듭니다. BaseURL이 비어 있으면 오류입니다.
func New(cfg Config) (*Client, error) {
	if strings.TrimSpace(cfg.BaseURL) == "" {
		return nil, fmt.Errorf("%s 주소가 비어 있습니다", cfg.What)
	}
	u, err := url.Parse(cfg.BaseURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("%s 주소를 해석할 수 없습니다", cfg.What)
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	if cfg.CircuitThreshold <= 0 {
		cfg.CircuitThreshold = 3
	}
	if cfg.CircuitCooldown <= 0 {
		cfg.CircuitCooldown = 5 * time.Second
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	upstream := cfg.Upstream
	if upstream > UpstreamAlertmanager {
		upstream = UpstreamOther
	}
	c := &Client{
		base: u,
		http: &http.Client{
			Transport: &http.Transport{
				MaxIdleConns:        16,
				MaxIdleConnsPerHost: 8,
				IdleConnTimeout:     90 * time.Second,
			},
		},
		user:    cfg.Username,
		pass:    cfg.Password,
		headers: cfg.Headers,
		what:    cfg.What,
		timeout: timeout, now: cfg.Now, breakerThreshold: cfg.CircuitThreshold, breakerCooldown: cfg.CircuitCooldown,
		observer: cfg.Observer, upstream: upstream,
	}
	if c.observer != nil {
		c.observer.SetCircuit(c.upstream, CircuitClosed, 0)
	}
	return c, nil
}

// GetJSON은 GET 요청을 보내고 JSON 응답을 out에 담습니다.
func (c *Client) GetJSON(ctx context.Context, path string, query url.Values, out any) error {
	return c.doJSON(ctx, http.MethodGet, path, query, nil, out, true)
}

// PostJSON은 JSON 본문으로 POST하고 JSON 응답을 out에 담습니다.
// 조회용 POST(검색 API)에만 씁니다. 재시도 규칙이 GET과 같으므로
// 상태를 바꾸는 요청에 쓰면 안 됩니다.
func (c *Client) PostJSON(ctx context.Context, path string, body any, out any) error {
	return c.PostJSONQuery(ctx, path, nil, body, out)
}

// PostJSONQuery는 JSON 조회 POST에 query parameter를 함께 보냅니다.
func (c *Client) PostJSONQuery(ctx context.Context, path string, query url.Values, body any, out any) error {
	raw, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("%s 요청을 만들지 못했습니다: %w", c.what, err)
	}
	return c.doJSON(ctx, http.MethodPost, path, query, raw, out, true)
}

// GetJSONBodyOnce는 body가 있는 조회 GET을 재시도 없이 한 번 보냅니다.
// scroll처럼 같은 capability 재사용의 멱등성이 보장되지 않는 API에만 씁니다.
func (c *Client) GetJSONBodyOnce(ctx context.Context, path string, body any, out any) error {
	raw, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("%s 요청을 만들지 못했습니다: %w", c.what, err)
	}
	return c.doJSON(ctx, http.MethodGet, path, nil, raw, out, false)
}

func (c *Client) doJSON(ctx context.Context, method, path string, query url.Values, body []byte, out any, allowRetry bool) (resultErr error) {
	started := c.now()
	defer func() {
		if c.observer != nil {
			c.observer.ObserveUpstream(ctx, c.upstream, outcome(ctx, resultErr), c.now().Sub(started))
		}
	}()
	probe, ok := c.breakerAllow()
	if !ok {
		return fmt.Errorf("%s: %w: %w", c.what, datasource.ErrUnavailable, errCircuitOpen)
	}
	logicalCtx, cancelLogical := context.WithTimeout(ctx, c.timeout)
	defer cancelLogical()
	backoff := retryBackoff
	attemptTimeout := c.timeout
	if allowRetry {
		if max := c.timeout / 4; max < backoff {
			backoff = max
		}
		attemptTimeout = (c.timeout - backoff) / 2
		if attemptTimeout <= 0 {
			attemptTimeout = c.timeout
		}
	}
	do := func() (retryable bool, err error) {
		attemptCtx, cancel := context.WithTimeout(logicalCtx, attemptTimeout)
		defer cancel()
		req, err := c.newRequest(attemptCtx, method, path, query, body)
		if err != nil {
			return false, err
		}
		res, err := c.http.Do(req)
		if err != nil {
			// 컨텍스트 취소는 사용자가 떠난 것이므로 재시도하지 않습니다.
			if ctx.Err() != nil {
				return false, ctx.Err()
			}
			if errors.Is(attemptCtx.Err(), context.DeadlineExceeded) {
				return true, fmt.Errorf("%s: %w: %w", c.what, datasource.ErrUnavailable, context.DeadlineExceeded)
			}
			return true, fmt.Errorf("%s: %w", c.what, datasource.ErrUnavailable)
		}
		defer res.Body.Close()

		switch {
		case res.StatusCode >= 200 && res.StatusCode < 300:
			// 통과
		case res.StatusCode == http.StatusBadGateway,
			res.StatusCode == http.StatusServiceUnavailable,
			res.StatusCode == http.StatusGatewayTimeout,
			res.StatusCode == http.StatusTooManyRequests:
			io.Copy(io.Discard, io.LimitReader(res.Body, 4096))
			return true, fmt.Errorf("%s: %w", c.what, datasource.ErrUnavailable)
		case res.StatusCode >= 500:
			return false, fmt.Errorf("%s: %w", c.what, datasource.ErrUnavailable)
		default: // 4xx — 우리가 만든 질의가 잘못된 경우입니다.
			return false, fmt.Errorf("%s: %w", c.what, ErrBadQuery)
		}

		if out == nil {
			return false, nil
		}
		limited := &io.LimitedReader{R: res.Body, N: maxBodyBytes + 1}
		dec := json.NewDecoder(limited)
		if err := dec.Decode(out); err != nil {
			return false, fmt.Errorf("%s 응답을 해석하지 못했습니다: %w", c.what, datasource.ErrUnavailable)
		}
		var trailing any
		trailingErr := dec.Decode(&trailing)
		consumed := int64(maxBodyBytes+1) - limited.N
		if consumed > maxBodyBytes || !errors.Is(trailingErr, io.EOF) {
			return false, fmt.Errorf("%s 응답이 크기 또는 단일 JSON 한도를 위반했습니다: %w", c.what, datasource.ErrUnavailable)
		}
		return false, nil
	}

	retryable, err := do()
	if err == nil || !retryable || !allowRetry {
		c.breakerFinish(probe, err == nil, c.breakerCounts(ctx, err))
		return err
	}
	select {
	case <-logicalCtx.Done():
		c.breakerFinish(probe, false, ctx.Err() == nil)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("%s: %w: %w", c.what, datasource.ErrUnavailable, context.DeadlineExceeded)
	case <-time.After(backoff):
	}
	retryable, err = do()
	c.breakerFinish(probe, err == nil, c.breakerCounts(ctx, err))
	return err
}

func outcome(caller context.Context, err error) Outcome {
	if err == nil {
		return OutcomeSuccess
	}
	if caller.Err() != nil {
		if errors.Is(caller.Err(), context.DeadlineExceeded) {
			return OutcomeTimeout
		}
		return OutcomeCanceled
	}
	if errors.Is(err, errCircuitOpen) {
		return OutcomeCircuitOpen
	}
	if errors.Is(err, ErrBadQuery) {
		return OutcomeBadQuery
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return OutcomeTimeout
	}
	return OutcomeUnavailable
}

func (c *Client) breakerCounts(caller context.Context, err error) bool {
	return caller.Err() == nil && errors.Is(err, datasource.ErrUnavailable)
}

func (c *Client) breakerAllow() (bool, bool) {
	c.breakerMu.Lock()
	now := c.now()
	if c.breakerOpenUntil.IsZero() {
		c.breakerMu.Unlock()
		return false, true
	}
	if now.Before(c.breakerOpenUntil) {
		c.breakerMu.Unlock()
		return false, false
	}
	if c.halfOpen {
		c.breakerMu.Unlock()
		return false, false
	}
	c.halfOpen = true
	c.circuitState = CircuitHalfOpen
	c.circuitGen++
	state, generation := c.circuitState, c.circuitGen
	c.breakerMu.Unlock()
	c.setCircuit(state, generation)
	return true, true
}
func (c *Client) breakerFinish(probe, success, countFailure bool) {
	c.breakerMu.Lock()
	notify := false
	if success {
		notify = c.circuitState != CircuitClosed
		c.breakerFailures = 0
		c.breakerOpenUntil = time.Time{}
		c.halfOpen = false
		c.circuitState = CircuitClosed
		if notify {
			c.circuitGen++
		}
		state, generation := c.circuitState, c.circuitGen
		c.breakerMu.Unlock()
		if notify {
			c.setCircuit(state, generation)
		}
		return
	}
	if probe {
		c.halfOpen = false
	}
	if !countFailure {
		c.breakerMu.Unlock()
		return
	}
	c.breakerFailures++
	if probe || c.breakerFailures >= c.breakerThreshold {
		c.breakerOpenUntil = c.now().Add(c.breakerCooldown)
		c.halfOpen = false
		c.circuitState = CircuitOpen
		c.circuitGen++
		notify = true
	}
	state, generation := c.circuitState, c.circuitGen
	c.breakerMu.Unlock()
	if notify {
		c.setCircuit(state, generation)
	}
}

func (c *Client) setCircuit(state CircuitState, generation uint64) {
	if c.observer != nil {
		c.observer.SetCircuit(c.upstream, state, generation)
	}
}

func (c *Client) newRequest(ctx context.Context, method, path string, query url.Values, body []byte) (*http.Request, error) {
	u := *c.base
	u.Path = strings.TrimSuffix(u.Path, "/") + path
	if query != nil {
		u.RawQuery = query.Encode()
	}
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), rd)
	if err != nil {
		return nil, fmt.Errorf("%s 요청을 만들지 못했습니다: %w", c.what, err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	if c.user != "" || c.pass != "" {
		req.SetBasicAuth(c.user, c.pass)
	}
	for k, v := range c.headers {
		req.Header.Set(k, v)
	}
	return req, nil
}
