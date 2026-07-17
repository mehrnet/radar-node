# Bundled engine modules

The module YAML + wrapper scripts install.sh's `--install-xray`/
`--install-wireguard`/`--install-openvpn` flags drop into `modules.d`,
paired with the matching static binary fetched from
[mehrnet/static-builds](https://github.com/mehrnet/static-builds).

`__MODULES_DIR__`/`__TOOLS_DIR__` are placeholders install.sh substitutes
with the real, resolved paths at install time (root vs. non-root installs
use different directories -- see the main README) -- these files are not
usable as-is without that substitution.

## `xray.yaml` + `xray-prepare.sh`

Generic, protocol-agnostic proxy test: `config` (the engine's full client
config, however it was built -- e.g. converted client-side from a share
link) plus `socks_port`, which inbound in that config is the local test
entry point. This replaces what used to be a built-in Go action
(`xray_proxy_test`) with the same behavior implemented as an ordinary
`prepare`/`run` module -- `xray-prepare.sh` remaps the declared inbound
onto the port the executor actually allocated (never bound as declared,
since this node's environment isn't known in advance) and `exec`s the
real `xray` binary as the long-lived process the executor supervises;
`run` then curls through it via SOCKS5.

## `wireguard.yaml` + `wireguard-test.sh`

No `prepare`/`teardown` -- unlike xray's SOCKS proxy, `radar-wg up` isn't
a long-lived process bound to a port the executor can wait on for
readiness (see [mehrnet/static-builds](https://github.com/mehrnet/static-builds)'s
own `tools/wireguard-go` README); it configures a kernel tunnel and exits
immediately. The whole `up` -> curl -> `down` sequence happens in one
`run` step instead. Request fields (`private_key`/`address`/
`peer_public_key`/`peer_preshared_key`/`endpoint`/`allowed_ips`/
`persistent_keepalive`) are deliberately named to match `radar-wg`'s own
config.json 1:1, so `{{params_json}}` passes straight through as its
`--config` with no reshaping.

Needs `CAP_NET_ADMIN` -- install.sh applies it to the `radar-wg` binary
via `setcap` at install time so the agent process itself doesn't need to
run fully as root just for this one prober.

## `openvpn.yaml` + `openvpn-test.sh`

Same up/test/down-in-one-`run`-step shape as wireguard, for the same
reason (no port the executor can wait on for readiness). `config` is the
*raw* `.ovpn` file content as a string, not JSON -- OpenVPN's config
format isn't JSON, so unlike wireguard's params this really is just an
opaque blob written straight to a real config file. `auth_user`/
`auth_pass` are optional, for a server that needs `--auth-user-pass`.
