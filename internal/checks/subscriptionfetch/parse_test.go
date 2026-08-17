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
