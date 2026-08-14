//go:build integration

// live.go는 **실제 kube-apiserver**에 붙는 테스트를 위한 연결 계층입니다.
//
// fake clientset은 우리 코드가 규칙을 지키는지는 보여주지만, API 서버가
// 실제로 protobuf를 협상하는지 · 필드 셀렉터를 서버에서 걸러주는지 ·
// watch가 몇 초 만에 변경을 전달하는지는 보여주지 못합니다. 그건 진짜 서버에서만 나옵니다.
//
// 연결 대상은 두 가지입니다.
//   - ITEST_KUBECONFIG(또는 KUBECONFIG)가 있으면 그 클러스터. 운영 클러스터에 겨눠도
//     기본 동작은 **읽기 전용**입니다.
//   - 없으면 KUBEBUILDER_ASSETS의 etcd/kube-apiserver 바이너리를 직접 띄웁니다.
//     kubelet이 없어 Pod가 실제로 실행되지는 않지만, API 서버 자체는 진짜입니다.
package testcluster

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/xenx96/k8s-dashboard/apps/api/internal/clusterstate"
)

/* ── 요청 계수기 ────────────────────────────────────────────────────────── */

// Counter는 API 서버로 나간 HTTP 요청을 셉니다.
//
// ADR 0004의 "요청당 API 서버 호출 0회"를 **가짜가 아니라 실제 트래픽으로** 확인하는 장치입니다.
// 응답의 Content-Type도 기록해 protobuf 협상이 실제로 적용됐는지 봅니다.
type Counter struct {
	mu sync.Mutex
	// reqs는 지금까지 나간 요청입니다. watch는 하나로 계속 열려 있으므로 한 번만 잡힙니다.
	reqs []Request
}

type Request struct {
	Method      string
	Path        string
	Query       string
	ContentType string
	Status      int
}

func (c *Counter) record(r Request) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.reqs = append(c.reqs, r)
}

// Len은 지금까지의 요청 수입니다.
func (c *Counter) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.reqs)
}

// Since는 n번째 이후에 나간 요청입니다.
func (c *Counter) Since(n int) []Request {
	c.mu.Lock()
	defer c.mu.Unlock()
	if n >= len(c.reqs) {
		return nil
	}
	out := make([]Request, len(c.reqs)-n)
	copy(out, c.reqs[n:])
	return out
}

// All은 전체 요청 사본입니다.
func (c *Counter) All() []Request { return c.Since(0) }

type countingTransport struct {
	inner http.RoundTripper
	c     *Counter
}

func (t countingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	resp, err := t.inner.RoundTrip(r)
	rec := Request{Method: r.Method, Path: r.URL.Path, Query: r.URL.RawQuery}
	if resp != nil {
		rec.ContentType = resp.Header.Get("Content-Type")
		rec.Status = resp.StatusCode
	}
	t.c.record(rec)
	return resp, err
}

/* ── 연결 ───────────────────────────────────────────────────────────────── */

// Live는 실제 API 서버에 붙은 rest.Config와 요청 계수기를 돌려줍니다.
// 붙을 곳이 없으면 테스트를 건너뜁니다 — 클러스터가 없다고 CI가 빨개지면 안 됩니다.
func Live(t *testing.T) (*rest.Config, *Counter) {
	t.Helper()

	var (
		cfg *rest.Config
		err error
	)
	switch {
	case kubeconfigPath() != "":
		path := kubeconfigPath()
		cfg, err = clientcmd.BuildConfigFromFlags("", path)
		if err != nil {
			t.Fatalf("kubeconfig(%s)를 읽지 못했습니다: %v", path, err)
		}
		t.Logf("대상: kubeconfig %s (%s)", path, cfg.Host)
	case os.Getenv("KUBEBUILDER_ASSETS") != "":
		cfg = startAPIServer(t)
		t.Logf("대상: 로컬 kube-apiserver (%s)", cfg.Host)
	default:
		t.Skip("실클러스터 대상이 없습니다. ITEST_KUBECONFIG 또는 KUBEBUILDER_ASSETS를 설정하세요.")
	}

	// 프로덕션과 **같은 경로**로 설정을 만듭니다. 여기서 다르게 만들면
	// 테스트가 통과해도 실제 동작을 증명하지 못합니다.
	applyProductionClientOptions(cfg)

	counter := &Counter{}
	cfg.Wrap(func(rt http.RoundTripper) http.RoundTripper {
		return countingTransport{inner: rt, c: counter}
	})
	return cfg, counter
}

func kubeconfigPath() string {
	if v := os.Getenv("ITEST_KUBECONFIG"); v != "" {
		return v
	}
	return os.Getenv("KUBECONFIG")
}

// applyProductionClientOptions는 clusterstate.RestConfig가 하는 것과 같은 설정을 입힙니다.
// RestConfig는 kubeconfig 경로에서 새로 만들기 때문에, 이미 만들어진 config에는 이쪽을 씁니다.
func applyProductionClientOptions(cfg *rest.Config) {
	cfg.AcceptContentTypes = clusterstate.ProtobufAccept
	cfg.ContentType = clusterstate.ProtobufContentType
	cfg.UserAgent = "k8s-dashboard-api/itest"
	cfg.QPS = 20
	cfg.Burst = 30
}

// LiveStore는 실서버에 붙은 Store를 만들고 동기화까지 마칩니다.
func LiveStore(t *testing.T, ctx context.Context, cfg *rest.Config, opts clusterstate.Options) *clusterstate.Store {
	t.Helper()
	clients, err := clusterstate.NewClients(cfg)
	if err != nil {
		t.Fatalf("클라이언트 생성 실패: %v", err)
	}
	store, err := clusterstate.New(clients, opts)
	if err != nil {
		t.Fatalf("store 생성 실패: %v", err)
	}
	start := time.Now()
	if err := store.Start(ctx); err != nil {
		t.Fatalf("informer 동기화 실패: %v", err)
	}
	t.Logf("informer 캐시 동기화 %.2fs", time.Since(start).Seconds())
	return store
}

// Clientset은 픽스처를 만들거나 상태를 바꿀 때 쓰는 별도 클라이언트입니다.
// 계수기에 잡히지 않도록 **감싸지 않은** 설정을 씁니다 — 테스트가 만든 트래픽이
// "요청당 0회" 측정에 섞이면 안 됩니다.
func Clientset(t *testing.T, cfg *rest.Config) kubernetes.Interface {
	t.Helper()
	plain := rest.CopyConfig(cfg)
	plain.WrapTransport = nil
	cs, err := kubernetes.NewForConfig(plain)
	if err != nil {
		t.Fatalf("클라이언트셋 생성 실패: %v", err)
	}
	return cs
}

/* ── 로컬 kube-apiserver 기동 ───────────────────────────────────────────── */

// startAPIServer는 etcd와 kube-apiserver를 직접 띄웁니다.
// controller-runtime/envtest를 의존성으로 들이지 않으려고 최소한만 직접 합니다.
func startAPIServer(t *testing.T) *rest.Config {
	t.Helper()
	assets := os.Getenv("KUBEBUILDER_ASSETS")
	dir := t.TempDir()

	pki := writePKI(t, dir)

	etcdPort, apiPort := freePort(t), freePort(t)
	etcd := exec.Command(filepath.Join(assets, "etcd"),
		"--data-dir", filepath.Join(dir, "etcd"),
		"--listen-client-urls", fmt.Sprintf("http://127.0.0.1:%d", etcdPort),
		"--advertise-client-urls", fmt.Sprintf("http://127.0.0.1:%d", etcdPort),
		"--listen-peer-urls", fmt.Sprintf("http://127.0.0.1:%d", freePort(t)),
	)
	startProcess(t, etcd, filepath.Join(dir, "etcd.log"))

	api := exec.Command(filepath.Join(assets, "kube-apiserver"),
		"--advertise-address=127.0.0.1",
		"--bind-address=127.0.0.1",
		fmt.Sprintf("--secure-port=%d", apiPort),
		fmt.Sprintf("--etcd-servers=http://127.0.0.1:%d", etcdPort),
		"--client-ca-file="+pki.caCert,
		"--tls-cert-file="+pki.serverCert,
		"--tls-private-key-file="+pki.serverKey,
		"--service-account-key-file="+pki.saPub,
		"--service-account-signing-key-file="+pki.saKey,
		fmt.Sprintf("--service-account-issuer=https://127.0.0.1:%d", apiPort),
		"--api-audiences=itest",
		// RBAC을 켜야 최소 권한 검증이 의미를 갖습니다.
		"--authorization-mode=RBAC",
		"--disable-admission-plugins=ServiceAccount",
		"--service-cluster-ip-range=10.0.0.0/24",
	)
	startProcess(t, api, filepath.Join(dir, "apiserver.log"))

	cfg := &rest.Config{
		Host: fmt.Sprintf("https://127.0.0.1:%d", apiPort),
		TLSClientConfig: rest.TLSClientConfig{
			CAFile:   pki.caCert,
			CertFile: pki.adminCert,
			KeyFile:  pki.adminKey,
		},
	}
	waitHealthy(t, cfg, filepath.Join(dir, "apiserver.log"))
	return cfg
}

func startProcess(t *testing.T, cmd *exec.Cmd, logPath string) {
	t.Helper()
	log, err := os.Create(logPath)
	if err != nil {
		t.Fatalf("로그 파일 생성 실패: %v", err)
	}
	cmd.Stdout, cmd.Stderr = log, log
	if err := cmd.Start(); err != nil {
		t.Fatalf("%s 기동 실패: %v", filepath.Base(cmd.Path), err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
		_ = log.Close()
	})
}

func waitHealthy(t *testing.T, cfg *rest.Config, logPath string) {
	t.Helper()
	cs, err := kubernetes.NewForConfig(rest.CopyConfig(cfg))
	if err != nil {
		t.Fatalf("헬스체크 클라이언트 생성 실패: %v", err)
	}
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := cs.Discovery().ServerVersion(); err == nil {
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
	tail, _ := os.ReadFile(logPath)
	t.Fatalf("kube-apiserver가 준비되지 않았습니다. 로그 끝부분:\n%s", lastLines(string(tail), 20))
}

func lastLines(s string, n int) string {
	lines := []rune(s)
	if len(lines) > 4000 {
		return string(lines[len(lines)-4000:])
	}
	return s
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("포트를 잡지 못했습니다: %v", err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

type pkiPaths struct {
	caCert, serverCert, serverKey, adminCert, adminKey, saKey, saPub string
}

// writePKI는 API 서버와 admin 클라이언트 인증서를 만듭니다.
func writePKI(t *testing.T, dir string) pkiPaths {
	t.Helper()

	caKey, caCert, caDER := selfSignedCA(t)
	p := pkiPaths{
		caCert:     filepath.Join(dir, "ca.crt"),
		serverCert: filepath.Join(dir, "apiserver.crt"),
		serverKey:  filepath.Join(dir, "apiserver.key"),
		adminCert:  filepath.Join(dir, "admin.crt"),
		adminKey:   filepath.Join(dir, "admin.key"),
		saKey:      filepath.Join(dir, "sa.key"),
		saPub:      filepath.Join(dir, "sa.pub"),
	}
	writePEM(t, p.caCert, "CERTIFICATE", caDER)

	// API 서버 서빙 인증서 — 127.0.0.1로만 접속합니다.
	srvKey, srvDER := signedCert(t, caKey, caCert, pkix.Name{CommonName: "kube-apiserver"},
		x509.ExtKeyUsageServerAuth, []net.IP{net.ParseIP("127.0.0.1")}, []string{"localhost"})
	writePEM(t, p.serverCert, "CERTIFICATE", srvDER)
	writePEM(t, p.serverKey, "RSA PRIVATE KEY", x509.MarshalPKCS1PrivateKey(srvKey))

	// admin 클라이언트 — system:masters 그룹이라 RBAC을 통과합니다.
	admKey, admDER := signedCert(t, caKey, caCert,
		pkix.Name{CommonName: "admin", Organization: []string{"system:masters"}},
		x509.ExtKeyUsageClientAuth, nil, nil)
	writePEM(t, p.adminCert, "CERTIFICATE", admDER)
	writePEM(t, p.adminKey, "RSA PRIVATE KEY", x509.MarshalPKCS1PrivateKey(admKey))

	saKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("ServiceAccount 키 생성 실패: %v", err)
	}
	writePEM(t, p.saKey, "RSA PRIVATE KEY", x509.MarshalPKCS1PrivateKey(saKey))
	pub, err := x509.MarshalPKIXPublicKey(&saKey.PublicKey)
	if err != nil {
		t.Fatalf("ServiceAccount 공개키 직렬화 실패: %v", err)
	}
	writePEM(t, p.saPub, "PUBLIC KEY", pub)
	return p
}

func selfSignedCA(t *testing.T) (*rsa.PrivateKey, *x509.Certificate, []byte) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("CA 키 생성 실패: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "k8s-dashboard-itest-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("CA 인증서 생성 실패: %v", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("CA 인증서 파싱 실패: %v", err)
	}
	return key, cert, der
}

func signedCert(t *testing.T, caKey *rsa.PrivateKey, ca *x509.Certificate, subject pkix.Name,
	usage x509.ExtKeyUsage, ips []net.IP, dns []string) (*rsa.PrivateKey, []byte) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("키 생성 실패: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      subject,
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{usage},
		IPAddresses:  ips,
		DNSNames:     dns,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca, &key.PublicKey, caKey)
	if err != nil {
		t.Fatalf("인증서 서명 실패: %v", err)
	}
	return key, der
}

func writePEM(t *testing.T, path, blockType string, der []byte) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("%s 생성 실패: %v", path, err)
	}
	defer f.Close()
	if err := pem.Encode(f, &pem.Block{Type: blockType, Bytes: der}); err != nil {
		t.Fatalf("%s 인코딩 실패: %v", path, err)
	}
}
