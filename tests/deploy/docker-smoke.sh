#!/usr/bin/env bash
# Smoke-tests the container image and compose.yaml: non-root user, read-only
# root filesystem, writable fresh named volume, and graceful SIGTERM shutdown.
# Needs docker (or podman's docker emulation) and a locally built image:
#   make docker-build && tests/deploy/docker-smoke.sh
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
ENGINE="$(command -v docker || command -v podman || true)"
[[ -n "$ENGINE" ]] || { echo "docker or podman is required" >&2; exit 1; }
IMAGE="${IMAGE:-git-keepr:local}"
tag="gksmoke$$"
tmp="$(mktemp -d)"
pass=0
# On SELinux-enforcing hosts bind mounts need relabelling (README: append :z); the test just disables labelling.
selinux_opt=()
selinux_enforcing=0
if command -v getenforce >/dev/null 2>&1 && [[ "$(getenforce)" == "Enforcing" ]]; then
  selinux_opt=(--security-opt label=disable)
  selinux_enforcing=1
fi
# Copy compose.yaml, adding "label=disable" to security_opt on SELinux hosts.
prepare_compose() {
  if [[ "$selinux_enforcing" -eq 1 ]]; then
    sed -E 's/^([[:space:]]*)- no-new-privileges:true$/&\n\1- label=disable/'
  else
    cat
  fi
}
ok()   { pass=$((pass + 1)); printf 'ok   - %s\n' "$*"; }
fail() { printf 'FAIL - %s\n' "$*" >&2; exit 1; }

cleanup() {
  "$ENGINE" compose -p "$tag" -f "${tmp}/proj/compose.yaml" down -v >/dev/null 2>&1 || true
  "$ENGINE" volume rm -f "${tag}-vol" >/dev/null 2>&1 || true
  rm -rf "$tmp"
}
trap cleanup EXIT

cat >"${tmp}/config.yaml" <<YAML
version: 1
credentials:
  personal: {type: github_pat, token_env: SMOKE_PAT}
repositories:
  - {id: github/example/backend, url: "https://github.com/example/backend.git", credential: personal, interval: 1h}
storage:
  local: {root: /data}
  s3: {enabled: false}
YAML
echo "SMOKE_PAT=dummy-smoke-token-not-real" >"${tmp}/.env"
chmod 600 "${tmp}/.env"   # read by compose on the host, never by the container
chmod 644 "${tmp}/config.yaml"   # holds no secrets; must be readable by container UID 10001
chmod 755 "${tmp}"

[[ "$("$ENGINE" run --rm --entrypoint id "$IMAGE" -u)" == "10001" ]] || fail "container must run as UID 10001"
ok "runs as non-root UID 10001"
"$ENGINE" run --rm "$IMAGE" version | grep -q '^git-keepr ' || fail "version"
ok "entrypoint is the git-keepr binary"
"$ENGINE" run --rm --entrypoint git "$IMAGE" --version >/dev/null || fail "git missing from image"
ok "git is installed in the image"
if "$ENGINE" run --rm --read-only --entrypoint sh "$IMAGE" -c 'touch /should-fail' 2>/dev/null; then fail "root fs should be read-only"; fi
ok "root filesystem is read-only under --read-only"

"$ENGINE" volume create "${tag}-vol" >/dev/null
"$ENGINE" run --rm --read-only --tmpfs /tmp --security-opt no-new-privileges:true "${selinux_opt[@]}" \
  -v "${tag}-vol:/data" -v "${tmp}/config.yaml:/etc/git-keepr/config.yaml:ro" --env-file "${tmp}/.env" \
  "$IMAGE" config validate --config /etc/git-keepr/config.yaml >/dev/null || fail "validate with a fresh named volume failed (is /data writable?)"
ok "fresh named volume is writable by the non-root user (config validate)"

if "$ENGINE" compose version >/dev/null 2>&1; then
  mkdir -p "${tmp}/proj"
  # same file, minus the fixed container_name so the test cannot collide with a real deployment
  grep -v 'container_name:' "${ROOT}/compose.yaml" | prepare_compose >"${tmp}/proj/compose.yaml"
  cp "${tmp}/config.yaml" "${tmp}/.env" "${tmp}/proj/"
  "$ENGINE" compose -p "$tag" -f "${tmp}/proj/compose.yaml" up -d --no-build >/dev/null || fail "compose up failed"
  cid="$("$ENGINE" compose -p "$tag" -f "${tmp}/proj/compose.yaml" ps -q git-keepr)"
  for _ in $(seq 1 30); do
    "$ENGINE" logs "$cid" 2>&1 | grep -q 'scheduler started' && break
    sleep 1
  done
  "$ENGINE" logs "$cid" 2>&1 | grep -q 'scheduler started' || { "$ENGINE" logs "$cid" 2>&1 | tail -20; fail "daemon did not start under compose"; }
  [[ "$("$ENGINE" inspect -f '{{.State.Running}}' "$cid")" == "true" ]] || fail "container not running"
  ok "compose up: daemon runs (scheduler started)"
  [[ "$("$ENGINE" exec "$cid" id -u)" == "10001" ]] || fail "compose container not non-root"
  ok "compose container runs as non-root"
  if "$ENGINE" logs "$cid" 2>&1 | grep -q 'dummy-smoke-token-not-real'; then fail "token appeared in logs"; fi
  ok "token is not in container logs"
  "$ENGINE" compose -p "$tag" -f "${tmp}/proj/compose.yaml" stop -t 30 >/dev/null
  [[ "$("$ENGINE" inspect -f '{{.State.ExitCode}}' "$cid")" == "0" ]] || fail "SIGTERM should give exit code 0"
  ok "docker compose stop => graceful exit 0"
else
  echo "SKIPPED - 'compose' subcommand not available; compose.yaml was not exercised" >&2
fi
printf '\n%d docker checks passed\n' "$pass"
