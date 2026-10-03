package scim

// The caller address the token endpoint's failure limiter keys on.
//
// The limiter key is (client id, caller address). The client id is public, so
// the address is what stops one caller from locking another out. That only
// holds if the address is the REAL client: behind an ingress the socket peer is
// the proxy, and every caller would share one key.
//
// This resolver is built at the composition root from the deployment's trusted
// proxy CIDRs on EVERY authentication plane (Form, OIDC, SAML), not only on the
// Form plane. X-Forwarded-For is read only when the socket peer is inside those
// CIDRs, and the client is the rightmost hop that is NOT a trusted proxy. An
// untrusted peer's X-Forwarded-For is ignored, and the peer itself is the
// client.
//
// When the address cannot be known — no CIDRs are configured, or a trusted
// proxy sent no usable X-Forwarded-For, or every hop is a trusted proxy —
// Resolve reports false. The token endpoint then does NOT block before the
// secret check: it verifies the secret first, so a correct secret always gets a
// token, and it throttles only failures.

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"strings"
)

const (
	maxClientAddressCIDRs  = 64
	maxForwardedForBytes   = 2048
	maxForwardedForHops    = 32
	forwardedForHeaderName = "X-Forwarded-For"
)

// ClientAddressResolver resolves the caller address of a request.
type ClientAddressResolver struct {
	trusted []netip.Prefix
}

// ErrInvalidTrustedProxyCIDR refuses a CIDR list the resolver cannot use.
var ErrInvalidTrustedProxyCIDR = errors.New("invalid trusted proxy CIDR")

// NewClientAddressResolver builds a resolver. An empty list is valid and means
// "no proxy is trusted": Resolve then reports false for every request, because
// a deployment without the list cannot tell a proxy from a client.
func NewClientAddressResolver(cidrs []string) (*ClientAddressResolver, error) {
	if len(cidrs) > maxClientAddressCIDRs {
		return nil, fmt.Errorf("%w: more than %d entries", ErrInvalidTrustedProxyCIDR, maxClientAddressCIDRs)
	}
	trusted := make([]netip.Prefix, 0, len(cidrs))
	seen := map[netip.Prefix]bool{}
	for _, raw := range cidrs {
		prefix, err := netip.ParsePrefix(strings.TrimSpace(raw))
		if err != nil {
			return nil, fmt.Errorf("%w: %q", ErrInvalidTrustedProxyCIDR, raw)
		}
		prefix = prefix.Masked()
		if !seen[prefix] {
			seen[prefix] = true
			trusted = append(trusted, prefix)
		}
	}
	return &ClientAddressResolver{trusted: trusted}, nil
}

// Configured reports whether any trusted proxy CIDR is set.
func (r *ClientAddressResolver) Configured() bool { return r != nil && len(r.trusted) > 0 }

// Resolve returns the caller address and whether it is the real client.
func (r *ClientAddressResolver) Resolve(request *http.Request) (string, bool) {
	if !r.Configured() || request == nil {
		return "", false
	}
	peer, ok := parseAddress(request.RemoteAddr)
	if !ok {
		return "", false
	}
	if !r.isTrusted(peer) {
		// A direct caller: its own X-Forwarded-For is never believed.
		return peer.String(), true
	}
	values := request.Header.Values(forwardedForHeaderName)
	if len(values) != 1 || values[0] == "" || len(values[0]) > maxForwardedForBytes {
		return "", false
	}
	parts := strings.Split(values[0], ",")
	if len(parts) > maxForwardedForHops {
		return "", false
	}
	hops := make([]netip.Addr, 0, len(parts))
	for _, part := range parts {
		address, err := netip.ParseAddr(strings.TrimSpace(part))
		if err != nil || address.Zone() != "" {
			return "", false
		}
		hops = append(hops, address.Unmap())
	}
	for index := len(hops) - 1; index >= 0; index-- {
		if !r.isTrusted(hops[index]) {
			return hops[index].String(), true
		}
	}
	// Every hop is a trusted proxy: no client address is known.
	return "", false
}

func (r *ClientAddressResolver) isTrusted(address netip.Addr) bool {
	for _, prefix := range r.trusted {
		if prefix.Contains(address) {
			return true
		}
	}
	return false
}

func parseAddress(value string) (netip.Addr, bool) {
	host, _, err := net.SplitHostPort(value)
	if err != nil {
		host = value
	}
	address, err := netip.ParseAddr(host)
	if err != nil || address.Zone() != "" {
		return netip.Addr{}, false
	}
	return address.Unmap(), true
}
