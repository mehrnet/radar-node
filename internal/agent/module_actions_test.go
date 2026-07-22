package agent

import (
	"reflect"
	"testing"
)

func TestModuleActionFlags_MapsInstallAndRemoveForEachEngine(t *testing.T) {
	got := moduleActionFlags([]string{"install_xray", "remove_wireguard", "install_openvpn"})
	want := []string{"--install-xray", "--remove-wireguard", "--install-openvpn"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestModuleActionFlags_DropsUnrecognizedActionsRatherThanFailing(t *testing.T) {
	got := moduleActionFlags([]string{"install_xray", "reboot_everything"})
	want := []string{"--install-xray"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestModuleActionFlags_EmptyInputYieldsEmptyOutput(t *testing.T) {
	got := moduleActionFlags(nil)
	if len(got) != 0 {
		t.Fatalf("expected no flags, got %v", got)
	}
}
