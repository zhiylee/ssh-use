#!/bin/sh
set -eu
command -v protoc >/dev/null
rpc_generator_dir=$(mktemp -d)
trap 'rm -rf "$rpc_generator_dir"' EXIT HUP INT TERM
GOBIN="$rpc_generator_dir" go install google.golang.org/protobuf/cmd/protoc-gen-go@v1.36.12
GOBIN="$rpc_generator_dir" go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@v1.6.2
cd ../..
PATH="$rpc_generator_dir:$PATH" protoc \
  --go_out=. --go_opt=module=github.com/zhiylee/ssh-use \
  --go-grpc_out=. --go-grpc_opt=module=github.com/zhiylee/ssh-use \
  internal/rpcpb/gateway.proto
