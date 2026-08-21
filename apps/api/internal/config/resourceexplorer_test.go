package config

// Resource Explorer 설정 검증입니다. (ADR 0018)
//
// 기본은 비활성이고, 켜면 상한이 전부 엄격합니다 — allowlist 오타 하나가 조용히
// "리소스 없음"으로 보이는 것보다 기동 실패가 낫습니다.

import (
	"math"
	"strings"
	"testing"
	"time"
)

func TestResourceExplorerIsDisabledByDefault(t *testing.T) {
	cfg := Load()
	if cfg.ResourceExplorer.Enabled {
		t.Fatal("Resource Explorer 기본값은 비활성이어야 합니다")
	}
	if cfg.ResourceExplorer.AllowCRDs {
		t.Fatal("CRD는 언제나 명시적 opt-in입니다")
	}
	if cfg.ResourceExplorer.Refresh != 10*time.Minute {
		t.Fatalf("discovery 갱신 기본은 10분입니다: %v", cfg.ResourceExplorer.Refresh)
	}
	// 비활성이면 다른 상한이 무엇이든 기동을 막지 않습니다(기존 동작 유지).
	cfg.ResourceExplorer.Refresh = time.Millisecond
	cfg.ResourceExplorer.Resources = []string{"not a gvr"}
	if err := cfg.Validate(); err != nil && strings.Contains(err.Error(), "RESOURCE_EXPLORER") {
		t.Fatalf("비활성 설정이 기동을 막았습니다: %v", err)
	}
}

func enabledExplorerConfig() Config {
	cfg := Load()
	cfg.ResourceExplorer.Enabled = true
	return cfg
}

func TestResourceExplorerDefaultAllowlistIsUsableAndCRDFree(t *testing.T) {
	cfg := enabledExplorerConfig()
	list, err := cfg.ResourceAllowlist()
	if err != nil {
		t.Fatalf("기본 allowlist가 실패했습니다: %v", err)
	}
	if len(list) == 0 {
		t.Fatal("기본 allowlist가 비어 있습니다")
	}
	for _, gvr := range list {
		if gvr.Group != "" && !strings.HasSuffix(gvr.Group, ".k8s.io") &&
			gvr.Group != "apps" && gvr.Group != "batch" && gvr.Group != "autoscaling" && gvr.Group != "policy" {
			t.Fatalf("기본 allowlist에 CRD처럼 보이는 group이 있습니다: %s", gvr.Group)
		}
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("기본값으로 켠 설정이 실패했습니다: %v", err)
	}
}

func TestResourceExplorerRejectsBadAllowlist(t *testing.T) {
	for _, bad := range [][]string{
		{"services"},
		{"core/V1/services"},
		{"core/v1/Services"},
		{"core/v1/pods/log"},
		{"example.com/v1/widgets"}, // CRD는 opt-in 없이는 통과하지 못합니다.
	} {
		cfg := enabledExplorerConfig()
		cfg.ResourceExplorer.Resources = bad
		if _, err := cfg.ResourceAllowlist(); err == nil {
			t.Fatalf("잘못된 allowlist가 통과했습니다: %v", bad)
		}
		if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "RESOURCE_EXPLORER_RESOURCES") {
			t.Fatalf("%v에 대한 기동 검증이 없습니다: %v", bad, err)
		}
	}
	cfg := enabledExplorerConfig()
	cfg.ResourceExplorer.Resources = []string{"example.com/v1/widgets"}
	cfg.ResourceExplorer.AllowCRDs = true
	if _, err := cfg.ResourceAllowlist(); err != nil {
		t.Fatalf("opt-in한 CRD가 거절됐습니다: %v", err)
	}
}

func TestResourceExplorerBoundsAreStrict(t *testing.T) {
	cases := map[string]func(*Config){
		"RESOURCE_EXPLORER_REFRESH 하한":        func(c *Config) { c.ResourceExplorer.Refresh = 30 * time.Second },
		"RESOURCE_EXPLORER_REFRESH 상한":        func(c *Config) { c.ResourceExplorer.Refresh = 48 * time.Hour },
		"RESOURCE_EXPLORER_REFRESH 파싱 실패":     func(c *Config) { c.ResourceExplorer.Refresh = -1 },
		"RESOURCE_EXPLORER_DETAIL_RATE":       func(c *Config) { c.ResourceExplorer.DetailRate = 0 },
		"RESOURCE_EXPLORER_DETAIL_BURST":      func(c *Config) { c.ResourceExplorer.DetailBurst = 0 },
		"RESOURCE_EXPLORER_DETAIL_CONCURRENT": func(c *Config) { c.ResourceExplorer.DetailConcurrent = 99 },
		"RESOURCE_EXPLORER_DETAIL_TIMEOUT":    func(c *Config) { c.ResourceExplorer.DetailTimeout = time.Minute },
		"RESOURCE_EXPLORER_MAX_OBJECT_BYTES":  func(c *Config) { c.ResourceExplorer.MaxObjectBytes = 64 },
	}
	for name, mutate := range cases {
		cfg := enabledExplorerConfig()
		mutate(&cfg)
		if err := cfg.Validate(); err == nil {
			t.Fatalf("%s 위반이 통과했습니다", name)
		}
	}
}

// TestResourceExplorerRejectsNaNDetailRate — "NaN"은 ParseFloat을 통과하고 <=0·>100
// 비교도 **둘 다** 빠져나갑니다. 그대로 두면 상세 조회 token bucket이 fail-open이
// 되므로 기동 단계에서 막아야 합니다.
func TestResourceExplorerRejectsNaNDetailRate(t *testing.T) {
	t.Setenv("RESOURCE_EXPLORER_ENABLED", "true")
	t.Setenv("RESOURCE_EXPLORER_DETAIL_RATE", "NaN")

	cfg := Load()
	if !math.IsNaN(cfg.ResourceExplorer.DetailRate) {
		t.Fatalf("환경값 NaN이 %v로 해석됐습니다 — 이 회귀 시나리오가 성립하지 않습니다", cfg.ResourceExplorer.DetailRate)
	}
	/* 기존 상한 비교만으로는 잡히지 않는다는 사실 자체를 고정합니다. */
	if cfg.ResourceExplorer.DetailRate <= 0 || cfg.ResourceExplorer.DetailRate > 100 {
		t.Fatal("NaN이 기존 범위 비교에 걸렸습니다 — 명시적 검사가 필요 없다는 뜻이므로 확인하세요")
	}
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "RESOURCE_EXPLORER_DETAIL_RATE") {
		t.Fatalf("NaN rate가 기동 검증을 통과했습니다: %v", err)
	}
}

// TestResourceExplorerRequiresDirectMode — central은 로컬 informer도 kubeconfig도
// 없습니다. 켜져 있다면 설정 오류이며, 기동을 막는 편이 안전합니다.
func TestResourceExplorerRequiresDirectMode(t *testing.T) {
	cfg := enabledExplorerConfig()
	cfg.ClusterState.Mode = "central"
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "CLUSTER_STATE_MODE=direct") {
		t.Fatalf("central + Resource Explorer가 통과했습니다: %v", err)
	}
}

func TestResourceExplorerEnabledFlagIsStrict(t *testing.T) {
	cfg := Load()
	cfg.ResourceExplorer.EnabledInvalid = true
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "RESOURCE_EXPLORER_ENABLED") {
		t.Fatalf("잘못된 boolean이 통과했습니다: %v", err)
	}
}

// TestAuthNoneGrantsExploreCapability — 개발·데모 모드는 탐색을 허용합니다.
func TestAuthNoneGrantsExploreCapability(t *testing.T) {
	if !Load().Scope().CanExploreResources {
		t.Fatal("AUTH_MODE=none Scope가 탐색 capability를 주지 않습니다")
	}
	if !Load().Scope().CanManageWorkloads {
		t.Fatal("기존 관리 capability 동작이 바뀌었습니다")
	}
}
