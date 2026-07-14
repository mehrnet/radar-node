package proxytest

import "testing"

func TestRemapInboundPort_MatchesPortKey(t *testing.T) {
	config := map[string]any{
		"inbounds": []any{
			map[string]any{"port": 1234.0, "protocol": "socks"},
		},
	}
	remapped, ok := remapInboundPort(config, 1234, 5555)
	if !ok {
		t.Fatal("expected a match")
	}
	inbounds := remapped["inbounds"].([]any)
	got := inbounds[0].(map[string]any)["port"]
	if got != 5555.0 {
		t.Fatalf("expected remapped port 5555, got %v", got)
	}
}

func TestRemapInboundPort_MatchesListenPortKey(t *testing.T) {
	config := map[string]any{
		"inbounds": []any{
			map[string]any{"listen_port": 1234.0, "type": "socks"},
		},
	}
	remapped, ok := remapInboundPort(config, 1234, 5555)
	if !ok {
		t.Fatal("expected a match")
	}
	inbounds := remapped["inbounds"].([]any)
	got := inbounds[0].(map[string]any)["listen_port"]
	if got != 5555.0 {
		t.Fatalf("expected remapped listen_port 5555, got %v", got)
	}
}

func TestRemapInboundPort_NoMatchReturnsFalse(t *testing.T) {
	config := map[string]any{
		"inbounds": []any{
			map[string]any{"port": 1234.0},
		},
	}
	_, ok := remapInboundPort(config, 9999, 5555)
	if ok {
		t.Fatal("expected no match for a declared port that isn't in any inbound")
	}
}

func TestRemapInboundPort_OnlyRemapsFirstMatch(t *testing.T) {
	config := map[string]any{
		"inbounds": []any{
			map[string]any{"port": 1234.0, "tag": "a"},
			map[string]any{"port": 1234.0, "tag": "b"},
		},
	}
	remapped, ok := remapInboundPort(config, 1234, 5555)
	if !ok {
		t.Fatal("expected a match")
	}
	inbounds := remapped["inbounds"].([]any)
	first := inbounds[0].(map[string]any)
	second := inbounds[1].(map[string]any)
	if first["port"] != 5555.0 {
		t.Fatalf("expected the first matching inbound to be remapped, got %v", first["port"])
	}
	if second["port"] != 1234.0 {
		t.Fatalf("expected only the first match to be remapped, second inbound got %v", second["port"])
	}
}

func TestRemapInboundPort_DoesNotMutateInput(t *testing.T) {
	inbound := map[string]any{"port": 1234.0}
	config := map[string]any{"inbounds": []any{inbound}}
	remapInboundPort(config, 1234, 5555)
	if inbound["port"] != 1234.0 {
		t.Fatalf("expected the original inbound map to be left untouched, got %v", inbound["port"])
	}
}

func TestRemapInboundPort_MissingInboundsReturnsFalse(t *testing.T) {
	_, ok := remapInboundPort(map[string]any{}, 1234, 5555)
	if ok {
		t.Fatal("expected no match when config has no inbounds at all")
	}
}
