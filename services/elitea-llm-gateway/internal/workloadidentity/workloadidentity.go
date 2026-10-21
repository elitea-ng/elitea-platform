// Package workloadidentity holds the gateway's keyless credential mode: a
// PLATFORM credential that authenticates with the gateway pod's own Google
// identity (GKE Workload Identity, read through Application Default
// Credentials) instead of a stored secret.
//
// # Why this is a separate, narrow mode
//
// The gateway refuses ambient identity everywhere else, on purpose. A tenant
// credential with an empty key would otherwise borrow the platform's cloud
// identity: bifrost's Vertex provider falls back to
// google.FindDefaultCredentials for an empty AuthCredentials, its Azure
// provider falls back to DefaultAzureCredential for an empty key, and the AWS
// SDK does the same for Bedrock (issue #454, account/account.go). That
// protection stays. This package adds ONE explicit exception, and the account
// package admits it only when BOTH hold:
//
//  1. the operator armed it (GATEWAY_WORKLOAD_IDENTITY_ENABLED=true), and
//  2. the row is a platform credential: a `shared = true` row of the public
//     project's schema, which only the admin providers surface writes.
//
// Every other case is refused with WORKLOAD_IDENTITY_NOT_ALLOWED. It is never
// a silent fallback to a key, and never a silent fallback to ambient identity.
//
// # Two token kinds
//
//   - vertex_ai needs an OAuth ACCESS token with the cloud-platform scope.
//     bifrost mints it itself when the key carries an empty AuthCredentials;
//     the connection check mints it here, through the same ADC chain.
//   - ai_dial (EPAM AI DIAL) needs a Google-signed ID token for the DIAL
//     origin. DIAL verifies that token as a JWT with the audience it expects,
//     so the token is minted per audience and cached per audience.
//
// On GKE both come from the metadata server (169.254.169.254). The ADC
// clients use their own net/http client, not bifrost's SSRF-safe dialer and
// not the connection-check probe client, so the gateway's link-local refusal
// for TENANT-dialled traffic is unchanged by this package. The chart opens the
// metadata address in the gateway NetworkPolicy only when
// llmGateway.workloadIdentity.enabled is true.
package workloadidentity

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"sync"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
	"google.golang.org/api/idtoken"
)

// AuthModeKey and AuthModeWorkloadIdentity are the values of a credential's
// `data.auth_mode`. An absent or empty value is AuthModeKey, so every row
// written before the field existed keeps its meaning.
const (
	AuthModeKey              = "key"
	AuthModeWorkloadIdentity = "workload_identity"
)

// ParseAuthMode reads a stored auth_mode. The second result is false for a
// value this gateway does not know. The caller must REFUSE such a credential:
// reading an unknown mode as "key" would send an empty key, and reading it as
// workload identity would borrow the platform identity on a guess.
func ParseAuthMode(raw string) (string, bool) {
	switch strings.TrimSpace(raw) {
	case "", AuthModeKey:
		return AuthModeKey, true
	case AuthModeWorkloadIdentity:
		return AuthModeWorkloadIdentity, true
	default:
		return "", false
	}
}

// VertexScope is the OAuth scope Vertex AI requires. It is the scope bifrost's
// own Vertex provider requests from google.FindDefaultCredentials.
const VertexScope = "https://www.googleapis.com/auth/cloud-platform"

// DefaultAudience returns the ID-token audience for an AI DIAL api_base when
// the credential names no token_audience: the api_base ORIGIN,
// scheme://host[:port]. The path is dropped, because DIAL checks the token
// against the deployment's origin and not against one route under it.
//
// It refuses a value that is not an absolute http(s) URL, or that carries
// userinfo: an audience is put into a signed token, and a malformed one is a
// configuration error, not a value to guess at.
func DefaultAudience(apiBase string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(apiBase))
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.User != nil {
		return "", errors.New("workloadidentity: api_base is not an absolute http(s) URL")
	}
	host := strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
	port := u.Port()
	if port == "" || (u.Scheme == "https" && port == "443") || (u.Scheme == "http" && port == "80") {
		return u.Scheme + "://" + host, nil
	}
	return u.Scheme + "://" + net.JoinHostPort(host, port), nil
}

// Audience returns the audience for one DIAL credential: tokenAudience when it
// is set, otherwise DefaultAudience(apiBase).
func Audience(apiBase, tokenAudience string) (string, error) {
	if a := strings.TrimSpace(tokenAudience); a != "" {
		return a, nil
	}
	return DefaultAudience(apiBase)
}

// Tokens mints the two token kinds from the gateway's ambient Google identity.
// It is an interface so tests can supply a fake: the real implementation needs
// a metadata server or a credentials file.
type Tokens interface {
	// IDToken returns a Google-signed ID token for audience.
	IDToken(ctx context.Context, audience string) (string, error)
	// AccessToken returns an OAuth access token with VertexScope.
	AccessToken(ctx context.Context) (string, error)
}

// maxCachedAudiences bounds the per-audience cache. Audiences come from
// platform credential rows, which an operator authors, so the real count is a
// handful. The bound stops a long-lived process from growing without limit if
// that ever stops being true; past it a source is built per call, uncached.
const maxCachedAudiences = 64

// ADC implements Tokens through Google Application Default Credentials. On
// GKE with Workload Identity that is the metadata server, which answers as the
// Google service account the Kubernetes service account is bound to
// (`iam.gke.io/gcp-service-account`).
type ADC struct {
	mu        sync.Mutex
	idSources map[string]oauth2.TokenSource
	access    oauth2.TokenSource

	// The constructors are fields so a test can count how often a source is
	// built. Production uses idtoken.NewTokenSource and
	// google.FindDefaultCredentials.
	newIDSource     func(ctx context.Context, audience string) (oauth2.TokenSource, error)
	newAccessSource func(ctx context.Context) (oauth2.TokenSource, error)
}

var _ Tokens = (*ADC)(nil)

// NewADC builds the production minter. It contacts nothing until the first
// token is requested.
func NewADC() *ADC {
	return &ADC{
		idSources: map[string]oauth2.TokenSource{},
		newIDSource: func(ctx context.Context, audience string) (oauth2.TokenSource, error) {
			return idtoken.NewTokenSource(ctx, audience)
		},
		newAccessSource: func(ctx context.Context) (oauth2.TokenSource, error) {
			creds, err := google.FindDefaultCredentials(ctx, VertexScope)
			if err != nil {
				return nil, err
			}
			return creds.TokenSource, nil
		},
	}
}

// IDToken returns a cached or newly minted ID token for audience.
//
// The cached source is an oauth2.ReuseTokenSource (idtoken returns one), so a
// token is fetched again only near its expiry. The source is built with
// context.Background, not the request context: it outlives this request, and a
// source bound to a cancelled context would fail every later refresh. The
// request context still bounds the WAIT, through await.
func (a *ADC) IDToken(ctx context.Context, audience string) (string, error) {
	if strings.TrimSpace(audience) == "" {
		return "", errors.New("workloadidentity: empty ID-token audience")
	}
	src, err := a.idSource(ctx, audience)
	if err != nil {
		return "", err
	}
	return await(ctx, src)
}

// AccessToken returns a cached or newly minted cloud-platform access token.
func (a *ADC) AccessToken(ctx context.Context) (string, error) {
	a.mu.Lock()
	src := a.access
	a.mu.Unlock()
	if src == nil {
		// FindDefaultCredentials can probe the metadata server, so the build is
		// bounded by the request context like idSource's. The source itself is
		// built with context.Background: it is cached past this request.
		built, err := buildBounded(ctx, func() (oauth2.TokenSource, error) {
			return a.newAccessSource(context.Background())
		})
		if err != nil {
			return "", fmt.Errorf("workloadidentity: find default credentials: %w", err)
		}
		a.mu.Lock()
		if a.access == nil {
			a.access = built
		}
		src = a.access
		a.mu.Unlock()
	}
	return await(ctx, src)
}

func (a *ADC) idSource(ctx context.Context, audience string) (oauth2.TokenSource, error) {
	a.mu.Lock()
	src, ok := a.idSources[audience]
	a.mu.Unlock()
	if ok {
		return src, nil
	}
	// idtoken.NewTokenSource mints the first token while it builds the
	// source. Building outside the lock keeps one slow metadata answer from
	// stalling the other audiences; the cost is that two first requests for one
	// audience may both build, and the first one stored wins.
	built, err := buildBounded(ctx, func() (oauth2.TokenSource, error) {
		return a.newIDSource(context.Background(), audience)
	})
	if err != nil {
		// A failed build is NOT cached: the next request tries again, so a
		// metadata blip does not refuse every later request for this audience.
		return nil, fmt.Errorf("workloadidentity: build ID-token source: %w", err)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if existing, ok := a.idSources[audience]; ok {
		return existing, nil
	}
	if len(a.idSources) < maxCachedAudiences {
		a.idSources[audience] = built
	}
	return built, nil
}

// await reads one token from src and gives up when ctx ends. oauth2 token
// sources take no context, so the read runs in a goroutine; the metadata
// client's own timeouts bound that goroutine.
func await(ctx context.Context, src oauth2.TokenSource) (string, error) {
	type result struct {
		tok *oauth2.Token
		err error
	}
	done := make(chan result, 1)
	go func() {
		tok, err := src.Token()
		done <- result{tok, err}
	}()
	select {
	case <-ctx.Done():
		return "", fmt.Errorf("workloadidentity: token request: %w", ctx.Err())
	case r := <-done:
		if r.err != nil {
			return "", fmt.Errorf("workloadidentity: token request: %w", r.err)
		}
		if r.tok == nil || r.tok.AccessToken == "" {
			return "", errors.New("workloadidentity: the identity provider returned an empty token")
		}
		return r.tok.AccessToken, nil
	}
}

// buildBounded runs a source constructor that mints a token as it builds, and
// gives up when ctx ends.
func buildBounded(ctx context.Context, build func() (oauth2.TokenSource, error)) (oauth2.TokenSource, error) {
	type result struct {
		src oauth2.TokenSource
		err error
	}
	done := make(chan result, 1)
	go func() {
		src, err := build()
		done <- result{src, err}
	}()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case r := <-done:
		return r.src, r.err
	}
}
