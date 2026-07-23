#!/bin/sh
# Wraps xray.yaml's `run` step so curl's own %{time_total} -- reported
# in *seconds*, not milliseconds -- gets converted before being
# labeled "latency_ms". Every native Go check (see
# internal/probe/latency()) reports genuine milliseconds; passing
# time_total straight through, as an earlier version of this module
# did, silently under-reports by exactly 1000x -- a real ~220ms round
# trip showing as "0.22ms", a number that reads as a suspiciously fast
# success rather than the obviously-wrong value it is.
#
# curl's own --max-time used to be a fixed 5s, independent of this
# probe's actual timeout_ms -- but `prepare` (see xray-prepare.sh) can
# use up to half of that budget getting the local proxy ready, and
# checker.go's outer context is bounded by the *whole* timeout_ms with
# no per-step reservation for `run`. On any probe configured under
# ~10s (the default is 5000ms), or any check where prepare genuinely
# needed most of its own half, curl's 5s never had a chance to fire on
# its own -- the outer context always killed the process first, via a
# raw SIGKILL that surfaces as the unhelpful "signal: killed" instead
# of curl's own readable timeout error. --max-time is now a fraction
# of the probe's real timeout_ms (passed in as $3) instead, so curl
# always has genuine room to time out gracefully before that happens,
# regardless of how tight or generous timeout_ms is configured.
set -e
ALLOC_PORT="$1"
TARGET="$2"
TIMEOUT_MS="${3:-5000}"

max_time=$(awk -v t="$TIMEOUT_MS" 'BEGIN { printf "%.3f", (t > 0 ? t : 5000) * 0.4 / 1000 }')

out=$(curl --silent --max-time "$max_time" --socks5-hostname "127.0.0.1:${ALLOC_PORT}" -o /dev/null -w '%{time_total} %{http_code}' "$TARGET")
time_total=${out% *}
http_code=${out##* }
latency_ms=$(awk -v t="$time_total" 'BEGIN { printf "%.3f", t * 1000 }')
printf '{"latency_ms": %s, "http_code": %s}\n' "$latency_ms" "$http_code"
