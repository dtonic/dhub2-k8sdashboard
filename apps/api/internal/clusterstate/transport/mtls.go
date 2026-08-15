// Package transport implements reload-on-handshake mTLS for cluster-state.
package transport

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"
	"strings"

	"github.com/xenx96/k8s-dashboard/apps/api/internal/clusterid"
)

type TLSFiles struct{ CertFile, KeyFile, CAFile, TrustDomain string }

func (f TLSFiles) validate() error {
	if f.CertFile == "" || f.KeyFile == "" || f.CAFile == "" || f.TrustDomain == "" || strings.ContainsAny(f.TrustDomain, "/: ") {
		return fmt.Errorf("all TLS file paths and a valid trust domain are required")
	}
	return nil
}
func loadCert(f TLSFiles) (tls.Certificate, error) { return tls.LoadX509KeyPair(f.CertFile, f.KeyFile) }
func loadPool(path string) (*x509.CertPool, error) {
	b, e := os.ReadFile(path)
	if e != nil {
		return nil, e
	}
	p := x509.NewCertPool()
	if !p.AppendCertsFromPEM(b) {
		return nil, fmt.Errorf("CA bundle has no certificates")
	}
	return p, nil
}

// ServerTLS reloads certificate and CA files for every new TLS connection.
// Existing connections are short-lived at the application layer so rotation
// and revocation take effect without restarting the process.
func ServerTLS(f TLSFiles) (*tls.Config, error) {
	return ServerTLSForRole(f, "cluster-state-agent")
}
func ServerTLSForRole(f TLSFiles, role string) (*tls.Config, error) {
	if e := f.validate(); e != nil {
		return nil, e
	}
	if _, e := loadCert(f); e != nil {
		return nil, e
	}
	if _, e := loadPool(f.CAFile); e != nil {
		return nil, e
	}
	base := &tls.Config{MinVersion: tls.VersionTLS13, ClientAuth: tls.RequireAndVerifyClientCert}
	base.GetConfigForClient = func(*tls.ClientHelloInfo) (*tls.Config, error) {
		cert, e := loadCert(f)
		if e != nil {
			return nil, e
		}
		pool, e := loadPool(f.CAFile)
		if e != nil {
			return nil, e
		}
		c := base.Clone()
		c.GetConfigForClient = nil
		c.Certificates = []tls.Certificate{cert}
		c.ClientCAs = pool
		c.VerifyConnection = func(cs tls.ConnectionState) error { _, e := SPIFFEIdentity(cs, f.TrustDomain, role); return e }
		return c, nil
	}
	return base, nil
}
func ClientTLS(f TLSFiles, serverName string) (*tls.Config, error) {
	if e := f.validate(); e != nil {
		return nil, e
	}
	if _, e := loadCert(f); e != nil {
		return nil, e
	}
	pool, e := loadPool(f.CAFile)
	if e != nil {
		return nil, e
	}
	return &tls.Config{MinVersion: tls.VersionTLS13, ServerName: serverName, RootCAs: pool, GetClientCertificate: func(*tls.CertificateRequestInfo) (*tls.Certificate, error) { c, e := loadCert(f); return &c, e }, VerifyConnection: func(cs tls.ConnectionState) error {
		pool, e := loadPool(f.CAFile)
		if e != nil {
			return e
		}
		if len(cs.PeerCertificates) == 0 {
			return fmt.Errorf("server certificate missing")
		}
		_, e = cs.PeerCertificates[0].Verify(x509.VerifyOptions{Roots: pool, DNSName: serverName, Intermediates: intermediates(cs.PeerCertificates[1:])})
		return e
	}}, nil
}
func intermediates(cs []*x509.Certificate) *x509.CertPool {
	p := x509.NewCertPool()
	for _, c := range cs {
		p.AddCert(c)
	}
	return p
}

func AgentClusterID(cs tls.ConnectionState, trustDomain string) (string, error) {
	return SPIFFEIdentity(cs, trustDomain, "cluster-state-agent")
}
func SPIFFEIdentity(cs tls.ConnectionState, trustDomain, role string) (string, error) {
	if len(cs.VerifiedChains) == 0 || len(cs.VerifiedChains[0]) == 0 {
		return "", fmt.Errorf("verified client certificate missing")
	}
	leaf := cs.VerifiedChains[0][0]
	if len(leaf.URIs) != 1 {
		return "", fmt.Errorf("exactly one URI SAN required")
	}
	u := leaf.URIs[0]
	if u.Scheme != "spiffe" || u.Host != trustDomain || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.RawPath != "" {
		return "", fmt.Errorf("invalid agent URI SAN")
	}
	parts := strings.Split(u.Path, "/")
	if len(parts) != 3 || parts[0] != "" || parts[1] != role || !clusterid.Valid(parts[2]) {
		return "", fmt.Errorf("required agent URI SAN missing")
	}
	return parts[2], nil
}
