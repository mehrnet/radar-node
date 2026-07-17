#!/bin/sh
# This module has no `prepare`/`teardown` step (see wireguard.yaml) --
# unlike xray's SOCKS proxy, `radar-wg up` isn't a long-lived process
# bound to a port the executor can wait on for readiness; it
# configures a kernel tunnel and exits immediately. So the whole
# up -> test -> down sequence happens here, in one `run` step, instead
# of trying to force it into the prepare/teardown shape.
#
# {{params_json}} is passed straight through as radar-wg's own
# --config: the module's request fields (private_key/address/
# peer_public_key/...) are deliberately named to match radar-wg's
# config.json 1:1, so there's nothing to reshape here.
set -e
PARAMS_JSON="$1"
TARGET="$2"
TIMEOUT_MS="$3"
RADAR_WG_BIN="${RADAR_WG_BIN:-__TOOLS_DIR__/radar-wg}"

STATE=$(mktemp /tmp/radar-wg-state-XXXXXX.json)
cleanup() { "$RADAR_WG_BIN" down --state "$STATE" >/dev/null 2>&1 || true; rm -f "$STATE"; }
trap cleanup EXIT

"$RADAR_WG_BIN" up --config "$PARAMS_JSON" --state "$STATE" 1>&2

# curl's own --max-time wants whole seconds; round up so a short
# millisecond timeout never becomes an instant 0s failure.
timeout_s=$(( (TIMEOUT_MS + 999) / 1000 ))
[ "$timeout_s" -lt 1 ] && timeout_s=1

curl --silent --max-time "$timeout_s" -o /dev/null -w '{"http_code": %{http_code}}' "$TARGET"
