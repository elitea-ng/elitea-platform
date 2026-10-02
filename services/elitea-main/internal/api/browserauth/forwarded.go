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

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth/browserflow"
)

const (
	maxTrustedProxyCIDRs = 64
	maxForwardedForBytes = 2048
	maxForwardedHops     = 32
	maxForwardedMethod   = 32
	maxMapperValueBytes  = 512
	maxAuthQueryBytes    = 2048
	maxAuthQueryValues   = 16
)

var ErrInvalidForwardedRequest = errors.New("invalid or untrusted forwarded request")

// TrustedProxyConfig defines the only network boundary from which forwarded
// request metadata is accepted. PublicOrigin is also authoritative: forwarded
// scheme and host values cannot select a redirect origin.
type TrustedProxyConfig struct {
	TrustedProxyCIDRs []string
	PublicOrigin      string
	Development       bool
}

// ForwardedRequest is the normalized source used by EdgeAuth policy. The
// presence bits preserve the current distinction between an omitted mapper
// target and an explicitly empty target.
type ForwardedRequest struct {
	Method        string
	Proto         string
	Host          string
	URI           string
	ClientIP      string
	Target        string
	TargetPresent bool
	Scope         string
	ScopePresent  bool
}

// TrustedProxyResolver validates one or more explicitly trusted proxy hops.
// It never consumes X-Forwarded-* metadata from an arbitrary direct client.
// Request.RemoteAddr must still contain the raw socket peer; generic RealIP
// middleware must not run before this resolver.
type TrustedProxyResolver struct {
	trusted      []netip.Prefix
	publicScheme string
	publicHost   string
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

	return &TrustedProxyResolver{
		trusted:      trusted,
		publicScheme: origin.Scheme,
		publicHost:   origin.Host,
	}, nil
}

func (r *TrustedProxyResolver) Resolve(request *http.Request) (ForwardedRequest, error) {
	if r == nil || request == nil {
		return ForwardedRequest{}, ErrInvalidForwardedRequest
	}
	clientIP, err := r.resolveClientIP(request)
	if err != nil {
		return ForwardedRequest{}, err
	}

	method, err := uniqueRequiredHeader(request.Header, "X-Forwarded-Method")
	if err != nil || len(method) > maxForwardedMethod || !httpguts.ValidHeaderFieldName(method) {
		return ForwardedRequest{}, ErrInvalidForwardedRequest
	}
	proto, err := uniqueRequiredHeader(request.Header, "X-Forwarded-Proto")
	if err != nil || !strings.EqualFold(proto, r.publicScheme) {
		return ForwardedRequest{}, ErrInvalidForwardedRequest
	}
	host, err := uniqueRequiredHeader(request.Header, "X-Forwarded-Host")
	if err != nil || !httpguts.ValidHostHeader(host) || !strings.EqualFold(host, r.publicHost) {
		return ForwardedRequest{}, ErrInvalidForwardedRequest
	}
	uri, err := uniqueRequiredHeader(request.Header, "X-Forwarded-Uri")
	if err != nil || browserflow.ValidateReturnTarget(uri) != nil {
		return ForwardedRequest{}, ErrInvalidForwardedRequest
	}

	if len(request.URL.RawQuery) > maxAuthQueryBytes {
		return ForwardedRequest{}, ErrInvalidForwardedRequest
	}
	query, err := url.ParseQuery(request.URL.RawQuery)
	if err != nil {
		return ForwardedRequest{}, ErrInvalidForwardedRequest
	}
	queryValues := 0
	for _, values := range query {
		queryValues += len(values)
		if queryValues > maxAuthQueryValues {
			return ForwardedRequest{}, ErrInvalidForwardedRequest
		}
	}
	target, targetPresent, err := optionalSingleQueryValue(query, "target")
	if err != nil {
		return ForwardedRequest{}, err
	}
	scope, scopePresent, err := optionalSingleQueryValue(query, "scope")
	if err != nil {
		return ForwardedRequest{}, err
	}

	return ForwardedRequest{
		Method:        method,
		Proto:         r.publicScheme,
		Host:          r.publicHost,
		URI:           uri,
		ClientIP:      clientIP.String(),
		Target:        target,
		TargetPresent: targetPresent,
		Scope:         scope,
		ScopePresent:  scopePresent,
	}, nil
}

// ResolveClientKey shares the exact trusted-hop interpretation used by the
// EdgeAuth source. Login attempt limiting therefore cannot be bypassed by a
// second, weaker X-Forwarded-For parser.
func (r *TrustedProxyResolver) ResolveClientKey(request *http.Request) (string, error) {
	clientIP, err := r.resolveClientIP(request)
	if err != nil {
		return "", err
	}
	return clientIP.String(), nil
}

// VerifyForwardedIdentityPeer proves only that the immediate socket peer is a
// configured, header-stripping proxy. EdgeAuth has already produced the
// identity projection; ordinary product requests do not need to replay its
// X-Forwarded-* source contract before Auth can consume that projection.
func (r *TrustedProxyResolver) VerifyForwardedIdentityPeer(request *http.Request) error {
	if r == nil || request == nil {
		return ErrInvalidForwardedRequest
	}
	peer, err := remoteAddress(request.RemoteAddr)
	if err != nil || !r.isTrusted(peer) {
		return ErrInvalidForwardedRequest
	}
	return nil
}

func (r *TrustedProxyResolver) resolveClientIP(request *http.Request) (netip.Addr, error) {
	if r == nil || request == nil {
		return netip.Addr{}, ErrInvalidForwardedRequest
	}
	peer, err := remoteAddress(request.RemoteAddr)
	if err != nil || !r.isTrusted(peer) {
		return netip.Addr{}, ErrInvalidForwardedRequest
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

func optionalSingleQueryValue(values url.Values, name string) (string, bool, error) {
	items, present := values[name]
	if !present {
		return "", false, nil
	}
	if len(items) != 1 || len(items[0]) > maxMapperValueBytes || !utf8.ValidString(items[0]) ||
		strings.ContainsFunc(items[0], unicode.IsControl) {
		return "", false, ErrInvalidForwardedRequest
	}
	return items[0], true, nil
}
