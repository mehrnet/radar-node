package poolconfig

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func decodeJobs(t *testing.T, raw string) []job {
	t.Helper()
	var jobs []job
	if err := json.Unmarshal([]byte(raw), &jobs); err != nil {
		t.Fatalf("decode jobs: %v", err)
	}
	return jobs
}

func findOutbound(outbounds []map[string]any, tag string) map[string]any {
	for _, out := range outbounds {
		if out["tag"] == tag {
			return out
		}
	}
	return nil
}

func TestBuildConfigRemapsInboundsAndTagsRoutes(t *testing.T) {
	jobs := decodeJobs(t, `[
		{"index":0,"alloc_port":30001,"target":"https://test/","timeout_ms":12000,
		 "params":{"socks_port":10808,"config":{
			"inbounds":[{"port":10808,"listen":"127.0.0.1","protocol":"socks"}],
			"outbounds":[{"protocol":"vless","settings":{"vnext":[{"address":"a.example","port":443}]},"streamSettings":{"network":"ws","security":"tls"}}]
		}}},
		{"index":1,"alloc_port":30002,"timeout_ms":12000,
		 "params":{"socks_port":10808,"config":{
			"inbounds":[{"listen_port":10808,"protocol":"socks"}],
			"outbounds":[{"protocol":"trojan","settings":{"servers":[{"address":"b.example","port":8443}]},"streamSettings":{"network":"tcp","security":"tls"}}]
		}}}
	]`)

	merged := buildConfig(jobs, "")
	if len(merged.Inbounds) != 2 {
		t.Fatalf("inbounds = %d, want 2", len(merged.Inbounds))
	}
	if merged.Inbounds[0]["port"] != 30001 || merged.Inbounds[0]["tag"] != "job-0-in" {
		t.Fatalf("job 0 inbound = %+v", merged.Inbounds[0])
	}
	if merged.Inbounds[1]["listen_port"] != 30002 || merged.Inbounds[1]["tag"] != "job-1-in" {
		t.Fatalf("job 1 inbound = %+v", merged.Inbounds[1])
	}
	if len(merged.Routing.Rules) != 2 {
		t.Fatalf("rules = %d, want 2", len(merged.Routing.Rules))
	}
	if merged.Routing.Rules[0]["outboundTag"] != "job-0-out-0" || merged.Routing.Rules[1]["outboundTag"] != "job-1-out-0" {
		t.Fatalf("rules = %+v", merged.Routing.Rules)
	}
	if merged.Outbounds[0]["mux"] == nil {
		t.Fatalf("expected mux on a non-Reality, non-XHTTP outbound: %+v", merged.Outbounds[0])
	}
}

func TestBuildConfigSkipsXHTTPMuxAndFragmentsReality(t *testing.T) {
	jobs := decodeJobs(t, `[
		{"index":0,"alloc_port":30101,"params":{"socks_port":10808,"config":{
			"inbounds":[{"port":10808,"protocol":"socks"}],
			"outbounds":[{"protocol":"vless","streamSettings":{"network":"xhttp","security":"tls"}}]
		}}},
		{"index":1,"alloc_port":30102,"params":{"socks_port":10808,"config":{
			"inbounds":[{"port":10808,"protocol":"socks"}],
			"outbounds":[{"protocol":"vless","streamSettings":{"network":"tcp","security":"reality","sockopt":{"mark":1}}}]
		}}}
	]`)

	merged := buildConfig(jobs, "")
	xhttp := findOutbound(merged.Outbounds, "job-0-out-0")
	if xhttp == nil {
		t.Fatalf("missing xhttp outbound: %+v", merged.Outbounds)
	}
	if _, has := xhttp["mux"]; has {
		t.Fatalf("XHTTP must never get mux: %+v", xhttp)
	}

	reality := findOutbound(merged.Outbounds, "job-1-out-0")
	if reality == nil {
		t.Fatalf("missing reality outbound: %+v", merged.Outbounds)
	}
	if _, has := reality["mux"]; has {
		t.Fatalf("Reality must never get mux: %+v", reality)
	}
	stream := reality["streamSettings"].(map[string]any)
	sockopt := stream["sockopt"].(map[string]any)
	if sockopt["dialerProxy"] != "job-1-frag" {
		t.Fatalf("sockopt = %+v, want dialerProxy job-1-frag", sockopt)
	}
	if sockopt["mark"] != float64(1) {
		t.Fatalf("existing sockopt fields were lost: %+v", sockopt)
	}
	frag := findOutbound(merged.Outbounds, "job-1-frag")
	if frag == nil || frag["protocol"] != "freedom" {
		t.Fatalf("missing reality fragment outbound: %+v", merged.Outbounds)
	}
	settings := frag["settings"].(map[string]any)["fragment"].(map[string]any)
	if settings["packets"] != "tlshello" || settings["length"] != "10-100" || settings["interval"] != "10-20" {
		t.Fatalf("fragment settings = %+v", settings)
	}
}

func TestBuildConfigAppliesDomainStrategyEverywhere(t *testing.T) {
	jobs := decodeJobs(t, `[
		{"index":0,"alloc_port":30101,"params":{"socks_port":10808,"config":{
			"inbounds":[{"port":10808,"protocol":"socks"}],
			"outbounds":[{"protocol":"vless","streamSettings":{"network":"xhttp","security":"tls"}}]
		}}},
		{"index":1,"alloc_port":30102,"params":{"socks_port":10808,"config":{
			"inbounds":[{"port":10808,"protocol":"socks"}],
			"outbounds":[{"protocol":"vless","streamSettings":{"network":"tcp","security":"reality","sockopt":{"mark":1}}}]
		}}}
	]`)

	merged := buildConfig(jobs, "UseIPv4")
	for _, tag := range []string{"job-0-out-0", "job-1-out-0", "job-1-frag"} {
		out := findOutbound(merged.Outbounds, tag)
		if out == nil {
			t.Fatalf("missing outbound %s: %+v", tag, merged.Outbounds)
		}
		stream, _ := out["streamSettings"].(map[string]any)
		sockopt, _ := stream["sockopt"].(map[string]any)
		if sockopt["domainStrategy"] != "UseIPv4" {
			t.Fatalf("%s domainStrategy = %v, want UseIPv4 (outbound %+v)", tag, sockopt["domainStrategy"], out)
		}
	}
	realitySockopt := findOutbound(merged.Outbounds, "job-1-out-0")["streamSettings"].(map[string]any)["sockopt"].(map[string]any)
	if realitySockopt["dialerProxy"] != "job-1-frag" || realitySockopt["mark"] != float64(1) {
		t.Fatalf("existing sockopt keys lost: %+v", realitySockopt)
	}
}

func TestDomainStrategyFromEnv(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  string
		err   bool
	}{
		{"", "", false},
		{"use_ipv4", "UseIPv4", false},
		{"UseIPv4", "UseIPv4", false},
		{"use_ip", "UseIP", false},
		{"use_ipv6", "UseIPv6", false},
		{"as_is", "AsIs", false},
		{"nonsense", "", true},
	} {
		t.Run(tc.value, func(t *testing.T) {
			t.Setenv("RADAR_DOMAIN_STRATEGY", tc.value)
			got, err := domainStrategyFromEnv()
			if tc.err {
				if err == nil {
					t.Fatalf("expected an error for %q", tc.value)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("domainStrategyFromEnv(%q) = %q, %v; want %q", tc.value, got, err, tc.want)
			}
		})
	}
}

func TestBuildConfigSkipsMalformedJobs(t *testing.T) {
	jobs := decodeJobs(t, `[
		{"index":0,"alloc_port":30201,"params":{"socks_port":10808,"config":{"inbounds":[],"outbounds":[{"protocol":"vless"}]}}},
		{"index":1,"alloc_port":30202,"params":{"socks_port":10808,"config":{"inbounds":[{"port":9999}],"outbounds":[{"protocol":"vless"}]}}},
		{"index":2,"alloc_port":30203,"params":{"config":{"inbounds":[{"port":10808}],"outbounds":[{"protocol":"vless"}]}}},
		{"index":3,"alloc_port":30204,"params":{"socks_port":10808,"config":{"inbounds":[{"port":10808}],"outbounds":[{"protocol":"vless"}]}}}
	]`)

	merged := buildConfig(jobs, "")
	if len(merged.Inbounds) != 1 || merged.Inbounds[0]["tag"] != "job-3-in" {
		t.Fatalf("only the well-formed job should survive: %+v", merged.Inbounds)
	}
	if len(merged.Routing.Rules) != 1 || merged.Routing.Rules[0]["inboundTag"].([]string)[0] != "job-3-in" {
		t.Fatalf("rules = %+v", merged.Routing.Rules)
	}
}

func TestRunWritesCompactConfig(t *testing.T) {
	dir := t.TempDir()
	jobsPath := filepath.Join(dir, "jobs.json")
	outPath := filepath.Join(dir, "config.json")
	jobs := `[{"index":0,"alloc_port":30301,"params":{"socks_port":10808,"config":{"inbounds":[{"port":10808}],"outbounds":[{"protocol":"vless"}]}}}]`
	if err := os.WriteFile(jobsPath, []byte(jobs), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Run([]string{"--jobs", jobsPath, "--out", outPath}, os.Stdout); err != nil {
		t.Fatalf("Run: %v", err)
	}
	raw, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatal(err)
	}
	var merged mergedConfig
	if err := json.Unmarshal(raw, &merged); err != nil {
		t.Fatalf("output is not valid JSON: %v (%s)", err, raw)
	}
	if len(merged.Inbounds) != 1 || merged.Inbounds[0]["port"] != float64(30301) {
		t.Fatalf("merged = %+v", merged)
	}
}

func TestRunRequiresBothPaths(t *testing.T) {
	if err := Run([]string{"--jobs", "x"}, os.Stdout); err == nil {
		t.Fatal("expected an error without --out")
	}
}
