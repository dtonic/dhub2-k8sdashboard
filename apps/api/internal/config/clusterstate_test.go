package config

import (
	"path/filepath"
	"strings"
	"testing"
)

func validCentral(t *testing.T) Config {
	t.Helper()
	c := Load()
	c.Auth.Mode = "oidc"
	c.Auth.Issuer = "https://issuer.example"
	c.Auth.Audience = "dashboard"
	c.ClusterState.Mode = "central"
	c.UseDemoData = false
	c.ClusterState.RegistryEndpoint = "registry:9444"
	c.ClusterState.RegistryServerName = "registry"
	c.ClusterState.Clusters = []string{"a", "b"}
	p := filepath.Join(t.TempDir(), "tls.pem")
	c.ClusterState.CertFile = p
	c.ClusterState.KeyFile = p
	c.ClusterState.CAFile = p
	c.ClusterState.TrustDomain = "example.test"
	return c
}
func TestCentralConfigValidation(t *testing.T) {
	if e := validCentral(t).Validate(); e != nil {
		t.Fatal(e)
	}
	cases := []struct {
		name   string
		mutate func(*Config)
	}{{"endpoint", func(c *Config) { c.ClusterState.RegistryEndpoint = "https://registry" }}, {"server name", func(c *Config) { c.ClusterState.RegistryServerName = "" }}, {"server empty label", func(c *Config) { c.ClusterState.RegistryServerName = "a..b" }}, {"server hyphen label", func(c *Config) { c.ClusterState.RegistryServerName = "a-.b" }}, {"server long label", func(c *Config) { c.ClusterState.RegistryServerName = strings.Repeat("a", 64) + ".test" }}, {"server total long", func(c *Config) { c.ClusterState.RegistryServerName = strings.Repeat("a.", 127) + "a" }}, {"duplicate", func(c *Config) { c.ClusterState.Clusters = []string{"a", "a"} }}, {"invalid id", func(c *Config) { c.ClusterState.Clusters = []string{"UPPER"} }}, {"over capacity", func(c *Config) { c.ClusterState.MaxClusters = 1 }}, {"relative TLS", func(c *Config) { c.ClusterState.CAFile = "ca.pem" }}, {"no auth", func(c *Config) { c.Auth.Mode = "none" }}, {"mock auth", func(c *Config) { c.Auth.Mode = "mock" }}, {"demo", func(c *Config) { c.UseDemoData = true }}, {"missing Quickwit cluster mapping", func(c *Config) { c.Quickwit.URL = "http://quickwit" }}, {"unsafe Quickwit cluster mapping", func(c *Config) {
		c.Quickwit.URL = "http://quickwit"
		c.Quickwit.Fields = map[string]string{"cluster": "other"}
	}}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := validCentral(t)
			tc.mutate(&c)
			if c.Validate() == nil {
				t.Fatal("accepted")
			}
		})
	}
}

func TestCentralEnvLimitsFailClosed(t *testing.T) {
	dir := t.TempDir()
	base := map[string]string{
		"AUTH_MODE": "oidc", "OIDC_ISSUER": "https://issuer.example", "OIDC_AUDIENCE": "dashboard",
		"CLUSTER_STATE_MODE": "central", "CLUSTER_STATE_REGISTRY_ENDPOINT": "registry:9444",
		"CLUSTER_STATE_REGISTRY_SERVER_NAME": "registry", "CLUSTER_STATE_CLUSTERS": "a",
		"CLUSTER_STATE_TRUST_DOMAIN": "example.test", "CLUSTER_STATE_TLS_CERT_FILE": filepath.Join(dir, "cert.pem"),
		"CLUSTER_STATE_TLS_KEY_FILE": filepath.Join(dir, "key.pem"), "CLUSTER_STATE_TLS_CA_FILE": filepath.Join(dir, "ca.pem"),
	}
	for key, value := range base {
		t.Setenv(key, value)
	}
	t.Setenv("USE_DEMO_DATA", "false")
	for _, tc := range []struct{ name, key, value string }{
		{"clusters-malformed", "CLUSTER_STATE_MAX_CLUSTERS", "NaN"}, {"clusters-overflow", "CLUSTER_STATE_MAX_CLUSTERS", "999999999999999999999999"}, {"clusters-zero", "CLUSTER_STATE_MAX_CLUSTERS", "0"}, {"clusters-range", "CLUSTER_STATE_MAX_CLUSTERS", "65"},
		{"resources-malformed", "CLUSTER_STATE_MAX_RESOURCES", "bad"}, {"resources-overflow", "CLUSTER_STATE_MAX_RESOURCES", "999999999999999999999999"}, {"resources-zero", "CLUSTER_STATE_MAX_RESOURCES", "0"}, {"resources-range", "CLUSTER_STATE_MAX_RESOURCES", "100001"},
		{"chunks-malformed", "CLUSTER_STATE_MAX_CHUNK_RESOURCES", "NaN"}, {"chunks-overflow", "CLUSTER_STATE_MAX_CHUNK_RESOURCES", "999999999999999999999999"}, {"chunks-zero", "CLUSTER_STATE_MAX_CHUNK_RESOURCES", "0"}, {"chunks-range", "CLUSTER_STATE_MAX_CHUNK_RESOURCES", "1001"},
		{"message-malformed", "CLUSTER_STATE_MAX_MESSAGE_BYTES", "bad"}, {"message-overflow", "CLUSTER_STATE_MAX_MESSAGE_BYTES", "999999999999999999999999"}, {"message-zero", "CLUSTER_STATE_MAX_MESSAGE_BYTES", "0"}, {"message-small", "CLUSTER_STATE_MAX_MESSAGE_BYTES", "1023"}, {"message-range", "CLUSTER_STATE_MAX_MESSAGE_BYTES", "4194305"},
		{"stale-malformed", "CLUSTER_STATE_STALE_TTL", "NaN"}, {"stale-overflow", "CLUSTER_STATE_STALE_TTL", "999999999999999999999999h"}, {"stale-zero", "CLUSTER_STATE_STALE_TTL", "0s"}, {"stale-range", "CLUSTER_STATE_STALE_TTL", "25h"},
		{"heartbeat-malformed", "CLUSTER_STATE_HEARTBEAT_TIMEOUT", "bad"}, {"heartbeat-overflow", "CLUSTER_STATE_HEARTBEAT_TIMEOUT", "999999999999999999999999h"}, {"heartbeat-zero", "CLUSTER_STATE_HEARTBEAT_TIMEOUT", "0s"}, {"heartbeat-range", "CLUSTER_STATE_HEARTBEAT_TIMEOUT", "61m"}, {"heartbeat-after-stale", "CLUSTER_STATE_HEARTBEAT_TIMEOUT", "6m"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(tc.key, tc.value)
			if err := Load().Validate(); err == nil {
				t.Fatalf("accepted %s=%q", tc.key, tc.value)
			}
		})
	}
}

func TestStrictClusterStateParsingDoesNotChangeLegacyFallbacks(t *testing.T) {
	t.Setenv("CLUSTER_STATE_MODE", "direct")
	t.Setenv("CLUSTER_STATE_MAX_RESOURCES", "malformed-but-unused-in-direct")
	t.Setenv("CLUSTER_STATE_STALE_TTL", "malformed-but-unused-in-direct")
	t.Setenv("K8S_BURST", "bad")
	if got := Load().Burst; got != 30 {
		t.Fatalf("legacy fallback changed: %d", got)
	}
	if err := Load().Validate(); err != nil {
		t.Fatalf("direct defaults rejected: %v", err)
	}
}
