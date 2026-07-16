#!/bin/sh
# radar-node installer -- POSIX sh, no bashisms, safe to pipe from curl:
#
#   curl -fsSL https://raw.githubusercontent.com/mehrnet/radar-node/main/install.sh \
#     | sh -s -- --node_id=node_xxx --api_key=xxxxx
#
# Downloads the right release asset for this OS/arch from GitHub
# Releases, installs the binary, and sets up radar-node as a
# persistent service (systemd on Linux, launchd on macOS) so the
# copy-pasted command above is the only step a user ever has to take.
set -e

REPO="mehrnet/radar-node"
BIN_NAME="radar-node"
API_URL_DEFAULT="https://radar-api.mehrnet.com"

NODE_ID=""
API_KEY=""
API_URL="$API_URL_DEFAULT"
PROXY=""
VERSION="latest"
UNINSTALL=0

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
  --version=VERSION  install a specific release instead of the latest, e.g. 0.2
  --uninstall        stop and fully remove radar-node from this machine (no
                      other flag is needed -- this ignores --node_id/--api_key)
  -h, --help         show this help

--node_id/--api_key/--api_url/--proxy are only required the first time --
re-running this same command on a machine that already has radar-node
installed (e.g. to pick up a new release) reuses whatever's already
configured there for any of these you don't pass again, so a bare
`| sh -s` upgrades an existing install with no arguments at all.
EOF
}

log() { printf '==> %s\n' "$*" >&2; }
err() { printf 'error: %s\n' "$*" >&2; exit 1; }

for arg in "$@"; do
  case "$arg" in
    --node_id=*) NODE_ID="${arg#*=}" ;;
    --api_key=*) API_KEY="${arg#*=}" ;;
    --api_url=*) API_URL="${arg#*=}" ;;
    --proxy=*) PROXY="${arg#*=}" ;;
    --version=*) VERSION="${arg#*=}" ;;
    --uninstall) UNINSTALL=1 ;;
    -h|--help) usage; exit 0 ;;
    *) err "unknown argument: $arg (see --help)" ;;
  esac
done

# ---------------------------------------------------------------------
# Platform detection -> goreleaser's os/arch naming (see .goreleaser.yaml).
# Needed by both --uninstall (to find the right service manager) and a
# real install (which also needs ARCH, resolved further down).
# ---------------------------------------------------------------------
os_raw="$(uname -s)"
case "$os_raw" in
  Linux) OS=linux ;;
  Darwin) OS=darwin ;;
  *) err "unsupported OS: $os_raw -- radar-node ships linux/darwin/windows releases; for windows grab a release asset manually from https://github.com/$REPO/releases" ;;
esac

# Root gets a real system service (systemd/launchd) so the node
# survives reboots with zero further action; a non-root install still
# works, just user-scoped. Needed by --uninstall, the "reuse an
# existing install's credentials" check below, and the real install
# further down -- computed once, here, rather than three times.
if [ "$(id -u)" = "0" ]; then
  INSTALL_BIN_DIR="/usr/local/bin"
  MODULES_DIR="/etc/radar-node/modules.d"
  IS_ROOT=1
else
  INSTALL_BIN_DIR="${HOME}/.local/bin"
  MODULES_DIR="${HOME}/.config/radar-node/modules.d"
  IS_ROOT=0
fi
label="com.mehrnet.radar-node"

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
  rm -rf "$MODULES_DIR"
  log "removed ${INSTALL_BIN_DIR}/${BIN_NAME} and ${MODULES_DIR}"
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

curl_get() {
  # $1 = url, $2 = output path
  if [ -n "$PROXY" ]; then
    curl -fsSL --proxy "$PROXY" "$1" -o "$2"
  else
    curl -fsSL "$1" -o "$2"
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
# Resolve the release tag (skips the API call entirely if --version
# pinned a specific one), then download + verify + extract the binary.
# ---------------------------------------------------------------------
if [ "$VERSION" = "latest" ]; then
  log "resolving latest release..."
  tmp_meta="$(mktemp)"
  curl_get "https://api.github.com/repos/${REPO}/releases/latest" "$tmp_meta"
  TAG="$(grep -m1 '"tag_name"' "$tmp_meta" | sed -E 's/.*"tag_name":[[:space:]]*"([^"]+)".*/\1/')"
  rm -f "$tmp_meta"
  [ -n "$TAG" ] || err "couldn't resolve the latest release -- pass --version=X.Y explicitly"
else
  TAG="v${VERSION#v}"
fi
VERSION_NUM="${TAG#v}"
ASSET="${BIN_NAME}_${VERSION_NUM}_${OS}_${ARCH}.tar.gz"
BASE_URL="https://github.com/${REPO}/releases/download/${TAG}"

WORKDIR="$(mktemp -d)"
trap 'rm -rf "$WORKDIR"' EXIT

log "downloading ${ASSET} (${TAG})..."
curl_get "${BASE_URL}/${ASSET}" "${WORKDIR}/${ASSET}"

log "verifying checksum..."
if curl_get "${BASE_URL}/checksums.txt" "${WORKDIR}/checksums.txt" 2>/dev/null; then
  expected="$(grep "  ${ASSET}\$" "${WORKDIR}/checksums.txt" | awk '{print $1}')"
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
  log "checksums.txt not found for ${TAG}, skipping verification"
fi

log "extracting..."
tar -xzf "${WORKDIR}/${ASSET}" -C "$WORKDIR"
[ -f "${WORKDIR}/${BIN_NAME}" ] || err "extracted archive doesn't contain ${BIN_NAME} -- unexpected archive layout"
chmod +x "${WORKDIR}/${BIN_NAME}"

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

"${INSTALL_BIN_DIR}/${BIN_NAME}" init -C "$MODULES_DIR" >/dev/null 2>&1 || true

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
ExecStart=${INSTALL_BIN_DIR}/${BIN_NAME} agent --api-url "${API_URL}" --api-key "${API_KEY_COMBINED}" --modules-dir "${MODULES_DIR}" ${EXTRA_ARGS}
Restart=always
RestartSec=2

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
  label="com.mehrnet.radar-node"
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
  log "  ${INSTALL_BIN_DIR}/${BIN_NAME} agent --api-url \"${API_URL}\" --api-key \"${API_KEY_COMBINED}\" --modules-dir \"${MODULES_DIR}\"${PROXY:+ --api-proxy \"$PROXY\"}"
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
