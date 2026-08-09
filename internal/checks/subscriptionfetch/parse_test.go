package subscriptionfetch_test

import (
	"encoding/base64"
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
