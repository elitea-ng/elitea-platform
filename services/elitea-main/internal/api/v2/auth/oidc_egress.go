package auth

// Identity-provider egress (ADR-0011, "Provider, MCP and tool egress").
//
// OpenID Connect discovery, the JWKS fetch and the token exchange reach a URL
// an administrator typed into the identity-provider editor (or an operator put
// in OIDC_ISSUER_URL). Every one of those requests goes through elitea-main's
// egress guard, as webhook, MCP and toolkit-check traffic already does: each
// dial re-resolves the host, refuses link-local, cloud-metadata, multicast and
// reserved addresses outright, refuses private and loopback addresses unless
// the operator permits them, and dials the address it checked.
//
//   - ELITEA_IDENTITY_PROVIDER_EGRESS_ALLOWLIST (egresslib grammar) permits an
//     on-premises identity provider on a private address: a CIDR entry its
//     block, an IP literal that address, optionally on one port.
//   - Loopback is a development affordance only. It is admitted when
//     ELITEA_IDENTITY_PROVIDER_ALLOW_LOOPBACK=true, and a loopback entry in the
//     allowlist is ignored without it, so a production allowlist cannot open
//     the pod's own listeners by accident.
//
// discoverOIDCProvider is the ONE place this package calls oidc.NewProvider
// (oidc_egress_test.go holds that line), so no discovery can use
// http.DefaultClient again.

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/egress"
)

const (
	// IdentityProviderEgressAllowlistEnv names the private destinations an
	// identity provider may live on.
	IdentityProviderEgressAllowlistEnv = "ELITEA_IDENTITY_PROVIDER_EGRESS_ALLOWLIST"
	// IdentityProviderAllowLoopbackEnv admits loopback identity providers
	// (development stacks only).
	IdentityProviderAllowLoopbackEnv = "ELITEA_IDENTITY_PROVIDER_ALLOW_LOOPBACK"

	// identityProviderHTTPTimeout bounds one discovery, key-set or token
	// request end to end. A login waits on it, so it is short.
	identityProviderHTTPTimeout = 30 * time.Second
)

// NewIdentityProviderHTTPClient builds the guarded client every identity
// provider request uses. allowlist is egresslib grammar; loopback entries are
// dropped unless allowLoopback, which adds the loopback blocks itself. opts
// reach the guard (tests inject a resolver).
func NewIdentityProviderHTTPClient(allowlist []string, allowLoopback bool, opts ...egress.Option) (*http.Client, error) {
	entries := make([]string, 0, len(allowlist)+1)
	for _, raw := range allowlist {
		entry := strings.TrimSpace(raw)
		if entry == "" {
			continue
		}
		if !allowLoopback {
			switch loopbackScope(entry) {
			case loopbackOnly:
				slog.Warn("identity-provider egress: a loopback allowlist entry is ignored without the development flag",
					"entry", entry, "flag", IdentityProviderAllowLoopbackEnv)
				continue
			case loopbackAndMore:
				// Dropping it would silently leave the private ranges the
				// operator meant to permit refused; keeping it would permit
				// loopback. Neither is what was asked for, so boot fails.
				return nil, fmt.Errorf("%s: entry %q also covers loopback, which needs %s=true; list the private ranges instead (e.g. 10.0.0.0/8,172.16.0.0/12,192.168.0.0/16)",
					IdentityProviderEgressAllowlistEnv, entry, IdentityProviderAllowLoopbackEnv)
			}
		}
		entries = append(entries, entry)
	}
	if allowLoopback {
		entries = append(entries, "localhost")
	}
	parsed, err := egress.ParseAllowlist(entries)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", IdentityProviderEgressAllowlistEnv, err)
	}
	return &http.Client{
		Transport: egress.New(parsed, opts...).RoundTripper(),
		Timeout:   identityProviderHTTPTimeout,
	}, nil
}

// IdentityProviderHTTPClientFromEnv reads the two variables above.
func IdentityProviderHTTPClientFromEnv() (*http.Client, error) {
	return NewIdentityProviderHTTPClient(
		splitAllowlist(os.Getenv(IdentityProviderEgressAllowlistEnv)),
		strings.EqualFold(strings.TrimSpace(os.Getenv(IdentityProviderAllowLoopbackEnv)), "true"),
	)
}

// discoverOIDCProvider is oidc.NewProvider with the guarded client attached.
// The provider it returns keeps that client for its key set, so the JWKS
// fetches behind its Verifier are guarded too. ctx may already carry
// oidc.InsecureIssuerURLContext (Entra); both values survive.
func discoverOIDCProvider(ctx context.Context, client *http.Client, issuer string) (*oidc.Provider, error) {
	if client == nil {
		return nil, fmt.Errorf("identity-provider egress: no guarded HTTP client is configured")
	}
	return oidc.NewProvider(oidc.ClientContext(ctx, client), issuer)
}

// identityClient returns the handler's guarded client. A handler built by a
// struct literal (tests) reads the environment on first use.
func (h *OIDCHandler) identityClient() (*http.Client, error) {
	h.egressMu.Lock()
	defer h.egressMu.Unlock()
	if h.egressClient != nil {
		return h.egressClient, nil
	}
	client, err := IdentityProviderHTTPClientFromEnv()
	if err != nil {
		return nil, err
	}
	h.egressClient = client
	return client, nil
}

func splitAllowlist(raw string) []string {
	return strings.FieldsFunc(raw, func(r rune) bool { return r == ',' || r == ' ' || r == '\n' || r == '\t' })
}

// How much of an allowlist entry is loopback.
type loopbackCoverage int

const (
	loopbackNone    loopbackCoverage = iota
	loopbackOnly                     // localhost, a loopback IP, or a block inside 127.0.0.0/8 or ::1
	loopbackAndMore                  // a block that contains loopback and other addresses, e.g. 0.0.0.0/0
)

// loopbackScope reports whether an allowlist entry would permit a loopback
// address, and whether it permits anything else.
func loopbackScope(entry string) loopbackCoverage {
	host := entry
	if h, _, err := net.SplitHostPort(entry); err == nil {
		host = h
	}
	host = strings.Trim(host, "[]")
	if strings.EqualFold(host, "localhost") {
		return loopbackOnly
	}
	if ip := net.ParseIP(host); ip != nil {
		if ip.IsLoopback() {
			return loopbackOnly
		}
		return loopbackNone
	}
	_, block, err := net.ParseCIDR(host)
	if err != nil {
		return loopbackNone
	}
	_, loop4, _ := net.ParseCIDR("127.0.0.0/8")
	_, loop6, _ := net.ParseCIDR("::1/128")
	for _, loop := range []*net.IPNet{loop4, loop6} {
		blockBits, blockLen := block.Mask.Size()
		loopBits, loopLen := loop.Mask.Size()
		if blockLen != loopLen {
			continue
		}
		switch {
		case loop.Contains(block.IP) && blockBits >= loopBits:
			return loopbackOnly
		case block.Contains(loop.IP):
			return loopbackAndMore
		}
	}
	// An IPv4-mapped IPv6 block (::ffff:0:0/96) covers 127.0.0.1 too.
	if block.Contains(net.IPv4(127, 0, 0, 1)) || block.Contains(net.IPv6loopback) {
		return loopbackAndMore
	}
	return loopbackNone
}

// namesLoopback reports whether an allowlist entry would permit a loopback
// address at all.
func namesLoopback(entry string) bool { return loopbackScope(entry) != loopbackNone }
