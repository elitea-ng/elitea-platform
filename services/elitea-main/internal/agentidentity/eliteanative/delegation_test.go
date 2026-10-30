package eliteanative_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	josejwt "github.com/go-jose/go-jose/v4/jwt"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/agentidentity/eliteanative"
)

const delegationAudience = "https://mcp.partner.example/app/9/mcp"

func delegationSigner(t *testing.T, now *time.Time) (*eliteanative.DelegationSigner, *countingSigner) {
	t.Helper()
	esPEM, _ := ecPEM(t)
	es := &countingSigner{KeySigner: localSigner(t, "es-1", eliteanative.ES256, esPEM)}
	cfg := baseConfig()
	cfg.Keys = cfg.Keys[:1]
	cfg.RelyingParties = nil
	ring, err := eliteanative.NewKeyRing([]eliteanative.RingKey{{Signer: es}}, cfg.RotationOverlap, 16*time.Minute, nil)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := eliteanative.NewDelegationSigner(cfg, ring, eliteanative.WithClock(func() time.Time { return *now }))
	if err != nil {
		t.Fatal(err)
	}
	return signer, es
}

func delegationRequest() eliteanative.DelegationRequest {
	return eliteanative.DelegationRequest{
		Audience: delegationAudience, Agent: "binding:" + bindingID, TenantProjectID: tenantProject,
		ExecutionID: executionID, Generation: 2, UserID: "42", MandateID: "mandate-1",
		Scope: eliteanative.DelegationScope{Digest: "sha256:" + strings.Repeat("ab", 32), Toolkits: []string{"github", "mcp"}},
	}
}

// The assertion verifies with go-jose against the published JWKS, for the
// endpoint as audience, and carries the run, the agent, the user, the
// mandate and the scope. It lives 5 minutes at most.
func TestDelegationAssertionVerifiesAgainstTheJWKS(t *testing.T) {
	now := fixedNow
	signer, _ := delegationSigner(t, &now)
	token, claims, err := signer.Sign(context.Background(), delegationRequest())
	if err != nil {
		t.Fatal(err)
	}
	set := publishedKeys(t, signer.WellKnown())
	parsed, err := josejwt.ParseSigned(token, []jose.SignatureAlgorithm{jose.ES256, jose.RS256})
	if err != nil {
		t.Fatal(err)
	}
	if typ := parsed.Headers[0].ExtraHeaders[jose.HeaderType]; typ != eliteanative.TokenType {
		t.Fatalf("typ = %v", typ)
	}
	keys := set.Key(parsed.Headers[0].KeyID)
	if len(keys) != 1 {
		t.Fatalf("kid %q not published", parsed.Headers[0].KeyID)
	}
	var registered josejwt.Claims
	var custom eliteanative.DelegationClaims
	if err := parsed.Claims(keys[0].Key, &registered, &custom); err != nil {
		t.Fatalf("signature: %v", err)
	}
	if err := registered.ValidateWithLeeway(josejwt.Expected{
		Issuer: origin + eliteanative.IssuerPath, AnyAudience: josejwt.Audience{delegationAudience}, Time: now,
	}, 0); err != nil {
		t.Fatalf("registered claims: %v", err)
	}
	if registered.Expiry.Time().Sub(registered.IssuedAt.Time()) != eliteanative.DelegationMaxTTL {
		t.Fatalf("lifetime = %v", registered.Expiry.Time().Sub(registered.IssuedAt.Time()))
	}
	if custom.Subject != "agent:binding:"+bindingID || custom.Run != executionID || custom.AgentInstanceID != executionID+":2" ||
		custom.Purpose != eliteanative.DelegationPurpose || custom.User == nil || custom.User.Subject != "user:42" ||
		custom.Mandate != "mandate-1" || custom.Project != "7" || len(custom.Scope.Toolkits) != 2 || custom.ID == "" {
		t.Fatalf("claims = %+v", custom)
	}
	if custom.ID != claims.ID || signer.Issuer() != origin+eliteanative.IssuerPath || signer.JWKSURI() != origin+eliteanative.JWKSPath {
		t.Fatal("the returned claims or issuer differ from the token")
	}
	// After expiry the token no longer validates.
	if err := registered.ValidateWithLeeway(josejwt.Expected{Time: now.Add(eliteanative.DelegationMaxTTL + time.Second)}, 0); err == nil {
		t.Fatal("an expired assertion validated")
	}
}

// A longer TTL is capped at 5 minutes; a shorter one is kept.
func TestDelegationAssertionLifetimeIsCapped(t *testing.T) {
	now := fixedNow
	signer, _ := delegationSigner(t, &now)
	for ttl, want := range map[time.Duration]time.Duration{time.Hour: 5 * time.Minute, 2 * time.Minute: 2 * time.Minute, 0: 5 * time.Minute} {
		request := delegationRequest()
		request.TTL = ttl
		_, claims, err := signer.Sign(context.Background(), request)
		if err != nil {
			t.Fatal(err)
		}
		if got := time.Duration(claims.Expiry-claims.IssuedAt) * time.Second; got != want {
			t.Fatalf("ttl %v: lifetime %v, want %v", ttl, got, want)
		}
	}
}

// Every check runs before anything is signed.
func TestDelegationAssertionRefusesInvalidRequests(t *testing.T) {
	now := fixedNow
	signer, es := delegationSigner(t, &now)
	for name, mutate := range map[string]func(*eliteanative.DelegationRequest){
		"audience not normalized": func(r *eliteanative.DelegationRequest) { r.Audience = "https://MCP.partner.example/app/9/mcp/" },
		"http audience":           func(r *eliteanative.DelegationRequest) { r.Audience = "http://mcp.partner.example/mcp" },
		"no agent":                func(r *eliteanative.DelegationRequest) { r.Agent = "" },
		"no project":              func(r *eliteanative.DelegationRequest) { r.TenantProjectID = 0 },
		"no generation":           func(r *eliteanative.DelegationRequest) { r.Generation = 0 },
		"bad scope digest":        func(r *eliteanative.DelegationRequest) { r.Scope.Digest = "md5:x" },
		"unsorted scope":          func(r *eliteanative.DelegationRequest) { r.Scope.Toolkits = []string{"mcp", "github"} },
		"too short":               func(r *eliteanative.DelegationRequest) { r.TTL = time.Second },
	} {
		request := delegationRequest()
		mutate(&request)
		if _, _, err := signer.Sign(context.Background(), request); !errors.Is(err, eliteanative.ErrInvalidDelegation) {
			t.Fatalf("%s: err = %v", name, err)
		}
	}
	if es.calls.Load() != 0 {
		t.Fatalf("signed %d times before refusing", es.calls.Load())
	}
}

func TestNormalizeAudience(t *testing.T) {
	for raw, want := range map[string]string{
		"https://MCP.Example.com/app/1/mcp/":   "https://mcp.example.com/app/1/mcp",
		"https://mcp.example.com:443/mcp":      "https://mcp.example.com/mcp",
		"https://mcp.example.com:8443/mcp":     "https://mcp.example.com:8443/mcp",
		"https://mcp.example.com":              "https://mcp.example.com",
		"http://mcp.example.com/mcp":           "",
		"https://user:pw@mcp.example.com/mcp":  "",
		"https://mcp.example.com/mcp?x=1":      "",
		"https://mcp.example.com/mcp#fragment": "",
		"not a url":                            "",
	} {
		got, ok := eliteanative.NormalizeAudience(raw)
		if (want == "") == ok || got != want {
			t.Fatalf("%q: got %q, %v; want %q", raw, got, ok, want)
		}
	}
}
