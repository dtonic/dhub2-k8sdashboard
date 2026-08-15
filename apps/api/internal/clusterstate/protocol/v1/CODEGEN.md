# Cluster-state protocol code generation

The checked-in bindings are generated from `cluster_state.proto` with:

- `libprotoc 28.3` (`protoc` SHA-256 `0e8c86f9b69b2b0fff91d56d8906a846ad89ce56dfeb1b673287b61c959bd633`)
- `protoc-gen-go v1.34.2`
- `protoc-gen-go-grpc v1.5.1`

Run from `apps/api`:

```sh
protoc --go_out=. --go_opt=paths=source_relative \
  --go-grpc_out=. --go-grpc_opt=paths=source_relative \
  internal/clusterstate/protocol/v1/cluster_state.proto
go test ./internal/clusterstate/protocol/v1 -run TestProtocolGeneratedDrift
```

The drift test independently pins the schema and both generated outputs, so a
schema-only or generated-only mutation fails CI. Generated `.pb.go` files must
never be edited manually.
