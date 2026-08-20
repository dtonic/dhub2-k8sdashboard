package v1

import (
	"crypto/sha256"
	"fmt"
	"os"
	"testing"
)

func TestProtocolGeneratedDrift(t *testing.T) {
	want := map[string]string{
		"cluster_state.proto":      "94c38d25701f4a8b3eb28d9e862f6b5e421b2c73318c9fa8c166d50c4f187742",
		"cluster_state.pb.go":      "22672b41effe4cd4a2cb163021cd81c844a856d3fd237233c4e4f73bbdce0165",
		"cluster_state_grpc.pb.go": "d22496dcff37c7525ab532e2789d077b8d64eab4337df151cc2c3642475cc287",
	}
	for path, hash := range want {
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := fmt.Sprintf("%x", sha256.Sum256(b)); got != hash {
			t.Fatalf("%s drift: run pinned code generation", path)
		}
	}
}
