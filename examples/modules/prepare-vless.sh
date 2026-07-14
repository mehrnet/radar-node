#!/bin/sh
set -e
PARAMS_JSON="$1"
SOCKS_PORT="$2"
# Pinned engine binaries live at /opt/radar-mehrnet/engines/<engine>/<version>/
# per the installer layout; override for local testing.
XRAY_BIN="${XRAY_BIN:-/opt/radar-mehrnet/engines/xray/26.3.27/xray}"

# server_host/server_port/uuid are required; everything else is
# optional and maps 1:1 onto a standard vless:// link's query string
# (security, sni, fp, pbk, type, headerType, flow) -- a job's params
# are expected to be lifted straight from whatever link the account
# holder was given by their VPN provider. Absent `security` means
# plaintext vless, same as this module's original tested fixture;
# `security=reality` or `security=tls` add the matching streamSettings.
jget() {
  python3 -c "import json,sys; v=json.load(open(sys.argv[1])).get(sys.argv[2], sys.argv[3] if len(sys.argv)>3 else ''); print(v if v is not None else (sys.argv[3] if len(sys.argv)>3 else ''))" "$PARAMS_JSON" "$1" "$2"
}

SERVER_HOST=$(jget server_host)
SERVER_PORT=$(jget server_port)
UUID=$(jget uuid)
SECURITY=$(jget security "none")
NETWORK=$(jget type "tcp")
HEADER_TYPE=$(jget headerType "none")
FLOW=$(jget flow "")

STREAM_SETTINGS="{\"network\": \"$NETWORK\""
if [ "$NETWORK" = "tcp" ]; then
  STREAM_SETTINGS="$STREAM_SETTINGS, \"tcpSettings\": {\"header\": {\"type\": \"$HEADER_TYPE\"}}"
fi

case "$SECURITY" in
  reality)
    SNI=$(jget sni)
    FP=$(jget fp "chrome")
    PBK=$(jget pbk)
    SID=$(jget sid "")
    STREAM_SETTINGS="$STREAM_SETTINGS, \"security\": \"reality\", \"realitySettings\": {\"serverName\": \"$SNI\", \"fingerprint\": \"$FP\", \"publicKey\": \"$PBK\", \"shortId\": \"$SID\", \"spiderX\": \"\"}"
    ;;
  tls)
    SNI=$(jget sni "$SERVER_HOST")
    STREAM_SETTINGS="$STREAM_SETTINGS, \"security\": \"tls\", \"tlsSettings\": {\"serverName\": \"$SNI\"}"
    ;;
  *)
    STREAM_SETTINGS="$STREAM_SETTINGS, \"security\": \"none\""
    ;;
esac
STREAM_SETTINGS="$STREAM_SETTINGS}"

USER_JSON="{\"id\": \"$UUID\", \"encryption\": \"none\""
if [ -n "$FLOW" ]; then
  USER_JSON="$USER_JSON, \"flow\": \"$FLOW\""
fi
USER_JSON="$USER_JSON}"

CONFIG=$(mktemp /tmp/radar-vless-client-XXXXXX.json)
cat > "$CONFIG" <<EOF
{
  "log": {"loglevel": "warning"},
  "inbounds": [{
    "port": $SOCKS_PORT, "listen": "127.0.0.1", "protocol": "socks",
    "settings": {"auth": "noauth", "udp": false}
  }],
  "outbounds": [{
    "protocol": "vless",
    "settings": {"vnext": [{"address": "$SERVER_HOST", "port": $SERVER_PORT,
      "users": [$USER_JSON]}]},
    "streamSettings": $STREAM_SETTINGS
  }]
}
EOF

exec "$XRAY_BIN" run -c "$CONFIG"
