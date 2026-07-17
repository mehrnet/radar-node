#!/bin/sh
# Same reasoning as wireguard-test.sh: openvpn brings up a tun
# interface via its own handshake, not a port the executor can wait
# on the way xray's SOCKS proxy is -- up/wait-for-tunnel/test/down all
# happen in this one `run` step (see openvpn.yaml, no prepare/teardown).
#
# `config` is the *raw* .ovpn file content as a string, not JSON --
# OpenVPN's config format isn't JSON, so unlike wireguard's params
# this one really is just an opaque blob passed straight through to a
# real config file for the openvpn binary to read.
set -e
PARAMS_JSON="$1"
TARGET="$2"
TIMEOUT_MS="$3"
OPENVPN_BIN="${OPENVPN_BIN:-__TOOLS_DIR__/openvpn}"

jget() {
  python3 -c "
import json, sys
v = json.load(open(sys.argv[1])).get(sys.argv[2])
print(v if v is not None else '')
" "$PARAMS_JSON" "$1"
}

WORKDIR=$(mktemp -d /tmp/radar-openvpn-XXXXXX)
OVPN_PID=""
cleanup() {
  [ -n "$OVPN_PID" ] && kill "$OVPN_PID" >/dev/null 2>&1 || true
  rm -rf "$WORKDIR"
}
trap cleanup EXIT

CONFIG_FILE="$WORKDIR/client.ovpn"
jget config > "$CONFIG_FILE"

set -- --config "$CONFIG_FILE"
auth_user=$(jget auth_user)
if [ -n "$auth_user" ]; then
  AUTH_FILE="$WORKDIR/auth.txt"
  { printf '%s\n' "$auth_user"; jget auth_pass; } > "$AUTH_FILE"
  chmod 600 "$AUTH_FILE"
  set -- "$@" --auth-user-pass "$AUTH_FILE"
fi

# --up fires once the tunnel is actually established (not just once
# the process starts) -- touching a file here is the only reliable
# "are we through the handshake yet" signal OpenVPN gives a wrapper
# like this one.
UP_SIGNAL="$WORKDIR/up"
cat > "$WORKDIR/up.sh" <<SCRIPT
#!/bin/sh
touch "$UP_SIGNAL"
SCRIPT
chmod +x "$WORKDIR/up.sh"

timeout_s=$(( (TIMEOUT_MS + 999) / 1000 ))
[ "$timeout_s" -lt 1 ] && timeout_s=1

"$OPENVPN_BIN" "$@" \
  --script-security 2 --up "$WORKDIR/up.sh" \
  --log "$WORKDIR/openvpn.log" --daemon
sleep 0.2
OVPN_PID=$(pgrep -f "openvpn --config $CONFIG_FILE" | head -n1)

waited=0
while [ ! -f "$UP_SIGNAL" ] && [ "$waited" -lt "$timeout_s" ]; do
  sleep 1
  waited=$((waited + 1))
done
if [ ! -f "$UP_SIGNAL" ]; then
  echo '{"http_code": 0}'
  exit 0
fi

curl --silent --max-time "$timeout_s" -o /dev/null -w '{"http_code": %{http_code}}' "$TARGET"
