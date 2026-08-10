#!/bin/sh
# radar-node installer -- POSIX sh, no bashisms, safe to pipe from curl:
#
#   curl -fsSL https://radar.mehrnet.com/install/node.sh \
#     | sh -s -- --node_id=node_xxx --api_key=xxxxx
#
# Kept byte-identical in three places -- this file, ../install.sh in
# this same repo, and radar-node's own install.sh -- so a node whose
# binary has any of those three URLs baked in as its own self-update
# target always finds a real, working script there. Edit this copy,
# then copy it verbatim over the other two; there's no meaningful
# difference between them otherwise.
#
# Served from this same origin, which is also where every binary this
# script downloads (its own release, and the optional xray/wireguard-
# go/openvpn engine modules) is mirrored: see releases/ at this repo's
# root and releases-sync.sh, which populates it from mehrnet/radar-
# node's and mehrnet/static-builds' GitHub releases. Nothing at
# runtime here ever talks to GitHub at all -- a node whose network
# policy allowlists radar.mehrnet.com (it already needs to reach this
# origin's API) but not GitHub can fully install/update regardless.
# Installs the binary and sets up radar-node as a persistent service
# (systemd on Linux, launchd on macOS) so the copy-pasted command
# above is the only step a user ever has to take.
set -e

BIN_NAME="radar-node"
RELEASES_BASE="https://radar.mehrnet.com/releases"
MODULES_BASE="https://radar.mehrnet.com/install/modules"
API_URL_DEFAULT="https://radar-api.mehrnet.com"

NODE_ID=""
API_KEY=""
API_URL="$API_URL_DEFAULT"
PROXY=""
UNINSTALL=0

# Optional bundled engine modules (see install/modules/ and
# https://github.com/mehrnet/static-builds) -- off unless explicitly
# requested (--install-module=<name>/--remove-module=<name>, space-
# separated lists here), or already installed (see the "still opted
# in" check further down, once TOOLS_DIR is known). One flag pair for
# any of the fixed set in module_dispatch below, mirroring radar-
# node's own `install-module <name>`/`remove-module <name>` subcommand
# naming (see cmd/radar-node/main.go) -- replaces the old one-flag-
# per-module --install-xray/--remove-wireguard/etc, which no longer
# parse.
INSTALL_MODULES=""
REMOVE_MODULES=""

usage() {
  cat <<'EOF'
Usage: install.sh --node_id=<id> --api_key=<secret> [options]

Required (shown once when you register a node in the radar UI):
  --node_id=ID       the node id from registration
  --api_key=SECRET   the node secret from registration

Options:
  --api_url=URL      radar-api base URL (default: https://radar-api.mehrnet.com)
  --proxy=URL        proxy for both this installer's downloads and the running
                     agent's radar-api traffic (http://, https://, socks5://, socks5h://)
  --uninstall        stop and fully remove radar-node from this machine (no
                      other flag is needed -- this ignores --node_id/--api_key)
  -h, --help         show this help

Optional bundled engine modules (each fetches a statically-built binary
from mehrnet/static-builds and drops the matching module + wrapper
script into modules.d -- see that repo's README for what's actually
installed). Repeatable -- e.g. --install-module=xray --install-module=
wireguard installs both in one run:
  --install-module=xray        xray-core, for a generic proxy-config probe
  --install-module=wireguard   a WireGuard tunnel probe (needs CAP_NET_ADMIN
                                -- applied via setcap on a root install)
  --install-module=openvpn     an OpenVPN tunnel probe (linux only)
  --remove-module=<name>       undo the corresponding --install-module=<name>
                                above

--node_id/--api_key/--api_url/--proxy are only required the first time --
re-running this same command on a machine that already has radar-node
installed (e.g. to pick up a new release) reuses whatever's already
configured there for any of these you don't pass again, so a bare
`| sh -s` upgrades an existing install with no arguments at all -- this
includes any --install-module=<name> engine module already opted into:
it's kept up to date on every re-run without needing to repeat the
flag, unless the matching --remove-module=<name> is passed.
EOF
}

log() { printf '==> %s\n' "$*" >&2; }
warn() { printf 'warning: %s\n' "$*" >&2; }
err() { printf 'error: %s\n' "$*" >&2; exit 1; }

for arg in "$@"; do
  case "$arg" in
    --node_id=*) NODE_ID="${arg#*=}" ;;
    --api_key=*) API_KEY="${arg#*=}" ;;
    --api_url=*) API_URL="${arg#*=}" ;;
    --proxy=*) PROXY="${arg#*=}" ;;
    --uninstall) UNINSTALL=1 ;;
    --install-module=*)
      m="${arg#*=}"
      case "$m" in xray|wireguard|openvpn) ;; *) err "unknown module: $m (supported: xray, wireguard, openvpn)" ;; esac
      INSTALL_MODULES="${INSTALL_MODULES} ${m}"
      ;;
    --remove-module=*)
      m="${arg#*=}"
      case "$m" in xray|wireguard|openvpn) ;; *) err "unknown module: $m (supported: xray, wireguard, openvpn)" ;; esac
      REMOVE_MODULES="${REMOVE_MODULES} ${m}"
      ;;
    -h|--help) usage; exit 0 ;;
    *) err "unknown argument: $arg (see --help)" ;;
  esac
done

# $1 = space-separated list (INSTALL_MODULES or REMOVE_MODULES), $2 =
# module name -- both lists start with a leading space (built via
# "${LIST} name"), so wrapping the needle in spaces too catches an
# exact match at the start/end without a false hit on a name that's
# merely a substring of another (there are none today, but the fixed
# set below could grow one).
module_requested() {
  case " $1 " in *" $2 "*) return 0 ;; *) return 1 ;; esac
}

# ---------------------------------------------------------------------
# Platform detection -> goreleaser's os/arch naming (see .goreleaser.yaml).
# Needed by both --uninstall (to find the right service manager) and a
# real install (which also needs ARCH, resolved further down).
# ---------------------------------------------------------------------
os_raw="$(uname -s)"
case "$os_raw" in
  Linux) OS=linux ;;
  Darwin) OS=darwin ;;
  *) err "unsupported OS: $os_raw -- radar-node ships linux/darwin/windows releases; for windows grab a release asset manually from ${RELEASES_BASE}/radar-node/" ;;
esac

# Root gets a real system service (systemd/launchd) so the node
# survives reboots with zero further action; a non-root install still
# works, just user-scoped. Needed by --uninstall, the "reuse an
# existing install's credentials" check below, and the real install
# further down -- computed once, here, rather than three times.
if [ "$(id -u)" = "0" ]; then
  INSTALL_BIN_DIR="/usr/local/bin"
  MODULES_DIR="/etc/radar-node/modules.d"
  TOOLS_DIR="/etc/radar-node/tools"
  IS_ROOT=1
else
  INSTALL_BIN_DIR="${HOME}/.local/bin"
  MODULES_DIR="${HOME}/.config/radar-node/modules.d"
  TOOLS_DIR="${HOME}/.config/radar-node/tools"
  IS_ROOT=0
fi
label="com.mehrnet.radar-node"

# A bundled engine module already opted into on a previous run is kept
# up to date on every later bare re-run, the same way radar-node's own
# binary is -- its presence on disk *is* the "still opted in" record,
# no separate state file needed. Only skipped if this exact run is the
# one removing it (--remove-* was just passed above).
if [ "$UNINSTALL" != "1" ]; then
  for m in xray wireguard openvpn; do
    if ! module_requested "$INSTALL_MODULES" "$m" && ! module_requested "$REMOVE_MODULES" "$m"; then
      case "$m" in
        xray) [ -f "${TOOLS_DIR}/xray" ] && INSTALL_MODULES="${INSTALL_MODULES} xray" ;;
        wireguard) [ -f "${TOOLS_DIR}/radar-wg" ] && INSTALL_MODULES="${INSTALL_MODULES} wireguard" ;;
        openvpn) [ -f "${TOOLS_DIR}/openvpn" ] && INSTALL_MODULES="${INSTALL_MODULES} openvpn" ;;
      esac
    fi
  done
fi

if [ "$UNINSTALL" = "1" ]; then
  if [ "$OS" = "linux" ] && command -v systemctl >/dev/null 2>&1; then
    if [ "$IS_ROOT" = "1" ]; then
      systemctl stop radar-node >/dev/null 2>&1 || true
      systemctl disable radar-node >/dev/null 2>&1 || true
      rm -f /etc/systemd/system/radar-node.service
      systemctl daemon-reload >/dev/null 2>&1 || true
    else
      systemctl --user stop radar-node >/dev/null 2>&1 || true
      systemctl --user disable radar-node >/dev/null 2>&1 || true
      rm -f "${HOME}/.config/systemd/user/radar-node.service"
      systemctl --user daemon-reload >/dev/null 2>&1 || true
    fi
  elif [ "$OS" = "darwin" ]; then
    if [ "$IS_ROOT" = "1" ]; then
      plist="/Library/LaunchDaemons/${label}.plist"
    else
      plist="${HOME}/Library/LaunchAgents/${label}.plist"
    fi
    [ -f "$plist" ] && launchctl unload "$plist" >/dev/null 2>&1
    rm -f "$plist"
  fi

  rm -f "${INSTALL_BIN_DIR}/${BIN_NAME}"
  rm -rf "$MODULES_DIR" "$TOOLS_DIR"
  log "removed ${INSTALL_BIN_DIR}/${BIN_NAME}, ${MODULES_DIR}, and ${TOOLS_DIR}"
  log "radar-node has been fully uninstalled from this machine."
  exit 0
fi

# ---------------------------------------------------------------------
# Re-running this exact command with no (or partial) arguments -- e.g.
# a bare `| sh -s` to pick up a new release -- reuses whatever's
# already configured in the existing service definition instead of
# forcing every value to be re-supplied just to upgrade. Only kicks in
# when *both* --node_id and --api_key are omitted (a value given for
# one but not the other is ambiguous -- safer to require both explicit
# than guess whether the other belongs to the same node), and only
# when an existing install is actually found; a first-time install has
# nothing to reuse, so both stay required in that case.
# ---------------------------------------------------------------------
if [ "$OS" = "linux" ]; then
  existing_unit="/etc/systemd/system/radar-node.service"
  [ "$IS_ROOT" = "1" ] || existing_unit="${HOME}/.config/systemd/user/radar-node.service"
elif [ "$OS" = "darwin" ]; then
  existing_unit="/Library/LaunchDaemons/${label}.plist"
  [ "$IS_ROOT" = "1" ] || existing_unit="${HOME}/Library/LaunchAgents/${label}.plist"
else
  existing_unit=""
fi

if [ -n "$existing_unit" ] && [ -f "$existing_unit" ]; then
  if [ "$OS" = "linux" ]; then
    existing_api_key="$(sed -n 's/.*--api-key "\([^"]*\)".*/\1/p' "$existing_unit" | head -n1)"
    existing_api_url="$(sed -n 's/.*--api-url "\([^"]*\)".*/\1/p' "$existing_unit" | head -n1)"
    existing_proxy="$(sed -n 's/.*--api-proxy "\([^"]*\)".*/\1/p' "$existing_unit" | head -n1)"
  else
    existing_api_key="$(awk '/<string>--api-key<\/string>/{getline; gsub(/<\/?string>/,""); print; exit}' "$existing_unit")"
    existing_api_url="$(awk '/<string>--api-url<\/string>/{getline; gsub(/<\/?string>/,""); print; exit}' "$existing_unit")"
    existing_proxy="$(awk '/<string>--api-proxy<\/string>/{getline; gsub(/<\/?string>/,""); print; exit}' "$existing_unit")"
  fi

  if [ -z "$NODE_ID" ] && [ -z "$API_KEY" ] && [ -n "$existing_api_key" ]; then
    NODE_ID="${existing_api_key%%:*}"
    API_KEY="${existing_api_key#*:}"
    log "reusing node_id/api_key already configured in ${existing_unit}"
  fi
  [ "$API_URL" = "$API_URL_DEFAULT" ] && [ -n "$existing_api_url" ] && API_URL="$existing_api_url"
  [ -z "$PROXY" ] && [ -n "$existing_proxy" ] && PROXY="$existing_proxy"
fi

[ -n "$NODE_ID" ] || { usage; err "--node_id is required (no existing installation found at ${existing_unit:-<none>} to reuse it from)"; }
[ -n "$API_KEY" ] || { usage; err "--api_key is required (no existing installation found at ${existing_unit:-<none>} to reuse it from)"; }

command -v curl >/dev/null 2>&1 || err "curl is required"
command -v tar >/dev/null 2>&1 || err "tar is required"

# ---------------------------------------------------------------------
# A wildly wrong system clock breaks far more than this installer --
# xray's own Reality handshake carries a timestamp-based anti-replay
# check tight enough that even a one-minute clock skew makes *every*
# Reality-secured proxy check fail outright, with an error that never
# points anywhere near "clock" (observed in production: a node running
# ~49s fast failed 99%+ of its xray checks while otherwise-identical
# sibling nodes with a correct clock succeeded normally on the exact
# same probes). Checked here, once, up front: cheap (one response's
# own Date header, off a request this installer sends to $API_URL
# regardless) and long before that failure mode would otherwise show
# up as a mystery days or weeks later. Never fatal either way -- a
# skewed clock doesn't stop the install, it just gets a loud warning
# (and, where it's safe to, a fix attempt) so it isn't the next
# person's multi-hour debugging session.
# ---------------------------------------------------------------------
check_clock_skew() {
  headers="$(curl -s -D - -o /dev/null --max-time 5 ${PROXY:+--proxy "$PROXY"} "$API_URL/v1/health" 2>/dev/null)"
  server_date="$(printf '%s' "$headers" | tr -d '\r' | awk -F': ' 'tolower($1)=="date"{print $2; exit}')"
  [ -n "$server_date" ] || return 0 # couldn't determine the server's own clock -- not worth guessing further

  # GNU date (-d, most Linux) and BSD/macOS date (-j -f) parse an RFC
  # 7231 Date header differently -- try both, silently, since which
  # one this host has is exactly what OS/ARCH detection above already
  # had to figure out once, but redoing that here isn't worth it for
  # a two-command try.
  server_epoch="$(date -d "$server_date" +%s 2>/dev/null || date -j -f '%a, %d %b %Y %T %Z' "$server_date" +%s 2>/dev/null || true)"
  [ -n "$server_epoch" ] || return 0

  local_epoch="$(date +%s)"
  skew=$((local_epoch - server_epoch))
  [ "$skew" -lt 0 ] && skew=$((0 - skew))
  [ "$skew" -gt 10 ] || return 0

  warn "this machine's clock looks ~${skew}s off from ${API_URL}'s own -- Reality/TLS-secured proxy checks (xray) fail outright on skew this size, usually with no error that points at the clock at all"

  if [ "$IS_ROOT" != "1" ]; then
    warn "re-run this installer as root to have it correct the clock automatically"
    return 0
  fi

  # Set straight from the same HTTPS response that just diagnosed the
  # skew, before even trying real NTP -- deliberately, not just as a
  # fallback. Real NTP (UDP/123, to whatever pool a distro defaults to)
  # is routinely one of the first things blocked in exactly the
  # network environments this node tends to get deployed into --
  # nothing about enabling it (timedatectl/sntp below) fails loudly
  # when that's the case, it just silently never converges, clock
  # still wrong, no error, no warning, the same mystery all over again.
  # This has no such dependency: if $API_URL was reachable enough to
  # get this far at all, this fix path is too, no matter what happens
  # to be blocking NTP specifically -- accurate to roughly the HTTPS
  # round trip (a couple seconds at most), comfortably inside the
  # threshold that actually matters.
  if [ "$OS" = "linux" ]; then
    log "setting the clock directly from ${API_URL}'s own Date header (works even when real NTP is itself blocked)"
    date -s "@${server_epoch}" >/dev/null 2>&1 || date -s "$server_date" >/dev/null 2>&1 || warn "couldn't set the clock directly -- fix it manually"
  elif [ "$OS" = "darwin" ]; then
    log "setting the clock directly from ${API_URL}'s own Date header (works even when real NTP is itself blocked)"
    date -f '%a, %d %b %Y %T %Z' "$server_date" >/dev/null 2>&1 || warn "couldn't set the clock directly -- fix it manually"
  fi

  # Also enable real NTP, best-effort, for ongoing accuracy going
  # forward -- the fix above is a one-shot snapshot; NTP is what keeps
  # the clock from just drifting right back out over the following
  # days or weeks. Silently left unconverged if it can't reach out --
  # that's fine, the one-shot fix above already solved the actual
  # problem, this is purely a nice-to-have on top of it.
  if [ "$OS" = "linux" ] && command -v timedatectl >/dev/null 2>&1; then
    timedatectl set-ntp true >/dev/null 2>&1 || true
  elif [ "$OS" = "darwin" ] && command -v sntp >/dev/null 2>&1; then
    sntp -sS time.apple.com >/dev/null 2>&1 || true
  fi
}
# Subshelled so a strict-mode failure inside -- this function inherits
# the parent script's own `set -e`, and something as ordinary as curl
# timing out or `date -j` not existing on this platform would
# otherwise abort the *entire* install over what's meant to be a best-
# effort check -- can never take the real installation down with it.
( check_clock_skew ) || true

curl_get() {
  # $1 = url, $2 = output path
  if [ -n "$PROXY" ]; then
    curl -fsSL --proxy "$PROXY" "$1" -o "$2"
  else
    curl -fsSL "$1" -o "$2"
  fi
}

# Same as curl_get, but prints the response's Content-Type to stdout --
# see fetch_with_retry's own comment on why that, not the HTTP status,
# is what actually distinguishes a real asset from this origin's own
# SPA fallback page.
curl_get_with_type() {
  # $1 = url, $2 = output path
  if [ -n "$PROXY" ]; then
    curl -fsSL --proxy "$PROXY" -w '%{content_type}' "$1" -o "$2"
  else
    curl -fsSL -w '%{content_type}' "$1" -o "$2"
  fi
}

# ---------------------------------------------------------------------
# ARCH resolution -> goreleaser's naming (OS was already resolved above,
# before the --uninstall branch, since that needs it too).
# ---------------------------------------------------------------------
arch_raw="$(uname -m)"
case "$arch_raw" in
  x86_64|amd64) ARCH=amd64 ;;
  arm64|aarch64) ARCH=arm64 ;;
  *) err "unsupported architecture: $arch_raw" ;;
esac

# ---------------------------------------------------------------------
# Prefer a specific, permanently-immutable version-numbered asset over
# the old "_latest_" one -- once published, a version-numbered file's
# content never changes again, so its checksum sidecar can never
# disagree with it the way "_latest_"'s could (observed in production:
# a self-update mid-propagation on the mirror could see a binary from
# one CDN edge and a checksum from another that had already moved on,
# a genuine mismatch between two individually-valid-but-different
# states, not corruption). The actual current version comes from
# radar-api directly -- a live database read, never itself CDN-cached
# the way a static pointer file under this same mirror would be.
# Best-effort: if radar-api can't be reached at all, this falls back
# to the old "_latest_" URL below, same as every release before this
# one, rather than blocking the update over it entirely.
# ---------------------------------------------------------------------
BASE_URL="${RELEASES_BASE}/radar-node"
NODE_VERSION=""
health_json="$(curl -fsSL ${PROXY:+--proxy "$PROXY"} --max-time 10 "${API_URL}/v1/health" 2>/dev/null || true)"
if [ -n "$health_json" ]; then
  NODE_VERSION="$(printf '%s' "$health_json" | sed -n 's/.*"latest_node_version"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p')"
fi
if [ -n "$NODE_VERSION" ]; then
  ASSET="${BIN_NAME}_${NODE_VERSION}_${OS}_${ARCH}.tar.gz"
else
  log "couldn't resolve a specific version from ${API_URL}/v1/health -- falling back to the mutable _latest_ asset"
  ASSET="${BIN_NAME}_latest_${OS}_${ARCH}.tar.gz"
fi

WORKDIR="$(mktemp -d)"
# On any failure from here on (bad download, a checksum mismatch --
# e.g. from catching the mirror mid-update, a bad archive, ...), make
# a best-effort attempt to bring the *previous* installation's service
# back up before this script exits. The agent that triggered a self-
# update already stopped itself on purpose, to hand off to this exact
# script (see radar-node's reinstall(), and start_systemd's own
# RestartPreventExitStatus= below, which deliberately keeps systemd
# from auto-restarting that specific exit) -- before this trap
# existed, a failure anywhere past this point left that service
# completely down (no heartbeats at all) until a human noticed and
# SSHed in to `systemctl start radar-node` by hand, observed in
# production for tens of minutes on more than one node after a
# checksum mismatch during a mirror update in flight. A first-time
# install has no $existing_unit to fall back to -- nothing to recover,
# so nothing extra happens there, same as today.
cleanup_and_recover() {
  status=$?
  rm -rf "$WORKDIR"
  if [ "$status" -ne 0 ] && [ -n "${existing_unit:-}" ] && [ -f "$existing_unit" ]; then
    log "install failed -- attempting to bring the previous installation back up"
    if [ "$OS" = "linux" ] && command -v systemctl >/dev/null 2>&1; then
      if [ "$IS_ROOT" = "1" ]; then
        systemctl start radar-node >/dev/null 2>&1 || true
      else
        systemctl --user start radar-node >/dev/null 2>&1 || true
      fi
    elif [ "$OS" = "darwin" ] && command -v launchctl >/dev/null 2>&1; then
      launchctl load -w "$existing_unit" >/dev/null 2>&1 || true
    fi
  fi
  exit "$status"
}
trap cleanup_and_recover EXIT

# Retries only the fetch itself (a not-yet-published version-numbered
# asset means "radar-api already knows about this version, but the
# mirror hasn't finished publishing it to this edge yet" -- a real,
# expected, transient state, not an error) -- a *mismatched checksum*
# on a version-numbered asset is never retried, since that file's
# content can never legitimately change once published, so a mismatch
# there means real corruption. Only used for the version-numbered
# path; the "_latest_" fallback below keeps its original single-
# attempt, tolerate-a-missing-sidecar behavior unchanged.
#
# Can't just check the HTTP status: this origin serves a 200 (its own
# SPA's index.html) for *any* unmatched path rather than a real 404,
# so a not-yet-published asset looks identical to a genuinely broken
# URL at the status-code level. The content-type header still tells
# them apart -- a real asset is application/gzip or text/plain (the
# checksum sidecar), the fallback page is always text/html -- so that,
# not the status code, is what actually gates the retry here.
fetch_with_retry() {
  # $1 = url, $2 = output path, $3 = human-readable label for logging
  _attempt=1
  _max_attempts=5
  while :; do
    _ctype="$(curl_get_with_type "$1" "$2")" || _ctype=""
    case "$_ctype" in
      text/html*|"") : ;; # SPA fallback (not yet published), or the request itself failed -- either way, not a real file
      *) return 0 ;;
    esac
    if [ "$_attempt" -ge "$_max_attempts" ]; then
      return 1
    fi
    log "  ${3} not available yet (attempt ${_attempt}/${_max_attempts}) -- retrying in $((_attempt * 2))s..."
    sleep $((_attempt * 2))
    _attempt=$((_attempt + 1))
  done
}

log "downloading ${ASSET}..."
if [ -n "$NODE_VERSION" ]; then
  fetch_with_retry "${BASE_URL}/${ASSET}" "${WORKDIR}/${ASSET}" "${ASSET}" || err "failed to download ${ASSET} after retries"
else
  curl_get "${BASE_URL}/${ASSET}" "${WORKDIR}/${ASSET}"
fi

log "verifying checksum..."
# One sidecar per asset (just the raw sha256 digest, nothing else) --
# see releases-sync.sh -- instead of a shared checksums.txt manifest,
# so there's no line to grep out of a multi-asset file, just the one
# file that already matches the one asset just downloaded.
checksum_fetched=0
if [ -n "$NODE_VERSION" ]; then
  fetch_with_retry "${BASE_URL}/${ASSET}.checksum.txt" "${WORKDIR}/${ASSET}.checksum.txt" "${ASSET}.checksum.txt" && checksum_fetched=1
else
  curl_get "${BASE_URL}/${ASSET}.checksum.txt" "${WORKDIR}/${ASSET}.checksum.txt" 2>/dev/null && checksum_fetched=1
fi
if [ "$checksum_fetched" = "1" ]; then
  expected="$(cat "${WORKDIR}/${ASSET}.checksum.txt")"
  if [ -n "$expected" ]; then
    if command -v sha256sum >/dev/null 2>&1; then
      actual="$(sha256sum "${WORKDIR}/${ASSET}" | awk '{print $1}')"
    elif command -v shasum >/dev/null 2>&1; then
      actual="$(shasum -a 256 "${WORKDIR}/${ASSET}" | awk '{print $1}')"
    else
      actual=""
      log "no sha256sum/shasum available, skipping checksum verification"
    fi
    if [ -n "$actual" ]; then
      [ "$actual" = "$expected" ] || err "checksum mismatch for ${ASSET} (expected $expected, got $actual)"
    fi
  fi
else
  log "${ASSET}.checksum.txt not found, skipping verification"
fi

log "extracting..."
tar -xzf "${WORKDIR}/${ASSET}" -C "$WORKDIR"
[ -f "${WORKDIR}/${BIN_NAME}" ] || err "extracted archive doesn't contain ${BIN_NAME} -- unexpected archive layout"
chmod +x "${WORKDIR}/${BIN_NAME}"

# The real version, straight from the binary's own embedded build
# info -- "latest" is only ever a filename on the mirror, never a
# real version string, so it's not what gets reported to radar-api
# below or run as the actual service.
VERSION_NUM="$("${WORKDIR}/${BIN_NAME}" version 2>/dev/null | awk '{print $2}')"
[ -n "$VERSION_NUM" ] || VERSION_NUM="unknown"

# ---------------------------------------------------------------------
# Verify these credentials are actually valid before installing
# anything -- a wrong node_id/api_key would otherwise still
# "successfully" install and start a service that just fails to
# authenticate forever in the background, with no feedback here at
# all. Piggybacks on the real heartbeat endpoint (the same call the
# running agent itself makes) rather than a dedicated check, since
# there's no lighter node-authed endpoint and an empty-probers
# heartbeat is already about as cheap as this gets. A definite 401 is
# treated as a real credential problem; anything else (including not
# being able to reach radar-api at all) is inconclusive -- a network
# hiccup here isn't proof the credentials are wrong, so it doesn't
# block the install; the running agent's own retries are what actually
# deal with a flaky network.
# ---------------------------------------------------------------------
log "verifying node credentials against ${API_URL}..."
verify_body="{\"node_id\":\"${NODE_ID}\",\"agent_version\":\"${VERSION_NUM}\",\"probers\":[],\"since_seq\":0,\"sent_at\":\"$(date -u +%Y-%m-%dT%H:%M:%SZ)\"}"
if [ -n "$PROXY" ]; then
  verify_status="$(curl -s -o /dev/null -w '%{http_code}' --proxy "$PROXY" --max-time 10 -X POST "${API_URL}/v1/nodes/heartbeat" -H "Authorization: Bearer ${NODE_ID}:${API_KEY}" -H "Content-Type: application/json" -d "$verify_body" || true)"
else
  verify_status="$(curl -s -o /dev/null -w '%{http_code}' --max-time 10 -X POST "${API_URL}/v1/nodes/heartbeat" -H "Authorization: Bearer ${NODE_ID}:${API_KEY}" -H "Content-Type: application/json" -d "$verify_body" || true)"
fi
if [ "$verify_status" = "401" ]; then
  err "radar-api rejected these credentials (401) -- double check --node_id/--api_key (or the existing installation's, if you didn't pass fresh ones)"
fi
if [ "$verify_status" != "200" ]; then
  log "couldn't confirm credentials against ${API_URL} (HTTP ${verify_status:-no response}) -- continuing anyway, since this looks like a network issue rather than a credential one"
fi

# ---------------------------------------------------------------------
# Install location + service setup -- IS_ROOT/INSTALL_BIN_DIR/
# MODULES_DIR were already resolved near the top of the script.
#
# Re-running this script to upgrade an already-installed, already-
# running node hits ETXTBSY on the cp below unless the service
# holding the old binary open is stopped first -- best-effort, since
# on a first install there's nothing to stop yet.
# ---------------------------------------------------------------------
if [ "$OS" = "linux" ] && command -v systemctl >/dev/null 2>&1; then
  if [ "$IS_ROOT" = "1" ]; then
    systemctl stop radar-node >/dev/null 2>&1 || true
  else
    systemctl --user stop radar-node >/dev/null 2>&1 || true
  fi
elif [ "$OS" = "darwin" ]; then
  if [ "$IS_ROOT" = "1" ]; then
    existing_plist="/Library/LaunchDaemons/${label}.plist"
  else
    existing_plist="${HOME}/Library/LaunchAgents/${label}.plist"
  fi
  [ -f "$existing_plist" ] && launchctl unload "$existing_plist" >/dev/null 2>&1
fi

mkdir -p "$INSTALL_BIN_DIR" "$MODULES_DIR"
cp "${WORKDIR}/${BIN_NAME}" "${INSTALL_BIN_DIR}/${BIN_NAME}"
chmod +x "${INSTALL_BIN_DIR}/${BIN_NAME}"
log "installed ${INSTALL_BIN_DIR}/${BIN_NAME}"

# Deliberately NOT running `radar-node init -C "$MODULES_DIR"` here
# anymore -- every built-in module (tcp/udp/dns/icmp/http/system) is
# already embedded in the binary itself and loads with zero on-disk
# files needed (see registry.Default()); --modules-dir only exists so
# a *custom* module (or a deliberate override of a built-in) can be
# dropped in. `init` without --force only writes a file if it doesn't
# already exist, which sounds harmless but is exactly what broke
# built-in module upgrades: the first install (or the first self-
# update that ever ran this) materialized whatever that version's
# embedded system.yaml/tcp.yaml/etc. looked like onto disk, and
# because LoadModules() always prefers an on-disk file of the same
# name over the embedded default, every later binary update's improved
# built-in module was silently shadowed by that permanently-frozen
# on-disk copy forever after -- e.g. a node still reporting a "system"
# schema from months ago despite running today's binary. Leaving
# $MODULES_DIR empty (still created, still passed to --modules-dir) is
# the actually-correct default; a user who wants to inspect or
# override a built-in module can still run `radar-node init` by hand.

# The icmp default prober uses an unprivileged "ping socket", which
# needs net.ipv4.ping_group_range to include this process's group --
# being root does NOT bypass this, it's a separate kernel mechanism
# from raw sockets/CAP_NET_RAW. Without it every icmp check fails with
# "permission denied" even on a root install. Only settable as root;
# best-effort (harmless if already configured or the sysctl is absent).
if [ "$OS" = "linux" ] && [ "$IS_ROOT" = "1" ] && command -v sysctl >/dev/null 2>&1; then
  log "enabling unprivileged ICMP (net.ipv4.ping_group_range)..."
  sysctl -w net.ipv4.ping_group_range="0 2147483647" >/dev/null 2>&1 || true
  mkdir -p /etc/sysctl.d 2>/dev/null
  echo "net.ipv4.ping_group_range = 0 2147483647" > /etc/sysctl.d/99-radar-node-icmp.conf 2>/dev/null || true
fi

# ---------------------------------------------------------------------
# Optional bundled engine modules -- fetched from this same mirror
# (releases/<tool>/, populated from mehrnet/static-builds by
# releases-sync.sh) rather than built or bundled here. Runs before the
# service (re)start further down so a module just dropped into
# $MODULES_DIR is actually picked up -- modules load once at agent
# startup, not on a file-system watch.
# ---------------------------------------------------------------------

# Delegates the actual fetch/verify/place to the just-installed
# radar-node binary's own fetch-module/install-module/remove-module
# subcommands (internal/moduleinstall) instead of reimplementing that
# in shell a second time (the old install_static_tool/remove_static_
# tool, removed here) -- that shell copy was still on the old mutable
# "_latest_" asset naming, never updated to the version-pinned,
# retry-past-CDN-propagation-lag fetch this same install.sh's own
# self-update above already gained (see fetch_with_retry). One
# implementation, in Go, covers both paths from here on -- and every
# module's own install: list already carries the version-pinned URLs
# that implementation needs (see install/modules/*.yaml's own
# comments).
#
# fetch-module (module not yet locally known: no <name>.yaml in
# MODULES_DIR yet) vs. install-module (already known -- re-fetches
# using its own recorded url, same as a bare re-run keeping it
# updated) mirrors radar-node's own subcommand split; see cmd/radar-
# node/main.go and moduleinstall.Install's doc comment.
fetch_or_update_module() {
  # $1 = module name (matches its own install/modules/<name>.yaml)
  name="$1"
  if [ -f "${MODULES_DIR}/${name}.yaml" ]; then
    "${INSTALL_BIN_DIR}/${BIN_NAME}" install-module "$name" --modules-dir "$MODULES_DIR" --tools-dir "$TOOLS_DIR" ${PROXY:+--proxy "$PROXY"}
  else
    "${INSTALL_BIN_DIR}/${BIN_NAME}" fetch-module "${MODULES_BASE}/${name}.yaml" --modules-dir "$MODULES_DIR" --tools-dir "$TOOLS_DIR" ${PROXY:+--proxy "$PROXY"}
  fi
}

# A module named in both lists at once (only possible when hand-typed
# -- radar-api's own nodeModuleActionsSchema already rejects it at the
# wire level) resolves to remove, same precedence the old per-module
# if/elif always had.
for m in xray wireguard openvpn; do
  if module_requested "$REMOVE_MODULES" "$m"; then
    "${INSTALL_BIN_DIR}/${BIN_NAME}" remove-module "$m" --modules-dir "$MODULES_DIR" --tools-dir "$TOOLS_DIR"
  elif module_requested "$INSTALL_MODULES" "$m"; then
    case "$m" in
      wireguard) [ "$OS" = "linux" ] || err "--install-module=wireguard is linux-only (radar-wg's netlink dependency doesn't target $OS)" ;;
      openvpn) [ "$OS" = "linux" ] || err "--install-module=openvpn is linux-only (only linux/amd64+arm64 static builds are published)" ;;
    esac
    fetch_or_update_module "$m"
    # CAP_NET_ADMIN (creating the TUN device) via setcap on the binary
    # itself, rather than requiring the whole agent process to run as
    # root just for this one prober -- only possible (and only needed)
    # on a root install; harmless to skip otherwise, radar-wg just
    # won't work until this node's agent runs with that capability
    # some other way. Not something moduleinstall.go itself does --
    # this stays install.sh's own concern, after the binary lands.
    if [ "$m" = "wireguard" ] && [ "$IS_ROOT" = "1" ] && command -v setcap >/dev/null 2>&1; then
      setcap cap_net_admin+ep "${TOOLS_DIR}/radar-wg" || log "setcap failed -- radar-wg will need CAP_NET_ADMIN some other way"
    fi
  fi
done

API_KEY_COMBINED="${NODE_ID}:${API_KEY}"
EXTRA_ARGS=""
[ -n "$PROXY" ] && EXTRA_ARGS="--api-proxy \"$PROXY\""

start_systemd() {
  unit_dir="$1"   # /etc/systemd/system or ~/.config/systemd/user
  systemctl_flags="$2"  # "" for system, "--user" for user

  mkdir -p "$unit_dir"
  unit_file="${unit_dir}/radar-node.service"
  cat > "$unit_file" <<EOF
[Unit]
Description=radar-node agent
After=network-online.target
Wants=network-online.target

[Service]
ExecStart=${INSTALL_BIN_DIR}/${BIN_NAME} agent --api-url "${API_URL}" --api-key "${API_KEY_COMBINED}" --modules-dir "${MODULES_DIR}" --tools-dir "${TOOLS_DIR}" ${EXTRA_ARGS}
Restart=always
RestartSec=2
# Exit code 42 is the agent's own deliberate self-update handoff (see
# internal/agent/agent.go's selfUpdateExitCode) -- it's about to be
# replaced/restarted by the installer it just launched, so systemd
# auto-restarting the still-old binary in the meantime only races
# that installer's own later "systemctl stop radar-node" step.
RestartPreventExitStatus=42

[Install]
WantedBy=$( [ "$systemctl_flags" = "--user" ] && echo "default.target" || echo "multi-user.target" )
EOF
  chmod 600 "$unit_file"

  # Never let a systemd hiccup (no working user session, dbus not up
  # in a minimal container, etc.) abort the whole install -- the
  # binary is already in place either way, so fall back to printing
  # the manual command instead of exiting non-zero.
  # shellcheck disable=SC2086
  if systemctl $systemctl_flags daemon-reload 2>/dev/null && systemctl $systemctl_flags enable --now radar-node 2>/dev/null; then
    log "radar-node is running as a systemd $( [ -z "$systemctl_flags" ] && echo "system" || echo "user" ) service"
    if [ "$systemctl_flags" = "--user" ]; then
      log "run 'loginctl enable-linger $(id -un)' so it keeps running after you log out"
    fi
  else
    rm -f "$unit_file"
    log "systemd is present but not usable here, skipping service setup"
    print_manual_run
  fi
}

start_launchd() {
  plist_dir="$1"    # /Library/LaunchDaemons or ~/Library/LaunchAgents
  # label is already set globally, near the top of this script.
  mkdir -p "$plist_dir"
  plist_file="${plist_dir}/${label}.plist"

  proxy_args=""
  if [ -n "$PROXY" ]; then
    proxy_args="    <string>--api-proxy</string>
    <string>${PROXY}</string>
"
  fi

  cat > "$plist_file" <<EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>${label}</string>
  <key>ProgramArguments</key>
  <array>
    <string>${INSTALL_BIN_DIR}/${BIN_NAME}</string>
    <string>agent</string>
    <string>--api-url</string>
    <string>${API_URL}</string>
    <string>--api-key</string>
    <string>${API_KEY_COMBINED}</string>
    <string>--modules-dir</string>
    <string>${MODULES_DIR}</string>
    <string>--tools-dir</string>
    <string>${TOOLS_DIR}</string>
${proxy_args}  </array>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><true/>
  <key>StandardOutPath</key><string>/tmp/radar-node.log</string>
  <key>StandardErrorPath</key><string>/tmp/radar-node.log</string>
</dict>
</plist>
EOF
  chmod 600 "$plist_file"
  launchctl unload "$plist_file" >/dev/null 2>&1 || true
  if launchctl load -w "$plist_file" 2>/dev/null; then
    log "radar-node is running as a launchd service ($plist_file)"
  else
    rm -f "$plist_file"
    log "launchd is present but not usable here, skipping service setup"
    print_manual_run
  fi
}

print_manual_run() {
  log "run it yourself:"
  log "  ${INSTALL_BIN_DIR}/${BIN_NAME} agent --api-url \"${API_URL}\" --api-key \"${API_KEY_COMBINED}\" --modules-dir \"${MODULES_DIR}\" --tools-dir \"${TOOLS_DIR}\"${PROXY:+ --api-proxy \"$PROXY\"}"
}

if [ "$OS" = "linux" ] && command -v systemctl >/dev/null 2>&1; then
  if [ "$IS_ROOT" = "1" ]; then
    start_systemd "/etc/systemd/system" ""
  else
    start_systemd "${HOME}/.config/systemd/user" "--user"
  fi
elif [ "$OS" = "darwin" ]; then
  if [ "$IS_ROOT" = "1" ]; then
    start_launchd "/Library/LaunchDaemons"
  else
    start_launchd "${HOME}/Library/LaunchAgents"
  fi
else
  print_manual_run
fi

log "done."
