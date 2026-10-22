package browserauth

import (
	"errors"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/net/http/httpguts"

	browserapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/browserauth"
)

const (
	maxTrustedProxyCIDRs = 64
	maxForwardedForBytes = 2048
	maxForwardedHops     = 32
)

var ErrInvalidForwardedRequest = errors.New("invalid or untrusted forwarded request")

// TrustedProxyConfig defines the only network boundary from which forwarded
// request metadata is accepted. PublicOrigin is validated so the document
// keeps one meaning for every consumer of the auth configuration.
//
// It no longer carries an identity-projection secret: the X-Auth-* projection
// this resolver used to sign and verify is gone (#1359), and what remains is
// the client-address interpretation login attempt limiting depends on.
type TrustedProxyConfig struct {
	TrustedProxyCIDRs []string
	PublicOrigin      string
	Development       bool
}

// TrustedProxyResolver validates one or more explicitly trusted proxy hops.
// It never consumes X-Forwarded-* metadata from an arbitrary direct client:
// a peer outside the trusted set IS the client, whatever X-Forwarded-For it
// sends. That is the compose case, where elitea-main is the browser edge
// with no proxy in front (#1359). Request.RemoteAddr must still contain the
// raw socket peer; generic RealIP middleware must not run before this
// resolver.
type TrustedProxyResolver struct {
	trusted []netip.Prefix
}

func NewTrustedProxyResolver(config TrustedProxyConfig) (*TrustedProxyResolver, error) {
	if len(config.TrustedProxyCIDRs) == 0 || len(config.TrustedProxyCIDRs) > maxTrustedProxyCIDRs {
		return nil, ErrInvalidForwardedRequest
	}
	origin, err := url.Parse(config.PublicOrigin)
	if err != nil || !origin.IsAbs() || origin.Opaque != "" || origin.User != nil ||
		origin.Host == "" || origin.RawQuery != "" || origin.Fragment != "" ||
		(origin.EscapedPath() != "" && origin.EscapedPath() != "/") ||
		(origin.Scheme != "https" && (!config.Development || origin.Scheme != "http")) ||
		!httpguts.ValidHostHeader(origin.Host) {
		return nil, ErrInvalidForwardedRequest
	}

	trusted := make([]netip.Prefix, 0, len(config.TrustedProxyCIDRs))
	for _, raw := range config.TrustedProxyCIDRs {
		if raw == "" || raw != strings.TrimSpace(raw) {
			return nil, ErrInvalidForwardedRequest
		}
		prefix, parseErr := netip.ParsePrefix(raw)
		if parseErr != nil {
			return nil, ErrInvalidForwardedRequest
		}
		trusted = append(trusted, prefix.Masked())
	}

	return &TrustedProxyResolver{trusted: trusted}, nil
}

// ResolveClientKey is the one trusted-hop interpretation of X-Forwarded-For.
// Login attempt limiting therefore cannot be bypassed by a second, weaker
// parser.
//
// The key is the client's address for IPv4 and its /64 for IPv6. One IPv6
// subscriber is routinely given a whole /64 (SLAAC, privacy addresses) and
// can rotate through it at will, so a per-/128 key would give one client 2^64
// independent limits. An IPv4-mapped IPv6 address is already unmapped by
// resolveClientIP and keys as the IPv4 address it is.
func (r *TrustedProxyResolver) ResolveClientKey(request *http.Request) (string, error) {
	clientIP, err := r.resolveClientIP(request)
	if err != nil {
		return "", err
	}
	return attemptClientKey(clientIP), nil
}

// attemptClientKey is browserapp.AttemptClientKey, which the single-sign-on
// planes (internal/api/v2/auth) key their attempts with too.
func attemptClientKey(address netip.Addr) string {
	return browserapp.AttemptClientKey(address)
}

func (r *TrustedProxyResolver) resolveClientIP(request *http.Request) (netip.Addr, error) {
	if r == nil || request == nil {
		return netip.Addr{}, ErrInvalidForwardedRequest
	}
	peer, err := remoteAddress(request.RemoteAddr)
	if err != nil {
		return netip.Addr{}, ErrInvalidForwardedRequest
	}
	if !r.isTrusted(peer) {
		// A direct client: its X-Forwarded-For is its own claim, and
		// believing it would let it pick a fresh attempt window per request.
		return peer, nil
	}
	raw, err := uniqueRequiredHeader(request.Header, "X-Forwarded-For")
	if err != nil || len(raw) > maxForwardedForBytes {
		return netip.Addr{}, ErrInvalidForwardedRequest
	}
	parts := strings.Split(raw, ",")
	if len(parts) == 0 || len(parts) > maxForwardedHops {
		return netip.Addr{}, ErrInvalidForwardedRequest
	}
	hops := make([]netip.Addr, len(parts))
	for index, part := range parts {
		value := strings.TrimSpace(part)
		address, parseErr := netip.ParseAddr(value)
		if parseErr != nil || address.Zone() != "" {
			return netip.Addr{}, ErrInvalidForwardedRequest
		}
		hops[index] = address.Unmap()
	}

	client := peer
	for index := len(hops) - 1; index >= 0 && r.isTrusted(client); index-- {
		client = hops[index]
	}
	return client, nil
}

func (r *TrustedProxyResolver) isTrusted(address netip.Addr) bool {
	address = address.Unmap()
	for _, prefix := range r.trusted {
		if prefix.Contains(address) {
			return true
		}
	}
	return false
}

func remoteAddress(value string) (netip.Addr, error) {
	host, _, err := net.SplitHostPort(value)
	if err != nil {
		host = value
	}
	address, err := netip.ParseAddr(host)
	if err != nil || address.Zone() != "" {
		return netip.Addr{}, ErrInvalidForwardedRequest
	}
	return address.Unmap(), nil
}

func uniqueRequiredHeader(headers http.Header, name string) (string, error) {
	var values []string
	for key, current := range headers {
		if strings.EqualFold(key, name) {
			values = append(values, current...)
		}
	}
	if len(values) != 1 || values[0] == "" || values[0] != strings.TrimSpace(values[0]) ||
		!utf8.ValidString(values[0]) || strings.ContainsFunc(values[0], unicode.IsControl) {
		return "", ErrInvalidForwardedRequest
	}
	return values[0], nil
}
