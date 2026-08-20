#!/usr/bin/env sh
set -eu
ROOT=$(CDPATH= cd -- "$(dirname "$0")/../.." && pwd)
DIR="$ROOT/apps/api/internal/clusterstate/protocol/v1"
EXPECTED='94c38d25701f4a8b3eb28d9e862f6b5e421b2c73318c9fa8c166d50c4f187742  cluster_state.proto
22672b41effe4cd4a2cb163021cd81c844a856d3fd237233c4e4f73bbdce0165  cluster_state.pb.go
d22496dcff37c7525ab532e2789d077b8d64eab4337df151cc2c3642475cc287  cluster_state_grpc.pb.go'
check() { printf '%s\n' "$EXPECTED" | (cd "$1" && sha256sum -c - >/dev/null); }
check "$DIR"
grep -q 'protoc-gen-go v1.34.2' "$DIR/cluster_state.pb.go"
grep -q 'protoc-gen-go-grpc v1.5.1' "$DIR/cluster_state_grpc.pb.go"
grep -q 'protoc .*v5.28.3' "$DIR/cluster_state.pb.go"
if [ "${1:-}" = --self-test ]; then
  TMP_BASE=$(realpath "${TMPDIR:-/tmp}"); TMP=$(mktemp -d "$TMP_BASE/issue25-proto.XXXXXX")
  cleanup() { case "$(realpath "$TMP")" in "$TMP_BASE"/issue25-proto.*) rm -rf -- "$TMP";; *) return 1;; esac; }; trap cleanup EXIT HUP INT TERM
  cp "$DIR"/* "$TMP/"
  printf '\n' >> "$TMP/cluster_state.proto"
  if check "$TMP" 2>/dev/null; then echo 'proto mutation unexpectedly passed' >&2; exit 1; fi
  cp "$DIR/cluster_state.proto" "$TMP/cluster_state.proto"; printf '\n' >> "$TMP/cluster_state.pb.go"
  if check "$TMP" 2>/dev/null; then echo 'generated mutation unexpectedly passed' >&2; exit 1; fi
fi
