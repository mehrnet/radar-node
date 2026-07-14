#!/bin/sh
set -e
PARAMS_JSON="$1"
SOCKS_PORT="$2"
# Pinned engine binaries live at /opt/radar-mehrnet/engines/<engine>/<version>/
# per the installer layout; override for local testing.
SINGBOX_BIN="${SINGBOX_BIN:-/opt/radar-mehrnet/engines/singbox/1.13.14/sing-box}"

SERVER_HOST=$(python3 -c "import json,sys; print(json.load(open(sys.argv[1]))['server_host'])" "$PARAMS_JSON")
SERVER_PORT=$(python3 -c "import json,sys; print(json.load(open(sys.argv[1]))['server_port'])" "$PARAMS_JSON")
PASSWORD=$(python3 -c "import json,sys; print(json.load(open(sys.argv[1]))['password'])" "$PARAMS_JSON")

CONFIG=$(mktemp /tmp/radar-singbox-client-XXXXXX.json)
cat > "$CONFIG" <<EOF
{
  "log": {"level": "warn"},
  "inbounds": [{"type": "socks", "listen": "127.0.0.1", "listen_port": $SOCKS_PORT}],
  "outbounds": [{
    "type": "trojan",
    "server": "$SERVER_HOST",
    "server_port": $SERVER_PORT,
    "password": "$PASSWORD"
  }]
}
EOF

exec "$SINGBOX_BIN" run -c "$CONFIG"
