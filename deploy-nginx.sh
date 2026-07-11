#!/usr/bin/env bash
#
# deploy-nginx.sh — Safe, idempotent nginx SITE-CONFIG deployment for the
# LLM API Gateway
# ============================================================================
#
# WHAT THIS DOES
#   1. Verifies the local site snippet (deploy/nginx.conf) exists.
#   2. Runs an ADVISORY local `nginx -t -c deploy/nginx.conf` (skipped if nginx
#      is absent locally; never blocking — a site snippet has no http{} block
#      and references server TLS certs that only exist on the real server).
#   3. Uploads the snippet to the server under a temporary name
#      (${NGINX_SITE_PATH}.new) so the running config is never clobbered.
#   4. Over SSH: backs up the current site snippet, stages the new one, and
#      runs `nginx -t` against the REAL server config (the safety gate). Only
#      if `nginx -t` passes is the new snippet committed and nginx reloaded.
#      If `nginx -t` fails, the previous good config is restored (via mv,
#      never rm) and the script exits non-zero WITHOUT reloading.
#
# SECURITY / SAFETY RULES (do not violate — kept consistent with deploy.sh)
#   * NO secrets are ever written, echoed, or logged by this script.
#     config.yaml is LOCAL-ONLY and gitignored. This script NEVER uploads,
#     reads, or prints it. It only ever touches deploy/nginx.conf.
#   * Rollback uses `cp` / `mv` ONLY. This script NEVER uses `rm` on any
#     server-side file. A failed deploy leaves the previous good config in
#     place and the running nginx untouched.
#   * All remote commands are passed as DATA (env vars + single-quoted heredoc),
#     never via local string interpolation, to avoid command injection.
#     Every value that originates from user input / the environment is passed
#     through `validate_value` to reject shell metacharacters.
#   * No hardcoded credentials. SSH keys / agents are used via BatchMode.
#   * Re-running is idempotent: backups are overwrite-style, temp files are
#     reused, and a valid config is only reloaded (cheap, no-op if unchanged).
#
# REQUIRED ENVIRONMENT VARIABLES (override via env or positional arg)
#   DEPLOY_HOST        Target server, "user@host" or "host".      [REQUIRED]
#                      If "host" only, the local $USER is used as SSH user.
#
# OPTIONAL ENVIRONMENT VARIABLES
#   NGINX_SITE_PATH    Remote site-snippet path.                  [default:
#                        /etc/nginx/sites-available/llm-gateway]
#   DEPLOY_ROOT        Alias for NGINX_SITE_PATH (same default).  [optional]
#   SSH_PORT           SSH port.                                 [default: 22]
#   SSH_OPTS           Extra ssh/scp options (e.g. "-o ProxyCommand=...").
#                                                                 [default: ""]
#   LOCAL_NGINX_CONF   Local snippet to deploy.                  [default:
#                        <script-dir>/deploy/nginx.conf]
#
# USAGE
#   ./deploy-nginx.sh                                 # uses env vars
#   ./deploy-nginx.sh user@host                       # host override
#   DEPLOY_HOST=user@host ./deploy-nginx.sh
#   NGINX_SITE_PATH=/etc/nginx/conf.d/llm-gateway.conf ./deploy-nginx.sh user@host
#   ./deploy-nginx.sh -h                              # show this help
#
# Run this script from anywhere; it resolves deploy/nginx.conf relative to
# its own location. This script does NOT push to any git branch and does NOT
# touch the server unless you actually run it.
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
  # Build the forbidden-char class explicitly. The pattern is assembled via
  # printf so the literal double quote can be injected without the nested-quote
  # mistake that previously DROPPED '"' from the class (CVE-style RCE vector
  # via the double-quoted scp/ssh arguments). Forbidden: ; & | < > ' " \ ` $
  local _pat
  _pat="$(printf '[\;&|<>%s"\`\\$]' "'")"
  if printf '%s' "$value" | grep -Eq "$_pat"; then
    echo "ERROR: '$name' contains unsafe shell metacharacters: $value" >&2
    exit 1
  fi
}

# ---------------------------------------------------------------------------
# Configuration (env-overridable, with sensible defaults)
# ---------------------------------------------------------------------------
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
LOCAL_NGINX_CONF="${LOCAL_NGINX_CONF:-${SCRIPT_DIR}/deploy/nginx.conf}"
DEPLOY_HOST="${DEPLOY_HOST:-}"
# DEPLOY_ROOT is accepted as an alias for NGINX_SITE_PATH (kept for parity with
# deploy.sh's parameter style). NGINX_SITE_PATH wins if both are set.
NGINX_SITE_PATH="${NGINX_SITE_PATH:-${DEPLOY_ROOT:-/etc/nginx/sites-available/llm-gateway}}"
SSH_PORT="${SSH_PORT:-22}"
SSH_OPTS="${SSH_OPTS:-}"

# ---------------------------------------------------------------------------
# Positional argument overrides: only <host> is accepted (matches the
# "DEPLOY_HOST required" contract). Extra args are an error.
# ---------------------------------------------------------------------------
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
      if [ -z "$DEPLOY_HOST" ]; then
        DEPLOY_HOST="$1"
      else
        echo "ERROR: too many positional arguments (only <host> is allowed)." >&2
        usage >&2
        exit 1
      fi
      ;;
  esac
  shift
done

# ---------------------------------------------------------------------------
# Split DEPLOY_HOST into user + host (mirrors the "user@host or host" contract)
# ---------------------------------------------------------------------------
if [ -z "$DEPLOY_HOST" ]; then
  echo "ERROR: DEPLOY_HOST is required (set env var or pass as first argument)." >&2
  usage >&2
  exit 1
fi

REMOTE_USER=""
REMOTE_HOST=""
if printf '%s' "$DEPLOY_HOST" | grep -q '@'; then
  REMOTE_USER="${DEPLOY_HOST%%@*}"
  REMOTE_HOST="${DEPLOY_HOST#*@}"
else
  REMOTE_USER="${USER:-$(id -un)}"
  REMOTE_HOST="$DEPLOY_HOST"
fi

if [ -z "$REMOTE_HOST" ]; then
  echo "ERROR: could not parse a host from DEPLOY_HOST='$DEPLOY_HOST'." >&2
  exit 1
fi

# ---------------------------------------------------------------------------
# Validate inputs (reject shell metacharacters; check port is numeric)
# ---------------------------------------------------------------------------
validate_value "DEPLOY_HOST"      "$DEPLOY_HOST"
validate_value "REMOTE_USER"      "$REMOTE_USER"
validate_value "REMOTE_HOST"      "$REMOTE_HOST"
validate_value "NGINX_SITE_PATH"  "$NGINX_SITE_PATH"
validate_value "SSH_OPTS"         "$SSH_OPTS"
# LOCAL_NGINX_CONF is env-overridable and used as the scp SOURCE argument;
# validate it too so a crafted path cannot break the (double-quoted) scp call.
validate_value "LOCAL_NGINX_CONF"  "$LOCAL_NGINX_CONF"

if ! [ "$SSH_PORT" -eq "$SSH_PORT" ] 2>/dev/null || [ "$SSH_PORT" -le 0 ]; then
  echo "ERROR: SSH_PORT must be a positive integer, got: $SSH_PORT" >&2
  exit 1
fi

# Build a reusable array of ssh/scp base options. Extra options from SSH_OPTS
# are split on whitespace (intentional) but were metacharacter-validated above.
# NOTE: we use '-o Port=' (not '-p' / '-P') because the SAME option array is
# passed to both `ssh` (port = -p) and `scp` (port = -P). '-o Port=' is the
# uniform, correct form for both, avoiding a classic -p/-P mix-up bug.
ssh_opts_array=(-o "Port=$SSH_PORT" -o StrictHostKeyChecking=accept-new -o BatchMode=yes)
if [ -n "$SSH_OPTS" ]; then
  read -ra _extra_opts <<< "$SSH_OPTS"
  ssh_opts_array+=("${_extra_opts[@]}")
fi

REMOTE="${REMOTE_USER}@${REMOTE_HOST}"

# ---------------------------------------------------------------------------
# Step 1/4 — Verify the local site snippet exists (abort if missing)
# ---------------------------------------------------------------------------
echo "[1/4] Verifying local site snippet: $LOCAL_NGINX_CONF"
if [ ! -f "$LOCAL_NGINX_CONF" ]; then
  echo "ERROR: local config not found: $LOCAL_NGINX_CONF" >&2
  echo "       (This script ONLY deploys deploy/nginx.conf. It never touches config.yaml.)" >&2
  exit 1
fi
echo "      Found local site snippet."

# ---------------------------------------------------------------------------
# Step 2/4 — Advisory local syntax check (never blocking)
# ---------------------------------------------------------------------------
echo "[2/4] Local pre-check (advisory)..."
if command -v nginx >/dev/null 2>&1; then
  if nginx -t -c "$LOCAL_NGINX_CONF" >/dev/null 2>&1; then
    echo "      Local 'nginx -t -c' passed."
  else
    # Expected: deploy/nginx.conf is a SITE SNIPPET (only server{} blocks, no
    # http{} block, and it references server TLS certs absent on this machine).
    # The authoritative validation is the REMOTE 'nginx -t' gate below.
    echo "      WARN: local 'nginx -t -c $LOCAL_NGINX_CONF' did not pass." >&2
    echo "            This is EXPECTED for a site snippet and is NOT blocking." >&2
    echo "            The authoritative check is the remote 'nginx -t' gate." >&2
  fi
else
  echo "      WARN: nginx not installed locally; skipping local pre-check."
fi

# ---------------------------------------------------------------------------
# Step 3/4 — Upload the new snippet under a temporary name (.new)
#             Never overwrites the live config in place.
# ---------------------------------------------------------------------------
echo "[3/4] Uploading $LOCAL_NGINX_CONF -> ${REMOTE}:${NGINX_SITE_PATH}.new"
scp "${ssh_opts_array[@]}" \
    "$LOCAL_NGINX_CONF" \
    "${REMOTE}:${NGINX_SITE_PATH}.new"

# ---------------------------------------------------------------------------
# Step 4/4 — Remote: backup, stage, SAFETY GATE (nginx -t), reload
# ---------------------------------------------------------------------------
echo "[4/4] Backing up, validating (nginx -t), and reloading nginx on ${REMOTE}..."
# NOTE: NGINX_SITE_PATH is passed as an environment value to the remote
# `bash -s`, and the script body is a single-quoted heredoc (no local
# expansion). This eliminates any command-injection surface from the
# configured values. Sudo usage is decided ON THE REMOTE based on the real
# uid, so we never assume a fixed admin prefix.
ssh "${ssh_opts_array[@]}" \
    "$REMOTE" \
    NGINX_SITE_PATH="$NGINX_SITE_PATH" \
    'bash -s' <<'REMOTE'
  set -euo pipefail

  SITE="$NGINX_SITE_PATH"

  # Decide whether admin commands need sudo (detected remotely — robust
  # whether the SSH user is root or an unprivileged sudoer).
  SUDO=""
  if [ "$(id -u)" -ne 0 ]; then
    SUDO="sudo"
  fi

  # 0) The target site file (or its parent directory) must already exist.
  if [ ! -f "$SITE" ] && [ ! -d "$(dirname "$SITE")" ]; then
    echo "ERROR: site path '$SITE' (or its parent dir) does not exist on the server." >&2
    exit 1
  fi

  # 1) Back up the CURRENT live snippet first (overwrite-style, idempotent).
  #    IRON RULE: never 'rm' server config; rollback always uses cp/mv.
  if [ -f "$SITE" ]; then
    cp -f "$SITE" "$SITE.bak"
    echo "      Backed up current site config -> $SITE.bak"
  else
    echo "      WARN: no existing '$SITE'; this looks like a first deploy."
  fi

  # 2) Stage the uploaded new snippet. We COPY (not move) so the uploaded
  #    .new copy is preserved for inspection if validation fails.
  if [ ! -f "$SITE.new" ]; then
    echo "ERROR: uploaded '$SITE.new' not found on the server." >&2
    exit 1
  fi
  cp -f "$SITE.new" "$SITE"

  # 3) SAFETY GATE: validate the WHOLE nginx config (this includes the new
  #    site snippet plus the server's global http{} and its limit_req_zone
  #    definitions). A failing 'nginx -t' means the new snippet is rejected
  #    and the previous good config is restored — nginx is NEVER reloaded
  #    with a broken configuration.
  if $SUDO nginx -t; then
    # 4) Config is valid -> reload nginx to pick up the new snippet.
    if $SUDO systemctl reload nginx; then
      echo "      nginx reloaded successfully with the new site config."
    else
      echo "WARNING: 'nginx -t' passed but 'systemctl reload nginx' failed." >&2
      echo "         The new (valid) config is on disk; nginx may still serve" >&2
      echo "         the old in-memory config. Retry manually on the server:" >&2
      echo "           sudo systemctl reload nginx" >&2
      exit 1
    fi
  else
    # New config invalid -> restore previous good config (mv, never rm).
    # The uploaded .new is preserved for inspection.
    if [ -f "$SITE.bak" ]; then
      mv -f "$SITE.bak" "$SITE"
      echo "      Restored previous good config from $SITE.bak."
    else
      # First deploy with no backup: quarantine the rejected snippet (mv, never rm).
      mv -f "$SITE" "$SITE.rejected" 2>/dev/null || true
      echo "      No previous backup; quarantined rejected snippet to $SITE.rejected."
    fi
    echo "ERROR: 'nginx -t' rejected the new config. nginx was NOT reloaded." >&2
    echo "       The running nginx is unaffected (it was never reloaded)." >&2
    echo "       Inspect the rejected snippet at: $SITE.new" >&2
    echo "       To re-apply the last known-good config and reload, run on the server:" >&2
    echo "         cp $SITE.bak $SITE && sudo systemctl reload nginx" >&2
    exit 1
  fi

  echo "      Remote nginx deployment steps complete."
REMOTE

# ---------------------------------------------------------------------------
# Success summary
# ---------------------------------------------------------------------------
echo "-------------------------------------------------------------------"
echo "SUCCESS: Deployed nginx site config."
echo "  Local source : $LOCAL_NGINX_CONF"
echo "  Remote host  : $REMOTE"
echo "  Remote path  : $NGINX_SITE_PATH"
echo "  Backup kept  : $NGINX_SITE_PATH.bak"
echo "  Uploaded copy: $NGINX_SITE_PATH.new (kept for inspection; harmless)"
echo "  nginx        : reloaded"
echo "-------------------------------------------------------------------"
