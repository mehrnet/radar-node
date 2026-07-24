#!/bin/sh
# This module has no `prepare`/`teardown` step (see wireguard.yaml) --
# unlike xray's SOCKS proxy, `radar-wg up` isn't a long-lived process
# bound to a port the executor can wait on for readiness; it
# configures a kernel tunnel and exits immediately. So the whole
# up -> test -> down sequence happens here, in one `run` step, instead
# of trying to force it into the prepare/teardown shape.
#
# `config` is the *raw* wg-quick(8)-style .conf content as a string,
# same "opaque blob passed straight through" choice openvpn-test.sh
# makes for its own `config` field -- radar-wg parses wg-quick syntax
# natively (see static-builds/tools/wireguard-go/config.go), so there's
# nothing to reshape here beyond pulling that one field out of
# {{params_json}} into a real file radar-wg's --config can point at.
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

WORKDIR=$(mktemp -d /tmp/radar-wg-XXXXXX)
STATE="$WORKDIR/state.json"
cleanup() { "$RADAR_WG_BIN" down --state "$STATE" >/dev/null 2>&1 || true; rm -rf "$WORKDIR"; }
trap cleanup EXIT

CONFIG_FILE="$WORKDIR/tunnel.conf"
jget config > "$CONFIG_FILE"

"$RADAR_WG_BIN" up --config "$CONFIG_FILE" --state "$STATE" 1>&2

# curl's own --max-time wants whole seconds; round up so a short
# millisecond timeout never becomes an instant 0s failure.
timeout_s=$(( (TIMEOUT_MS + 999) / 1000 ))
[ "$timeout_s" -lt 1 ] && timeout_s=1

# curl's %{time_total} is seconds, not milliseconds -- every native
# Go check reports genuine ms (see internal/probe/latency()), so this
# converts before labeling the field "latency_ms" rather than silently
# under-reporting by 1000x.
out=$(curl --silent --max-time "$timeout_s" -o /dev/null -w '%{time_total} %{http_code}' "$TARGET")
time_total=${out% *}
http_code=${out##* }
latency_ms=$(awk -v t="$time_total" 'BEGIN { printf "%.3f", t * 1000 }')
printf '{"latency_ms": %s, "http_code": %s}\n' "$latency_ms" "$http_code"
