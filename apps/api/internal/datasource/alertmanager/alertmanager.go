// Package alertmanager는 읽기 전용 Alertmanager API v2 어댑터를 구현합니다.
package alertmanager

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/xenx96/k8s-dashboard/apps/api/internal/contract"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/datasource"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/datasource/mask"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/datasource/upstream"
)

const (
	defaultTimeout       = 5 * time.Second
	defaultMaxBody       = 4 << 20
	defaultMaxAlerts     = 2000
	defaultMaxConcurrent = 4
	maxCatalogPods       = 100000
	maxLabels            = 32
	maxAnnotations       = 8
	maxKeyBytes          = 128
	maxLabelBytes        = 1 << 10
	maxAnnotationBytes   = 4 << 10
	maxAlertBytes        = 16 << 10
	maxTokenFileBytes    = 8 << 10
	maxCAFileBytes       = 1 << 20
	maxClientFileBytes   = 1 << 20
	maxScopedNamespaces  = 256
	maxQueryBytes        = 8 << 10
	maxReceivers         = 32
	maxStatusRefs        = 128
)

var (
	ErrHistoryNotConfigured = datasource.ErrAlertHistoryNotConfigured
	errUnavailable          = errors.New("alertmanager_unavailable")
	errUnauthorized         = errors.New("alertmanager_unauthorized")
	errInvalidResponse      = errors.New("alertmanager_invalid_response")
	errResponseTooLarge     = errors.New("alertmanager_response_too_large")
	errCircuitOpen          = errors.New("alertmanager_circuit_open")
)

type Observer interface {
	upstream.Observer
	ObserveAlertSeverityFallback()
}

// Config는 서버가 소유한 Alertmanager 설정만 담습니다.
type Config struct {
	Enabled        bool
	EnabledInvalid bool
	BaseURL        string
	PublicURL      string
	TokenFile      string
	CAFile         string
	ClientCertFile string
	ClientKeyFile  string
	ServerName     string
	ClusterLabel   string
	NamespaceLabel string
	Timeout        time.Duration
	MaxBodyBytes   int64
	MaxAlerts      int
	MaxConcurrent  int
	Observer       Observer
	Now            func() time.Time
}

type validated struct {
	base, public *url.URL
	token        string
	tls          *tls.Config
}

// Validate는 연결하지 않고 설정과 credential/TLS 자료를 검사합니다.
func Validate(cfg Config) error {
	_, err := validate(cfg)
	return err
}

func validate(cfg Config) (validated, error) {
	if cfg.EnabledInvalid {
		return validated{}, errors.New("ALERTMANAGER_ENABLED must be a boolean")
	}
	if !cfg.Enabled {
		return validated{}, nil
	}
	if !promLabel(cfg.ClusterLabel) {
		return validated{}, errors.New("ALERTMANAGER_CLUSTER_LABEL is invalid")
	}
	if !promLabel(cfg.NamespaceLabel) {
		return validated{}, errors.New("ALERTMANAGER_NAMESPACE_LABEL is invalid")
	}
	if cfg.ClusterLabel == cfg.NamespaceLabel {
		return validated{}, errors.New("Alertmanager cluster and namespace labels must be distinct")
	}
	base, err := strictHTTPSURL(cfg.BaseURL)
	if err != nil {
		return validated{}, errors.New("ALERTMANAGER_URL is invalid")
	}
	public, err := strictHTTPSURL(cfg.PublicURL)
	if err != nil {
		return validated{}, errors.New("ALERTMANAGER_PUBLIC_URL is invalid")
	}
	if cfg.TokenFile == "" || cfg.CAFile == "" {
		return validated{}, errors.New("Alertmanager token and CA files are required")
	}
	if (cfg.ClientCertFile == "") != (cfg.ClientKeyFile == "") {
		return validated{}, errors.New("Alertmanager client certificate and key must be configured together")
	}
	if cfg.Timeout < 100*time.Millisecond || cfg.Timeout > 30*time.Second {
		return validated{}, errors.New("ALERTMANAGER_TIMEOUT is out of range")
	}
	if cfg.MaxBodyBytes < 64<<10 || cfg.MaxBodyBytes > 16<<20 {
		return validated{}, errors.New("ALERTMANAGER_MAX_BODY_BYTES is out of range")
	}
	if cfg.MaxAlerts < 1 || cfg.MaxAlerts > 10000 {
		return validated{}, errors.New("ALERTMANAGER_MAX_ALERTS is out of range")
	}
	if cfg.MaxConcurrent < 1 || cfg.MaxConcurrent > 32 {
		return validated{}, errors.New("ALERTMANAGER_MAX_CONCURRENT is out of range")
	}
	if cfg.ServerName == "" || !validServerName(cfg.ServerName) {
		return validated{}, errors.New("ALERTMANAGER_SERVER_NAME is invalid")
	}
	tokenBytes, err := readRegularFile(cfg.TokenFile, maxTokenFileBytes)
	if err != nil {
		return validated{}, errors.New("Alertmanager token file is unavailable")
	}
	token := strings.TrimSpace(string(tokenBytes))
	if token == "" || len(token) > maxTokenFileBytes || !bearerTokenRE.MatchString(token) {
		return validated{}, errors.New("Alertmanager token file is invalid")
	}
	caBytes, err := readRegularFile(cfg.CAFile, maxCAFileBytes)
	if err != nil {
		return validated{}, errors.New("Alertmanager CA file is unavailable")
	}
	roots, err := validCertPool(caBytes, time.Now())
	if err != nil {
		return validated{}, errors.New("Alertmanager CA file is invalid")
	}
	tlsCfg := &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots, ServerName: cfg.ServerName}
	if cfg.ClientCertFile != "" {
		certPEM, certErr := readRegularFile(cfg.ClientCertFile, maxClientFileBytes)
		keyPEM, keyErr := readRegularFile(cfg.ClientKeyFile, maxClientFileBytes)
		if certErr != nil || keyErr != nil {
			return validated{}, errors.New("Alertmanager client certificate is unavailable")
		}
		cert, err := tls.X509KeyPair(certPEM, keyPEM)
		if err != nil {
			return validated{}, errors.New("Alertmanager client certificate is invalid")
		}
		now := time.Now()
		for _, der := range cert.Certificate {
			parsed, err := x509.ParseCertificate(der)
			if err != nil || now.Before(parsed.NotBefore) || now.After(parsed.NotAfter) {
				return validated{}, errors.New("Alertmanager client certificate is invalid")
			}
		}
		tlsCfg.Certificates = []tls.Certificate{cert}
	}
	return validated{base: base, public: public, token: token, tls: tlsCfg}, nil
}

func readRegularFile(name string, maxBytes int64) ([]byte, error) {
	if !filepath.IsAbs(name) || filepath.Clean(name) != name {
		return nil, errors.New("invalid file path")
	}
	// O_NONBLOCK은 open과 handle 검증 사이 경로가 FIFO로 바뀌어도 시작이 멈추지 않게 합니다.
	// projected Secret symlink는 계속 최종 regular file로 해석됩니다.
	f, err := os.OpenFile(name, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, errors.New("invalid credential file")
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() < 1 || info.Size() > maxBytes {
		return nil, errors.New("invalid credential file")
	}
	b, err := io.ReadAll(io.LimitReader(f, maxBytes+1))
	if err != nil || int64(len(b)) > maxBytes {
		return nil, errors.New("invalid credential file")
	}
	return b, nil
}

func validCertPool(pemBytes []byte, now time.Time) (*x509.CertPool, error) {
	pool := x509.NewCertPool()
	valid := 0
	for len(pemBytes) > 0 {
		block, rest := pem.Decode(pemBytes)
		if block == nil {
			return nil, errors.New("invalid PEM")
		}
		pemBytes = rest
		if block.Type != "CERTIFICATE" {
			continue
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil || now.Before(cert.NotBefore) || now.After(cert.NotAfter) {
			return nil, errors.New("invalid certificate")
		}
		pool.AddCert(cert)
		valid++
	}
	if valid == 0 {
		return nil, errors.New("missing certificate")
	}
	return pool, nil
}

func strictHTTPSURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.RawPath != "" {
		return nil, errors.New("invalid URL")
	}
	hostname := u.Hostname()
	if hostname == "" || net.ParseIP(hostname) == nil && !validServerName(hostname) {
		return nil, errors.New("invalid URL host")
	}
	if port := u.Port(); port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return nil, errors.New("invalid URL port")
		}
	}
	if u.Path != "" && (path.Clean(u.Path) != u.Path || strings.Contains(u.Path, "//")) {
		return nil, errors.New("unsafe URL path")
	}
	for _, part := range strings.Split(u.Path, "/") {
		if part == "." || part == ".." {
			return nil, errors.New("unsafe URL path")
		}
	}
	return u, nil
}

func validServerName(v string) bool {
	if net.ParseIP(v) != nil {
		return true
	}
	if len(v) > 253 || strings.ContainsAny(v, "/: *\t\r\n") {
		return false
	}
	for _, label := range strings.Split(v, ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, c := range label {
			if !(c == '-' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9') {
				return false
			}
		}
	}
	return true
}

// Source는 direct 요청과 central cluster poller가 함께 사용해도 안전합니다.
type Source struct {
	cfg               Config
	base              *url.URL
	public            *url.URL
	token             string
	http              *http.Client
	catalog           datasource.PodCatalog
	sem               chan struct{}
	mu                sync.Mutex
	fails             int
	openTil           time.Time
	halfOpen          bool
	circuitGeneration uint64
	closeOnce         sync.Once
	now               func() time.Time
	closed            atomic.Bool
}

func New(cfg Config, catalog datasource.PodCatalog) (*Source, error) {
	if catalog == nil {
		return nil, errors.New("Alertmanager pod catalog is required")
	}
	s, err := NewUnbound(cfg)
	if err != nil {
		return nil, err
	}
	s.catalog = catalog
	return s, nil
}

// NewUnbound는 cluster 연결 전에 모든 로컬 자료를 검증하고 transport를 만듭니다.
func NewUnbound(cfg Config) (*Source, error) {
	v, err := validate(cfg)
	if err != nil {
		return nil, err
	}
	if !cfg.Enabled {
		return nil, errors.New("Alertmanager is disabled")
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	transport := &http.Transport{TLSClientConfig: v.tls, MaxIdleConns: cfg.MaxConcurrent, MaxIdleConnsPerHost: cfg.MaxConcurrent, IdleConnTimeout: 90 * time.Second}
	client := &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	s := &Source{cfg: cfg, base: v.base, public: v.public, token: v.token, http: client, sem: make(chan struct{}, cfg.MaxConcurrent), now: cfg.Now}
	if cfg.Observer != nil {
		cfg.Observer.SetCircuit(upstream.UpstreamAlertmanager, upstream.CircuitClosed, 0)
	}
	return s, nil
}

// BindCatalog는 informer/Registry catalog가 생긴 뒤 정확히 한 번 호출합니다.
func (s *Source) BindCatalog(catalog datasource.PodCatalog) error {
	if catalog == nil {
		return errors.New("Alertmanager pod catalog is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.catalog != nil {
		return errors.New("Alertmanager pod catalog is already bound")
	}
	s.catalog = catalog
	return nil
}

// Close는 멱등이며 재사용 중인 upstream connection을 정리합니다.
func (s *Source) Close() error {
	if s == nil {
		return nil
	}
	s.closeOnce.Do(func() {
		s.closed.Store(true)
		if tr, ok := s.http.Transport.(interface{ CloseIdleConnections() }); ok {
			tr.CloseIdleConnections()
		}
	})
	return nil
}

func (s *Source) SetObserver(observer Observer) {
	if observer == nil {
		return
	}
	s.cfg.Observer = observer
	observer.SetCircuit(upstream.UpstreamAlertmanager, upstream.CircuitClosed, 0)
}

type apiAlert struct {
	Annotations  strictStringMap `json:"annotations"`
	EndsAt       string          `json:"endsAt"`
	Fingerprint  string          `json:"fingerprint"`
	GeneratorURL string          `json:"generatorURL"`
	StartsAt     string          `json:"startsAt"`
	Labels       strictStringMap `json:"labels"`
	Receivers    []apiReceiver   `json:"receivers"`
	Status       *apiStatus      `json:"status"`
	UpdatedAt    string          `json:"updatedAt"`
}

type apiReceiver struct {
	Name string `json:"name"`
}
type apiStatus struct {
	InhibitedBy strictStrings `json:"inhibitedBy"`
	MutedBy     strictStrings `json:"mutedBy"`
	SilencedBy  strictStrings `json:"silencedBy"`
	State       string        `json:"state"`
}

type strictStringMap map[string]string

func (m *strictStringMap) UnmarshalJSON(data []byte) error {
	var raw map[string]*string
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	if raw == nil {
		*m = nil
		return nil
	}
	out := make(strictStringMap, len(raw))
	for key, value := range raw {
		if value == nil {
			return errInvalidResponse
		}
		out[key] = *value
	}
	*m = out
	return nil
}

type strictStrings []string

func (s *strictStrings) UnmarshalJSON(data []byte) error {
	var raw []*string
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	if raw == nil {
		*s = nil
		return nil
	}
	out := make(strictStrings, len(raw))
	for i, value := range raw {
		if value == nil {
			return errInvalidResponse
		}
		out[i] = *value
	}
	*s = out
	return nil
}

func (r *apiReceiver) UnmarshalJSON(data []byte) error {
	if string(data) == "null" {
		return errInvalidResponse
	}
	var wire struct {
		Name *string `json:"name"`
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&wire); err != nil || wire.Name == nil {
		return errInvalidResponse
	}
	r.Name = *wire.Name
	return nil
}

func (s *Source) List(ctx context.Context, q datasource.AlertQuery) (result datasource.AlertResult, resultErr error) {
	started := time.Now()
	referenceTime := s.now()
	defer func() {
		if s.cfg.Observer != nil {
			s.cfg.Observer.ObserveUpstream(ctx, upstream.UpstreamAlertmanager, alertOutcome(ctx, resultErr), time.Since(started))
		}
	}()
	if s.closed.Load() {
		return datasource.AlertResult{}, errUnavailable
	}
	if q.Target.ClusterID == "" {
		return datasource.AlertResult{}, errInvalidResponse
	}
	if len(q.Target.Namespaces) > maxScopedNamespaces {
		return datasource.AlertResult{}, errInvalidResponse
	}
	logical, cancel := context.WithTimeout(ctx, s.cfg.Timeout)
	defer cancel()
	probe, generation, ok := s.breakerAllow()
	if !ok {
		return datasource.AlertResult{}, errCircuitOpen
	}
	select {
	case s.sem <- struct{}{}:
		defer func() { <-s.sem }()
	case <-logical.Done():
		s.breakerAbort(probe, generation)
		if ctx.Err() != nil {
			return datasource.AlertResult{}, ctx.Err()
		}
		return datasource.AlertResult{}, logical.Err()
	}
	if !probe {
		var allowed bool
		probe, generation, allowed = s.breakerAllow()
		if !allowed {
			return datasource.AlertResult{}, errCircuitOpen
		}
	}
	query := url.Values{}
	query.Add("filter", matcher(s.cfg.ClusterLabel, "=", q.Target.ClusterID))
	if q.Target.Namespace != "" {
		query.Add("filter", matcher(s.cfg.NamespaceLabel, "=", q.Target.Namespace))
	} else if len(q.Target.Namespaces) > 0 {
		query.Add("filter", matcher(s.cfg.NamespaceLabel, "=~", namespaceRegex(q.Target.Namespaces)))
	}
	if len(query.Encode()) > maxQueryBytes {
		s.breakerAbort(probe, generation)
		return datasource.AlertResult{}, errInvalidResponse
	}
	var raw []apiAlert
	err := s.get(logical, query, &raw)
	if err != nil && retryable(err) && logical.Err() == nil {
		t := time.NewTimer(minDuration(200*time.Millisecond, s.cfg.Timeout/4))
		select {
		case <-t.C:
			err = s.get(logical, query, &raw)
		case <-logical.Done():
			if !t.Stop() {
				select {
				case <-t.C:
				default:
				}
			}
			err = logical.Err()
		}
	}
	if err != nil {
		if ctx.Err() != nil {
			s.breakerAbort(probe, generation)
			return datasource.AlertResult{}, ctx.Err()
		}
		// caller 취소만 실패 집계에서 제외합니다. 어댑터 자체 total deadline 만료는
		// upstream 실패이므로 circuit에 반영합니다.
		s.breakerFinish(probe, generation, false)
		return datasource.AlertResult{}, safeError(err)
	}
	res, err := s.normalizeAt(q.Target, raw, referenceTime)
	if err != nil {
		s.breakerFinish(probe, generation, false)
		return datasource.AlertResult{}, err
	}
	s.breakerFinish(probe, generation, true)
	res.HistoryErr = ErrHistoryNotConfigured
	return res, nil
}

func (s *Source) breakerAbort(probe bool, permitGeneration uint64) {
	if !probe {
		return
	}
	s.mu.Lock()
	if permitGeneration != s.circuitGeneration {
		s.mu.Unlock()
		return
	}
	s.halfOpen = false
	s.circuitGeneration++
	generation := s.circuitGeneration
	s.mu.Unlock()
	if s.cfg.Observer != nil {
		s.cfg.Observer.SetCircuit(upstream.UpstreamAlertmanager, upstream.CircuitOpen, generation)
	}
}

type httpError struct{ code int }

func (e httpError) Error() string { return "alertmanager request failed" }

func (s *Source) get(ctx context.Context, values url.Values, out *[]apiAlert) error {
	u := *s.base
	u.Path = strings.TrimSuffix(u.Path, "/") + "/api/v2/alerts"
	u.RawQuery = values.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return errInvalidResponse
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+s.token)
	resp, err := s.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return httpError{resp.StatusCode}
	}
	mediaType, params, mimeErr := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if mimeErr != nil || mediaType != "application/json" || len(params) > 1 || (len(params) == 1 && !strings.EqualFold(params["charset"], "utf-8")) {
		return errInvalidResponse
	}
	limited := &io.LimitedReader{R: resp.Body, N: s.cfg.MaxBodyBytes + 1}
	dec := json.NewDecoder(limited)
	dec.DisallowUnknownFields()
	if err := dec.Decode(out); err != nil {
		if consumed := s.cfg.MaxBodyBytes + 1 - limited.N; consumed > s.cfg.MaxBodyBytes {
			return errResponseTooLarge
		}
		return errInvalidResponse
	}
	var extra any
	if consumed := s.cfg.MaxBodyBytes + 1 - limited.N; consumed > s.cfg.MaxBodyBytes {
		return errResponseTooLarge
	}
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return errInvalidResponse
	}
	if len(*out) > s.cfg.MaxAlerts {
		return errResponseTooLarge
	}
	return nil
}

func retryable(err error) bool {
	var h httpError
	if errors.As(err, &h) {
		return h.code == 429 || h.code == 502 || h.code == 503 || h.code == 504
	}
	var verifyErr *tls.CertificateVerificationError
	var recordErr tls.RecordHeaderError
	var alertErr tls.AlertError
	if errors.As(err, &verifyErr) || errors.As(err, &recordErr) || errors.As(err, &alertErr) {
		return false
	}
	return !errors.Is(err, errInvalidResponse) && !errors.Is(err, errResponseTooLarge)
}

func safeError(err error) error {
	var h httpError
	if errors.As(err, &h) {
		if h.code == http.StatusUnauthorized || h.code == http.StatusForbidden {
			return errUnauthorized
		}
		if h.code == http.StatusBadRequest || h.code >= 300 && h.code < 500 {
			return errInvalidResponse
		}
		return errUnavailable
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	if errors.Is(err, errInvalidResponse) || errors.Is(err, errResponseTooLarge) {
		return err
	}
	return errUnavailable
}

func (s *Source) breakerAllow() (bool, uint64, bool) {
	s.mu.Lock()
	if s.openTil.IsZero() {
		generation := s.circuitGeneration
		s.mu.Unlock()
		return false, generation, true
	}
	if s.now().Before(s.openTil) || s.halfOpen {
		generation := s.circuitGeneration
		s.mu.Unlock()
		return false, generation, false
	}
	s.halfOpen = true
	s.circuitGeneration++
	generation := s.circuitGeneration
	s.mu.Unlock()
	if s.cfg.Observer != nil {
		s.cfg.Observer.SetCircuit(upstream.UpstreamAlertmanager, upstream.CircuitHalfOpen, generation)
	}
	return true, generation, true
}
func (s *Source) breakerFinish(probe bool, permitGeneration uint64, ok bool) {
	s.mu.Lock()
	if permitGeneration != s.circuitGeneration {
		s.mu.Unlock()
		return
	}
	if ok {
		s.fails = 0
		s.openTil = time.Time{}
		s.halfOpen = false
		if probe {
			s.circuitGeneration++
		}
		generation := s.circuitGeneration
		s.mu.Unlock()
		if probe && s.cfg.Observer != nil {
			s.cfg.Observer.SetCircuit(upstream.UpstreamAlertmanager, upstream.CircuitClosed, generation)
		}
		return
	}
	if probe {
		s.halfOpen = false
	}
	s.fails++
	notify := probe || s.fails >= 3
	if notify {
		s.openTil = s.now().Add(5 * time.Second)
		s.circuitGeneration++
	}
	generation := s.circuitGeneration
	s.mu.Unlock()
	if notify && s.cfg.Observer != nil {
		s.cfg.Observer.SetCircuit(upstream.UpstreamAlertmanager, upstream.CircuitOpen, generation)
	}
}

func (s *Source) normalize(target datasource.Target, raw []apiAlert) (datasource.AlertResult, error) {
	return s.normalizeAt(target, raw, s.now())
}

func (s *Source) normalizeAt(target datasource.Target, raw []apiAlert, referenceTime time.Time) (datasource.AlertResult, error) {
	if s.catalog == nil {
		return datasource.AlertResult{}, errInvalidResponse
	}
	wantPods, wantWorkloads := make(map[string]struct{}), make(map[string]struct{})
	for i := range raw {
		if uid := raw[i].Labels["pod_uid"]; uid != "" {
			wantPods[uid] = struct{}{}
		}
		if uid := raw[i].Labels["workload_uid"]; uid != "" {
			wantWorkloads[uid] = struct{}{}
		}
	}
	pods := s.catalog.CatalogPods(target.ClusterID, target.Namespace, maxCatalogPods+1)
	if len(pods) > maxCatalogPods {
		return datasource.AlertResult{}, errResponseTooLarge
	}
	byPodUID := make(map[string]datasource.CatalogPod, len(wantPods))
	ambiguousPod := make(map[string]bool)
	byWorkloadUID := make(map[string]datasource.CatalogPod, len(wantWorkloads))
	ambiguousWorkload := make(map[string]bool)
	seenPodUID := make(map[string]datasource.CatalogPod, len(wantPods))
	for _, p := range pods {
		if _, wanted := wantPods[p.UID]; !wanted || p.UID == "" {
			continue
		}
		if old, ok := seenPodUID[p.UID]; ok && !sameCatalogIdentity(old, p) {
			ambiguousPod[p.UID] = true
		}
		seenPodUID[p.UID] = p
	}
	for _, p := range pods {
		if !target.AllowsNamespace(p.Namespace) {
			continue
		}
		if _, wanted := wantPods[p.UID]; p.UID != "" && wanted {
			byPodUID[p.UID] = p
		}
		if _, wanted := wantWorkloads[p.WorkloadUID]; p.WorkloadUID != "" && wanted {
			if old, ok := byWorkloadUID[p.WorkloadUID]; ok && (old.Namespace != p.Namespace || old.WorkloadName != p.WorkloadName || old.WorkloadKind != p.WorkloadKind) {
				ambiguousWorkload[p.WorkloadUID] = true
			}
			byWorkloadUID[p.WorkloadUID] = p
		}
	}
	seen := make(map[string]apiAlert, len(raw))
	type group struct {
		representative contract.AlertInstance
		size           int
	}
	groups := make(map[string]group, len(raw))
	for _, a := range raw {
		cluster, ok := a.Labels[s.cfg.ClusterLabel]
		if !ok || cluster != target.ClusterID {
			return datasource.AlertResult{}, errInvalidResponse
		}
		ns := a.Labels[s.cfg.NamespaceLabel]
		if target.Namespace != "" || len(target.Namespaces) > 0 {
			if ns == "" || !target.AllowsNamespace(ns) {
				return datasource.AlertResult{}, errInvalidResponse
			}
		}
		if old, ok := seen[a.Fingerprint]; ok {
			if !reflect.DeepEqual(old, a) {
				return datasource.AlertResult{}, errInvalidResponse
			}
			continue
		}
		item, groupID, err := s.normalizeOne(target.ClusterID, ns, a, referenceTime, byPodUID, ambiguousPod, byWorkloadUID, ambiguousWorkload)
		if err != nil {
			return datasource.AlertResult{}, err
		}
		seen[a.Fingerprint] = a
		g := groups[groupID]
		g.size++
		if g.representative.ID == "" || representativeBefore(item, g.representative) {
			g.representative = item
		}
		groups[groupID] = g
	}
	items := make([]contract.AlertInstance, 0, len(groups))
	for groupID, g := range groups {
		g.representative.GroupSize = g.size
		h := sha256.Sum256([]byte("alertmanager-group\x00" + groupID))
		g.representative.ID = "amg-" + hex.EncodeToString(h[:16])
		if b, err := json.Marshal(g.representative); err != nil || len(b) > maxAlertBytes {
			return datasource.AlertResult{}, errResponseTooLarge
		}
		items = append(items, g.representative)
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].GroupKey != items[j].GroupKey {
			return items[i].GroupKey < items[j].GroupKey
		}
		if items[i].StartsAt != items[j].StartsAt {
			return items[i].StartsAt > items[j].StartsAt
		}
		return items[i].ID < items[j].ID
	})
	return datasource.AlertResult{Firing: items, GroupingRule: "alertname + namespace + workload UID (pod UID fallback)"}, nil
}

func (s *Source) normalizeOne(cluster, namespace string, raw apiAlert, now time.Time, pods map[string]datasource.CatalogPod, ambiguousPods map[string]bool, workloads map[string]datasource.CatalogPod, ambiguousWorkloads map[string]bool) (contract.AlertInstance, string, error) {
	if raw.Labels == nil || raw.Annotations == nil || raw.Receivers == nil || raw.Status == nil || raw.Status.InhibitedBy == nil || raw.Status.MutedBy == nil || raw.Status.SilencedBy == nil {
		return contract.AlertInstance{}, "", errInvalidResponse
	}
	if len(raw.Labels) > maxLabels || len(raw.Annotations) > maxAnnotations || !bounded(raw.Fingerprint, maxKeyBytes) || raw.Fingerprint == "" {
		return contract.AlertInstance{}, "", errInvalidResponse
	}
	if len(raw.Receivers) > maxReceivers || len(raw.Status.InhibitedBy) > maxStatusRefs || len(raw.Status.MutedBy) > maxStatusRefs || len(raw.Status.SilencedBy) > maxStatusRefs {
		return contract.AlertInstance{}, "", errResponseTooLarge
	}
	for _, receiver := range raw.Receivers {
		if !bounded(receiver.Name, maxKeyBytes) {
			return contract.AlertInstance{}, "", errInvalidResponse
		}
	}
	for _, refs := range [][]string{raw.Status.InhibitedBy, raw.Status.MutedBy, raw.Status.SilencedBy} {
		for _, ref := range refs {
			if !bounded(ref, maxKeyBytes) {
				return contract.AlertInstance{}, "", errInvalidResponse
			}
		}
	}
	name := raw.Labels["alertname"]
	if name == "" || !bounded(name, maxLabelBytes) {
		return contract.AlertInstance{}, "", errInvalidResponse
	}
	start, err := time.Parse(time.RFC3339Nano, raw.StartsAt)
	if err != nil || start.IsZero() || start.After(now.Add(5*time.Minute)) {
		return contract.AlertInstance{}, "", errInvalidResponse
	}
	ends := ""
	var endTime time.Time
	if raw.EndsAt != "" {
		t, err := time.Parse(time.RFC3339Nano, raw.EndsAt)
		if err != nil {
			return contract.AlertInstance{}, "", errInvalidResponse
		}
		if t.IsZero() || t.Before(start) || t.Before(now) {
			return contract.AlertInstance{}, "", errInvalidResponse
		}
		ends = t.UTC().Format(time.RFC3339Nano)
		endTime = t
	} else {
		return contract.AlertInstance{}, "", errInvalidResponse
	}
	updated, err := time.Parse(time.RFC3339Nano, raw.UpdatedAt)
	if err != nil || updated.IsZero() || updated.Before(start) || updated.After(now.Add(5*time.Minute)) || updated.After(endTime) {
		return contract.AlertInstance{}, "", errInvalidResponse
	}
	if raw.Status.State != "active" && raw.Status.State != "suppressed" && raw.Status.State != "unprocessed" {
		return contract.AlertInstance{}, "", errInvalidResponse
	}
	for k, v := range raw.Labels {
		if !bounded(k, maxKeyBytes) || !bounded(v, maxLabelBytes) {
			return contract.AlertInstance{}, "", errInvalidResponse
		}
	}
	for k, v := range raw.Annotations {
		if !bounded(k, maxKeyBytes) || !bounded(v, maxAnnotationBytes) {
			return contract.AlertInstance{}, "", errInvalidResponse
		}
	}
	severity := raw.Labels["severity"]
	if severity != "critical" && severity != "warning" && severity != "info" {
		severity = "info"
		if s.cfg.Observer != nil {
			s.cfg.Observer.ObserveAlertSeverityFallback()
		}
	}
	labels := make(map[string]string)
	for _, k := range []string{"alertname", "severity", "pod", "job", "instance", "node", "workload", "workload_kind"} {
		if v := raw.Labels[k]; v != "" && !hasControl(v) {
			labels[k], _ = mask.Apply(v)
		}
	}
	labels["severity"] = severity
	if namespace != "" {
		labels["namespace"] = namespace
	}
	annotations := make(map[string]string)
	for _, k := range []string{"summary", "description", "runbook_url", "dashboard_url"} {
		if v := raw.Annotations[k]; v != "" && !hasControl(v) {
			if k == "runbook_url" || k == "dashboard_url" {
				if clean, ok := s.safeAnnotationURL(v); ok {
					annotations[k] = clean
				}
			} else {
				annotations[k], _ = mask.Apply(v)
			}
		}
	}
	var entity *contract.EntityRef
	if uid := raw.Labels["pod_uid"]; uid != "" {
		if p, ok := pods[uid]; ok && !ambiguousPods[uid] && identityMatches(namespace, raw.Labels, p) {
			entity = entityRef(cluster, p)
		}
	}
	if entity == nil {
		if uid := raw.Labels["workload_uid"]; uid != "" && !ambiguousWorkloads[uid] {
			if p, ok := workloads[uid]; ok && identityMatches(namespace, raw.Labels, p) {
				entity = entityRef(cluster, p)
				entity.PodName, entity.PodUID, entity.ContainerName = "", "", ""
			}
		}
	}
	h := sha256.Sum256([]byte(cluster + "\x00" + raw.Fingerprint))
	id := "am-" + hex.EncodeToString(h[:16])
	groupEntity := id
	entityName := ""
	if entity != nil {
		if entity.WorkloadUID != "" {
			groupEntity = entity.WorkloadUID
			entityName = entity.WorkloadName
		} else if entity.PodUID != "" {
			groupEntity = entity.PodUID
			entityName = entity.PodName
		}
	}
	groupID := cluster + "\x00" + name + "\x00" + namespace + "\x00" + groupEntity
	item := contract.AlertInstance{ID: id, Name: name, Severity: severity, Status: "firing", StartsAt: start.UTC().Format(time.RFC3339Nano), EndsAt: ends, Labels: labels, Annotations: annotations, Entity: entity, EntityName: entityName, SourceURL: s.sourceURL(cluster, namespace, name), Source: "alertmanager", GroupKey: humanGroupKey(name, namespace, entityName, id)}
	b, _ := json.Marshal(item)
	if len(b) > maxAlertBytes {
		return contract.AlertInstance{}, "", errResponseTooLarge
	}
	return item, groupID, nil
}

func identityMatches(namespace string, labels map[string]string, p datasource.CatalogPod) bool {
	return (namespace == "" || namespace == p.Namespace) && (labels["pod"] == "" || labels["pod"] == p.Name) && (labels["workload"] == "" || labels["workload"] == p.WorkloadName) && (labels["workload_kind"] == "" || labels["workload_kind"] == p.WorkloadKind)
}
func sameCatalogIdentity(a, b datasource.CatalogPod) bool {
	return a.Namespace == b.Namespace && a.Name == b.Name && a.UID == b.UID && a.WorkloadKind == b.WorkloadKind && a.WorkloadName == b.WorkloadName && a.WorkloadUID == b.WorkloadUID && a.Node == b.Node
}
func entityRef(cluster string, p datasource.CatalogPod) *contract.EntityRef {
	return &contract.EntityRef{ClusterID: cluster, Namespace: p.Namespace, WorkloadKind: p.WorkloadKind, WorkloadName: p.WorkloadName, WorkloadUID: p.WorkloadUID, PodName: p.Name, PodUID: p.UID}
}
func (s *Source) sourceURL(cluster, namespace, name string) string {
	filters := []string{matcher(s.cfg.ClusterLabel, "=", cluster), matcher("alertname", "=", name)}
	if namespace != "" {
		filters = append(filters, matcher(s.cfg.NamespaceLabel, "=", namespace))
	}
	values := url.Values{}
	for _, filter := range filters {
		values.Add("filter", filter)
	}
	return strings.TrimSuffix(s.public.String(), "/") + "/#/alerts?" + values.Encode()
}
func (s *Source) safeAnnotationURL(raw string) (string, bool) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || !strings.EqualFold(u.Host, s.public.Host) || u.User != nil {
		return "", false
	}
	u.RawQuery, u.Fragment = "", ""
	return u.String(), true
}
func representativeBefore(a, b contract.AlertInstance) bool {
	if a.StartsAt != b.StartsAt {
		return a.StartsAt > b.StartsAt
	}
	return a.ID < b.ID
}
func humanGroupKey(name, namespace, entityName, id string) string {
	identity := entityName
	if identity == "" {
		identity = "unlinked-" + strings.TrimPrefix(id, "am-")[:8]
	}
	v := name + " / " + valueOr(namespace, "cluster") + " / " + identity
	for len(v) > maxKeyBytes {
		_, size := utf8.DecodeLastRuneInString(v)
		v = v[:len(v)-size]
	}
	return v
}
func valueOr(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}
func matcher(key, op, value string) string {
	return key + op + `"` + matcherEscaper.Replace(value) + `"`
}
func namespaceRegex(values []string) string {
	cp := append([]string(nil), values...)
	sort.Strings(cp)
	for i := range cp {
		cp[i] = regexp.QuoteMeta(cp[i])
	}
	return "^(?:" + strings.Join(cp, "|") + ")$"
}

var (
	promLabelRE    = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	bearerTokenRE  = regexp.MustCompile(`^[A-Za-z0-9._~+/\-]+=*$`)
	matcherEscaper = strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)
)

func promLabel(v string) bool      { return len(v) <= maxKeyBytes && promLabelRE.MatchString(v) }
func bounded(v string, n int) bool { return utf8.ValidString(v) && len(v) <= n && !hasControl(v) }
func hasControl(v string) bool {
	for _, r := range v {
		if unicode.IsControl(r) {
			return true
		}
	}
	return false
}
func minDuration(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}

func alertOutcome(ctx context.Context, err error) upstream.Outcome {
	if err == nil {
		return upstream.OutcomeSuccess
	}
	if ctx.Err() != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return upstream.OutcomeTimeout
		}
		return upstream.OutcomeCanceled
	}
	if errors.Is(err, errCircuitOpen) {
		return upstream.OutcomeCircuitOpen
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return upstream.OutcomeTimeout
	}
	return upstream.OutcomeUnavailable
}

var _ datasource.Alerts = (*Source)(nil)
