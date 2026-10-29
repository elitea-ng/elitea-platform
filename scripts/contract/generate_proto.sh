#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
proto_root="${repo_root}/libs/proto"
go_tools="${ELITEA_PROTO_GO_TOOLS:-/tmp/elitea-proto-tools}"
python_tools="${ELITEA_PROTO_PYTHON_TOOLS:-/tmp/elitea-proto-tools-py}"
python_bin="${ELITEA_PROTO_PYTHON:-python3}"

export BUF_CACHE_DIR="${BUF_CACHE_DIR:-/tmp/elitea-buf-cache}"
export PATH="${go_tools}:${PATH}"
export PYTHONDONTWRITEBYTECODE=1

check_version() {
  local tool="$1"
  local actual="$2"
  local expected="$3"
  if [[ "${actual}" != "${expected}" ]]; then
    echo "${tool} version ${actual}; expected ${expected}" >&2
    return 1
  fi
}

# The versions come from libs/proto/toolchain.lock, the one place they are
# pinned (CI installs from it, and checks pyproject.toml agrees with it).
pinned() {
  "${python_bin}" -c 'import json, sys; print(json.load(open(sys.argv[1]))[sys.argv[2]])' \
    "${proto_root}/toolchain.lock" "$1"
}

check_version "buf" "$(buf --version)" "$(pinned buf)"
check_version \
  "protoc-gen-go" \
  "$(protoc-gen-go --version | awk '{print $2}')" \
  "v$(pinned protoc-gen-go)"
check_version \
  "protoc-gen-go-grpc" \
  "$(protoc-gen-go-grpc --version | awk '{print $2}')" \
  "$(pinned protoc-gen-go-grpc)"
check_version \
  "grpcio-tools" \
  "$(PYTHONPATH="${python_tools}${PYTHONPATH:+:${PYTHONPATH}}" "${python_bin}" -c 'import importlib.metadata as m; print(m.version("grpcio-tools"))')" \
  "$(pinned grpcio-tools)"
check_version \
  "grpcio-tools protoc" \
  "$(PYTHONPATH="${python_tools}${PYTHONPATH:+:${PYTHONPATH}}" "${python_bin}" -m grpc_tools.protoc --version | awk '{print $2}')" \
  "$(pinned grpcio-tools-protoc)"
check_version \
  "protobuf Python runtime" \
  "$(PYTHONPATH="${python_tools}${PYTHONPATH:+:${PYTHONPATH}}" "${python_bin}" -c 'import importlib.metadata as m; print(m.version("protobuf"))')" \
  "$(pinned protobuf-python)"

cd "${proto_root}"
buf lint .
buf generate . --template buf.gen.yaml

PYTHONPATH="${python_tools}${PYTHONPATH:+:${PYTHONPATH}}" \
  "${python_bin}" -m grpc_tools.protoc \
  -I . \
  --python_out=gen/python \
  --pyi_out=gen/python \
  elitea/config/v1/*.proto \
  elitea/runtime/v1/*.proto

PYTHONPATH="${python_tools}${PYTHONPATH:+:${PYTHONPATH}}" \
  "${python_bin}" -m grpc_tools.protoc \
  -I . \
  --grpc_python_out=gen/python \
  elitea/runtime/v1/control.proto \
  elitea/runtime/v1/output.proto \
  elitea/runtime/v1/sandbox.proto
