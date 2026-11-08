#!/usr/bin/env bash
# git-keepr native installer for Linux hosts with systemd.
#
#   sudo ./scripts/install.sh --binary ./dist/git-keepr-linux-amd64 --config ./config.yaml
#   sudo ./scripts/install.sh --build --config ./config.yaml      # needs Go
#
# Safe to re-run: existing config, credentials and data are never overwritten or
# deleted; the binary and unit are replaced atomically; the service is only
# (re)started once the configuration validates.
#
# GIT_KEEPR_SKIP_SYSTEMD=1 installs files, user and directories but skips the
# systemd presence check and every systemctl call (container images, chroots, CI).
set -euo pipefail

SERVICE_USER="git-keepr"
BIN_PATH="/usr/local/bin/git-keepr"
ETC_DIR="/etc/git-keepr"
CONFIG_PATH="${ETC_DIR}/config.yaml"
CREDENTIALS_PATH="${ETC_DIR}/credentials.env"
DATA_DIR="/var/lib/git-keepr"
UNIT_PATH="/etc/systemd/system/git-keepr.service"
SKIP_SYSTEMD="${GIT_KEEPR_SKIP_SYSTEMD:-0}"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# Unit file: next to the repository root (source checkout) or beside the script (release archive).
UNIT_SRC=""
for candidate in "${SCRIPT_DIR}/../deploy/git-keepr.service" "${SCRIPT_DIR}/deploy/git-keepr.service"; do
  if [[ -r "$candidate" ]]; then UNIT_SRC="$candidate"; break; fi
done

BINARY=""
BUILD=0
CONFIG_SRC=""
NO_START=0
UNINSTALL=0
TMP_DIR=""

usage() {
  cat <<'EOF'
Usage: install.sh [--binary PATH | --build] [--config PATH] [--no-start]
       install.sh --uninstall

  --binary PATH   install this prebuilt Linux binary (amd64 or arm64)
  --build         build from source with Go (run from a source checkout)
  --config PATH   configuration to install as /etc/git-keepr/config.yaml
                  (required on first install; an existing config is never overwritten)
  --no-start      install and enable, but do not start or restart the service
  --uninstall     stop and remove the service, unit and binary; KEEPS config,
                  credentials, the data directory and the system account
EOF
}

log()  { printf '==> %s\n' "$*"; }
warn() { printf 'warning: %s\n' "$*" >&2; }
die()  { printf 'error: %s\n' "$*" >&2; exit 1; }

cleanup() { [[ -n "$TMP_DIR" ]] && rm -rf "$TMP_DIR"; return 0; }
trap cleanup EXIT

while [[ $# -gt 0 ]]; do
  case "$1" in
    --binary)    [[ $# -ge 2 ]] || die "--binary needs a path"; BINARY="$2"; shift 2 ;;
    --build)     BUILD=1; shift ;;
    --config)    [[ $# -ge 2 ]] || die "--config needs a path"; CONFIG_SRC="$2"; shift 2 ;;
    --no-start)  NO_START=1; shift ;;
    --uninstall) UNINSTALL=1; shift ;;
    -h|--help)   usage; exit 0 ;;
    *)           usage >&2; die "unknown option: $1" ;;
  esac
done

# ---------------------------------------------------------------- preflight
[[ "$(uname -s)" == "Linux" ]] || die "this installer supports Linux only"
[[ "$(id -u)" -eq 0 ]] || die "run as root (for example with sudo)"
if [[ "$SKIP_SYSTEMD" != "1" ]]; then
  [[ -d /run/systemd/system ]] || die "systemd is not running on this host (set GIT_KEEPR_SKIP_SYSTEMD=1 to install files only)"
  command -v systemctl >/dev/null || die "systemctl not found"
fi

systemctl_do() { if [[ "$SKIP_SYSTEMD" == "1" ]]; then log "skipping: systemctl $*"; else systemctl "$@"; fi; }

if [[ "$UNINSTALL" -eq 1 ]]; then
  if [[ "$SKIP_SYSTEMD" != "1" ]]; then
    systemctl disable --now git-keepr 2>/dev/null || true
  fi
  rm -f "$UNIT_PATH" "$BIN_PATH"
  systemctl_do daemon-reload
  log "removed the service unit and binary"
  log "kept: ${ETC_DIR}, ${DATA_DIR} and the '${SERVICE_USER}' account (delete them yourself if you really want to)"
  exit 0
fi

[[ -n "$UNIT_SRC" ]] || die "deploy/git-keepr.service not found next to this script"
if [[ -n "$BINARY" && "$BUILD" -eq 1 ]]; then die "use either --binary or --build, not both"; fi
if [[ -z "$BINARY" && "$BUILD" -eq 0 ]]; then usage >&2; die "specify --binary PATH or --build"; fi

command -v git >/dev/null || die "git is required at runtime but was not found in PATH"
have_ca=0
for ca in /etc/ssl/certs/ca-certificates.crt /etc/pki/tls/certs/ca-bundle.crt /etc/ssl/cert.pem /etc/pki/ca-trust/extracted/pem/tls-ca-bundle.pem; do
  [[ -s "$ca" ]] && have_ca=1
done
[[ "$have_ca" -eq 1 ]] || die "no CA certificate bundle found; install your distribution's ca-certificates package"

TMP_DIR="$(mktemp -d)"

# ------------------------------------------------------------------- binary
if [[ "$BUILD" -eq 1 ]]; then
  command -v go >/dev/null || die "--build needs Go in PATH"
  [[ -f "${SCRIPT_DIR}/../go.mod" ]] || die "--build must be run from a source checkout"
  log "building from source"
  version="$(git -C "${SCRIPT_DIR}/.." describe --tags --always --dirty 2>/dev/null || echo dev)"
  (cd "${SCRIPT_DIR}/.." && CGO_ENABLED=0 go build -trimpath \
      -ldflags "-s -w -X github.com/amirkode/git-keepr/internal/cli.Version=${version}" \
      -o "${TMP_DIR}/git-keepr" ./cmd/git-keepr) || die "build failed"
  BINARY="${TMP_DIR}/git-keepr"
fi

[[ -f "$BINARY" && -r "$BINARY" ]] || die "binary not found or unreadable: $BINARY"
magic="$(od -An -tx1 -N4 "$BINARY" | tr -d ' \n')"
[[ "$magic" == "7f454c46" ]] || die "$BINARY is not a Linux (ELF) executable"
machine="$(od -An -tx1 -j18 -N2 "$BINARY" | tr -d ' \n')"
case "$(uname -m)" in
  x86_64|amd64)  want="3e00"; arch_name="amd64" ;;
  aarch64|arm64) want="b700"; arch_name="arm64" ;;
  *) die "unsupported host architecture: $(uname -m) (supported: x86_64, aarch64)" ;;
esac
[[ "$machine" == "$want" ]] || die "$BINARY was not built for this host (${arch_name} expected)"
chmod 0755 "$BINARY" 2>/dev/null || true
version_out="$("$BINARY" version 2>&1)" || die "$BINARY does not run: ${version_out}"
log "binary OK: ${version_out}"

# ------------------------------------------------------------------- config
config_root_of() {
  # Print storage.local.root from a YAML file (simple line-based extraction).
  awk '
    /^[[:space:]]*local:[[:space:]]*$/ { inlocal=1; next }
    inlocal && /^[[:space:]]*root:/ {
      sub(/^[[:space:]]*root:[[:space:]]*/, ""); sub(/[[:space:]]*#.*$/, "");
      gsub(/^["\047]|["\047]$/, ""); print; exit
    }
    /^[^[:space:]#]/ { inlocal=0 }
  ' "$1"
}

if [[ -n "$CONFIG_SRC" ]]; then
  [[ -f "$CONFIG_SRC" && -r "$CONFIG_SRC" ]] || die "config not readable: $CONFIG_SRC"
  root="$(config_root_of "$CONFIG_SRC")"
  if [[ "$root" != "$DATA_DIR" ]]; then
    if [[ -e "$CONFIG_PATH" ]]; then
      warn "$CONFIG_SRC sets storage.local.root to '${root:-<unset>}', not ${DATA_DIR}; it is ignored because ${CONFIG_PATH} already exists"
    else
      die "$CONFIG_SRC must set storage.local.root to ${DATA_DIR} (found '${root:-<unset>}'); the systemd unit only allows writes there"
    fi
  fi
elif [[ ! -e "$CONFIG_PATH" ]]; then
  die "first install needs --config PATH (start from config.example.yaml; set storage.local.root: ${DATA_DIR})"
fi
if [[ -e "$CONFIG_PATH" ]]; then
  existing_root="$(config_root_of "$CONFIG_PATH")"
  [[ "$existing_root" == "$DATA_DIR" ]] || warn "${CONFIG_PATH} sets storage.local.root to '${existing_root:-<unset>}'; the service can only write to ${DATA_DIR}"
fi

# ------------------------------------------------------------------- account
if ! getent passwd "$SERVICE_USER" >/dev/null; then
  nologin="$(command -v nologin || true)"
  [[ -n "$nologin" ]] || nologin=/usr/sbin/nologin
  log "creating system account ${SERVICE_USER}"
  if command -v useradd >/dev/null; then
    useradd --system --user-group --no-create-home --home-dir "$DATA_DIR" --shell "$nologin" "$SERVICE_USER"
  elif command -v adduser >/dev/null; then
    addgroup -S "$SERVICE_USER" 2>/dev/null || true
    adduser -S -D -H -h "$DATA_DIR" -s "$nologin" -G "$SERVICE_USER" "$SERVICE_USER"
  else
    die "neither useradd nor adduser found"
  fi
fi

# --------------------------------------------------------------- directories
install -d -m 0750 -o root -g "$SERVICE_USER" "$ETC_DIR"
install -d -m 0700 -o "$SERVICE_USER" -g "$SERVICE_USER" "$DATA_DIR"

if [[ -n "$CONFIG_SRC" && ! -e "$CONFIG_PATH" ]]; then
  install -m 0640 -o root -g "$SERVICE_USER" "$CONFIG_SRC" "$CONFIG_PATH"
  log "installed ${CONFIG_PATH}"
elif [[ -n "$CONFIG_SRC" ]]; then
  log "kept existing ${CONFIG_PATH} (not overwritten)"
fi

if [[ ! -e "$CREDENTIALS_PATH" ]]; then
  {
    echo "# git-keepr secrets. Root-owned, mode 0600. One KEY=value per line, no spaces around '='."
    echo "# Read by systemd (EnvironmentFile); never commit this file."
    echo "# Fine-grained PATs with 'Contents: Read-only' (variable names come from token_env in config.yaml):"
    { grep -oE 'token_env:[[:space:]]*["'\'']?[A-Za-z_][A-Za-z0-9_]*' "$CONFIG_PATH" 2>/dev/null || true; } \
      | sed -E 's/token_env:[[:space:]]*["'\'']?//' | sort -u | sed -E 's/^(.+)$/#\1=/'
    echo "# S3 / R2 / MinIO credentials (only when storage.s3.enabled is true):"
    echo "#AWS_ACCESS_KEY_ID="
    echo "#AWS_SECRET_ACCESS_KEY="
    echo "#AWS_SESSION_TOKEN="
  } >"${TMP_DIR}/credentials.env"
  install -m 0600 -o root -g root "${TMP_DIR}/credentials.env" "$CREDENTIALS_PATH"
  log "created ${CREDENTIALS_PATH} (mode 0600) - add your tokens"
else
  chmod 0600 "$CREDENTIALS_PATH"; chown root:root "$CREDENTIALS_PATH"
  log "kept existing ${CREDENTIALS_PATH}"
fi

# ----------------------------------------------- atomic binary + unit install
install -d -m 0755 "$(dirname "$BIN_PATH")" "$(dirname "$UNIT_PATH")"
install -m 0755 -o root -g root "$BINARY" "${BIN_PATH}.new.$$"
mv -f "${BIN_PATH}.new.$$" "$BIN_PATH"
install -m 0644 -o root -g root "$UNIT_SRC" "${UNIT_PATH}.new.$$"
mv -f "${UNIT_PATH}.new.$$" "$UNIT_PATH"
log "installed ${BIN_PATH} and ${UNIT_PATH}"

systemctl_do daemon-reload
systemctl_do enable git-keepr

# ----------------------------------------------------- validate, then start
# Load credentials.env without executing it and without echoing any value.
load_credentials() {
  local line key val
  while IFS= read -r line || [[ -n "$line" ]]; do
    line="${line#"${line%%[![:space:]]*}"}"
    [[ -z "$line" || "$line" == \#* || "$line" == \;* || "$line" != *=* ]] && continue
    key="${line%%=*}"; val="${line#*=}"
    [[ "$key" =~ ^[A-Za-z_][A-Za-z0-9_]*$ ]] || continue
    if [[ "$val" =~ ^\"(.*)\"$ || "$val" =~ ^\'(.*)\'$ ]]; then val="${BASH_REMATCH[1]}"; fi
    export "$key=$val"
  done <"$1"
}

validation_output=""
validate_ok=0
if (load_credentials "$CREDENTIALS_PATH"; "$BIN_PATH" config validate --config "$CONFIG_PATH") >"${TMP_DIR}/validate.out" 2>&1; then
  validate_ok=1
fi
validation_output="$(cat "${TMP_DIR}/validate.out")"

# The service account must be able to write the data directory.
if command -v runuser >/dev/null && ! runuser -u "$SERVICE_USER" -- test -w "$DATA_DIR"; then
  validate_ok=0
  validation_output+=$'\n'"error: ${SERVICE_USER} cannot write to ${DATA_DIR}"
fi

print_next_steps() {
  cat <<EOF

git-keepr is installed but NOT started. Next steps:
  1. Edit ${CONFIG_PATH} (repositories, credentials, storage).
  2. Put your tokens in ${CREDENTIALS_PATH} (keep it root:root, mode 0600).
  3. Check the result (values are never printed):
       sudo bash -c 'set -a; . ${CREDENTIALS_PATH}; set +a; ${BIN_PATH} config validate --config ${CONFIG_PATH}'
  4. Start it:
       sudo systemctl enable --now git-keepr
       sudo journalctl -u git-keepr -f
EOF
}

if [[ "$validate_ok" -eq 1 ]]; then
  log "configuration validated"
  if [[ "$NO_START" -eq 1 ]]; then
    log "--no-start given: leaving the service stopped; start it with: sudo systemctl enable --now git-keepr"
  else
    systemctl_do restart git-keepr
    [[ "$SKIP_SYSTEMD" == "1" ]] || log "git-keepr is running: sudo systemctl status git-keepr   |   sudo journalctl -u git-keepr -f"
  fi
else
  warn "configuration is not ready, so the service was not started:"
  printf '%s\n' "$validation_output" | sed 's/^/    /' >&2
  print_next_steps
fi
