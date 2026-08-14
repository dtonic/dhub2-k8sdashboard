package config

import (
	"testing"
	"time"
)

// TestLoadDefaultsAreClusterSafe — 기본값은 클러스터에 가장 안전한 쪽입니다.
func TestLoadDefaultsAreClusterSafe(t *testing.T) {
	cfg := Load()
	if cfg.Addr != ":8080" || cfg.ClusterID != "default" {
		t.Fatalf("기본 주소·클러스터: %+v", cfg)
	}
	if !cfg.AllNS || cfg.Namespaces != nil {
		t.Fatal("기본 Scope는 전체여야 합니다")
	}
	if cfg.Resync != 10*time.Minute {
		t.Fatalf("resync 기본은 10분입니다: %v", cfg.Resync)
	}
	if cfg.EventFieldSelector != "type=Warning" {
		t.Fatalf("Event는 기본으로 좁혀야 합니다: %q", cfg.EventFieldSelector)
	}
	if cfg.CacheTTL == 0 {
		t.Fatal("CacheTTL 0은 자동 갱신 팬아웃을 그대로 통과시킵니다")
	}
	if !cfg.UseDemoData || cfg.Auth.Mode != "none" {
		t.Fatal("기본은 데모 데이터 + 인증 없음입니다")
	}
	if cfg.Greptime.URL != "" || cfg.Quickwit.URL != "" {
		t.Fatal("기본은 실데이터소스 미사용입니다")
	}
}

// TestLoadReadsEnvironment — 환경변수가 각 필드로 흘러가는지 확인합니다.
func TestLoadReadsEnvironment(t *testing.T) {
	t.Setenv("ADDR", ":9999")
	t.Setenv("SCOPE_NAMESPACES", "b,a")
	t.Setenv("CLUSTER_ID", "seoul")
	t.Setenv("CLUSTER_NAME", "Seoul Production")
	t.Setenv("K8S_RESYNC", "30m")
	t.Setenv("K8S_QPS", "50")
	t.Setenv("K8S_BURST", "80")
	t.Setenv("K8S_DISABLE_PROTOBUF", "true")
	t.Setenv("USE_DEMO_DATA", "false")
	t.Setenv("GREPTIME_URL", "http://g:4000")
	t.Setenv("QUICKWIT_URL", "http://q:7280")
	t.Setenv("QUICKWIT_FIELDS", "message=body.message, level = severity ,broken, =x, k=")
	t.Setenv("AUTH_MODE", "oidc")
	t.Setenv("OIDC_ISSUER", "https://idp.example.com")

	cfg := Load()
	if cfg.Addr != ":9999" || cfg.ClusterID != "seoul" {
		t.Fatalf("기본 필드: %+v", cfg)
	}
	if cfg.AllNS || len(cfg.Namespaces) != 2 || cfg.Namespaces[0] != "a" {
		t.Fatalf("namespace 목록은 정렬되어야 합니다: %v", cfg.Namespaces)
	}
	if cfg.QPS != 50 || cfg.Burst != 80 || !cfg.DisableProtobuf || cfg.Resync != 30*time.Minute {
		t.Fatalf("k8s 설정: %+v", cfg)
	}
	if cfg.UseDemoData {
		t.Fatal("USE_DEMO_DATA=false가 무시되었습니다")
	}
	if cfg.Greptime.URL != "http://g:4000" || cfg.Quickwit.URL != "http://q:7280" {
		t.Fatalf("데이터소스 URL: %+v", cfg)
	}
	// 필드 재정의: 잘못된 항목(broken, 빈 키·값)은 조용히 버립니다.
	if len(cfg.Quickwit.Fields) != 2 || cfg.Quickwit.Fields["message"] != "body.message" || cfg.Quickwit.Fields["level"] != "severity" {
		t.Fatalf("QUICKWIT_FIELDS 해석: %v", cfg.Quickwit.Fields)
	}
	if cfg.Auth.Mode != "oidc" || cfg.Auth.Issuer != "https://idp.example.com" {
		t.Fatalf("인증 설정: %+v", cfg.Auth)
	}
}

// TestInvalidEnvFallsBackToDefaults — 형식이 틀린 환경변수는 기본값으로
// 조용히 물러납니다. 설정 오타가 서버를 못 띄우게 하지는 않되, 위험한 값이
// 되지도 않습니다.
func TestInvalidEnvFallsBackToDefaults(t *testing.T) {
	t.Setenv("K8S_RESYNC", "not-a-duration")
	t.Setenv("K8S_QPS", "not-a-number")
	t.Setenv("K8S_BURST", "NaN")
	t.Setenv("USE_DEMO_DATA", "maybe")

	cfg := Load()
	if cfg.Resync != 10*time.Minute || cfg.QPS != 20 || cfg.Burst != 30 || !cfg.UseDemoData {
		t.Fatalf("기본값 복귀 실패: %+v", cfg)
	}
}

// TestScopeFromConfig — 정적 Scope 변환입니다. 이름이 없으면 ID를 씁니다.
func TestScopeFromConfig(t *testing.T) {
	t.Setenv("CLUSTER_ID", "seoul")
	t.Setenv("SCOPE_NAMESPACES", "payments")
	cfg := Load()
	sc := cfg.Scope()
	c, ok := sc.Cluster("seoul")
	if !ok || c.Name != "seoul" || c.All || len(c.Namespaces) != 1 {
		t.Fatalf("정적 Scope: %+v", sc)
	}

	t.Setenv("CLUSTER_NAME", "Seoul Production")
	if c, _ := Load().Scope().Cluster("seoul"); c.Name != "Seoul Production" {
		t.Fatalf("클러스터 이름: %+v", c)
	}
}
