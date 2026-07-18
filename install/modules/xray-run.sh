#!/bin/sh
# Wraps xray.yaml's `run` step so curl's own %{time_total} -- reported
# in *seconds*, not milliseconds -- gets converted before being
# labeled "latency_ms". Every native Go check (see
# internal/probe/latency()) reports genuine milliseconds; passing
# time_total straight through, as an earlier version of this module
# did, silently under-reports by exactly 1000x -- a real ~220ms round
# trip showing as "0.22ms", a number that reads as a suspiciously fast
# success rather than the obviously-wrong value it is.
set -e
ALLOC_PORT="$1"
TARGET="$2"

out=$(curl --silent --max-time 5 --socks5-hostname "127.0.0.1:${ALLOC_PORT}" -o /dev/null -w '%{time_total} %{http_code}' "$TARGET")
time_total=${out% *}
http_code=${out##* }
latency_ms=$(awk -v t="$time_total" 'BEGIN { printf "%.3f", t * 1000 }')
printf '{"latency_ms": %s, "http_code": %s}\n' "$latency_ms" "$http_code"
