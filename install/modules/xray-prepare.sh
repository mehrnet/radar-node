#!/bin/sh
# Launched by the module executor as this check's `prepare` step (see
# xray.yaml) -- exec'd in place so it becomes the long-lived process
# the executor supervises and kills when the check's context is done,
# same shape as xray-vless's own prepare-vless.sh.
#
# Remaps whichever inbound in `config` declares `socks_port` (by its
# own port/listen_port field) onto the port the executor actually
# allocated for us. This node's environment isn't fully known in
# advance -- another process could already hold the declared port, or
# two probes could legitimately want to test through "the same" port
# concurrently -- so the caller's declared socks_port is only ever a
# *label* identifying which inbound to rewrite, never bound as-is.
set -e
PARAMS_JSON="$1"
SOCKS_PORT="$2"
XRAY_BIN="${XRAY_BIN:-__TOOLS_DIR__/xray}"

CONFIG_FILE=$(mktemp /tmp/radar-xray-config-XXXXXX.json)
python3 -c "
import json, sys
params = json.load(open(sys.argv[1]))
config = params['config']
declared_port = params['socks_port']
alloc_port = int(sys.argv[2])
found = False
for inbound in config.get('inbounds', []):
    for key in ('port', 'listen_port'):
        if inbound.get(key) == declared_port:
            inbound[key] = alloc_port
            found = True
            break
    if found:
        break
if not found:
    sys.exit('no inbound in config has port/listen_port ' + repr(declared_port))
json.dump(config, open(sys.argv[3], 'w'))
" "$PARAMS_JSON" "$SOCKS_PORT" "$CONFIG_FILE"

exec "$XRAY_BIN" run -c "$CONFIG_FILE"
