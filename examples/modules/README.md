# Example custom prober modules

These are real, tested reference implementations of the config-driven
module system (see `../../README.md` and `internal/module`), proving
the mechanism against genuine xray-core and sing-box binaries -- not
mocked.

## `xray-vless.yaml` / `singbox-trojan.yaml` -- `run:`-based reference

There is no built-in Go action for xray/sing-box (there used to be --
`xray_proxy_test`/`singbox_proxy_test`, removed once engine binaries
became an install.sh-managed concern; see
[mehrnet/static-builds](https://github.com/mehrnet/static-builds) and
`--install-xray` in the main README). Every engine integration is a
plain `run:`-based module now, these two included.

- `xray-vless.yaml` + `prepare-vless.sh` -- tests a VLESS proxy.
  Supports plaintext, TLS, and REALITY (`security=reality`, matching
  a standard `vless://...&security=reality&sni=...&fp=...&pbk=...`
  link's query params) via the `security`/`sni`/`fp`/`pbk`/`sid`/
  `flow`/`type`/`headerType` params.
- `singbox-trojan.yaml` + `prepare-singbox-trojan.sh` -- tests a
  Trojan proxy via sing-box.

Both follow the same shape: `prepare` is a small shell script that
reads the probe's `{{params_json}}` (server host/port and the
protocol's credential), generates a client config for the allocated
local SOCKS port (`{{alloc_port}}`), and `exec`s the real engine
binary so it becomes the long-lived process the executor supervises.
`run` then curls through that SOCKS proxy at `{{target}}` and reports
timing via curl's own JSON writeout format.

Verified end-to-end during development, twice:

1. A real xray-core 26.3.27 (VLESS) and sing-box 1.13.14 (Trojan)
   relay, each locally-controlled and reachable only through its own
   tunnel, correctly returned `http_code: 200` through the module
   system, and correctly failed (wrong UUID/password) when the
   tunnel credentials didn't match.
2. `xray-vless` was separately re-verified against a real,
   internet-facing VLESS+REALITY server (`security=reality`, a
   Google-camouflaged SNI, and a REALITY public key/fingerprint --
   the config shape actual VPN providers hand out today) via
   `radar-node probe --type xray-vless`: `ok: true`,
   `http_code: 200`, ~780ms round trip through the real REALITY
   handshake, and a deliberately wrong UUID against the same server
   correctly came back `ok: false`. This is the case the original
   "known simplification" below used to leave untested.

## Reproducing the fixture locally

```sh
# 1. Fetch pinned engine binaries (adjust versions/arch as needed)
mkdir -p /opt/radar-node/engines/xray/26.3.27 /opt/radar-node/engines/singbox/1.13.14
curl -L -o /tmp/xray.zip https://github.com/XTLS/Xray-core/releases/download/v26.3.27/Xray-linux-64.zip
unzip -j /tmp/xray.zip xray -d /opt/radar-node/engines/xray/26.3.27/
curl -L -o /tmp/singbox.tar.gz https://github.com/SagerNet/sing-box/releases/download/v1.13.14/sing-box-1.13.14-linux-amd64.tar.gz
tar xzf /tmp/singbox.tar.gz -C /tmp && mv /tmp/sing-box-1.13.14-linux-amd64/sing-box /opt/radar-node/engines/singbox/1.13.14/

# 2. Start a relay to test against (this is the thing a real user's
#    own VLESS/Trojan server would be -- here we run one locally)
UUID=$(python3 -c "import uuid; print(uuid.uuid4())")
cat > /tmp/vless-server.json <<EOF
{"inbounds":[{"port":28443,"listen":"127.0.0.1","protocol":"vless",
  "settings":{"clients":[{"id":"$UUID"}],"decryption":"none"}}],
 "outbounds":[{"protocol":"freedom"}]}
EOF
/opt/radar-node/engines/xray/26.3.27/xray run -c /tmp/vless-server.json &

# 3. Deploy the module + its prepare script where the YAML expects them
mkdir -p /etc/radar-node/modules.d
cp xray-vless.yaml prepare-vless.sh /etc/radar-node/modules.d/

# 4. Run a check through it
radar-node probe http://example.com/ \
  --type xray-vless --modules-dir /etc/radar-node/modules.d \
  --param server_host=127.0.0.1 --param server_port=28443 --param uuid="$UUID"
```

## Known simplification, called out honestly

The `singbox-trojan` fixture still only covers plaintext Trojan (no
TLS) between two locally-controlled sing-box processes -- proving the
module executor's prepare/run/collect/alloc_port machinery drives a
real engine correctly, not TLS handshake behavior specifically. The
same REALITY-style extension done for `xray-vless`'s `prepare-vless.sh`
(read `security`/`sni`/etc. from params, branch the generated
streamSettings) would apply the same way here if/when a real
Trojan+TLS fixture is needed; nothing about the module system itself
would need to change.
