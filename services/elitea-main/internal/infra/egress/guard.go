// Package egress is elitea-main's guard for outbound HTTP to a destination a
// tenant chose: webhook receivers, MCP servers and their OAuth endpoints, and
// GitHub Enterprise bases for Code workspaces. It is the one egress path future
// HTTP actions may use.
//
// # What it enforces
//
//   - Every connection re-resolves the host, refuses forbidden addresses, and
//     dials the checked IP literal, so DNS rebinding cannot swap the address
//     between the check and the connect (dialContext).
//   - Loopback and private-network addresses (RFC 1918, ULA, CGNAT,
//     benchmarking, site-local) are refused unless an operator allowlist entry
//     names that address: a CIDR entry permits its own block, an IP literal
//     that address, `localhost` loopback, each only on the entry's port when it
//     pins one (classify.go privatePermits). Unspecified, link-local, multicast, reserved,
//     IETF-protocol, discard, Teredo and cloud-metadata addresses are refused
//     whatever the allowlist says (classify.go).
//   - IPv4 reached through an IPv6 form (IPv4-mapped, NAT64 64:ff9b::/96, 6to4)
//     is classified as the IPv4 address it reaches.
//   - The transport never uses a proxy, so the address checked is the address
//     dialled. It caps response headers at MaxResponseHeaderBytes and verifies
//     TLS against the URL host (net/http's default; the pinned IP literal is
//     only the TCP address).
//   - RoundTripper returns a transport only. A redirect is returned to the
//     caller, never followed: only http.Client follows redirects.
//
// # Allowlists are the outer bound
//
// The operator allowlists (ELITEA_WEBHOOK_EGRESS_ALLOWLIST,
// ELITEA_MCP_EGRESS_ALLOWLIST, ELITEA_MCP_OAUTH_EGRESS_ALLOWLIST, and the
// Code workspace host allowlist) use libs/go/egresslib's grammar. This guard
// asks one question of them: which private addresses does an entry name? An
// entry permits only its own range, never the other private classes, and
// never widens past the forbidden classes.
//
// The guard used to live in internal/api/webhook. It moved here so
// infrastructure adapters can use it without importing the api layer.
package egress

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/EliteaAI/elitea-platform/libs/go/egresslib"
)

// ErrDestinationRefused is the sentinel every refusal wraps, so a caller can
// errors.Is it whichever reason fired. The text predates the move out of the
// webhook package and is kept because the webhook form shows it verbatim.
var ErrDestinationRefused = errors.New("webhook destination refused")

const (
	// dnsLookupTimeout bounds ONE resolution, at validation and at dial time.
	dnsLookupTimeout = 5 * time.Second
	// dialTimeout bounds ONE literal-IP TCP connect.
	dialTimeout = 10 * time.Second

	// MaxResponseHeaderBytes bounds one response's header block. net/http's
	// default is 10 MiB, which lets a hostile destination make Main buffer
	// megabytes before any caller-side body limit applies.
	MaxResponseHeaderBytes = 64 << 10
	// MaxResolvedAddresses bounds one resolution. A larger answer is refused
	// rather than truncated, so the dial loop never walks an unbounded list.
	MaxResolvedAddresses = 32
)

// Resolver is the seam the guard resolves host names through. Tests supply a
// fake so validation never depends on live DNS.
type Resolver interface {
	LookupIPAddr(ctx context.Context, host string) ([]net.IPAddr, error)
}

// Guard is the SSRF gate for a tenant-chosen destination. It is safe for
// concurrent use and immutable after New.
type Guard struct {
	private   []privatePermit
	resolver  Resolver
	httpsOnly bool
}

// Option configures a Guard built by New.
type Option func(*Guard)

// WithResolver replaces net.DefaultResolver. A nil resolver is ignored.
func WithResolver(resolver Resolver) Option {
	return func(g *Guard) {
		if resolver != nil {
			g.resolver = resolver
		}
	}
}

// RequireHTTPS refuses every http destination, in Validate and in every
// request sent through Transport or RoundTripper.
func RequireHTTPS() Option {
	return func(g *Guard) { g.httpsOnly = true }
}

// New builds a guard over an allowlist. A nil allowlist refuses every private
// destination.
func New(allowlist *egresslib.Allowlist, opts ...Option) *Guard {
	g := &Guard{resolver: net.DefaultResolver}
	for _, opt := range opts {
		opt(g)
	}
	g.private = privatePermits(allowlist)
	return g
}

// NewWithResolver is New with an injectable resolver, for tests that must not
// depend on live DNS.
func NewWithResolver(allowlist *egresslib.Allowlist, resolver Resolver) *Guard {
	return New(allowlist, WithResolver(resolver))
}

// ParseAllowlist reads egresslib's grammar: hosts, `*.` wildcards and CIDR
// blocks. An empty list refuses every private destination.
func ParseAllowlist(raw []string) (*egresslib.Allowlist, error) {
	return egresslib.Parse(raw)
}

// Validate checks a destination URL before it is stored or used. It applies
// the same filter as dialContext, so a URL it accepts is, at the moment it
// was checked, one dialContext would also accept. DNS can change afterwards,
// which is why dialContext checks again: this is the clear-400 UX, not the
// enforcement boundary.
func (g *Guard) Validate(ctx context.Context, rawURL string) error {
	u, host, err := parseDestinationURL(rawURL)
	if err != nil {
		return err
	}
	if err := g.checkScheme(u.Scheme); err != nil {
		return err
	}
	ips, err := g.resolveHost(ctx, host)
	if err != nil {
		return fmt.Errorf("%w: could not resolve %q: %v", ErrDestinationRefused, host, err)
	}
	if len(g.permittedIPs(ips, destinationPort(u))) == 0 {
		return notPermitted(host)
	}
	return nil
}

// destinationPort is the port a request to u dials: the explicit port, or the
// scheme's default. A port-pinned allowlist entry is compared against it.
func destinationPort(u *url.URL) string {
	if port := u.Port(); port != "" {
		return port
	}
	if strings.EqualFold(u.Scheme, "http") {
		return "80"
	}
	return "443"
}

// Transport returns an *http.Transport that dials only through dialContext,
// never uses a proxy, caps response headers, and enforces the scheme policy.
// An *http.Transport never follows redirects; an http.Client wrapped around
// it does, so such a client must set its own CheckRedirect (each hop still
// dials through the guard).
func (g *Guard) Transport() *http.Transport {
	var t *http.Transport
	if base, ok := http.DefaultTransport.(*http.Transport); ok {
		t = base.Clone()
	} else {
		// Another package wrapped DefaultTransport; start from net/http's
		// documented defaults instead of panicking.
		t = &http.Transport{
			ForceAttemptHTTP2:     true,
			MaxIdleConns:          100,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ExpectContinueTimeout: 1 * time.Second,
		}
	}
	t.Proxy = nil
	if g.httpsOnly {
		// net/http asks Proxy about every request before dialling, so this
		// is where the transport sees the scheme. It never returns a proxy.
		t.Proxy = func(req *http.Request) (*url.URL, error) {
			return nil, g.checkScheme(req.URL.Scheme)
		}
	}
	t.DialContext = g.dialContext
	t.DialTLSContext = nil
	t.MaxResponseHeaderBytes = MaxResponseHeaderBytes
	return t
}

// RoundTripper returns the guarded transport as an http.RoundTripper. It is
// the constructor for any new egress path: a RoundTripper cannot follow a
// redirect, so a 3xx is returned to the caller and its Location is never
// dialled.
func (g *Guard) RoundTripper() http.RoundTripper {
	return g.Transport()
}

// DialContext is dialContext for a caller that builds its own transport, such
// as the MCP authorization proxies (eliteacore/mcp_oauth_egress.go), which
// keep their configured trust bundle and replace only the dialer.
func (g *Guard) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	return g.dialContext(ctx, network, addr)
}

// dialContext is the enforcement boundary: it runs once per TCP connection.
// It resolves the host itself, filters the answers, and dials the checked IP
// literal, so net/http never resolves the name again after the check (the
// DNS-rebinding bypass). TLS still verifies the URL's host name: net/http
// sets ServerName from the request, not from the address dialled.
func (g *Guard) dialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	switch network {
	case "tcp", "tcp4", "tcp6":
	default:
		return nil, fmt.Errorf("%w: network %q is not tcp", ErrDestinationRefused, network)
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrDestinationRefused, err)
	}
	ips, err := g.resolveHost(ctx, host)
	if err != nil {
		return nil, fmt.Errorf("%w: could not resolve %q: %v", ErrDestinationRefused, host, err)
	}
	allowed := g.permittedIPs(ips, port)
	if len(allowed) == 0 {
		return nil, notPermitted(host)
	}

	dialer := &net.Dialer{Timeout: dialTimeout}
	var lastErr error
	for _, ip := range allowed {
		conn, dialErr := dialer.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
		if dialErr == nil {
			return conn, nil
		}
		lastErr = dialErr
	}
	return nil, lastErr
}

func notPermitted(host string) error {
	return fmt.Errorf(
		"%w: %q does not resolve to a permitted destination "+
			"(loopback, private-network, link-local, multicast and reserved addresses are refused)",
		ErrDestinationRefused, host)
}

// permittedIPs is the ONE filter Validate and dialContext both apply. It keeps
// public addresses, and a private one only when an allowlist entry names it
// (on this port, when the entry pins one). Forbidden addresses are never kept.
func (g *Guard) permittedIPs(ips []net.IP, port string) []net.IP {
	allowed := make([]net.IP, 0, len(ips))
	for _, ip := range ips {
		switch classify(ip) {
		case classPublic:
			allowed = append(allowed, ip)
		case classPrivate:
			if g.permitsPrivate(ip, port) {
				allowed = append(allowed, ip)
			}
		}
	}
	return allowed
}

// permitsPrivate reports whether an allowlist entry names this private
// address on this port.
func (g *Guard) permitsPrivate(ip net.IP, port string) bool {
	if g == nil || len(g.private) == 0 {
		return false
	}
	ip = canonicalIP(ip)
	for _, permit := range g.private {
		if permit.permits(ip, port) {
			return true
		}
	}
	return false
}

// resolveHost resolves host to at most MaxResolvedAddresses addresses. An IP
// literal resolves to itself. "localhost" is pinned to 127.0.0.1 so a hosts
// file cannot make it mean something else. Zoned answers are dropped: a zone
// only qualifies a link-local or interface-scoped address, never a
// destination this guard could permit.
func (g *Guard) resolveHost(ctx context.Context, host string) ([]net.IP, error) {
	if ip := net.ParseIP(host); ip != nil {
		return []net.IP{ip}, nil
	}
	if strings.EqualFold(host, "localhost") {
		return []net.IP{net.IPv4(127, 0, 0, 1)}, nil
	}
	lookupCtx, cancel := context.WithTimeout(ctx, dnsLookupTimeout)
	defer cancel()
	addrs, err := g.resolver.LookupIPAddr(lookupCtx, host)
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

func (g *Guard) checkScheme(scheme string) error {
	switch strings.ToLower(scheme) {
	case "https":
		return nil
	case "http":
		if g.httpsOnly {
			return fmt.Errorf("%w: this destination must use https", ErrDestinationRefused)
		}
		return nil
	default:
		return fmt.Errorf("%w: scheme %q is not http or https", ErrDestinationRefused, scheme)
	}
}

// parseDestinationURL validates the syntactic shape a destination must have,
// independent of where it resolves: a non-empty URL with a host. Validate
// checks the scheme against the guard's policy.
func parseDestinationURL(raw string) (*url.URL, string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return nil, "", fmt.Errorf("%w: the destination URL is empty", ErrDestinationRefused)
	}
	u, err := url.Parse(trimmed)
	if err != nil {
		// Never echo the input: it may carry userinfo or a token, and
		// refusals are logged and stored.
		return nil, "", fmt.Errorf("%w: the destination is not a valid URL", ErrDestinationRefused)
	}
	host := u.Hostname()
	if host == "" {
		return nil, "", fmt.Errorf("%w: the destination URL has no host", ErrDestinationRefused)
	}
	return u, host, nil
}
