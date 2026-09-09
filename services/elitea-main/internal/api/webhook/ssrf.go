package webhook

// SSRF hardening for outbound webhook destinations.
//
// # WHAT WAS MISSING
//
// #876 accepted any `url` a caller supplied and dialled it unchecked, both
// when a webhook was first created (Handler.Create/Update) and every time it
// actually fired (Dispatcher.attempt/send). A caller with the ordinary
// `configurations.configuration.create` permission on their own project could
// register a webhook pointed at 127.0.0.1, at 169.254.169.254 (the cloud
// instance-metadata endpoint every provider serves unauthenticated), or at a
// name that resolves inside the cluster's own private network — and this
// platform would then make that request FOR them, with this service's own
// network identity, on every matching domain event.
//
// # THE ESTABLISHED PATTERN THIS REUSES
//
// libs/go/egresslib is already this platform's ONE grammar for an operator-
// authored egress allowlist (GATEWAY_EGRESS_ALLOWLIST, the gateway's
// governance_config `egress_allowlist` rows). Its AllowsPrivateNetwork
// documents the exact distinction this file needs: an allowlist entry can
// explicitly declare that PRIVATE (RFC1918/ULA) egress is intended, but
// link-local (169.254.0.0/16, fe80::/10 — the metadata range) is refused
// UNCONDITIONALLY, because "an entry naming one declares nothing this package
// can grant" (egresslib's own doc comment). DestinationGuard below applies
// that exact split to a webhook destination: ELITEA_WEBHOOK_EGRESS_ALLOWLIST
// is parsed with egresslib.Parse, and AllowsPrivateNetwork() is the ONE
// question this file asks it — not Allows(host), which governs a different
// question (an operator NAMING specific public hosts) that does not apply
// here: a webhook destination is chosen by the tenant, not curated by the
// operator, so the control this file needs is "may this deployment's tenants
// reach private addresses at all", not "which public hosts may they reach".
//
// # WHAT elitea-main DID NOT ALREADY HAVE
//
// Unlike the LLM gateway, this service dials no bifrost SSRF-safe transport.
// internal/providerhost/material.GitEgressPolicy (DeepWiki/Inventory) and
// internal/api/v2/configurations/toolkit_check.go's ELITEA_TOOLKIT_CHECK_ALLOWLIST
// are both NAME allowlists with no DNS resolution and no dial-time pinning —
// they answer "did an operator name this host", never "does this host
// resolve somewhere it should not". A webhook destination is different: it is
// whatever a tenant types, so the control has to be about WHERE that name
// resolves, not a curated list of vendor hosts. DestinationGuard is the first
// dial-time, DNS-aware SSRF guard in this service, built from scratch but
// over egresslib's existing grammar and constant set.

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

// DestinationAllowlistEnv names the variable an operator sets to permit
// tenant webhooks to reach private-network destinations (an internal
// receiver, a service mesh host, a lab endpoint). Spelled out as a literal
// for the same reason ToolkitCheckAllowlistEnv is: the env-drift gate and the
// chart are both searched by the string.
//
// UNSET (the default): every RFC1918, ULA and loopback destination is
// refused. That is the safe default for a multi-tenant deployment, where a
// registered webhook is effectively "make elitea-main issue an HTTP request
// I chose, using elitea-main's own network identity" — the textbook SSRF
// primitive.
//
// Link-local (169.254.0.0/16, fe80::/10 — which is where every major cloud's
// instance-metadata endpoint lives, 169.254.169.254 among them) and multicast
// addresses are refused NO MATTER WHAT this variable says: see egresslib's
// own doc comment on AllowsPrivateNetwork for why that asymmetry is
// deliberate, not an oversight.
const DestinationAllowlistEnv = "ELITEA_WEBHOOK_EGRESS_ALLOWLIST"

// ErrDestinationRefused is the sentinel every refusal in this file wraps, so
// a caller can `errors.Is` it regardless of which specific reason fired.
var ErrDestinationRefused = errors.New("webhook destination refused")

// dnsLookupTimeout bounds ONE resolution — at create/update time, and again
// at dial time (see DestinationGuard.dialContext's doc comment for why dial
// time resolves AGAIN rather than trusting the create-time answer).
const dnsLookupTimeout = 5 * time.Second

// dialTimeout bounds ONE literal-IP TCP connect, once resolution and
// filtering have already picked which addresses are permitted.
const dialTimeout = 10 * time.Second

// ipResolver is the seam DestinationGuard resolves hostnames through. Tests
// substitute a fake so validation is deterministic and does not depend on the
// test runner's own network access or a real DNS answer for a fixture
// hostname — the same reason internal/api/v2/configurations/toolkit_check.go
// checks a host STRING against an allowlist rather than resolving it: a unit
// test must not depend on live DNS. Here resolution is the whole point, so
// instead of avoiding it, the resolver itself is substitutable.
type ipResolver interface {
	LookupIPAddr(ctx context.Context, host string) ([]net.IPAddr, error)
}

// DestinationGuard is a webhook `url`'s SSRF gate: DENIES loopback, private,
// link-local, multicast and unspecified destinations by default, has an
// operator-controlled escape hatch for the private (not link-local) class,
// and re-resolves and re-checks EVERY dial rather than trusting a check done
// minutes or days earlier — a webhook row, once created, can be dialled
// indefinitely, and DNS is not something this service controls.
type DestinationGuard struct {
	allowlist *egresslib.Allowlist
	resolver  ipResolver
}

// NewDestinationGuard builds a guard over an allowlist (nil is the same as an
// empty one — see egresslib.Allowlist's own nil-receiver methods). It uses
// net.DefaultResolver; NewDestinationGuardWithResolver is the seam tests use
// to substitute a fake.
func NewDestinationGuard(allowlist *egresslib.Allowlist) *DestinationGuard {
	return NewDestinationGuardWithResolver(allowlist, net.DefaultResolver)
}

// NewDestinationGuardWithResolver is NewDestinationGuard with an injectable
// resolver, for tests that must not depend on live DNS.
func NewDestinationGuardWithResolver(allowlist *egresslib.Allowlist, resolver ipResolver) *DestinationGuard {
	if resolver == nil {
		resolver = net.DefaultResolver
	}
	return &DestinationGuard{allowlist: allowlist, resolver: resolver}
}

// ParseDestinationAllowlist reads DestinationAllowlistEnv's grammar (see
// egresslib's package doc): a comma/whitespace-free list of hosts, `*.`
// wildcards and CIDR blocks. An empty string parses to an empty, "refuse all
// private destinations" allowlist.
func ParseDestinationAllowlist(raw []string) (*egresslib.Allowlist, error) {
	return egresslib.Parse(raw)
}

func (g *DestinationGuard) allowsPrivate() bool {
	return g != nil && g.allowlist != nil && g.allowlist.AllowsPrivateNetwork()
}

// Validate checks a webhook `url` BEFORE it is ever stored — Handler.Create
// and Handler.Update both call this and answer 400 on a refusal, so a
// disallowed destination never reaches the table in the first place. It is
// deliberately the SAME filtering dialContext applies at send time
// (permittedIPs), so a URL this method accepts is, at the moment it was
// checked, one dialContext would also accept.
//
// "At the moment it was checked" is doing real work in that sentence: DNS is
// not a fact this service controls, and a hostname that resolved to a public
// address today can resolve to 169.254.169.254 tomorrow (DNS rebinding, or
// simply an operator repointing a record). That is exactly why dialContext
// below re-resolves and re-checks on every single delivery attempt instead of
// trusting this method's answer — this method is the create-time UX (a clear
// 400 instead of a silently inert webhook), not the enforcement boundary.
func (g *DestinationGuard) Validate(ctx context.Context, rawURL string) error {
	_, host, err := parseDestinationURL(rawURL)
	if err != nil {
		return err
	}
	ips, err := g.resolveHost(ctx, host)
	if err != nil {
		return fmt.Errorf("%w: could not resolve %q: %v", ErrDestinationRefused, host, err)
	}
	if len(g.permittedIPs(ips)) == 0 {
		return fmt.Errorf(
			"%w: %q does not resolve to a permitted destination "+
				"(loopback, private-network, link-local and multicast addresses are refused)",
			ErrDestinationRefused, host)
	}
	return nil
}

// Transport returns an *http.Transport whose DialContext is dialContext, so
// every connection a Dispatcher makes through it is re-validated and pinned
// to a specific, already-checked IP literal. A Dispatcher built WITHOUT a
// guard (nil) uses http.DefaultTransport and performs no SSRF check at all —
// see NewDispatcher's doc comment for why that is an explicit, documented
// choice rather than an oversight.
func (g *DestinationGuard) Transport() *http.Transport {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.DialContext = g.dialContext
	return t
}

// dialContext is the actual SSRF enforcement boundary: it runs once per TCP
// connection net/http's Transport opens, which for a webhook POST is once per
// delivery attempt.
//
// # Why this re-resolves instead of trusting Validate's answer
//
// A destination is stored once and dialled for the lifetime of the webhook —
// possibly years apart from the create-time check. Trusting a stale
// resolution would let an attacker register a webhook pointed at a hostname
// that resolves PUBLIC at creation time and repoint it to 169.254.169.254
// (or anywhere else) the moment it passes validation: classic DNS rebinding,
// and the exact attack a "check the name, then dial the name" design is
// vulnerable to. Resolving HERE, immediately before the dial, and then
// dialling the resolved IP LITERAL rather than the original hostname (see
// below) closes that gap: net/http never gets a second chance to resolve the
// name again after this check has passed, because it is never given the name
// to resolve — only the connection this function already opened.
//
// # Why the IP literal, not the hostname, is what gets dialled
//
// If this returned a plain net.Dialer.DialContext(ctx, network, addr) using
// the ORIGINAL addr (still a hostname:port), Go's own dialer would resolve it
// AGAIN internally — a second, uncontrolled resolution this function has no
// visibility into, and the classic bypass: check public, dial private. By
// resolving ourselves and handing net.Dialer a literal IP:port, the address
// that gets a TCP SYN is provably the SAME address this function just
// filtered. TLS (for an https destination) is unaffected: net/http's
// Transport wraps the returned net.Conn in TLS using the URL's hostname as
// ServerName regardless of what address the connection itself was opened to,
// so certificate validation still checks the name the caller asked for.
func (g *DestinationGuard) dialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrDestinationRefused, err)
	}
	ips, err := g.resolveHost(ctx, host)
	if err != nil {
		return nil, fmt.Errorf("%w: could not resolve %q: %v", ErrDestinationRefused, host, err)
	}
	allowed := g.permittedIPs(ips)
	if len(allowed) == 0 {
		return nil, fmt.Errorf(
			"%w: %q does not resolve to a permitted destination "+
				"(loopback, private-network, link-local and multicast addresses are refused)",
			ErrDestinationRefused, host)
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

// permittedIPs is the ONE filtering rule Validate and dialContext both apply,
// so a URL cannot be accepted by one and refused by the other. It keeps every
// resolved address whose class is not blocked outright, and additionally
// keeps a loopback/private address when the allowlist explicitly permits
// private-network egress. Link-local, multicast and unspecified addresses are
// never kept, regardless of the allowlist — see classifyDestinationIP.
func (g *DestinationGuard) permittedIPs(ips []net.IP) []net.IP {
	allowPrivate := g.allowsPrivate()
	allowed := make([]net.IP, 0, len(ips))
	for _, ip := range ips {
		blocked, always, _ := classifyDestinationIP(ip)
		if !blocked {
			allowed = append(allowed, ip)
			continue
		}
		if !always && allowPrivate {
			allowed = append(allowed, ip)
		}
	}
	return allowed
}

// resolveHost resolves host to its IP addresses. An IP literal resolves to
// itself (no DNS involved, so nothing to rebind). "localhost" is resolved to
// 127.0.0.1 without a system lookup — the same special-case egresslib's own
// NamesPrivateNetwork documents ("the one name whose meaning is fixed by
// every resolver on the planet") — so a receiver's DNS or hosts file cannot
// make "localhost" resolve to something this function would not otherwise
// classify as loopback.
func (g *DestinationGuard) resolveHost(ctx context.Context, host string) ([]net.IP, error) {
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
	ips := make([]net.IP, 0, len(addrs))
	for _, a := range addrs {
		ips = append(ips, a.IP)
	}
	return ips, nil
}

// classifyDestinationIP reports whether ip belongs to a class this guard ever
// refuses (blocked), and whether that refusal is UNCONDITIONAL — never
// liftable by the allowlist (always). reason is a short, caller-safe
// description used in refusal messages.
//
// The unconditional classes (link-local, multicast, unspecified) are exactly
// the ranges egresslib's own privateBlocks deliberately EXCLUDES, for the
// same reason stated there: link-local is where cloud instance-metadata
// endpoints live (169.254.169.254 first among them), and an operator naming
// one in an allowlist "declares nothing this package can grant".
func classifyDestinationIP(ip net.IP) (blocked, always bool, reason string) {
	switch {
	case ip.IsUnspecified():
		return true, true, "the unspecified address"
	case ip.IsLoopback():
		return true, false, "a loopback address"
	case ip.IsLinkLocalUnicast(), ip.IsLinkLocalMulticast(), ip.IsInterfaceLocalMulticast():
		return true, true, "a link-local address (this range includes every cloud provider's instance-metadata endpoint)"
	case ip.IsMulticast():
		return true, true, "a multicast address"
	case ip.IsPrivate():
		return true, false, "a private-network address"
	default:
		return false, false, ""
	}
}

// parseDestinationURL validates the SYNTACTIC shape a webhook `url` must
// have, independent of where it resolves: a non-empty http(s) URL with a
// host. It is called by Validate and reused by dialContext's callers through
// Validate; dialContext itself receives an already-split host:port from
// net/http and does not re-parse a URL.
func parseDestinationURL(raw string) (*url.URL, string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return nil, "", fmt.Errorf("%w: the destination URL is empty", ErrDestinationRefused)
	}
	u, err := url.Parse(trimmed)
	if err != nil {
		return nil, "", fmt.Errorf("%w: %q is not a valid URL", ErrDestinationRefused, raw)
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https":
	default:
		return nil, "", fmt.Errorf("%w: scheme %q is not http or https", ErrDestinationRefused, u.Scheme)
	}
	host := u.Hostname()
	if host == "" {
		return nil, "", fmt.Errorf("%w: the destination URL has no host", ErrDestinationRefused)
	}
	return u, host, nil
}
