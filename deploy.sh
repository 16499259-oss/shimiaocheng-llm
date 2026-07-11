#!/usr/bin/env bash
#
# deploy.sh — One-shot, parameterized deployment script for the LLM API Gateway
# ============================================================================
#
# WHAT THIS DOES
#   1. Builds a zero-CGO Linux/amd64 static binary locally via `make build-linux`.
#   2. Uploads the new binary to the server under a temporary name
#      (${DEPLOY_ROOT}/llm_api_gateway.new) so the running binary is never
#      clobbered mid-flight.
#   3. Over SSH: backs up the current binary, atomically swaps in the new one,
#      restarts the systemd service, and reloads nginx.
#   4. Optionally performs a post-deploy health check (advisory only; never
#      auto-rolls-back — rollback commands are printed for the operator).
#
# SECURITY / SAFETY RULES (do not violate)
#   * NO secrets are ever written, echoed, or logged by this script.
#     The upstream API key (ZHIPU_API_KEY) is injected ONLY via the server's
#     systemd unit `Environment=` directive and read by the process from the
#     environment at runtime. config.yaml MUST NOT contain the plaintext key,
#     and this script never touches or prints it.
#   * config.yaml is LOCAL-ONLY and gitignored. This script NEVER uploads or
#     overwrites it. Re-running is idempotent and will not break server config.
#   * No `rm -rf` on binaries. Rollback uses `mv`, never deletion.
#   * All remote commands are passed as data (env vars + heredoc), not string
#     interpolation, to avoid command injection. Input values are validated for
#     shell metacharacters before use.
#
# REQUIRED ENVIRONMENT VARIABLES (override via env or positional args)
#   DEPLOY_HOST        Target server hostname or IP.            [REQUIRED]
#   DEPLOY_USER        SSH user on the server.                  [default: root]
#   DEPLOY_ROOT        Remote deployment directory.             [default: /opt/llm-gateway]
#   DEPLOY_SSH_PORT    SSH port.                               [default: 22]
#
# OPTIONAL ENVIRONMENT VARIABLES
#   SERVICE_NAME            systemd service to restart.         [default: llm-gateway]
#   BINARY                  Binary file name.                   [default: llm_api_gateway]
#   DEPLOY_USE_SUDO        Prefix remote admin cmds with sudo. [default: 0] (set 1 if DEPLOY_USER is non-root)
#   HEALTH_CHECK_URL       URL probed after deploy.            [default: https://${DEPLOY_HOST}/v1/models]
#   DEPLOY_SKIP_HEALTHCHECK Skip the post-deploy health check. [default: 0]
#
# USAGE
#   ./deploy.sh                                    # uses env vars
#   ./deploy.sh <host>                             # host override
#   ./deploy.sh <host> <user> <root> <port>        # positional overrides
#   DEPLOY_HOST=example.com DEPLOY_USER=deploy ./deploy.sh
#   ./deploy.sh -h                                 # show this help
#
# Run this script from the PROJECT ROOT (where the Makefile and main.go live).
# This script does NOT push to any remote git branch and does NOT connect to a
# server unless you actually run it.
#
set -euo pipefail

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------
usage() {
  grep '^#' "$0" | sed 's/^# \{0,1\}//'
}

# Reject values containing shell metacharacters that could break the safe
# env-var-based remote execution model.
validate_value() {
  local name="$1" value="$2"
  if printf '%s' "$value" | grep -Eq "$(printf '[\;&|<>%s"\`\\$]' "'")"; then
    echo "ERROR: '$name' contains unsafe shell metacharacters: $value" >&2
    exit 1
  fi
}

# ---------------------------------------------------------------------------
# Configuration (env-overridable, with sensible defaults)
# ---------------------------------------------------------------------------
DEPLOY_HOST="${DEPLOY_HOST:-}"
DEPLOY_USER="${DEPLOY_USER:-root}"
DEPLOY_ROOT="${DEPLOY_ROOT:-/opt/llm-gateway}"
DEPLOY_SSH_PORT="${DEPLOY_SSH_PORT:-22}"
SERVICE_NAME="${SERVICE_NAME:-llm-gateway}"
BINARY="${BINARY:-llm_api_gateway}"
DEPLOY_USE_SUDO="${DEPLOY_USE_SUDO:-0}"
HEALTH_CHECK_URL="${HEALTH_CHECK_URL:-https://${DEPLOY_HOST}/v1/models}"
DEPLOY_SKIP_HEALTHCHECK="${DEPLOY_SKIP_HEALTHCHECK:-0}"

# ---------------------------------------------------------------------------
# Positional argument overrides: <host> [user] [root] [port]
# ---------------------------------------------------------------------------
_HOST_SET=0; _USER_SET=0; _ROOT_SET=0; _PORT_SET=0
while [ $# -gt 0 ]; do
  case "$1" in
    -h|--help)
      usage
      exit 0
      ;;
    -*)
      echo "ERROR: unknown option '$1'" >&2
      usage >&2
      exit 1
      ;;
    *)
      if   [ "$_HOST_SET" -eq 0 ]; then DEPLOY_HOST="$1";   _HOST_SET=1
      elif [ "$_USER_SET" -eq 0 ]; then DEPLOY_USER="$1";   _USER_SET=1
      elif [ "$_ROOT_SET" -eq 0 ]; then DEPLOY_ROOT="$1";   _ROOT_SET=1
      elif [ "$_PORT_SET" -eq 0 ]; then DEPLOY_SSH_PORT="$1"; _PORT_SET=1
      else
        echo "ERROR: too many positional arguments" >&2
        usage >&2
        exit 1
      fi
      ;;
  esac
  shift
done

# ---------------------------------------------------------------------------
# Validate inputs
# ---------------------------------------------------------------------------
if [ -z "$DEPLOY_HOST" ]; then
  echo "ERROR: DEPLOY_HOST is required (set env var or pass as first argument)." >&2
  usage >&2
  exit 1
fi

if ! [ "$DEPLOY_SSH_PORT" -eq "$DEPLOY_SSH_PORT" ] 2>/dev/null || [ "$DEPLOY_SSH_PORT" -le 0 ]; then
  echo "ERROR: DEPLOY_SSH_PORT must be a positive integer, got: $DEPLOY_SSH_PORT" >&2
  exit 1
fi

for _v in DEPLOY_HOST DEPLOY_USER DEPLOY_ROOT SERVICE_NAME BINARY; do
  validate_value "$_v" "${!_v}"
done

if [ "$DEPLOY_USE_SUDO" != "0" ] && [ "$DEPLOY_USE_SUDO" != "1" ]; then
  echo "ERROR: DEPLOY_USE_SUDO must be 0 or 1, got: $DEPLOY_USE_SUDO" >&2
  exit 1
fi

# Must be run from the project root where the Makefile lives.
if [ ! -f Makefile ]; then
  echo "ERROR: Makefile not found. Run this script from the project root." >&2
  exit 1
fi

SUDO_PREFIX=""
if [ "$DEPLOY_USE_SUDO" = "1" ]; then
  SUDO_PREFIX="sudo "
fi

# ---------------------------------------------------------------------------
# Step 1/4 — Local build (abort on failure)
# ---------------------------------------------------------------------------
echo "[1/4] Building zero-CGO Linux binary via 'make build-linux'..."
make build-linux
if [ ! -x "./${BINARY}" ]; then
  echo "ERROR: build produced no executable './${BINARY}'. Aborting." >&2
  exit 1
fi
echo "      Built ./${BINARY} successfully."

# ---------------------------------------------------------------------------
# Step 2/4 — Upload new binary under a temporary name (never overwrite live)
# ---------------------------------------------------------------------------
echo "[2/4] Uploading ./${BINARY} -> ${DEPLOY_USER}@${DEPLOY_HOST}:${DEPLOY_ROOT}/${BINARY}.new"
scp -P "${DEPLOY_SSH_PORT}" \
    -o StrictHostKeyChecking=accept-new \
    -o BatchMode=yes \
    "./${BINARY}" \
    "${DEPLOY_USER}@${DEPLOY_HOST}:${DEPLOY_ROOT}/${BINARY}.new"

# ---------------------------------------------------------------------------
# Step 3/4 — Remote: backup, atomic swap, restart service, reload nginx
# ---------------------------------------------------------------------------
echo "[3/4] Swapping binary, restarting '${SERVICE_NAME}', reloading nginx on ${DEPLOY_HOST}..."
# NOTE: variables are passed as environment data to the remote `bash -s`, and
# the script body is a single-quoted heredoc (no local expansion). This avoids
# any command-injection surface from the configured values.
ssh -p "${DEPLOY_SSH_PORT}" \
    -o StrictHostKeyChecking=accept-new \
    -o BatchMode=yes \
    "${DEPLOY_USER}@${DEPLOY_HOST}" \
    DEPLOY_ROOT="${DEPLOY_ROOT}" \
    SERVICE_NAME="${SERVICE_NAME}" \
    BINARY="${BINARY}" \
    SUDO_PREFIX="${SUDO_PREFIX}" \
    'bash -s' <<'REMOTE'
  set -euo pipefail

  cd "$DEPLOY_ROOT"

  # 0) Sanity: the deployment directory must already exist on the server.
  if [ ! -d "$DEPLOY_ROOT" ]; then
    echo "ERROR: deployment directory '$DEPLOY_ROOT' does not exist on the server." >&2
    exit 1
  fi

  # 1) Back up the currently-running binary first (keep only the latest copy).
  if [ -f "$BINARY" ]; then
    cp -f "$BINARY" "$BINARY.bak"
    echo "      Backed up current binary -> $BINARY.bak"
  else
    echo "      WARN: no existing '$BINARY' found; this looks like a first deploy."
  fi

  # 2) Atomic replace: the uploaded temp file becomes the live binary.
  if [ ! -f "$BINARY.new" ]; then
    echo "ERROR: uploaded '$BINARY.new' not found in '$DEPLOY_ROOT'." >&2
    exit 1
  fi
  mv -f "$BINARY.new" "$BINARY"
  chmod +x "$BINARY"

  # Best-effort: restore ownership to the runtime user when we are root.
  if [ "$(id -u)" -eq 0 ]; then
    chown -f llm:llm "$BINARY" "$BINARY.bak" 2>/dev/null || true
  fi

  # 3) Restart the systemd service (backup is guaranteed to exist by now).
  ${SUDO_PREFIX}systemctl restart "$SERVICE_NAME"

  # 4) Validate nginx config, then reload (non-fatal if nginx is absent here).
  if command -v nginx >/dev/null 2>&1; then
    ${SUDO_PREFIX}nginx -t && ${SUDO_PREFIX}systemctl reload nginx
  else
    echo "      WARN: nginx not found on server; skipping nginx reload."
  fi

  echo "      Remote deployment steps complete."
REMOTE

# ---------------------------------------------------------------------------
# Step 4/4 — Advisory health check (never auto-rolls-back)
# ---------------------------------------------------------------------------
echo "[4/4] Post-deploy health check..."
if [ "$DEPLOY_SKIP_HEALTHCHECK" = "1" ]; then
  echo "      Health check skipped (DEPLOY_SKIP_HEALTHCHECK=1)."
else
  if curl -fsS --max-time 15 "$HEALTH_CHECK_URL" >/dev/null 2>&1; then
    echo "      Health check PASSED: $HEALTH_CHECK_URL"
  else
    echo "      WARNING: health check FAILED for: $HEALTH_CHECK_URL" >&2
    echo "      The new binary is live but did not pass the probe." >&2
    echo "      To roll back manually, run on the server (or via SSH):" >&2
    echo "        ssh -p ${DEPLOY_SSH_PORT} ${DEPLOY_USER}@${DEPLOY_HOST} \\" >&2
    echo "          \"cd ${DEPLOY_ROOT} && mv -f ${BINARY}.bak ${BINARY} && ${SUDO_PREFIX}systemctl restart ${SERVICE_NAME}\"" >&2
    echo "      (Rollback uses 'mv' to restore the previous binary; nothing is deleted.)" >&2
  fi
fi

echo "Deployment finished. Previous binary preserved at ${DEPLOY_ROOT}/${BINARY}.bak on the server."
