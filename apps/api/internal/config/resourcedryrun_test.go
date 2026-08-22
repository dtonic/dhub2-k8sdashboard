package config

// 변경 검토 dry-run 설정 검증입니다. (ADR 0019 Phase 1)
//
// 기본은 꺼짐이고 대상 목록도 비어 있습니다. 켜면 상한이 전부 엄격합니다 —
// 조용한 clamp도, 파싱 실패의 조용한 기본값 복귀도 없습니다.
//
// 이 파일의 테스트는 프로세스 환경변수를 읽는 Load()에 의존하므로 **t.Parallel을
// 쓰지 않습니다.** 환경에 남아 있는 값이 결과를 흔들지 않도록, Load()를 부르는
// 테스트는 관련 env를 전부 명시적으로 초기화합니다.

import (
	"math"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/xenx96/k8s-dashboard/apps/api/internal/contract"
	"github.com/xenx96/k8s-dashboard/apps/api/internal/resourcecatalog"
)

// dryRunEnvKeys는 이 기능이 읽는 env 전부입니다. 로컬 환경 오염을 막으려면
// Load() 전에 여기 있는 것을 모두 비우거나 명시해야 합니다.
var dryRunEnvKeys = []string{
	"RESOURCE_EXPLORER_ENABLED",
	"RESOURCE_EXPLORER_RESOURCES",
	"RESOURCE_EXPLORER_ALLOW_CRDS",
	"CLUSTER_STATE_MODE",
	"RESOURCE_EXPLORER_DRY_RUN_ENABLED",
	"RESOURCE_EXPLORER_DRY_RUN_RESOURCES",
	"RESOURCE_EXPLORER_DRY_RUN_DENY_RESOURCES",
	"RESOURCE_EXPLORER_DRY_RUN_TIMEOUT",
	"RESOURCE_EXPLORER_DRY_RUN_RATE",
	"RESOURCE_EXPLORER_DRY_RUN_BURST",
	"RESOURCE_EXPLORER_DRY_RUN_CONCURRENT",
	"RESOURCE_EXPLORER_DRY_RUN_MAX_MANIFEST_BYTES",
	"RESOURCE_EXPLORER_DRY_RUN_MAX_OBJECT_BYTES",
}

// loadDryRunConfig는 관련 env를 **전부 비운 뒤** overrides만 적용해 Load합니다.
// t.Setenv는 t.Cleanup으로 원복하므로 테스트 간 누수가 없습니다.
func loadDryRunConfig(t *testing.T, overrides map[string]string) Config {
	t.Helper()
	for _, key := range dryRunEnvKeys {
		t.Setenv(key, "")
	}
	for key, value := range overrides {
		t.Setenv(key, value)
	}
	return Load()
}

// enabledDryRunConfig는 검토가 켜진 유효한 기준 설정입니다.
// Explorer 기본 allowlist에 들어 있고 hard-deny가 아닌 GVR을 씁니다.
//
// 관련 env를 **전부 비운 상태**에서 시작하므로 로컬 환경에 남아 있는 값이
// 기준 설정을 흔들지 않습니다.
func enabledDryRunConfig(t *testing.T) Config {
	t.Helper()
	cfg := loadDryRunConfig(t, nil)
	cfg.ClusterState.Mode = "direct"
	cfg.ResourceExplorer.Enabled = true
	cfg.ResourceExplorer.DryRunEnabled = true
	cfg.ResourceExplorer.DryRunResources = []string{"core/v1/configmaps", "networking.k8s.io/v1/ingresses"}
	cfg.ResourceExplorer.DryRunDenyResources = nil
	cfg.ResourceExplorer.DryRunTimeout = 8 * time.Second
	cfg.ResourceExplorer.DryRunRate = 1
	cfg.ResourceExplorer.DryRunBurst = 3
	cfg.ResourceExplorer.DryRunConcurrent = 1
	cfg.ResourceExplorer.DryRunMaxManifestBytes = contract.DefaultDryRunManifestBytes
	cfg.ResourceExplorer.DryRunMaxObjectBytes = resourcecatalog.DefaultMaxObjectBytes
	return cfg
}

/* ── 기본값 ─────────────────────────────────────────────────────────────── */

func TestResourceDryRunIsDisabledByDefault(t *testing.T) {
	cfg := loadDryRunConfig(t, nil)
	d := cfg.ResourceExplorer
	if d.DryRunEnabled {
		t.Error("변경 검토 기본값은 비활성이어야 합니다")
	}
	if d.DryRunEnabledInvalid {
		t.Error("기본값이 boolean 파싱 실패로 표시되었습니다")
	}
	if len(d.DryRunResources) != 0 || len(d.DryRunDenyResources) != 0 {
		t.Errorf("대상 목록 기본값은 비어 있어야 합니다: %v / %v", d.DryRunResources, d.DryRunDenyResources)
	}
	if d.DryRunTimeout != 8*time.Second {
		t.Errorf("timeout 기본값=%v want 8s", d.DryRunTimeout)
	}
	if d.DryRunRate != 1 {
		t.Errorf("rate 기본값=%v want 1", d.DryRunRate)
	}
	if d.DryRunBurst != 3 {
		t.Errorf("burst 기본값=%d want 3", d.DryRunBurst)
	}
	if d.DryRunConcurrent != 1 {
		t.Errorf("concurrent 기본값=%d want 1", d.DryRunConcurrent)
	}
	if d.DryRunMaxManifestBytes != contract.DefaultDryRunManifestBytes {
		t.Errorf("manifest 상한 기본값=%d want %d", d.DryRunMaxManifestBytes, contract.DefaultDryRunManifestBytes)
	}
	if d.DryRunMaxObjectBytes != resourcecatalog.DefaultMaxObjectBytes {
		t.Errorf("object 상한 기본값=%d want %d", d.DryRunMaxObjectBytes, resourcecatalog.DefaultMaxObjectBytes)
	}
}

// TestResourceDryRunTuningIsIgnoredWhenDisabled — 꺼져 있으면 다른 상한이
// 무엇이든 기동을 막지 않습니다. Explorer의 기존 관례와 같습니다.
func TestResourceDryRunTuningIsIgnoredWhenDisabled(t *testing.T) {
	cfg := loadDryRunConfig(t, nil)
	cfg.ClusterState.Mode = "direct"
	cfg.ResourceExplorer.Enabled = true
	cfg.ResourceExplorer.DryRunEnabled = false
	cfg.ResourceExplorer.DryRunTimeout = -1
	cfg.ResourceExplorer.DryRunRate = math.NaN()
	cfg.ResourceExplorer.DryRunBurst = 0
	cfg.ResourceExplorer.DryRunConcurrent = 99
	cfg.ResourceExplorer.DryRunMaxManifestBytes = 1
	cfg.ResourceExplorer.DryRunMaxObjectBytes = 1
	cfg.ResourceExplorer.DryRunResources = []string{"not a gvr"}
	if err := cfg.Validate(); err != nil && strings.Contains(err.Error(), "DRY_RUN") {
		t.Fatalf("비활성 설정이 기동을 막았습니다: %v", err)
	}
}

// TestResourceDryRunEnabledFlagIsStrict — 기능 스위치 파싱 실패는 **꺼져 있어도**
// 기동을 막습니다. 조용히 off로 접으면 운영자는 켰다고 믿습니다.
func TestResourceDryRunEnabledFlagIsStrict(t *testing.T) {
	cfg := loadDryRunConfig(t, nil)
	cfg.ResourceExplorer.Enabled = false
	cfg.ResourceExplorer.DryRunEnabledInvalid = true
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "RESOURCE_EXPLORER_DRY_RUN_ENABLED") {
		t.Fatalf("boolean이 아닌 값이 통과했습니다: %v", err)
	}

	// 실제 env 경로로도 같은 결과여야 합니다.
	loaded := loadDryRunConfig(t, map[string]string{"RESOURCE_EXPLORER_DRY_RUN_ENABLED": "maybe"})
	if !loaded.ResourceExplorer.DryRunEnabledInvalid {
		t.Fatal("잘못된 boolean이 invalid로 표시되지 않았습니다")
	}
	if err := loaded.Validate(); err == nil || !strings.Contains(err.Error(), "RESOURCE_EXPLORER_DRY_RUN_ENABLED") {
		t.Fatalf("env 경로에서 통과했습니다: %v", err)
	}
}

/* ── 전제조건 ───────────────────────────────────────────────────────────── */

// TestResourceDryRunRequiresExplorerAndDirectMode — 검토는 Explorer 안에서만
// 존재하고, central에는 informer도 kubeconfig도 없습니다.
func TestResourceDryRunRequiresExplorerAndDirectMode(t *testing.T) {
	t.Run("Explorer가 꺼져 있음", func(t *testing.T) {
		cfg := loadDryRunConfig(t, nil)
		cfg.ClusterState.Mode = "direct"
		cfg.ResourceExplorer.Enabled = false
		cfg.ResourceExplorer.DryRunEnabled = true
		err := cfg.Validate()
		if err == nil || !strings.Contains(err.Error(), "RESOURCE_EXPLORER_DRY_RUN_ENABLED requires RESOURCE_EXPLORER_ENABLED") {
			t.Fatalf("Explorer 없이 검토가 통과했습니다: %v", err)
		}
	})
	t.Run("central 모드", func(t *testing.T) {
		cfg := enabledDryRunConfig(t)
		cfg.ClusterState.Mode = "central"
		err := cfg.Validate()
		if err == nil || !strings.Contains(err.Error(), "CLUSTER_STATE_MODE=direct") {
			t.Fatalf("central + 검토가 통과했습니다: %v", err)
		}
	})
	t.Run("central이고 Explorer도 꺼짐", func(t *testing.T) {
		cfg := loadDryRunConfig(t, nil)
		cfg.ClusterState.Mode = "central"
		cfg.ResourceExplorer.Enabled = false
		cfg.ResourceExplorer.DryRunEnabled = true
		if err := cfg.Validate(); err == nil {
			t.Fatal("central + Explorer off + 검토 on이 통과했습니다")
		}
	})
	t.Run("기준 설정은 통과", func(t *testing.T) {
		if err := enabledDryRunConfig(t).Validate(); err != nil {
			t.Fatalf("유효한 검토 설정이 거절되었습니다: %v", err)
		}
	})
}

/* ── 수치 상한 ──────────────────────────────────────────────────────────── */

// TestResourceDryRunRateRejectsNonFiniteAndParseErrors — rate는 유한해야 합니다.
// NaN·±Inf는 <=0과 >10 비교를 둘 다 통과하므로 명시적으로 막아야 하고, 파싱 실패도
// 조용한 기본값으로 흐르면 안 됩니다.
func TestResourceDryRunRateRejectsNonFiniteAndParseErrors(t *testing.T) {
	for _, raw := range []string{"NaN", "+Inf", "-Inf", "Inf", "abc", ""} {
		name := raw
		if name == "" {
			name = "빈 문자열이 아닌 공백"
			raw = "   "
		}
		t.Run(name, func(t *testing.T) {
			cfg := loadDryRunConfig(t, map[string]string{"RESOURCE_EXPLORER_DRY_RUN_RATE": raw})
			// Load()가 읽은 값을 그대로 두고 나머지만 유효하게 맞춥니다.
			rate := cfg.ResourceExplorer.DryRunRate
			enabled := enabledDryRunConfig(t)
			enabled.ResourceExplorer.DryRunRate = rate
			err := enabled.Validate()
			if err == nil || !strings.Contains(err.Error(), "RESOURCE_EXPLORER_DRY_RUN_RATE") {
				t.Fatalf("rate %q(%v)가 통과했습니다: %v", raw, rate, err)
			}
		})
	}
	// 유한한 경계값은 통과해야 합니다 — 상한이 실제로 상한인지 확인합니다.
	for _, ok := range []float64{0.001, 1, 10} {
		cfg := enabledDryRunConfig(t)
		cfg.ResourceExplorer.DryRunRate = ok
		if err := cfg.Validate(); err != nil {
			t.Errorf("허용 범위 rate %v가 거절되었습니다: %v", ok, err)
		}
	}
}

// TestResourceDryRunBoundsAreStrict — 경계 바깥과 파싱 실패 sentinel(-1)이
// 전부 기동을 막습니다.
func TestResourceDryRunBoundsAreStrict(t *testing.T) {
	cases := map[string]func(*Config){
		"TIMEOUT 0":                func(c *Config) { c.ResourceExplorer.DryRunTimeout = 0 },
		"TIMEOUT 상한":               func(c *Config) { c.ResourceExplorer.DryRunTimeout = 31 * time.Second },
		"TIMEOUT 파싱 실패":            func(c *Config) { c.ResourceExplorer.DryRunTimeout = -1 },
		"RATE 0":                   func(c *Config) { c.ResourceExplorer.DryRunRate = 0 },
		"RATE 상한":                  func(c *Config) { c.ResourceExplorer.DryRunRate = 10.5 },
		"BURST 하한":                 func(c *Config) { c.ResourceExplorer.DryRunBurst = 0 },
		"BURST 상한":                 func(c *Config) { c.ResourceExplorer.DryRunBurst = 21 },
		"BURST 파싱 실패":              func(c *Config) { c.ResourceExplorer.DryRunBurst = -1 },
		"CONCURRENT 하한":            func(c *Config) { c.ResourceExplorer.DryRunConcurrent = 0 },
		"CONCURRENT 상한":            func(c *Config) { c.ResourceExplorer.DryRunConcurrent = 5 },
		"MAX_MANIFEST_BYTES 하한":    func(c *Config) { c.ResourceExplorer.DryRunMaxManifestBytes = 4095 },
		"MAX_MANIFEST_BYTES 상한":    func(c *Config) { c.ResourceExplorer.DryRunMaxManifestBytes = contract.MaxDryRunManifestBytes + 1 },
		"MAX_MANIFEST_BYTES 파싱 실패": func(c *Config) { c.ResourceExplorer.DryRunMaxManifestBytes = -1 },
		"MAX_OBJECT_BYTES 하한":      func(c *Config) { c.ResourceExplorer.DryRunMaxObjectBytes = 4095 },
		"MAX_OBJECT_BYTES 상한":      func(c *Config) { c.ResourceExplorer.DryRunMaxObjectBytes = resourcecatalog.DefaultMaxObjectBytes + 1 },
		"MAX_OBJECT_BYTES 파싱 실패":   func(c *Config) { c.ResourceExplorer.DryRunMaxObjectBytes = -1 },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			cfg := enabledDryRunConfig(t)
			mutate(&cfg)
			if err := cfg.Validate(); err == nil {
				t.Fatalf("%s 위반이 통과했습니다", name)
			}
		})
	}
	// 경계 안쪽은 통과해야 합니다.
	edge := enabledDryRunConfig(t)
	edge.ResourceExplorer.DryRunTimeout = 30 * time.Second
	edge.ResourceExplorer.DryRunBurst = 20
	edge.ResourceExplorer.DryRunConcurrent = 4
	edge.ResourceExplorer.DryRunMaxManifestBytes = contract.MaxDryRunManifestBytes
	edge.ResourceExplorer.DryRunMaxObjectBytes = resourcecatalog.DefaultMaxObjectBytes
	if err := edge.Validate(); err != nil {
		t.Fatalf("경계값이 거절되었습니다: %v", err)
	}
}

// TestResourceDryRunIntegerEnvParseErrorsFailStartup — env 경로에서도 정수·duration
// 파싱 실패가 조용한 기본값이 아니라 기동 실패가 되는지 확인합니다.
func TestResourceDryRunIntegerEnvParseErrorsFailStartup(t *testing.T) {
	for _, tc := range []struct{ key, value, want string }{
		{"RESOURCE_EXPLORER_DRY_RUN_BURST", "three", "RESOURCE_EXPLORER_DRY_RUN_BURST"},
		{"RESOURCE_EXPLORER_DRY_RUN_CONCURRENT", "many", "RESOURCE_EXPLORER_DRY_RUN_CONCURRENT"},
		{"RESOURCE_EXPLORER_DRY_RUN_MAX_MANIFEST_BYTES", "256k", "RESOURCE_EXPLORER_DRY_RUN_MAX_MANIFEST_BYTES"},
		{"RESOURCE_EXPLORER_DRY_RUN_MAX_OBJECT_BYTES", "1m", "RESOURCE_EXPLORER_DRY_RUN_MAX_OBJECT_BYTES"},
		{"RESOURCE_EXPLORER_DRY_RUN_TIMEOUT", "8", "RESOURCE_EXPLORER_DRY_RUN_TIMEOUT"},
	} {
		t.Run(tc.key, func(t *testing.T) {
			loaded := loadDryRunConfig(t, map[string]string{tc.key: tc.value})
			cfg := enabledDryRunConfig(t)
			// Load()가 해석한 값 하나만 옮겨 심습니다.
			switch tc.key {
			case "RESOURCE_EXPLORER_DRY_RUN_BURST":
				cfg.ResourceExplorer.DryRunBurst = loaded.ResourceExplorer.DryRunBurst
			case "RESOURCE_EXPLORER_DRY_RUN_CONCURRENT":
				cfg.ResourceExplorer.DryRunConcurrent = loaded.ResourceExplorer.DryRunConcurrent
			case "RESOURCE_EXPLORER_DRY_RUN_MAX_MANIFEST_BYTES":
				cfg.ResourceExplorer.DryRunMaxManifestBytes = loaded.ResourceExplorer.DryRunMaxManifestBytes
			case "RESOURCE_EXPLORER_DRY_RUN_MAX_OBJECT_BYTES":
				cfg.ResourceExplorer.DryRunMaxObjectBytes = loaded.ResourceExplorer.DryRunMaxObjectBytes
			case "RESOURCE_EXPLORER_DRY_RUN_TIMEOUT":
				cfg.ResourceExplorer.DryRunTimeout = loaded.ResourceExplorer.DryRunTimeout
			}
			err := cfg.Validate()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("%s=%q가 통과했습니다: %v", tc.key, tc.value, err)
			}
		})
	}
}

/* ── 대상 목록 ──────────────────────────────────────────────────────────── */

// TestResourceDryRunAllowlistRules — 대상 목록의 규칙 전부입니다.
func TestResourceDryRunAllowlistRules(t *testing.T) {
	t.Run("core alias와 정렬된 최종 목록", func(t *testing.T) {
		cfg := enabledDryRunConfig(t)
		cfg.ResourceExplorer.DryRunResources = []string{"networking.k8s.io/v1/ingresses", "core/v1/configmaps"}
		final, err := cfg.ResourceDryRunAllowlist()
		if err != nil {
			t.Fatalf("유효한 목록이 거절되었습니다: %v", err)
		}
		got := make([]string, 0, len(final))
		for _, gvr := range final {
			got = append(got, resourcecatalog.FormatGVR(gvr))
		}
		want := []string{"core/v1/configmaps", "networking.k8s.io/v1/ingresses"}
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Fatalf("최종 목록=%v want %v", got, want)
		}
	})

	t.Run("형식이 틀린 GVR", func(t *testing.T) {
		for _, bad := range []string{"configmaps", "core/V1/configmaps", "core/v1/ConfigMaps", "core/v1/pods/log"} {
			cfg := enabledDryRunConfig(t)
			cfg.ResourceExplorer.DryRunResources = []string{bad}
			_, err := cfg.ResourceDryRunAllowlist()
			if err == nil || !strings.Contains(err.Error(), "RESOURCE_EXPLORER_DRY_RUN_RESOURCES") {
				t.Errorf("%q가 통과했습니다: %v", bad, err)
			}
		}
	})

	t.Run("Explorer allowlist 밖", func(t *testing.T) {
		cfg := enabledDryRunConfig(t)
		// batch/v1/jobs는 기본 Explorer allowlist에 있으므로, 명시적으로 좁힙니다.
		cfg.ResourceExplorer.Resources = []string{"core/v1/configmaps"}
		cfg.ResourceExplorer.DryRunResources = []string{"networking.k8s.io/v1/ingresses"}
		_, err := cfg.ResourceDryRunAllowlist()
		if err == nil || !strings.Contains(err.Error(), "RESOURCE_EXPLORER_DRY_RUN_RESOURCES") {
			t.Fatalf("부분집합이 아닌 목록이 통과했습니다: %v", err)
		}
	})

	// hard-deny는 core helper가 판정합니다. config는 더 넓은 규칙을 만들지 않습니다.
	t.Run("hard-deny 전 범주", func(t *testing.T) {
		for _, denied := range []string{
			"core/v1/secrets",
			"core/v1/serviceaccounts",
			"core/v1/nodes",
			"core/v1/namespaces",
			"rbac.authorization.k8s.io/v1/roles",
			"apiextensions.k8s.io/v1/customresourcedefinitions",
		} {
			cfg := enabledDryRunConfig(t)
			// Explorer allowlist에도 넣어 "부분집합 아님"이 아니라 hard-deny로 걸리게 합니다.
			cfg.ResourceExplorer.Resources = []string{"core/v1/configmaps", denied}
			cfg.ResourceExplorer.AllowCRDs = true
			cfg.ResourceExplorer.DryRunResources = []string{denied}
			_, err := cfg.ResourceDryRunAllowlist()
			if err == nil {
				t.Errorf("%s가 검토 대상으로 통과했습니다", denied)
			}
		}
	})

	// 승인 목록 밖의 무관한 대상은 막지 않습니다 — 넓은 group 금지를 새로 만들지
	// 않았다는 사실을 고정합니다.
	t.Run("무관한 대상은 허용", func(t *testing.T) {
		for _, allowed := range []string{
			"core/v1/configmaps",
			"core/v1/persistentvolumeclaims",
			"networking.k8s.io/v1/networkpolicies",
			"policy/v1/poddisruptionbudgets",
			"storage.k8s.io/v1/storageclasses",
		} {
			cfg := enabledDryRunConfig(t)
			cfg.ResourceExplorer.DryRunResources = []string{allowed}
			if _, err := cfg.ResourceDryRunAllowlist(); err != nil {
				t.Errorf("%s가 이유 없이 막혔습니다: %v", allowed, err)
			}
		}
	})

	t.Run("deny는 opt-in 목록의 부분집합", func(t *testing.T) {
		cfg := enabledDryRunConfig(t)
		cfg.ResourceExplorer.DryRunResources = []string{"core/v1/configmaps"}
		// Explorer allowlist에는 있지만 검토 opt-in 목록에는 없는 GVR입니다.
		cfg.ResourceExplorer.DryRunDenyResources = []string{"networking.k8s.io/v1/ingresses"}
		_, err := cfg.ResourceDryRunAllowlist()
		if err == nil || !strings.Contains(err.Error(), "RESOURCE_EXPLORER_DRY_RUN_DENY_RESOURCES") {
			t.Fatalf("opt-in 밖 deny가 통과했습니다: %v", err)
		}
	})

	t.Run("deny 형식 오류", func(t *testing.T) {
		cfg := enabledDryRunConfig(t)
		cfg.ResourceExplorer.DryRunDenyResources = []string{"nope"}
		_, err := cfg.ResourceDryRunAllowlist()
		if err == nil || !strings.Contains(err.Error(), "RESOURCE_EXPLORER_DRY_RUN_DENY_RESOURCES") {
			t.Fatalf("형식이 틀린 deny가 통과했습니다: %v", err)
		}
	})

	t.Run("deny가 적용되면 빠집니다", func(t *testing.T) {
		cfg := enabledDryRunConfig(t)
		cfg.ResourceExplorer.DryRunDenyResources = []string{"networking.k8s.io/v1/ingresses"}
		final, err := cfg.ResourceDryRunAllowlist()
		if err != nil {
			t.Fatalf("deny 적용이 실패했습니다: %v", err)
		}
		if len(final) != 1 || resourcecatalog.FormatGVR(final[0]) != "core/v1/configmaps" {
			t.Fatalf("deny가 적용되지 않았습니다: %v", final)
		}
	})

	t.Run("deny로 비면 기동 실패", func(t *testing.T) {
		cfg := enabledDryRunConfig(t)
		cfg.ResourceExplorer.DryRunDenyResources = []string{"core/v1/configmaps", "networking.k8s.io/v1/ingresses"}
		final, err := cfg.ResourceDryRunAllowlist()
		if err != nil {
			t.Fatalf("helper가 실패했습니다: %v", err)
		}
		if len(final) != 0 {
			t.Fatalf("최종 목록이 비어야 합니다: %v", final)
		}
		if verr := cfg.Validate(); verr == nil || !strings.Contains(verr.Error(), "RESOURCE_EXPLORER_DRY_RUN_RESOURCES") {
			t.Fatalf("빈 최종 목록이 통과했습니다: %v", verr)
		}
	})

	t.Run("켰는데 목록이 비면 기동 실패", func(t *testing.T) {
		cfg := enabledDryRunConfig(t)
		cfg.ResourceExplorer.DryRunResources = nil
		if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "RESOURCE_EXPLORER_DRY_RUN_RESOURCES") {
			t.Fatalf("빈 목록으로 켠 설정이 통과했습니다: %v", err)
		}
	})
}

// TestResourceDryRunOverflowFailsAtExplorerLayerFirst — 65개를 요청하면 Explorer
// allowlist 자체가 64개 상한이라 **그 계층에서 먼저** 실패합니다.
//
// 전용 상한(MaxDryRunAllowlistEntries)만 따로 증명하려고 계층을 우회하지 않습니다 —
// 그 상한은 U2 core 테스트가 이미 증명하고, 여기서는 실제로 어떤 순서로 실패하는지를
// 그대로 못박습니다.
func TestResourceDryRunOverflowFailsAtExplorerLayerFirst(t *testing.T) {
	many := make([]string, 0, 65)
	for i := 0; i < 65; i++ {
		many = append(many, "example.com/v1/widget"+strconv.Itoa(i))
	}
	cfg := enabledDryRunConfig(t)
	cfg.ResourceExplorer.AllowCRDs = true
	cfg.ResourceExplorer.Resources = many
	cfg.ResourceExplorer.DryRunResources = many

	_, err := cfg.ResourceAllowlist()
	if err == nil || !strings.Contains(err.Error(), "RESOURCE_EXPLORER_RESOURCES") {
		t.Fatalf("Explorer allowlist가 65개를 받아들였습니다: %v", err)
	}
	// 검토 helper도 같은 오류를 그대로 전파합니다 — 자기 상한에 닿기 전입니다.
	_, dryErr := cfg.ResourceDryRunAllowlist()
	if dryErr == nil || !strings.Contains(dryErr.Error(), "RESOURCE_EXPLORER_RESOURCES") {
		t.Fatalf("검토 helper가 Explorer 오류를 전파하지 않았습니다: %v", dryErr)
	}
	if strings.Contains(dryErr.Error(), "RESOURCE_EXPLORER_DRY_RUN_RESOURCES") {
		t.Errorf("전용 상한 오류로 잘못 보고했습니다: %v", dryErr)
	}
}

// TestResourceDryRunAllowlistIsNormalizedOnce — helper 결과는 고정점입니다.
// main이 그 목록을 nil deny와 함께 넘겨도 다시 걸러지지 않아야 합니다.
func TestResourceDryRunAllowlistIsNormalizedOnce(t *testing.T) {
	cfg := enabledDryRunConfig(t)
	cfg.ResourceExplorer.DryRunDenyResources = []string{"networking.k8s.io/v1/ingresses"}
	final, err := cfg.ResourceDryRunAllowlist()
	if err != nil {
		t.Fatal(err)
	}
	explorer, err := cfg.ResourceAllowlist()
	if err != nil {
		t.Fatal(err)
	}
	again, err := resourcecatalog.NormalizeDryRunAllowlist(final, explorer, nil)
	if err != nil {
		t.Fatalf("nil deny 재검증이 실패했습니다: %v", err)
	}
	if len(again) != len(final) {
		t.Fatalf("재검증이 목록을 바꿨습니다: %v → %v", final, again)
	}
	for i := range final {
		if again[i] != final[i] {
			t.Fatalf("재검증이 목록을 바꿨습니다: %v → %v", final, again)
		}
	}
}
