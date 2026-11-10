package clientip_test

import (
	"net/netip"
	"testing"

	"github.com/ElijahGlover/switchyard/internal/clientip"
)

func TestLastForwardedFor(t *testing.T) {
	tests := []struct {
		name   string
		values []string
		want   string
	}{
		{"one address", []string{"198.51.100.1"}, "198.51.100.1"},
		{"the proxy's entry is the last one", []string{"203.0.113.7, 198.51.100.1"}, "198.51.100.1"},
		{"no space after the comma", []string{"203.0.113.7,198.51.100.1"}, "198.51.100.1"},
		{"whatever the client sent first is ignored", []string{"not-an-ip, , 192.0.2.1,198.51.100.1"}, "198.51.100.1"},
		{"IPv6", []string{"203.0.113.7, 2001:db8::1"}, "2001:db8::1"},
		{"repeated headers: the last entry of the last one", []string{"203.0.113.7", "203.0.113.8, 198.51.100.1"}, "198.51.100.1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := clientip.LastForwardedFor(tt.values...)
			if err != nil || got != netip.MustParseAddr(tt.want) {
				t.Errorf("got %v, %v; want %s", got, err, tt.want)
			}
		})
	}
}

func TestLastForwardedForRejects(t *testing.T) {
	tests := []struct {
		name   string
		values []string
	}{
		{"no header", nil},
		{"empty header", []string{""}},
		{"trailing comma", []string{"198.51.100.1,"}},
		{"last entry not an address", []string{"198.51.100.1, unknown"}},
		{"last entry with a port", []string{"198.51.100.1:443"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got, err := clientip.LastForwardedFor(tt.values...); err == nil {
				t.Errorf("got %v, want an error", got)
			}
		})
	}
}

func TestViewerAddress(t *testing.T) {
	tests := []struct {
		name, value, want string
	}{
		{"IPv4 and port", "198.51.100.10:46532", "198.51.100.10"},
		{"IPv6 and port", "2001:db8::1:46532", "2001:db8::1"},
		// Without stripping the port first this would parse as the
		// address 2001:db8::1:8080.
		{"IPv6 whose port looks like a group", "2001:db8::1:8080", "2001:db8::1"},
		{"bracketed IPv6", "[2001:db8::1]:46532", "2001:db8::1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := clientip.ViewerAddress(tt.value)
			if err != nil || got != netip.MustParseAddr(tt.want) {
				t.Errorf("got %v, %v; want %s", got, err, tt.want)
			}
		})
	}
}

func TestViewerAddressRejects(t *testing.T) {
	for _, value := range []string{"", "198.51.100.10", "198.51.100.10:port", "not-an-ip:443", ":443"} {
		t.Run(value, func(t *testing.T) {
			if got, err := clientip.ViewerAddress(value); err == nil {
				t.Errorf("got %v, want an error", got)
			}
		})
	}
}
