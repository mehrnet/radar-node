package proxytest

import (
	"testing"

	"github.com/mehrnet/radar-node/internal/probe"
)

func TestExtractParams_RejectsMissingConfig(t *testing.T) {
	_, _, err := extractParams(probe.Options{Params: map[string]any{"socks_port": 1234.0}})
	if err == nil {
		t.Fatal("expected an error for a missing config param")
	}
}

func TestExtractParams_RejectsMissingSocksPort(t *testing.T) {
	_, _, err := extractParams(probe.Options{Params: map[string]any{"config": map[string]any{}}})
	if err == nil {
		t.Fatal("expected an error for a missing socks_port param")
	}
}

func TestExtractParams_AcceptsValidParams(t *testing.T) {
	config, port, err := extractParams(probe.Options{Params: map[string]any{
		"config":     map[string]any{"inbounds": []any{}},
		"socks_port": 1234.0,
	}})
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if port != 1234.0 {
		t.Fatalf("expected port 1234, got %v", port)
	}
	if config == nil {
		t.Fatal("expected a non-nil config")
	}
}

func TestType(t *testing.T) {
	if NewXray().Type() != "xray_proxy_test" {
		t.Fatalf("unexpected xray Type(): %s", NewXray().Type())
	}
	if NewSingBox().Type() != "singbox_proxy_test" {
		t.Fatalf("unexpected singbox Type(): %s", NewSingBox().Type())
	}
}
