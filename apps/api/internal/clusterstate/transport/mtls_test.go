package transport

import (
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestAgentIdentityRequiresExactURISAN(t *testing.T) {
	u, _ := url.Parse("spiffe://example.test/cluster-state-agent/a")
	leaf := &x509.Certificate{URIs: []*url.URL{u}, Subject: pkix.Name{CommonName: "ignored"}, SerialNumber: big.NewInt(1), NotAfter: time.Now().Add(time.Hour)}
	cs := tls.ConnectionState{VerifiedChains: [][]*x509.Certificate{{leaf}}}
	id, e := AgentClusterID(cs, "example.test")
	if e != nil || id != "a" {
		t.Fatal(id, e)
	}
	for name, cert := range map[string]*x509.Certificate{"cn-only": {Subject: pkix.Name{CommonName: "a"}}, "wrong-role": {URIs: []*url.URL{{Scheme: "spiffe", Host: "example.test", Path: "/api/a"}}}, "duplicate": {URIs: []*url.URL{u, u}}, "extra-uri": {URIs: []*url.URL{u, {Scheme: "https", Host: "example.test"}}}, "query": {URIs: []*url.URL{{Scheme: "spiffe", Host: "example.test", Path: "/cluster-state-agent/a", RawQuery: "x=1"}}}, "fragment": {URIs: []*url.URL{{Scheme: "spiffe", Host: "example.test", Path: "/cluster-state-agent/a", Fragment: "x"}}}, "escaped": {URIs: []*url.URL{{Scheme: "spiffe", Host: "example.test", Path: "/cluster-state-agent/a", RawPath: "/cluster-state-agent/%61"}}}} {
		t.Run(name, func(t *testing.T) {
			if _, e := AgentClusterID(tls.ConnectionState{VerifiedChains: [][]*x509.Certificate{{cert}}}, "example.test"); e == nil {
				t.Fatal("accepted")
			}
		})
	}
}

func TestTLSConfigurationAndReloadFailuresAreFailClosed(t *testing.T) {
	for name, files := range map[string]TLSFiles{
		"empty":        {},
		"missing cert": {KeyFile: "key", CAFile: "ca", TrustDomain: "example.test"},
		"missing key":  {CertFile: "cert", CAFile: "ca", TrustDomain: "example.test"},
		"missing ca":   {CertFile: "cert", KeyFile: "key", TrustDomain: "example.test"},
		"bad trust":    {CertFile: "cert", KeyFile: "key", CAFile: "ca", TrustDomain: "https://example.test"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := ServerTLS(files); err == nil {
				t.Fatal("invalid server TLS files accepted")
			}
			if _, err := ClientTLS(files, "registry"); err == nil {
				t.Fatal("invalid client TLS files accepted")
			}
		})
	}
	dir := t.TempDir()
	if _, err := loadPool(filepath.Join(dir, "missing.pem")); err == nil {
		t.Fatal("missing CA accepted")
	}
	badCA := filepath.Join(dir, "bad-ca.pem")
	if err := os.WriteFile(badCA, []byte("not a certificate"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadPool(badCA); err == nil {
		t.Fatal("invalid CA accepted")
	}

	ca, caKey := makeCA(t)
	caPath := writePEM(t, dir, "ca.pem", "CERTIFICATE", ca.Raw)
	serverCert, serverKey := makeLeaf(t, ca, caKey, "registry", "")
	serverFiles := TLSFiles{CertFile: writeCert(t, dir, "server", serverCert, serverKey), KeyFile: filepath.Join(dir, "server.key"), CAFile: caPath, TrustDomain: "example.test"}
	clientCert, clientKey := makeLeaf(t, ca, caKey, "", "spiffe://example.test/cluster-state-agent/a")
	clientFiles := TLSFiles{CertFile: writeCert(t, dir, "client", clientCert, clientKey), KeyFile: filepath.Join(dir, "client.key"), CAFile: caPath, TrustDomain: "example.test"}
	clientTLS, err := ClientTLS(clientFiles, "registry")
	if err != nil {
		t.Fatal(err)
	}
	if err := clientTLS.VerifyConnection(tls.ConnectionState{}); err == nil {
		t.Fatal("missing server certificate accepted")
	}
	if err := os.WriteFile(clientFiles.CAFile, []byte("rotated-invalid-ca"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := clientTLS.VerifyConnection(tls.ConnectionState{PeerCertificates: []*x509.Certificate{serverCert}}); err == nil {
		t.Fatal("invalid rotated CA accepted")
	}
	if _, err := clientTLS.GetClientCertificate(&tls.CertificateRequestInfo{}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(clientFiles.CertFile, []byte("rotated-invalid-cert"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := clientTLS.GetClientCertificate(&tls.CertificateRequestInfo{}); err == nil {
		t.Fatal("invalid rotated client certificate accepted")
	}

	writePEM(t, dir, "ca.pem", "CERTIFICATE", ca.Raw)
	serverTLS, err := ServerTLS(serverFiles)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(serverFiles.CertFile, []byte("rotated-invalid-cert"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := serverTLS.GetConfigForClient(&tls.ClientHelloInfo{}); err == nil {
		t.Fatal("invalid rotated server certificate accepted")
	}
	writeCert(t, dir, "server", serverCert, serverKey)
	if err := os.WriteFile(serverFiles.CAFile, []byte("rotated-invalid-ca"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := serverTLS.GetConfigForClient(&tls.ClientHelloInfo{}); err == nil {
		t.Fatal("invalid rotated server CA accepted")
	}
	pool := intermediates([]*x509.Certificate{serverCert})
	if len(pool.Subjects()) != 1 {
		t.Fatalf("intermediate subjects=%d", len(pool.Subjects()))
	}
}
