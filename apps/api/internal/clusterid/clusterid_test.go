package clusterid

import (
	"strings"
	"testing"
)

func TestCanonicalClusterID(t *testing.T) {
	for _, valid := range []string{"a", "cluster-a", "prod.seoul_1", "a" + strings.Repeat("b", MaxLength-1)} {
		if !Valid(valid) {
			t.Fatalf("valid cluster ID rejected: %q", valid)
		}
	}
	for _, invalid := range []string{"", "A", "-a", "a-", "a/b", "a b", "a" + strings.Repeat("b", MaxLength)} {
		if Valid(invalid) {
			t.Fatalf("invalid cluster ID accepted: %q", invalid)
		}
	}
}
