package http

import "testing"

func TestTheSecretGoesToLoopbackAddressesOnly(t *testing.T) {
	for host, want := range map[string]bool{
		"127.0.0.1":    true,
		"::1":          true,
		"localhost":    true,
		"192.168.1.20": false,
		"10.0.0.1":     false,
		"example.com":  false,
		"":             false,
	} {
		if got := isLoopbackHost(host); got != want {
			t.Errorf("%q: got %v, want %v", host, got, want)
		}
	}
}
