package utils

import (
	"errors"
	"net"
	"testing"
)

func TestHTTPURLTargetIPAllowed(t *testing.T) {
	tests := []struct {
		name    string
		address string
		allowed bool
	}{
		{name: "public IPv4", address: "1.1.1.1", allowed: true},
		{name: "public IPv6", address: "2606:4700:4700::1111", allowed: true},
		{name: "well-known NAT64", address: "64:ff9b::a9fe:a9fe", allowed: false},
		{name: "local-use NAT64", address: "64:ff9b:1::a9fe:a9fe", allowed: false},
		{name: "6to4 public IPv4 embedding", address: "2002:0101:0101::1", allowed: false},
		{name: "6to4 link-local IPv4 embedding", address: "2002:a9fe:a9fe::1", allowed: false},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ip := net.ParseIP(test.address)
			if ip == nil {
				t.Fatalf("ParseIP(%q) returned nil", test.address)
			}
			if got := HTTPURLTargetIPAllowed(ip); got != test.allowed {
				t.Fatalf("HTTPURLTargetIPAllowed(%q) = %t, want %t", test.address, got, test.allowed)
			}
		})
	}
}

func TestResolveAllowedHTTPURLRejectsSpecialIPv6Literals(t *testing.T) {
	for _, rawURL := range []string{
		"http://[64:ff9b:1::a9fe:a9fe]/metadata",
		"http://[2002:a9fe:a9fe::1]/metadata",
	} {
		t.Run(rawURL, func(t *testing.T) {
			_, _, err := ResolveAllowedHTTPURL(rawURL)
			if !errors.Is(err, ErrHTTPURLTargetNotAllowed) {
				t.Fatalf("ResolveAllowedHTTPURL(%q) error = %v, want %v", rawURL, err, ErrHTTPURLTargetNotAllowed)
			}
		})
	}
}
