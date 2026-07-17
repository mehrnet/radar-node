# Example custom prober modules

These are real, tested reference implementations of the config-driven
module system (see `../../README.md` and `internal/module`), proving
the mechanism against genuine xray-core and sing-box binaries -- not
mocked.

## `xray.yaml` / `singbox.yaml` -- the recommended way to test either engine

Generic, protocol-agnostic actions (`internal/checks/proxytest`, `action:
xray_proxy_test`/`singbox_proxy_test`) rather than `run:`-based modules:
a probe supplies the engine's full client config (however it was built --
e.g. converted client-side from a share link, the way 3x-ui does it) plus
`socks_port`, which inbound in that config is the local test entry point.
Neither action reads anything protocol-specific out of the config, so any
protocol either engine supports works with zero radar-node changes -- this
is what `xray-vless`/`singbox-trojan` below have been superseded by for
the xray/sing-box case specifically.

`socks_port` is never bound as-is -- the node always silently reallocates
the matching inbound to a real free local port first (see README.md's
"Generic xray/sing-box proxying" section), so a taken port or two
concurrent probes never collide. Verified against the same real,
internet-facing VLESS+REALITY server referenced below: `ok: true,
http_code: 200` through the generic `xray` module with the declared
`socks_port` deliberately pre-occupied by an unrelated process the whole
time (proving the silent remap, not just the proxy tunnel itself), and a
deliberately wrong declared port (matching no inbound at all) correctly
came back `error_code: "invalid_params"`.

```jsonc
// example probe params
{
  "config": { /* full xray or sing-box client config JSON */ },
  "socks_port": 1234
}
```

## `xray-vless.yaml` / `singbox-trojan.yaml` -- bespoke `run:`-based reference

Still valid, still tested -- kept as a reference for genuinely bespoke
external-tool integrations that don't fit the generic config+port shape
above.

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
