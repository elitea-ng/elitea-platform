package mcp

// The governed inbound MCP token verifier (ADR-0039, issue #1123). An MCP
// client that followed the MCP authorization specification presents an
// OAuth access token minted for THIS server: its audience is the MCP
// endpoint's resource URI (RFC 8707), never the platform's, and it is
// accepted from an allow-listed issuer only. A token minted for another
// resource is refused even when the issuer and signature are good, which is
// what keeps a platform token from being replayed at a remote MCP server and
// a remote server's token from being accepted here.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"time"

	coreoidc "github.com/coreos/go-oidc/v3/oidc"
)

// ErrInboundToken refuses an inbound MCP access token. The cause is never
// put in the response: it is one 401 with the challenge.
var ErrInboundToken = errors.New("mcp: the access token is not valid for this resource")

const (
	maxInboundTokenBytes = 16 << 10
	inboundClockSkew     = time.Minute
	maxInboundScopes     = 64
)

// InboundToken is a verified inbound access token. It holds no token value.
type InboundToken struct {
	Issuer   string
	Subject  string
	ClientID string
	Scopes   []string
	Expiry   time.Time
	// UserClaim is the value of the configured user claim (email by default).
	UserClaim string
}

// OAuthVerifier verifies inbound MCP access tokens: issuer allow-list,
// signature against the issuer's JWKS, `aud` equal to the MCP resource,
// `exp`/`nbf`, and the required scopes. Safe for concurrent use.
type OAuthVerifier struct {
	issuers   map[string]coreoidc.KeySet
	required  []string
	userClaim string
	now       func() time.Time
}

// OAuthVerifierConfig configures the verifier. Issuers maps each allowed
// issuer to its key set.
type OAuthVerifierConfig struct {
	Issuers        map[string]coreoidc.KeySet
	RequiredScopes []string
	// UserClaim names the claim that identifies the Elitea user; default
	// "email".
	UserClaim string
	Now       func() time.Time
}

// NewOAuthVerifier validates cfg.
func NewOAuthVerifier(cfg OAuthVerifierConfig) (*OAuthVerifier, error) {
	if len(cfg.Issuers) == 0 {
		return nil, errors.New("mcp: the inbound verifier needs at least one allowed issuer")
	}
	issuers := make(map[string]coreoidc.KeySet, len(cfg.Issuers))
	for issuer, keys := range cfg.Issuers {
		if !strings.HasPrefix(issuer, "https://") || keys == nil {
			return nil, errors.New("mcp: an inbound issuer must be https and have a key set")
		}
		issuers[issuer] = keys
	}
	if cfg.UserClaim == "" {
		cfg.UserClaim = "email"
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &OAuthVerifier{issuers: issuers, required: slices.Clone(cfg.RequiredScopes), userClaim: cfg.UserClaim, now: cfg.Now}, nil
}

// Issuers lists the allowed issuers (the metadata's authorization_servers).
func (v *OAuthVerifier) Issuers() []string {
	out := make([]string, 0, len(v.issuers))
	for issuer := range v.issuers {
		out = append(out, issuer)
	}
	slices.Sort(out)
	return out
}

// RequiredScopes are the scopes every token must carry.
func (v *OAuthVerifier) RequiredScopes() []string { return slices.Clone(v.required) }

// Recognizes reports whether raw is a JWT whose unverified issuer is
// allow-listed: such a bearer is this verifier's to accept or refuse, and
// never falls through to the platform's own credentials.
func (v *OAuthVerifier) Recognizes(raw string) bool {
	issuer, ok := unverifiedIssuer(raw)
	if !ok {
		return false
	}
	_, ok = v.issuers[issuer]
	return ok
}

type inboundClaims struct {
	Issuer    string          `json:"iss"`
	Subject   string          `json:"sub"`
	Audience  json.RawMessage `json:"aud"`
	Expiry    *float64        `json:"exp"`
	NotBefore *float64        `json:"nbf"`
	Scope     string          `json:"scope"`
	Scp       json.RawMessage `json:"scp"`
	ClientID  string          `json:"client_id"`
	AZP       string          `json:"azp"`
}

// Verify checks raw for resource. Every failure is ErrInboundToken.
func (v *OAuthVerifier) Verify(ctx context.Context, raw, resource string) (InboundToken, error) {
	if len(raw) == 0 || len(raw) > maxInboundTokenBytes || resource == "" {
		return InboundToken{}, ErrInboundToken
	}
	issuer, ok := unverifiedIssuer(raw)
	if !ok {
		return InboundToken{}, ErrInboundToken
	}
	keys, ok := v.issuers[issuer]
	if !ok {
		return InboundToken{}, ErrInboundToken
	}
	payload, err := keys.VerifySignature(ctx, raw)
	if err != nil {
		return InboundToken{}, ErrInboundToken
	}
	var claims inboundClaims
	if json.Unmarshal(payload, &claims) != nil || claims.Issuer != issuer || claims.Subject == "" || claims.Expiry == nil {
		return InboundToken{}, ErrInboundToken
	}
	// The audience is exactly this resource: a token also valid for
	// another server could be replayed there.
	if audience, ok := singleAudience(claims.Audience); !ok || audience != resource {
		return InboundToken{}, ErrInboundToken
	}
	now := v.now()
	expiry := time.Unix(int64(*claims.Expiry), 0)
	if !now.Add(-inboundClockSkew).Before(expiry) {
		return InboundToken{}, ErrInboundToken
	}
	if claims.NotBefore != nil && now.Add(inboundClockSkew).Before(time.Unix(int64(*claims.NotBefore), 0)) {
		return InboundToken{}, ErrInboundToken
	}
	scopes := tokenScopes(claims)
	for _, scope := range v.required {
		if !slices.Contains(scopes, scope) {
			return InboundToken{}, ErrInboundToken
		}
	}
	var extra map[string]any
	_ = json.Unmarshal(payload, &extra)
	user, _ := extra[v.userClaim].(string)
	client := claims.ClientID
	if client == "" {
		client = claims.AZP
	}
	return InboundToken{Issuer: issuer, Subject: claims.Subject, ClientID: client, Scopes: scopes, Expiry: expiry, UserClaim: user}, nil
}

func singleAudience(raw json.RawMessage) (string, bool) {
	var one string
	if json.Unmarshal(raw, &one) == nil {
		return one, one != ""
	}
	var many []string
	if json.Unmarshal(raw, &many) == nil && len(many) == 1 {
		return many[0], many[0] != ""
	}
	return "", false
}

func tokenScopes(c inboundClaims) []string {
	scopes := strings.Fields(c.Scope)
	var list []string
	if json.Unmarshal(c.Scp, &list) == nil {
		scopes = append(scopes, list...)
	} else {
		var text string
		if json.Unmarshal(c.Scp, &text) == nil {
			scopes = append(scopes, strings.Fields(text)...)
		}
	}
	if len(scopes) > maxInboundScopes {
		scopes = scopes[:maxInboundScopes]
	}
	return scopes
}

// unverifiedIssuer reads `iss` from a compact JWS without verifying it,
// only to pick the key set. Nothing else is read before verification.
func unverifiedIssuer(raw string) (string, bool) {
	parts := strings.Split(raw, ".")
	if len(parts) != 3 || len(raw) > maxInboundTokenBytes {
		return "", false
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", false
	}
	var claims struct {
		Issuer string `json:"iss"`
	}
	if json.Unmarshal(payload, &claims) != nil || claims.Issuer == "" {
		return "", false
	}
	return claims.Issuer, true
}
