package netguard

import (
	"net/netip"
	"testing"
)

func TestIsPublic(t *testing.T) {
	cases := map[string]bool{
		"8.8.8.8":         true,
		"2606:4700::1111": true,
		"127.0.0.1":       false,
		"10.1.2.3":        false,
		"172.16.0.1":      false,
		"192.168.1.1":     false,
		"169.254.169.254": false,
		"100.64.0.1":      false,
		"0.0.0.0":         false,
		"224.0.0.1":       false,
		"::1":             false,
		"fe80::1":         false,
		"fd00::1":         false,
		"::ffff:10.0.0.1": false,
		"64:ff9b::a00:1":  false,
		"203.0.113.10":    false,
	}
	for in, want := range cases {
		if got := IsPublic(netip.MustParseAddr(in)); got != want {
			t.Errorf("IsPublic(%s) = %v, want %v", in, got, want)
		}
	}
}

func TestIsLocalHostname(t *testing.T) {
	for _, h := range []string{"localhost", "LOCALHOST.", "app.localhost"} {
		if !IsLocalHostname(h) {
			t.Errorf("%q should be local", h)
		}
	}
	if IsLocalHostname("localhost.example.com") {
		t.Error("localhost.example.com is not local")
	}
}
