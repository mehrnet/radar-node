#!/bin/sh
# `radar-wg run` now does the entire tunnel lifecycle -- bring up,
# probe the target through it, tear back down -- in one process (see
# static-builds/tools/wireguard-go/run.go). It has to: wireguard-go
# here is a *userspace* implementation, so the WireGuard session itself
# (handshake, encrypt/decrypt) is Go goroutines running inside that
# process, not a kernel module -- it doesn't survive that process
# exiting, so an earlier "bring up, exit, run curl separately, tear
# down separately" design (three separate process lifetimes) never
# actually worked: curl always ran against an already-dead tunnel.
#
# `config` is the *raw* wg-quick(8)-style .conf content as a string,
# same "opaque blob passed straight through" choice openvpn-test.sh
# makes for its own `config` field -- so this script's only job is
# pulling that one field out of {{params_json}} into a real file
# radar-wg's --config can point at.
set -e
PARAMS_JSON="$1"
TARGET="$2"
TIMEOUT_MS="$3"
RADAR_WG_BIN="${RADAR_WG_BIN:-__TOOLS_DIR__/radar-wg}"

jget() {
  python3 -c "
import json, sys
v = json.load(open(sys.argv[1])).get(sys.argv[2])
print(v if v is not None else '')
" "$PARAMS_JSON" "$1"
}

CONFIG_FILE=$(mktemp /tmp/radar-wg-XXXXXX.conf)
trap 'rm -f "$CONFIG_FILE"' EXIT
jget config > "$CONFIG_FILE"

"$RADAR_WG_BIN" run --config "$CONFIG_FILE" --target "$TARGET" --timeout-ms "$TIMEOUT_MS"
