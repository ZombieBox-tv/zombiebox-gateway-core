package httpclient

import (
	"net"
	"testing"
)

func TestSharedURLAddressPolicy(t *testing.T) {
	for _, raw := range []string{"127.0.0.1", "10.1.2.3", "169.254.169.254", "100.64.0.1", "192.168.1.1", "::1", "::ffff:127.0.0.1", "fc00::1", "fe80::1", "224.0.0.1", "198.18.0.1", "64:ff9b::7f00:1"} {
		if publicIP(net.ParseIP(raw)) {
			t.Fatal("private/special address accepted", raw)
		}
	}
	if !publicIP(net.ParseIP("8.8.8.8")) || !publicIP(net.ParseIP("2606:4700::1111")) {
		t.Fatal("public address rejected")
	}
}
