package v1

import (
	"crypto/sha256"
	"fmt"
	"os"
	"testing"
)

func TestProtocolGeneratedDrift(t *testing.T) {
	want := map[string]string{
		"cluster_state.proto":      "65e1b0e8837d325f5cd0b964baa730ddbca6e8dd5f58e3730904f17aae3f214f",
		"cluster_state.pb.go":      "99372cfac4efeae0bab644e06257a15df02d7974a018fa83a6b48600a1c1e8c2",
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
