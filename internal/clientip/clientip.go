// Package clientip reads the client's address from what a proxy in front of
// the router adds. Which source to trust depends on how requests arrive:
//
//   - Public Function URL: the event's sourceIp, the TCP peer. Headers are
//     whatever the client sent.
//   - ALB: the last X-Forwarded-For entry, which the load balancer appends.
//     Every earlier entry came from the client.
//   - CloudFront in front of a Function URL: CloudFront-Viewer-Address.
//     Only safe when the Function URL accepts requests from CloudFront alone.
package clientip

import (
	"errors"
	"fmt"
	"net/netip"
	"strings"
)

// LastForwardedFor returns the last entry of X-Forwarded-For, given every
// value of the header in order. That is the address that connected to the
// proxy closest to the router; anything before it can be forged.
func LastForwardedFor(values ...string) (netip.Addr, error) {
	if len(values) == 0 {
		return netip.Addr{}, errors.New("no X-Forwarded-For header")
	}
	last := values[len(values)-1]
	if i := strings.LastIndexByte(last, ','); i >= 0 {
		last = last[i+1:]
	}
	addr, err := netip.ParseAddr(strings.TrimSpace(last))
	if err != nil {
		return netip.Addr{}, fmt.Errorf("X-Forwarded-For: last entry: %w", err)
	}
	return addr, nil
}

// ViewerAddress parses CloudFront-Viewer-Address, which is the viewer's
// address and source port, as in 198.51.100.10:46532. IPv6 addresses come
// without brackets, so the port is always split off at the last colon.
func ViewerAddress(value string) (netip.Addr, error) {
	if ap, err := netip.ParseAddrPort(value); err == nil {
		return ap.Addr(), nil
	}
	i := strings.LastIndexByte(value, ':')
	if i < 0 {
		return netip.Addr{}, fmt.Errorf("CloudFront-Viewer-Address %q: no port", value)
	}
	ap, err := netip.ParseAddrPort("[" + value[:i] + "]" + value[i:])
	if err != nil {
		return netip.Addr{}, fmt.Errorf("CloudFront-Viewer-Address %q: %w", value, err)
	}
	return ap.Addr(), nil
}
