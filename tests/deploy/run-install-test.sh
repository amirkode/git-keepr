#!/usr/bin/env bash
# Host-side runner: executes install-test.sh as root in a disposable Debian container.
# Needs docker or podman, network access (to apt-get git), and `make dist` output.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
ENGINE="$(command -v docker || command -v podman || true)"
[[ -n "$ENGINE" ]] || { echo "docker or podman is required" >&2; exit 1; }
IMAGE="${INSTALL_TEST_IMAGE:-docker.io/library/debian:stable-slim}"
exec "$ENGINE" run --rm --security-opt label=disable -v "${ROOT}:/src:ro" "$IMAGE" bash -c '
  set -e
  export DEBIAN_FRONTEND=noninteractive
  apt-get update -qq >/dev/null
  apt-get install -y -qq --no-install-recommends git ca-certificates util-linux passwd >/dev/null
  SRC=/src bash /src/tests/deploy/install-test.sh'
