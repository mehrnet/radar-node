package subscriptionfetch_test

import (
	"encoding/base64"
	"strings"
	"testing"

	"github.com/mehrnet/radar-node/internal/checks/subscriptionfetch"
)

func TestParse_Empty(t *testing.T) {
	proxies, err := subscriptionfetch.Parse([]byte("  \n  "), subscriptionfetch.Base64Xray)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(proxies) != 0 {
		t.Fatalf("expected no proxies, got %d", len(proxies))
	}
}

func TestParse_UnknownType(t *testing.T) {
	_, err := subscriptionfetch.Parse([]byte("21.22.23.24:1080"), subscriptionfetch.SubscriptionType("clash-yaml"))
	if err == nil {
		t.Fatalf("expected an error for an unrecognized subscription type")
	}
}

func TestParse_Base64XrayButNotActuallyBase64(t *testing.T) {
	// Explicitly typed base64-xray, but the content is a raw (non-
	// base64) URI line -- must fail loudly rather than silently
	// parsing zero proxies, since this is now a real config mistake
	// (wrong type picked), not an ambiguous input to guess at.
	line := "vless://a3482e88-686a-4a58-8126-99c9df64b7bf@5.6.7.8:443?encryption=none&security=tls&type=tcp&sni=example.org#my-vless"
	_, err := subscriptionfetch.Parse([]byte(line), subscriptionfetch.Base64Xray)
	if err == nil {
		t.Fatalf("expected an error when base64-xray content isn't valid base64")
	}
}

func TestParse_RawVmessLine(t *testing.T) {
	vmessJSON := `{"ps":"my-vmess","add":"1.2.3.4","port":"443","id":"a3482e88-686a-4a58-8126-99c9df64b7bf","aid":"0","scy":"auto","net":"ws","host":"example.com","path":"/ws","tls":"tls","sni":"example.com"}`
	line := "vmess://" + base64.StdEncoding.EncodeToString([]byte(vmessJSON))

	proxies, err := subscriptionfetch.Parse([]byte(line), subscriptionfetch.XrayURIList)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(proxies) != 1 {
		t.Fatalf("expected 1 proxy, got %d", len(proxies))
	}
	p := proxies[0]
	if p.Prober != "xray" || p.Name != "my-vmess" {
		t.Fatalf("unexpected proxy: %+v", p)
	}
	config, ok := p.Params["config"].(map[string]any)
	if !ok {
		t.Fatalf("expected config map, got %T", p.Params["config"])
	}
	outbounds, ok := config["outbounds"].([]map[string]any)
	if !ok || len(outbounds) != 1 || outbounds[0]["protocol"] != "vmess" {
		t.Fatalf("unexpected outbounds: %+v", config["outbounds"])
	}
}

func TestParse_Base64EncodedList(t *testing.T) {
	vlessLine := "vless://a3482e88-686a-4a58-8126-99c9df64b7bf@5.6.7.8:443?encryption=none&security=tls&type=tcp&sni=example.org#my-vless"
	whole := base64.StdEncoding.EncodeToString([]byte(vlessLine + "\n"))

	proxies, err := subscriptionfetch.Parse([]byte(whole), subscriptionfetch.Base64Xray)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(proxies) != 1 {
		t.Fatalf("expected 1 proxy, got %d", len(proxies))
	}
	if proxies[0].Name != "my-vless" || proxies[0].Prober != "xray" {
		t.Fatalf("unexpected proxy: %+v", proxies[0])
	}
}

// Regression for a real production report: a Reality-security vless
// entry (a genuinely common shape, not an edge case -- every kovira3.ir
// entry using the reality-cdn path uses it) produced a config missing
// its own realitySettings entirely, since streamSettingsFor's reality
// branch used to just stamp `"security": "reality"` and stop there.
// Without publicKey/shortId at minimum, Reality's handshake can't
// complete at all -- not "less secure", genuinely non-functional --
// so every check against it failed silently forever, reading as "no
// data yet" with no indication why.
func TestParse_VlessReality(t *testing.T) {
	line := "vless://1ae04638-e104-4981-8dde-08f24a1014a5@movies2.kovira3.ir:8090?encryption=none&fp=qq&pbk=FSGJiYtzXeGGDtYcrYGO89yeaxnr2aZqIU12ErTt42s&security=reality&sid=1b&sni=play.google.com&spx=%2F5ba417bcf26a54f&type=tcp#kovira"
	proxies, err := subscriptionfetch.Parse([]byte(line), subscriptionfetch.XrayURIList)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(proxies) != 1 {
		t.Fatalf("expected 1 proxy, got %d", len(proxies))
	}
	config := proxies[0].Params["config"].(map[string]any)
	outbounds := config["outbounds"].([]map[string]any)
	streamSettings := outbounds[0]["streamSettings"].(map[string]any)
	if streamSettings["security"] != "reality" {
		t.Fatalf("expected reality security, got %+v", streamSettings)
	}
	realitySettings, ok := streamSettings["realitySettings"].(map[string]any)
	if !ok {
		t.Fatalf("expected realitySettings to be present, got %+v", streamSettings)
	}
	if realitySettings["publicKey"] != "FSGJiYtzXeGGDtYcrYGO89yeaxnr2aZqIU12ErTt42s" {
		t.Fatalf("unexpected publicKey: %+v", realitySettings)
	}
	if realitySettings["shortId"] != "1b" {
		t.Fatalf("unexpected shortId: %+v", realitySettings)
	}
	if realitySettings["fingerprint"] != "qq" {
		t.Fatalf("unexpected fingerprint: %+v", realitySettings)
	}
	if realitySettings["spiderX"] != "/5ba417bcf26a54f" {
		t.Fatalf("unexpected spiderX: %+v", realitySettings)
	}
	if realitySettings["serverName"] != "play.google.com" {
		t.Fatalf("unexpected serverName: %+v", realitySettings)
	}
}

// Regression for a real production report: a Reality-secured config's
// own serverName/shortId/spiderX/fingerprint rotate every few minutes
// as normal camouflage, with the actual server completely unchanged.
// radar-api hashes Identity (when set) instead of the raw params
// precisely so that rotation doesn't read as a brand new proxy -- see
// subscriptionMaterialize.ts's own computeContentHash comment. This
// locks in the two properties that matter: identical except for those
// four fields -> identical Identity; a genuinely different server
// (even sharing everything else) -> a different one.
func TestParse_VlessRealityIdentityStableAcrossRotation(t *testing.T) {
	base := "vless://1ae04638-e104-4981-8dde-08f24a1014a5@movies2.kovira3.ir:8090?encryption=none&security=reality&type=tcp#kovira"
	rotated := base + "&fp=qq&pbk=SAMEKEY&sid=1b&sni=play.google.com&spx=%2Fone"
	rotatedAgain := base + "&fp=chrome&pbk=SAMEKEY&sid=9f&sni=www.example.com&spx=%2Ftwo"

	p1, err := subscriptionfetch.Parse([]byte(rotated), subscriptionfetch.XrayURIList)
	if err != nil || len(p1) != 1 {
		t.Fatalf("unexpected parse result: %v, %+v", err, p1)
	}
	p2, err := subscriptionfetch.Parse([]byte(rotatedAgain), subscriptionfetch.XrayURIList)
	if err != nil || len(p2) != 1 {
		t.Fatalf("unexpected parse result: %v, %+v", err, p2)
	}
	if p1[0].Identity == "" {
		t.Fatalf("expected a non-empty identity")
	}
	if p1[0].Identity != p2[0].Identity {
		t.Fatalf("expected identity to survive fp/sid/sni/spx rotation, got %q vs %q", p1[0].Identity, p2[0].Identity)
	}

	// A genuinely different server (different host) must not collide.
	differentServer := strings.Replace(rotated, "movies2.kovira3.ir", "movies3.kovira3.ir", 1)
	p3, err := subscriptionfetch.Parse([]byte(differentServer), subscriptionfetch.XrayURIList)
	if err != nil || len(p3) != 1 {
		t.Fatalf("unexpected parse result: %v, %+v", err, p3)
	}
	if p3[0].Identity == p1[0].Identity {
		t.Fatalf("expected a different host to produce a different identity")
	}
}

// Regression for a real production report: every xhttp-transport entry
// across an entire subscription failed 100% of the time, on every
// node, regardless of xray-core version -- streamSettingsFor's switch
// had no "xhttp" case at all, so path/host/mode/extra were silently
// dropped and the resulting outbound had no xhttpSettings whatsoever.
// xhttp genuinely can't negotiate without at least a matching path, so
// this wasn't a version-compatibility issue, it was a structurally
// incomplete generated config. Real URI from the report.
func TestParse_VlessXhttp(t *testing.T) {
	line := "vless://521fdaab-83cc-45dd-b141-511e5a5659c8@tun1.momasvps.ir:8016?security=reality&type=xhttp&headerType=&path=%2Fpath&host=hostname&mode=auto&extra=%7B%22scMaxEachPostBytes%22%3A+1000000%2C+%22scMaxConcurrentPosts%22%3A+100%2C+%22scMinPostsIntervalMs%22%3A+30%2C+%22xPaddingBytes%22%3A+%22100-1000%22%2C+%22noGRPCHeader%22%3A+false%7D&sni=amp-api-edge.apps.apple.com&fp=firefox&pbk=-z6cgn79fnYFLbm9SEHlhE4ygL3vt-11xMpvLGMWyj8&sid=3bec2937ffa88b18#test"
	proxies, err := subscriptionfetch.Parse([]byte(line), subscriptionfetch.XrayURIList)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(proxies) != 1 {
		t.Fatalf("expected 1 proxy, got %d", len(proxies))
	}
	config := proxies[0].Params["config"].(map[string]any)
	outbounds := config["outbounds"].([]map[string]any)
	streamSettings := outbounds[0]["streamSettings"].(map[string]any)
	if streamSettings["network"] != "xhttp" {
		t.Fatalf("expected xhttp network, got %+v", streamSettings)
	}
	xhttpSettings, ok := streamSettings["xhttpSettings"].(map[string]any)
	if !ok {
		t.Fatalf("expected xhttpSettings to be present, got %+v", streamSettings)
	}
	if xhttpSettings["path"] != "/path" {
		t.Fatalf("unexpected path: %+v", xhttpSettings)
	}
	if xhttpSettings["host"] != "hostname" {
		t.Fatalf("unexpected host: %+v", xhttpSettings)
	}
	if xhttpSettings["mode"] != "auto" {
		t.Fatalf("unexpected mode: %+v", xhttpSettings)
	}
	extra, ok := xhttpSettings["extra"].(map[string]any)
	if !ok {
		t.Fatalf("expected extra to be present and parsed, got %+v", xhttpSettings)
	}
	if extra["scMaxEachPostBytes"] != float64(1000000) {
		t.Fatalf("unexpected scMaxEachPostBytes: %+v", extra)
	}
	if extra["xPaddingBytes"] != "100-1000" {
		t.Fatalf("unexpected xPaddingBytes: %+v", extra)
	}
	if extra["noGRPCHeader"] != false {
		t.Fatalf("unexpected noGRPCHeader: %+v", extra)
	}
	// realitySettings must still be built correctly alongside xhttp --
	// this isn't an either/or with the tcp/xhttp-specific settings above.
	realitySettings, ok := streamSettings["realitySettings"].(map[string]any)
	if !ok || realitySettings["publicKey"] != "-z6cgn79fnYFLbm9SEHlhE4ygL3vt-11xMpvLGMWyj8" {
		t.Fatalf("unexpected realitySettings: %+v", streamSettings)
	}
}

// A missing/unparseable extra must never fail the whole probe -- it
// degrades to xhttp's own built-in defaults for those fields rather
// than the proxy never getting created at all.
func TestParse_VlessXhttpNoExtra(t *testing.T) {
	line := "vless://521fdaab-83cc-45dd-b141-511e5a5659c8@tun1.momasvps.ir:8016?security=none&type=xhttp&path=%2Fpath#test"
	proxies, err := subscriptionfetch.Parse([]byte(line), subscriptionfetch.XrayURIList)
	if err != nil || len(proxies) != 1 {
		t.Fatalf("unexpected parse result: %v, %+v", err, proxies)
	}
	config := proxies[0].Params["config"].(map[string]any)
	outbounds := config["outbounds"].([]map[string]any)
	streamSettings := outbounds[0]["streamSettings"].(map[string]any)
	xhttpSettings := streamSettings["xhttpSettings"].(map[string]any)
	if _, present := xhttpSettings["extra"]; present {
		t.Fatalf("expected no extra key when the URI carries none, got %+v", xhttpSettings)
	}
	// mode defaults to "auto" when the URI doesn't specify one.
	if xhttpSettings["mode"] != "auto" {
		t.Fatalf("unexpected default mode: %+v", xhttpSettings)
	}
}

// Regression for the other half of the same report: plain tcp with
// http header obfuscation (headerType=http) also fell straight through
// streamSettingsFor's switch with no tcpSettings at all. Real URI from
// the report (security=none here -- no TLS/reality, http camouflage is
// the only obfuscation this entry has).
func TestParse_VlessTcpHttpHeaderObfuscation(t *testing.T) {
	line := "vless://521fdaab-83cc-45dd-b141-511e5a5659c8@tun5.momasvps.ir:6016?security=none&type=tcp&headerType=http&path=%2F&host=#test"
	proxies, err := subscriptionfetch.Parse([]byte(line), subscriptionfetch.XrayURIList)
	if err != nil || len(proxies) != 1 {
		t.Fatalf("unexpected parse result: %v, %+v", err, proxies)
	}
	config := proxies[0].Params["config"].(map[string]any)
	outbounds := config["outbounds"].([]map[string]any)
	streamSettings := outbounds[0]["streamSettings"].(map[string]any)
	tcpSettings, ok := streamSettings["tcpSettings"].(map[string]any)
	if !ok {
		t.Fatalf("expected tcpSettings to be present, got %+v", streamSettings)
	}
	header, ok := tcpSettings["header"].(map[string]any)
	if !ok || header["type"] != "http" {
		t.Fatalf("unexpected header: %+v", tcpSettings)
	}
	request, ok := header["request"].(map[string]any)
	if !ok {
		t.Fatalf("expected header.request to be present, got %+v", header)
	}
	if paths, ok := request["path"].([]string); !ok || len(paths) != 1 || paths[0] != "/" {
		t.Fatalf("unexpected request path: %+v", request)
	}
}

// A "tcp" entry with no headerType at all (the overwhelmingly common
// case -- plain TCP, no camouflage) must not gain a spurious tcpSettings
// block just because the switch now has a "tcp" case.
func TestParse_VlessTcpNoHeaderType(t *testing.T) {
	line := "vless://521fdaab-83cc-45dd-b141-511e5a5659c8@example.com:443?security=none&type=tcp#test"
	proxies, err := subscriptionfetch.Parse([]byte(line), subscriptionfetch.XrayURIList)
	if err != nil || len(proxies) != 1 {
		t.Fatalf("unexpected parse result: %v, %+v", err, proxies)
	}
	config := proxies[0].Params["config"].(map[string]any)
	outbounds := config["outbounds"].([]map[string]any)
	streamSettings := outbounds[0]["streamSettings"].(map[string]any)
	if _, present := streamSettings["tcpSettings"]; present {
		t.Fatalf("expected no tcpSettings for a plain tcp entry, got %+v", streamSettings)
	}
}

// Regression: alpn/pinnedPeerCertSha256/verifyPeerCertByName -- tls's
// own modern replacement for the "allowInsecure" flag xray-core has
// since removed outright -- were never read from the URI at all,
// silently dropping a server's own TLS-hardening requirements. Confirmed
// field-for-field against kutovoys/xray-checker's own generation
// (github.com/xtls/libxray, the same org that maintains xray-core
// itself) as the reference for the correct shape.
func TestParse_VlessTlsAlpnAndPinnedCert(t *testing.T) {
	line := "vless://521fdaab-83cc-45dd-b141-511e5a5659c8@example.com:443?security=tls&type=tcp&sni=example.com&alpn=h2,http%2F1.1&pcs=deadbeef&vcn=example.org#test"
	proxies, err := subscriptionfetch.Parse([]byte(line), subscriptionfetch.XrayURIList)
	if err != nil || len(proxies) != 1 {
		t.Fatalf("unexpected parse result: %v, %+v", err, proxies)
	}
	config := proxies[0].Params["config"].(map[string]any)
	outbounds := config["outbounds"].([]map[string]any)
	streamSettings := outbounds[0]["streamSettings"].(map[string]any)
	tlsSettings, ok := streamSettings["tlsSettings"].(map[string]any)
	if !ok {
		t.Fatalf("expected tlsSettings to be present, got %+v", streamSettings)
	}
	alpn, ok := tlsSettings["alpn"].([]string)
	if !ok || len(alpn) != 2 || alpn[0] != "h2" || alpn[1] != "http/1.1" {
		t.Fatalf("unexpected alpn: %+v", tlsSettings)
	}
	if tlsSettings["pinnedPeerCertSha256"] != "deadbeef" {
		t.Fatalf("unexpected pinnedPeerCertSha256 (short pcs alias): %+v", tlsSettings)
	}
	if tlsSettings["verifyPeerCertByName"] != "example.org" {
		t.Fatalf("unexpected verifyPeerCertByName (short vcn alias): %+v", tlsSettings)
	}
}

// The long-form param names must work too, not just the short aliases.
func TestParse_VlessTlsPinnedCertLongFormNames(t *testing.T) {
	line := "vless://521fdaab-83cc-45dd-b141-511e5a5659c8@example.com:443?security=tls&type=tcp&pinnedPeerCertSha256=cafef00d&verifyPeerCertByName=example.net#test"
	proxies, err := subscriptionfetch.Parse([]byte(line), subscriptionfetch.XrayURIList)
	if err != nil || len(proxies) != 1 {
		t.Fatalf("unexpected parse result: %v, %+v", err, proxies)
	}
	config := proxies[0].Params["config"].(map[string]any)
	outbounds := config["outbounds"].([]map[string]any)
	tlsSettings := outbounds[0]["streamSettings"].(map[string]any)["tlsSettings"].(map[string]any)
	if tlsSettings["pinnedPeerCertSha256"] != "cafef00d" {
		t.Fatalf("unexpected pinnedPeerCertSha256: %+v", tlsSettings)
	}
	if tlsSettings["verifyPeerCertByName"] != "example.net" {
		t.Fatalf("unexpected verifyPeerCertByName: %+v", tlsSettings)
	}
}

// Regression: type=http/h2 fell straight through streamSettingsFor's
// switch with no httpSettings at all -- the exact same silent-failure
// shape xhttp had before it was fixed. host may be a comma-separated
// list (xray-core's own httpSettings.host accepts several).
func TestParse_VlessHttpTransport(t *testing.T) {
	line := "vless://521fdaab-83cc-45dd-b141-511e5a5659c8@example.com:443?security=none&type=http&path=%2Fapi&host=a.example.com,b.example.com#test"
	proxies, err := subscriptionfetch.Parse([]byte(line), subscriptionfetch.XrayURIList)
	if err != nil || len(proxies) != 1 {
		t.Fatalf("unexpected parse result: %v, %+v", err, proxies)
	}
	config := proxies[0].Params["config"].(map[string]any)
	outbounds := config["outbounds"].([]map[string]any)
	streamSettings := outbounds[0]["streamSettings"].(map[string]any)
	httpSettings, ok := streamSettings["httpSettings"].(map[string]any)
	if !ok {
		t.Fatalf("expected httpSettings to be present, got %+v", streamSettings)
	}
	if httpSettings["path"] != "/api" {
		t.Fatalf("unexpected path: %+v", httpSettings)
	}
	hosts, ok := httpSettings["host"].([]string)
	if !ok || len(hosts) != 2 || hosts[0] != "a.example.com" || hosts[1] != "b.example.com" {
		t.Fatalf("unexpected host list: %+v", httpSettings)
	}
}

// Regression: type=httpupgrade had the same gap as http/h2 above.
func TestParse_VlessHttpUpgradeTransport(t *testing.T) {
	line := "vless://521fdaab-83cc-45dd-b141-511e5a5659c8@example.com:443?security=none&type=httpupgrade&path=%2Fup&host=up.example.com#test"
	proxies, err := subscriptionfetch.Parse([]byte(line), subscriptionfetch.XrayURIList)
	if err != nil || len(proxies) != 1 {
		t.Fatalf("unexpected parse result: %v, %+v", err, proxies)
	}
	config := proxies[0].Params["config"].(map[string]any)
	outbounds := config["outbounds"].([]map[string]any)
	streamSettings := outbounds[0]["streamSettings"].(map[string]any)
	httpupgradeSettings, ok := streamSettings["httpupgradeSettings"].(map[string]any)
	if !ok {
		t.Fatalf("expected httpupgradeSettings to be present, got %+v", streamSettings)
	}
	if httpupgradeSettings["path"] != "/up" {
		t.Fatalf("unexpected path: %+v", httpupgradeSettings)
	}
	if httpupgradeSettings["host"] != "up.example.com" {
		t.Fatalf("unexpected host: %+v", httpupgradeSettings)
	}
}

// Regression: grpc's own multiMode flag (mode=multi) was never read,
// silently degrading to grpc's regular single-mode streaming even when
// the server actually runs multi mode.
func TestParse_VlessGrpcMultiMode(t *testing.T) {
	line := "vless://521fdaab-83cc-45dd-b141-511e5a5659c8@example.com:443?security=none&type=grpc&serviceName=svc&mode=multi#test"
	proxies, err := subscriptionfetch.Parse([]byte(line), subscriptionfetch.XrayURIList)
	if err != nil || len(proxies) != 1 {
		t.Fatalf("unexpected parse result: %v, %+v", err, proxies)
	}
	config := proxies[0].Params["config"].(map[string]any)
	outbounds := config["outbounds"].([]map[string]any)
	grpcSettings := outbounds[0]["streamSettings"].(map[string]any)["grpcSettings"].(map[string]any)
	if grpcSettings["multiMode"] != true {
		t.Fatalf("expected multiMode true, got %+v", grpcSettings)
	}
}

// A plain grpc entry (no mode=multi) must not gain a spurious
// multiMode key -- grpc's regular single-mode streaming is xray-core's
// own default and the overwhelmingly common case.
func TestParse_VlessGrpcNoMultiMode(t *testing.T) {
	line := "vless://521fdaab-83cc-45dd-b141-511e5a5659c8@example.com:443?security=none&type=grpc&serviceName=svc#test"
	proxies, err := subscriptionfetch.Parse([]byte(line), subscriptionfetch.XrayURIList)
	if err != nil || len(proxies) != 1 {
		t.Fatalf("unexpected parse result: %v, %+v", err, proxies)
	}
	config := proxies[0].Params["config"].(map[string]any)
	outbounds := config["outbounds"].([]map[string]any)
	grpcSettings := outbounds[0]["streamSettings"].(map[string]any)["grpcSettings"].(map[string]any)
	if _, present := grpcSettings["multiMode"]; present {
		t.Fatalf("expected no multiMode key for a plain grpc entry, got %+v", grpcSettings)
	}
	if grpcSettings["serviceName"] != "svc" {
		t.Fatalf("unexpected serviceName: %+v", grpcSettings)
	}
}

// Regression: grpcSettings.serviceName used to be read from the URI's
// path= param unconditionally -- real generators overwhelmingly use a
// dedicated serviceName= instead (grpc has no concept of an HTTP path
// at all), so a real grpc entry's serviceName was silently empty every
// time path= was absent, which is the common case for grpc URIs. path
// is now only a fallback for the rare generator that happens to
// overload it that way, confirmed here by omitting serviceName=
// entirely.
func TestParse_VlessGrpcServiceNameFallsBackToPath(t *testing.T) {
	line := "vless://521fdaab-83cc-45dd-b141-511e5a5659c8@example.com:443?security=none&type=grpc&path=fallback-svc#test"
	proxies, err := subscriptionfetch.Parse([]byte(line), subscriptionfetch.XrayURIList)
	if err != nil || len(proxies) != 1 {
		t.Fatalf("unexpected parse result: %v, %+v", err, proxies)
	}
	config := proxies[0].Params["config"].(map[string]any)
	outbounds := config["outbounds"].([]map[string]any)
	grpcSettings := outbounds[0]["streamSettings"].(map[string]any)["grpcSettings"].(map[string]any)
	if grpcSettings["serviceName"] != "fallback-svc" {
		t.Fatalf("expected serviceName to fall back to path, got %+v", grpcSettings)
	}
}

// serviceName= must win over path= when a URI carries both (an
// unlikely but possible generator quirk) -- serviceName is the
// dedicated, authoritative param for grpc.
func TestParse_VlessGrpcServiceNameWinsOverPath(t *testing.T) {
	line := "vless://521fdaab-83cc-45dd-b141-511e5a5659c8@example.com:443?security=none&type=grpc&serviceName=real-svc&path=stale-path#test"
	proxies, err := subscriptionfetch.Parse([]byte(line), subscriptionfetch.XrayURIList)
	if err != nil || len(proxies) != 1 {
		t.Fatalf("unexpected parse result: %v, %+v", err, proxies)
	}
	config := proxies[0].Params["config"].(map[string]any)
	outbounds := config["outbounds"].([]map[string]any)
	grpcSettings := outbounds[0]["streamSettings"].(map[string]any)["grpcSettings"].(map[string]any)
	if grpcSettings["serviceName"] != "real-svc" {
		t.Fatalf("expected serviceName to win over path, got %+v", grpcSettings)
	}
}

func TestParse_Trojan(t *testing.T) {
	line := "trojan://s3cr3t@9.10.11.12:443?sni=trojan.example#my-trojan"
	proxies, err := subscriptionfetch.Parse([]byte(line), subscriptionfetch.XrayURIList)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(proxies) != 1 || proxies[0].Prober != "xray" {
		t.Fatalf("unexpected proxies: %+v", proxies)
	}
	config := proxies[0].Params["config"].(map[string]any)
	outbounds := config["outbounds"].([]map[string]any)
	settings := outbounds[0]["settings"].(map[string]any)
	servers := settings["servers"].([]map[string]any)
	if servers[0]["password"] != "s3cr3t" {
		t.Fatalf("unexpected trojan settings: %+v", settings)
	}
}

func TestParse_ShadowsocksSIP002(t *testing.T) {
	userinfo := base64.StdEncoding.EncodeToString([]byte("aes-256-gcm:p@ssw0rd"))
	line := "ss://" + userinfo + "@13.14.15.16:8388#my-ss"
	proxies, err := subscriptionfetch.Parse([]byte(line), subscriptionfetch.XrayURIList)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(proxies) != 1 || proxies[0].Name != "my-ss" {
		t.Fatalf("unexpected proxies: %+v", proxies)
	}
}

func TestParse_ShadowsocksLegacy(t *testing.T) {
	whole := base64.StdEncoding.EncodeToString([]byte("aes-256-gcm:p@ssw0rd@17.18.19.20:8388"))
	line := "ss://" + whole + "#legacy-ss"
	proxies, err := subscriptionfetch.Parse([]byte(line), subscriptionfetch.XrayURIList)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(proxies) != 1 || proxies[0].Name != "legacy-ss" {
		t.Fatalf("unexpected proxies: %+v", proxies)
	}
}

func TestParse_PlainProxyList(t *testing.T) {
	content := "21.22.23.24:1080\nsocks5://user:pass@25.26.27.28:1080#my-socks\n"
	proxies, err := subscriptionfetch.Parse([]byte(content), subscriptionfetch.ProxyList)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(proxies) != 2 {
		t.Fatalf("expected 2 proxies, got %d: %+v", len(proxies), proxies)
	}
	if proxies[0].Prober != "tcp" || proxies[0].Target != "21.22.23.24:1080" {
		t.Fatalf("unexpected first proxy: %+v", proxies[0])
	}
	if proxies[1].Name != "my-socks" || proxies[1].Params["username"] != "user" {
		t.Fatalf("unexpected second proxy: %+v", proxies[1])
	}
}

func TestParse_XrayJSON(t *testing.T) {
	content := `{
		"outbounds": [
			{"tag": "proxy", "protocol": "vmess", "settings": {"vnext": [{"address": "1.2.3.4", "port": 443}]}},
			{"tag": "direct", "protocol": "freedom"},
			{"tag": "blocked", "protocol": "blackhole"}
		]
	}`
	proxies, err := subscriptionfetch.Parse([]byte(content), subscriptionfetch.XrayJSON)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(proxies) != 1 || proxies[0].Name != "proxy" {
		t.Fatalf("expected only the real proxy outbound, got %+v", proxies)
	}
}

func TestParse_MalformedLineIsSkippedNotFatal(t *testing.T) {
	content := "vmess://not-valid-base64!!!\ntrojan://s3cr3t@9.10.11.12:443#good-trojan\n"
	proxies, err := subscriptionfetch.Parse([]byte(content), subscriptionfetch.XrayURIList)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(proxies) != 1 || proxies[0].Name != "good-trojan" {
		t.Fatalf("expected the one good line to survive, got %+v", proxies)
	}
}
