package addrcheck

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"
)

const (
	// DNSLookupTimeout bounds ONE resolution.
	DNSLookupTimeout = 5 * time.Second
	// DialTimeout bounds ONE literal-IP TCP connect.
	DialTimeout = 10 * time.Second
	// MaxResolvedAddresses bounds one resolution. A larger answer is refused
	// rather than truncated, so the dial loop never walks an unbounded list.
	MaxResolvedAddresses = 32
)

// Resolver is the seam a boundary resolves host names through. Tests supply a
// fake so a check never depends on live DNS. *net.Resolver implements it.
type Resolver interface {
	LookupIPAddr(ctx context.Context, host string) ([]net.IPAddr, error)
}

// Resolve resolves host to at most MaxResolvedAddresses addresses. An IP
// literal resolves to itself. "localhost" is pinned to 127.0.0.1 so a hosts
// file cannot make it mean something else. Zoned answers are dropped: a zone
// only qualifies a link-local or interface-scoped address, never a
// destination a boundary could permit.
//
// The result is not filtered: the caller applies Filter (or its own stricter
// rule) to every address before it dials any of them.
func Resolve(ctx context.Context, resolver Resolver, host string) ([]net.IP, error) {
	if ip := net.ParseIP(host); ip != nil {
		return []net.IP{ip}, nil
	}
	if strings.EqualFold(host, "localhost") {
		return []net.IP{net.IPv4(127, 0, 0, 1)}, nil
	}
	if resolver == nil {
		resolver = net.DefaultResolver
	}
	lookupCtx, cancel := context.WithTimeout(ctx, DNSLookupTimeout)
	defer cancel()
	addrs, err := resolver.LookupIPAddr(lookupCtx, host)
	if err != nil {
		return nil, err
	}
	if len(addrs) == 0 {
		return nil, errors.New("no addresses returned")
	}
	if len(addrs) > MaxResolvedAddresses {
		return nil, fmt.Errorf("%d addresses returned, more than %d", len(addrs), MaxResolvedAddresses)
	}
	ips := make([]net.IP, 0, len(addrs))
	for _, a := range addrs {
		if a.Zone == "" {
			ips = append(ips, a.IP)
		}
	}
	return ips, nil
}

// ContextDialer dials one address. *net.Dialer implements it; tests supply a
// fake that records what was dialled.
type ContextDialer interface {
	DialContext(ctx context.Context, network, address string) (net.Conn, error)
}

// DialFirst dials the checked IP literals in order and returns the first
// connection that succeeds. It never resolves a name: every address it dials
// is one the caller checked, which is what closes the DNS-rebinding gap
// between the check and the connect. A nil dialer uses DialTimeout.
func DialFirst(ctx context.Context, dialer ContextDialer, network string, ips []net.IP, port string) (net.Conn, error) {
	if len(ips) == 0 {
		return nil, errors.New("no checked address to dial")
	}
	if dialer == nil {
		dialer = &net.Dialer{Timeout: DialTimeout}
	}
	var lastErr error
	for _, ip := range ips {
		conn, err := dialer.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
		if err == nil {
			return conn, nil
		}
		lastErr = err
	}
	return nil, lastErr
}
