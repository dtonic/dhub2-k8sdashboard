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
	"time"

	"github.com/xenx96/k8s-dashboard/apps/api/internal/datasource"
)

// ErrBadQuery는 서버가 만든 질의를 upstream이 거절한 경우입니다.
// 사용자 입력이 아니라 **우리 쿼리 카탈로그의 버그**이므로 로그로만 확인합니다.
var ErrBadQuery = errors.New("upstream이 질의를 거절했습니다")

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
	what string
}

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
	Headers map[string]string
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
	return &Client{
		base: u,
		http: &http.Client{
			Timeout: timeout,
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
	}, nil
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

func (c *Client) doJSON(ctx context.Context, method, path string, query url.Values, body []byte, out any, allowRetry bool) error {
	do := func() (retryable bool, err error) {
		req, err := c.newRequest(ctx, method, path, query, body)
		if err != nil {
			return false, err
		}
		res, err := c.http.Do(req)
		if err != nil {
			// 컨텍스트 취소는 사용자가 떠난 것이므로 재시도하지 않습니다.
			if ctx.Err() != nil {
				return false, ctx.Err()
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
		dec := json.NewDecoder(io.LimitReader(res.Body, maxBodyBytes))
		if err := dec.Decode(out); err != nil {
			return false, fmt.Errorf("%s 응답을 해석하지 못했습니다: %w", c.what, datasource.ErrUnavailable)
		}
		return false, nil
	}

	retryable, err := do()
	if err == nil || !retryable || !allowRetry {
		return err
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(retryBackoff):
	}
	_, err = do()
	return err
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
