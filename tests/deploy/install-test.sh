#!/usr/bin/env bash
# Runs INSIDE a throwaway Linux container as root (see run-install-test.sh).
# Exercises scripts/install.sh: validation, permissions, idempotency, uninstall.
set -euo pipefail

export GIT_KEEPR_SKIP_SYSTEMD=1
SRC="${SRC:-/src}"
case "$(uname -m)" in x86_64) arch=amd64 ;; aarch64) arch=arm64 ;; *) echo "unsupported arch"; exit 1 ;; esac
BIN="${SRC}/dist/git-keepr-linux-${arch}"
INSTALL="${SRC}/scripts/install.sh"
work="$(mktemp -d)"
pass=0

ok()   { pass=$((pass + 1)); printf 'ok   - %s\n' "$*"; }
fail() { printf 'FAIL - %s\n' "$*" >&2; exit 1; }
assert_eq() { [[ "$1" == "$2" ]] || fail "$3 (got '$1', want '$2')"; ok "$3"; }
mode_of() { stat -c '%a %U:%G' "$1"; }

cat >"${work}/config.yaml" <<YAML
version: 1
credentials:
  personal: {type: github_pat, token_env: IT_TOKEN}
repositories:
  - {id: github/example/backend, url: "https://github.com/example/backend.git", credential: personal}
storage:
  local:
    root: /var/lib/git-keepr
  s3: {enabled: false}
YAML

# 1. config with the wrong data root is rejected and nothing is installed
sed 's#/var/lib/git-keepr#/data#' "${work}/config.yaml" >"${work}/bad-root.yaml"
if "$INSTALL" --binary "$BIN" --config "${work}/bad-root.yaml" >"${work}/out" 2>&1; then fail "wrong storage root must be rejected"; fi
grep -q 'storage.local.root' "${work}/out" || fail "error should mention storage.local.root"
[[ ! -e /etc/git-keepr/config.yaml ]] || fail "nothing may be installed after a rejected config"
ok "rejects config whose storage.local.root is not /var/lib/git-keepr"

# 2. non-ELF and wrong-architecture binaries are rejected
echo "#!/bin/sh" >"${work}/fake"
if "$INSTALL" --binary "${work}/fake" --config "${work}/config.yaml" >"${work}/out" 2>&1; then fail "script must be rejected"; fi
ok "rejects non-ELF binary"
printf '\x7fELF\x02\x01\x01\x00\x00\x00\x00\x00\x00\x00\x00\x00\x02\x00\x28\x00' >"${work}/arm32"
if "$INSTALL" --binary "${work}/arm32" --config "${work}/config.yaml" >"${work}/out" 2>&1; then fail "foreign arch must be rejected"; fi
ok "rejects binary built for another architecture"

# 3. missing arguments
if "$INSTALL" >"${work}/out" 2>&1; then fail "no --binary/--build must fail"; fi
ok "requires --binary or --build"

# 4. fresh install
"$INSTALL" --binary "$BIN" --config "${work}/config.yaml" >"${work}/out" 2>&1 || { cat "${work}/out"; fail "fresh install failed"; }
getent passwd git-keepr >/dev/null || fail "service account missing"
shell="$(getent passwd git-keepr | cut -d: -f7)"
case "$shell" in */nologin|*/false) ok "service account has no login shell ($shell)" ;; *) fail "service account shell is $shell" ;; esac
assert_eq "$(mode_of /etc/git-keepr)" "750 root:git-keepr" "/etc/git-keepr permissions"
assert_eq "$(mode_of /etc/git-keepr/config.yaml)" "640 root:git-keepr" "config permissions"
assert_eq "$(mode_of /etc/git-keepr/credentials.env)" "600 root:root" "credentials.env permissions"
assert_eq "$(mode_of /var/lib/git-keepr)" "700 git-keepr:git-keepr" "data dir permissions"
assert_eq "$(mode_of /usr/local/bin/git-keepr)" "755 root:root" "binary permissions"
assert_eq "$(mode_of /etc/systemd/system/git-keepr.service)" "644 root:root" "unit permissions"
cmp -s "${SRC}/deploy/git-keepr.service" /etc/systemd/system/git-keepr.service || fail "unit differs from deploy/git-keepr.service"
grep -q '^#IT_TOKEN=' /etc/git-keepr/credentials.env || fail "credentials template should name IT_TOKEN"
grep -q 'NOT started' "${work}/out" || fail "service must not start before tokens exist"
ok "fresh install leaves the service stopped with next steps"

# 5. idempotent upgrade preserves config, credentials and data; replaces binary
echo "IT_TOKEN=dummy-install-token-123" >>/etc/git-keepr/credentials.env
echo "# operator edit" >>/etc/git-keepr/config.yaml
echo "keep me" >/var/lib/git-keepr/marker
before_cfg="$(sha256sum /etc/git-keepr/config.yaml)"
before_cred="$(sha256sum /etc/git-keepr/credentials.env)"
cp "$BIN" "${work}/newbin" && printf '\0' >>"${work}/newbin"   # a different but still valid binary
sed 's#github/example/backend#github/example/other#' "${work}/config.yaml" >"${work}/other.yaml"
"$INSTALL" --binary "${work}/newbin" --config "${work}/other.yaml" >"${work}/out" 2>&1 || { cat "${work}/out"; fail "upgrade failed"; }
assert_eq "$(sha256sum /etc/git-keepr/config.yaml)" "$before_cfg" "existing config is not overwritten"
assert_eq "$(sha256sum /etc/git-keepr/credentials.env)" "$before_cred" "existing credentials are not overwritten"
assert_eq "$(cat /var/lib/git-keepr/marker)" "keep me" "data directory is preserved"
assert_eq "$(sha256sum </usr/local/bin/git-keepr | cut -d' ' -f1)" "$(sha256sum <"${work}/newbin" | cut -d' ' -f1)" "binary is replaced"
grep -q 'configuration validated' "${work}/out" || { cat "${work}/out"; fail "valid config should validate"; }
grep -q 'skipping: systemctl restart git-keepr' "${work}/out" || fail "validated install should (re)start the service"
if grep -q 'dummy-install-token-123' "${work}/out"; then fail "token value was printed"; fi
ok "upgrade validates, restarts, and never prints secrets"
assert_eq "$(mode_of /etc/git-keepr/credentials.env)" "600 root:root" "credentials stay 0600 after upgrade"

# 6. upgrade without --config uses the installed one
"$INSTALL" --binary "$BIN" >"${work}/out" 2>&1 || fail "upgrade without --config failed"
ok "upgrade works without --config"

# 7. --no-start
"$INSTALL" --binary "$BIN" --no-start >"${work}/out" 2>&1 || fail "--no-start failed"
if grep -q 'systemctl restart' "${work}/out"; then fail "--no-start must not restart"; fi
grep -q 'leaving the service stopped' "${work}/out" || fail "--no-start should say so"
ok "--no-start leaves the service stopped"

# 8. broken credentials -> installed but not started
sed -i '/^IT_TOKEN=/d' /etc/git-keepr/credentials.env
"$INSTALL" --binary "$BIN" >"${work}/out" 2>&1 || fail "install with missing token should still succeed"
if ! { grep -q 'IT_TOKEN' "${work}/out" && grep -q 'NOT started' "${work}/out"; }; then cat "${work}/out"; fail "missing token must block start"; fi
ok "missing token blocks the start and names the variable"

# 9. uninstall keeps config, credentials, data and the account
"$INSTALL" --uninstall >"${work}/out" 2>&1 || fail "uninstall failed"
[[ ! -e /usr/local/bin/git-keepr && ! -e /etc/systemd/system/git-keepr.service ]] || fail "binary/unit should be removed"
[[ -f /etc/git-keepr/config.yaml && -f /etc/git-keepr/credentials.env && -f /var/lib/git-keepr/marker ]] || fail "config/credentials/data must survive uninstall"
getent passwd git-keepr >/dev/null || fail "account must survive uninstall"
ok "uninstall keeps config, credentials, data and account"

printf '\nall %d installer checks passed\n' "$pass"
