package eliteanative_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	josejwt "github.com/go-jose/go-jose/v4/jwt"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/agentidentity"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/agentidentity/eliteanative"
	identity "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/agentidentity"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/secretstore"
)

func fetch(t *testing.T, handler http.HandlerFunc, path string) (*httptest.ResponseRecorder, []byte) {
	t.Helper()
	rec := httptest.NewRecorder()
	handler(rec, httptest.NewRequest(http.MethodGet, path, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s = %d", path, rec.Code)
	}
	return rec, rec.Body.Bytes()
}

func publishedKeys(t *testing.T, w *eliteanative.WellKnown) jose.JSONWebKeySet {
	t.Helper()
	_, body := fetch(t, w.ServeJWKS, eliteanative.JWKSPath)
	var set jose.JSONWebKeySet
	if err := json.Unmarshal(body, &set); err != nil {
		t.Fatalf("the JWKS does not parse with go-jose: %v", err)
	}
	return set
}

// verify checks token against the published JWKS with go-jose only: an
// independent JOSE implementation, not this package's code.
func verify(t *testing.T, set jose.JSONWebKeySet, token, audience string, at time.Time) eliteanative.Claims {
	t.Helper()
	parsed, err := josejwt.ParseSigned(token, []jose.SignatureAlgorithm{jose.ES256, jose.RS256})
	if err != nil {
		t.Fatalf("go-jose cannot parse the assertion: %v", err)
	}
	if len(parsed.Headers) != 1 {
		t.Fatalf("headers = %d", len(parsed.Headers))
	}
	header := parsed.Headers[0]
	if typ := header.ExtraHeaders[jose.HeaderType]; typ != eliteanative.TokenType {
		t.Fatalf("typ = %v, want %s", typ, eliteanative.TokenType)
	}
	keys := set.Key(header.KeyID)
	if len(keys) != 1 {
		t.Fatalf("kid %q is not in the published JWKS exactly once", header.KeyID)
	}
	var registered josejwt.Claims
	var custom eliteanative.Claims
	if err := parsed.Claims(keys[0].Key, &registered, &custom); err != nil {
		t.Fatalf("the signature does not verify against the published key: %v", err)
	}
	if err := registered.ValidateWithLeeway(josejwt.Expected{
		Issuer: origin + eliteanative.IssuerPath, AnyAudience: josejwt.Audience{audience}, Time: at,
	}, 0); err != nil {
		t.Fatalf("registered claims: %v", err)
	}
	if len(registered.Audience) != 1 || registered.Audience[0] != audience {
		t.Fatalf("aud = %v, want exactly [%s]", registered.Audience, audience)
	}
	return custom
}

// Acceptance: an assertion verifies against the published JWKS, and aud is
// exactly one configured relying party. Both algorithms.
func TestAssertionVerifiesAgainstThePublishedJWKS(t *testing.T) {
	f := newFixture(t, baseConfig(), nil)
	set := publishedKeys(t, f.provider.WellKnown())
	for _, tc := range []struct {
		audience string
		alg      jose.SignatureAlgorithm
	}{{awsAudience, jose.ES256}, {vaultAudience, jose.RS256}} {
		t.Run(string(tc.alg), func(t *testing.T) {
			cred, err := f.credential(t, admission(30*time.Minute), tc.audience)
			if err != nil {
				t.Fatalf("Credential: %v", err)
			}
			parsed, err := josejwt.ParseSigned(cred.Token, []jose.SignatureAlgorithm{tc.alg})
			if err != nil {
				t.Fatalf("not %s: %v", tc.alg, err)
			}
			_ = parsed
			claims := verify(t, set, cred.Token, tc.audience, fixedNow)
			want := eliteanative.Claims{
				Issuer: origin + eliteanative.IssuerPath, Subject: "agent:" + externalID, Audience: tc.audience,
				IssuedAt: fixedNow.Unix(), NotBefore: fixedNow.Unix(), Expiry: claims.Expiry, ID: claims.ID,
				SubjectProfile: "ai_agent", AgentInstanceID: executionID + ":4", Tenant: "elitea.example.com",
				Project: "7", Binding: bindingID, VersionDigest: "abcd" + strings.Repeat("00", 30),
				PolicyRevision: "2026-10-r1", Epoch: 3,
			}
			if claims != want {
				t.Fatalf("claims = %+v\nwant     %+v", claims, want)
			}
			if claims.ID == "" || cred.TokenType != eliteanative.TokenTypeJWT || cred.Actor.Fidelity != agentidentity.FidelityAttributed {
				t.Fatalf("credential = %v, jti %q", cred, claims.ID)
			}
		})
	}
	// Two assertions never share a jti.
	a, _ := f.credential(t, admission(30*time.Minute), awsAudience)
	b, _ := f.credential(t, admission(30*time.Minute), awsAudience)
	if verify(t, set, a.Token, awsAudience, fixedNow).ID == verify(t, set, b.Token, awsAudience, fixedNow).ID {
		t.Fatal("two assertions share a jti")
	}
	// The raw payload carries no field outside the documented claim set.
	payload := strings.Split(a.Token, ".")[1]
	raw, err := jose.ParseSigned(a.Token, []jose.SignatureAlgorithm{jose.ES256})
	if err != nil || payload == "" {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(raw.UnsafePayloadWithoutVerification(), &fields); err != nil {
		t.Fatal(err)
	}
	for name := range fields {
		if strings.Contains(name, "email") || strings.Contains(name, "name") || name == "elitea_user" {
			t.Fatalf("the app-only assertion carries %q", name)
		}
	}
}

// Acceptance: TTL never exceeds the relying party max_ttl or the session
// expiry.
func TestTTLNeverExceedsRelyingPartyMaxTTLOrSessionExpiry(t *testing.T) {
	f := newFixture(t, baseConfig(), nil)
	set := publishedKeys(t, f.provider.WellKnown())
	for _, tc := range []struct {
		name     string
		audience string
		session  time.Duration
		want     time.Duration
	}{
		{"relying party max_ttl bounds it", vaultAudience, time.Hour, 2 * time.Minute},
		{"default max_ttl bounds it", awsAudience, time.Hour, eliteanative.DefaultTTL},
		{"session expiry bounds it", awsAudience, 90 * time.Second, 90 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cred, err := f.credential(t, admission(tc.session), tc.audience)
			if err != nil {
				t.Fatal(err)
			}
			claims := verify(t, set, cred.Token, tc.audience, fixedNow)
			if got := time.Duration(claims.Expiry-claims.IssuedAt) * time.Second; got != tc.want {
				t.Fatalf("lifetime = %s, want %s", got, tc.want)
			}
			if !cred.ExpiresAt.Equal(time.Unix(claims.Expiry, 0)) || cred.ExpiresAt.After(fixedNow.Add(tc.session)) {
				t.Fatalf("ExpiresAt %s is past the session or disagrees with exp", cred.ExpiresAt)
			}
		})
	}
	// Directly, a spec TTL above every bound is cut to the relying party's.
	cred, err := f.provider.AssumeApp(context.Background(), assumeSpec(vaultAudience, time.Hour, fixedNow.Add(time.Hour)))
	if err != nil {
		t.Fatal(err)
	}
	if got := cred.ExpiresAt.Sub(fixedNow); got != 2*time.Minute {
		t.Fatalf("lifetime = %s", got)
	}
	// A session with less than MinTTL left is refused, by the broker and by
	// the provider.
	if _, err := f.credential(t, admission(10*time.Second), awsAudience); !errors.Is(err, agentidentity.ErrDenied) {
		t.Fatalf("a 10s session was signed for: %v", err)
	}
	before := f.signs()
	if _, err := f.provider.AssumeApp(context.Background(), assumeSpec(awsAudience, time.Minute, fixedNow.Add(10*time.Second))); !errors.Is(err, eliteanative.ErrInvalidRequest) {
		t.Fatalf("err = %v", err)
	}
	if f.signs() != before {
		t.Fatal("a refused request reached the signer")
	}
}

func assumeSpec(audience string, ttl time.Duration, sessionExpiry time.Time) agentidentity.AssumeSpec {
	return agentidentity.AssumeSpec{
		Ref:      identity.ExternalRef{Provider: eliteanative.ProviderName, ProviderTenant: "elitea", Kind: eliteanative.RefKind, ID: externalID, Generation: 1},
		Audience: audience, TTL: ttl, SessionID: "s-1",
		Run: agentidentity.RunContext{
			TenantProjectID: tenantProject, ExecutionID: executionID, Generation: 4, AgentBindingID: bindingID,
			AgentBindingEpoch: 2, ExternalAgentIdentityID: externalID, ExternalEpoch: 3,
			PolicyRevisionID: "2026-10-r1", SessionExpiresAt: sessionExpiry,
		},
	}
}

// Acceptance (negative): an unknown relying party, a suspended identity and
// a stale epoch fail before anything is signed.
func TestUnknownRelyingPartySuspendedIdentityAndStaleEpochFailBeforeSigning(t *testing.T) {
	ctx := context.Background()
	t.Run("unknown relying party", func(t *testing.T) {
		f := newFixture(t, baseConfig(), nil)
		// In the session's ceiling, but not a configured relying party.
		_, err := f.credential(t, admission(30*time.Minute), "https://not-configured.example.com")
		if reason, _ := agentidentity.ReasonOf(err); reason != agentidentity.ReasonProviderFailed {
			t.Fatalf("err = %v", err)
		}
		_, err = f.provider.AssumeApp(ctx, assumeSpec("https://not-configured.example.com", time.Minute, fixedNow.Add(time.Hour)))
		if !errors.Is(err, eliteanative.ErrUnknownRelyingParty) {
			t.Fatalf("err = %v", err)
		}
		// An audience that only differs in case or a trailing slash is not
		// the configured one: the match is exact.
		_, err = f.provider.AssumeApp(ctx, assumeSpec(awsAudience+"/", time.Minute, fixedNow.Add(time.Hour)))
		if !errors.Is(err, eliteanative.ErrUnknownRelyingParty) {
			t.Fatalf("err = %v", err)
		}
		if f.signs() != 0 {
			t.Fatalf("the signer was called %d times", f.signs())
		}
	})
	t.Run("suspended identity", func(t *testing.T) {
		f := newFixture(t, baseConfig(), nil)
		ref, err := f.broker.OpenSession(ctx, admission(30*time.Minute))
		if err != nil {
			t.Fatal(err)
		}
		f.store.setIdentity(func(e *identity.ExternalAgentIdentity) {
			e.State = identity.ExternalIdentitySuspended
			e.RevocationEpoch++
		})
		if _, err := f.broker.Credential(ctx, claim(ref), agentidentity.CredentialRequest{Audience: awsAudience}); !errors.Is(err, agentidentity.ErrDenied) {
			t.Fatalf("err = %v", err)
		}
		// The provider re-reads the record itself, so even a caller that
		// skipped the broker's check is refused.
		spec := assumeSpec(awsAudience, time.Minute, fixedNow.Add(time.Hour))
		spec.Run.ExternalEpoch = 4
		if _, err := f.provider.AssumeApp(ctx, spec); !errors.Is(err, eliteanative.ErrIdentityNotActive) {
			t.Fatalf("err = %v", err)
		}
		if f.signs() != 0 {
			t.Fatalf("the signer was called %d times", f.signs())
		}
	})
	t.Run("stale epoch", func(t *testing.T) {
		f := newFixture(t, baseConfig(), nil)
		ref, err := f.broker.OpenSession(ctx, admission(30*time.Minute))
		if err != nil {
			t.Fatal(err)
		}
		f.store.setIdentity(func(e *identity.ExternalAgentIdentity) { e.RevocationEpoch++ })
		_, err = f.broker.Credential(ctx, claim(ref), agentidentity.CredentialRequest{Audience: awsAudience})
		if reason, _ := agentidentity.ReasonOf(err); reason != agentidentity.ReasonExternalIdentityStale {
			t.Fatalf("err = %v", err)
		}
		if _, err := f.provider.AssumeApp(ctx, assumeSpec(awsAudience, time.Minute, fixedNow.Add(time.Hour))); !errors.Is(err, eliteanative.ErrStaleEpoch) {
			t.Fatalf("err = %v", err)
		}
		if f.signs() != 0 {
			t.Fatalf("the signer was called %d times", f.signs())
		}
	})
	t.Run("another provider's ref or a missing run context", func(t *testing.T) {
		f := newFixture(t, baseConfig(), nil)
		spec := assumeSpec(awsAudience, time.Minute, fixedNow.Add(time.Hour))
		spec.Ref.Provider = "entra"
		if _, err := f.provider.AssumeApp(ctx, spec); !errors.Is(err, eliteanative.ErrInvalidRequest) {
			t.Fatalf("err = %v", err)
		}
		spec = assumeSpec(awsAudience, time.Minute, fixedNow.Add(time.Hour))
		spec.Run.SessionExpiresAt = time.Time{}
		if _, err := f.provider.AssumeApp(ctx, spec); !errors.Is(err, eliteanative.ErrInvalidRequest) {
			t.Fatalf("err = %v", err)
		}
		if f.signs() != 0 {
			t.Fatalf("the signer was called %d times", f.signs())
		}
	})
}

// Acceptance (negative): a policy that requires verified user evidence
// cannot be satisfied by this provider.
func TestVerifiedUserEvidenceIsNeverSatisfied(t *testing.T) {
	f := newFixture(t, baseConfig(), assertedAllowed{})
	if f.provider.Capabilities().VerifiesUser {
		t.Fatal("the provider claims to verify users")
	}
	in := admission(30 * time.Minute)
	in.Profile = agentidentity.ProfileDelegatedVerified
	in.DelegationID = "0192f5e2-7c3a-7b1e-8a9d-2b4c6d8e0d09"
	in.Evidence = agentidentity.VerifiedUserEvidence{Issuer: "https://login.example.com", TenantID: "t", ObjectID: "u", Audience: "a", DelegationID: in.DelegationID}
	_, err := f.broker.OpenSession(context.Background(), in)
	if reason, _ := agentidentity.ReasonOf(err); reason != agentidentity.ReasonVerifiedUserNotSupported {
		t.Fatalf("err = %v", err)
	}
	_, err = f.provider.AssumeOnBehalfOf(context.Background(), agentidentity.OBOSpec{
		AssumeSpec: assumeSpec(awsAudience, time.Minute, fixedNow.Add(time.Hour)),
		Evidence:   agentidentity.VerifiedUserEvidence{ObjectID: "u", DelegationID: in.DelegationID},
	})
	if !errors.Is(err, eliteanative.ErrUnsupported) {
		t.Fatalf("err = %v", err)
	}
	if f.signs() != 0 {
		t.Fatal("verified evidence reached the signer")
	}
}

// elitea_user is present only with user attribution, and always "asserted".
func TestAssertedUserIsStatedAsAssertedNeverVerified(t *testing.T) {
	f := newFixture(t, baseConfig(), assertedAllowed{})
	in := admission(30 * time.Minute)
	in.MandateID = "0192f5e2-7c3a-7b1e-8a9d-2b4c6d8e0d01"
	in.Evidence = agentidentity.AssertedUserEvidence{Issuer: "elitea", Subject: "user:42", MandateID: in.MandateID}
	cred, err := f.credential(t, in, awsAudience)
	if err != nil {
		t.Fatal(err)
	}
	claims := verify(t, publishedKeys(t, f.provider.WellKnown()), cred.Token, awsAudience, fixedNow)
	if claims.User == nil || *claims.User != (eliteanative.AssertedUser{Subject: "user:42", Evidence: "asserted"}) {
		t.Fatalf("elitea_user = %+v", claims.User)
	}
	if cred.Actor.Subject == nil || cred.Actor.Subject.Ref != "user:42" || cred.Actor.Fidelity != agentidentity.FidelityAttributed {
		t.Fatalf("actor = %+v", cred.Actor)
	}
}

// Acceptance: rotation. The new key is in the JWKS for the whole overlap
// before its first use, and an assertion signed with the old key verifies
// until it expires.
func TestRotationPublishesBeforeUseAndKeepsTheOldKeyUntilItsAssertionsExpire(t *testing.T) {
	oldPEM, _ := ecPEM(t)
	newPEM, _ := ecPEM(t)
	cfg := baseConfig()
	cfg.Keys = []eliteanative.KeySpec{
		{KeyID: "es-old", Algorithm: eliteanative.ES256, File: "/keys/old.pem"},
		{KeyID: "es-new", Algorithm: eliteanative.ES256, File: "/keys/new.pem", NotBefore: fixedNow.Add(time.Hour)},
	}
	cfg.RelyingParties = cfg.RelyingParties[:1] // ES256 only
	files := map[string][]byte{"/keys/old.pem": oldPEM, "/keys/new.pem": newPEM}
	ring, err := eliteanative.LoadKeyRing(context.Background(), cfg, eliteanative.KeySources{
		ReadFile: func(path string) ([]byte, error) { return append([]byte(nil), files[path]...), nil },
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	store := newMemStore()
	now := fixedNow
	provider, err := eliteanative.New(cfg, ring, store, eliteanative.WithClock(func() time.Time { return now }))
	if err != nil {
		t.Fatal(err)
	}
	wk := provider.WellKnown()
	kids := func() []string {
		var out []string
		for _, k := range publishedKeys(t, wk).Keys {
			out = append(out, k.KeyID)
		}
		return out
	}
	signedBy := func() (string, string) {
		cred, err := provider.AssumeApp(context.Background(), assumeSpec(awsAudience, time.Hour, now.Add(time.Hour)))
		if err != nil {
			t.Fatal(err)
		}
		parsed, err := josejwt.ParseSigned(cred.Token, []jose.SignatureAlgorithm{jose.ES256})
		if err != nil {
			t.Fatal(err)
		}
		return parsed.Headers[0].KeyID, cred.Token
	}
	overlap := eliteanative.DefaultRotationOverlap
	newUsable := fixedNow.Add(time.Hour + overlap)

	// Before publication: only the old key, which signs.
	if got := kids(); fmt.Sprint(got) != "[es-old]" {
		t.Fatalf("JWKS before publication = %v", got)
	}
	// Published, inside the overlap: both keys, the old one still signs.
	now = fixedNow.Add(time.Hour)
	if got := kids(); fmt.Sprint(got) != "[es-old es-new]" {
		t.Fatalf("JWKS at publication = %v", got)
	}
	now = newUsable.Add(-time.Second)
	kid, lastOld := signedBy()
	if kid != "es-old" {
		t.Fatalf("the new key signed %s before the overlap ended", newUsable.Sub(now))
	}
	set := publishedKeys(t, wk)
	// The overlap is over: the new key signs, and it has been published for
	// the whole overlap.
	now = newUsable
	if kid, _ := signedBy(); kid != "es-new" {
		t.Fatalf("signed by %s after the overlap", kid)
	}
	if !set.Key("es-new")[0].Valid() {
		t.Fatal("the new key was not in the JWKS before its first use")
	}
	// The last old-key assertion still verifies against the JWKS served
	// just before it expires.
	now = newUsable.Add(eliteanative.DefaultTTL - 2*time.Second)
	if got := kids(); fmt.Sprint(got) != "[es-old es-new]" {
		t.Fatalf("the old key left the JWKS while its assertions are live: %v", got)
	}
	verify(t, publishedKeys(t, wk), lastOld, awsAudience, now)
	// Past the longest assertion it may have signed (plus skew), it is gone.
	now = newUsable.Add(eliteanative.DefaultTTL + 2*time.Minute)
	if got := kids(); fmt.Sprint(got) != "[es-new]" {
		t.Fatalf("JWKS after retention = %v", got)
	}
}

// Acceptance (negative): a key ring that reuses the command signing key or
// the code-sandbox grant key refuses to build.
func TestKeyRingRefusesAnotherPurposesKeyIDOrMaterial(t *testing.T) {
	material, _ := ecPEM(t)
	signer := localSigner(t, "shared-kid", eliteanative.ES256, material)
	fp, err := eliteanative.Fingerprint(signer.Public())
	if err != nil {
		t.Fatal(err)
	}
	for _, foreign := range []eliteanative.ForeignKey{
		{Purpose: "command-signing", KeyID: "shared-kid"},
		{Purpose: "code-sandbox-grant", KeyID: "other", Fingerprint: fp},
	} {
		_, err := eliteanative.NewKeyRing([]eliteanative.RingKey{{Signer: signer}}, time.Hour, time.Hour, []eliteanative.ForeignKey{foreign})
		if !errors.Is(err, eliteanative.ErrKeyReuse) || !strings.Contains(err.Error(), foreign.Purpose) {
			t.Fatalf("%s: err = %v", foreign.Purpose, err)
		}
	}
	// A different key and kid is accepted.
	other, _ := ecPEM(t)
	otherSigner := localSigner(t, "other", eliteanative.ES256, other)
	otherPrint, _ := eliteanative.Fingerprint(otherSigner.Public())
	if _, err := eliteanative.NewKeyRing([]eliteanative.RingKey{{Signer: signer}}, time.Hour, time.Hour,
		[]eliteanative.ForeignKey{{Purpose: "command-signing", KeyID: "cmd-1", Fingerprint: otherPrint}}); err != nil {
		t.Fatal(err)
	}
}

func TestLocalSignerRefusesEdDSAAndMismatchedAlgorithms(t *testing.T) {
	_, edKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	ec, _ := ecPEM(t)
	rs, _ := rsaPEM(t)
	for name, tc := range map[string]struct {
		alg      eliteanative.Algorithm
		material []byte
	}{
		"ed25519":       {eliteanative.ES256, pkcs8(t, edKey)},
		"ec as RS256":   {eliteanative.RS256, ec},
		"rsa as ES256":  {eliteanative.ES256, rs},
		"EdDSA":         {eliteanative.Algorithm("EdDSA"), pkcs8(t, edKey)},
		"not PEM":       {eliteanative.ES256, []byte("not a key")},
		"trailing data": {eliteanative.ES256, append(append([]byte(nil), ec...), "x"...)},
	} {
		if _, err := eliteanative.NewLocalSigner("k", tc.alg, secretstore.NewRuntimeSecret(tc.material)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// Acceptance: assertions and keys never appear in logs. Every formatting
// path of every value that holds one is logged at debug and searched.
func TestAssertionsAndKeysNeverAppearInLogs(t *testing.T) {
	material, key := ecPEM(t)
	canaryPEM := string(material)
	f := newFixture(t, baseConfig(), nil)
	signer := localSigner(t, "canary", eliteanative.ES256, material)
	ring, err := eliteanative.NewKeyRing([]eliteanative.RingKey{{Signer: signer}}, time.Hour, time.Hour, nil)
	if err != nil {
		t.Fatal(err)
	}
	cred, err := f.credential(t, admission(30*time.Minute), awsAudience)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&out, &slog.HandlerOptions{Level: slog.LevelDebug}))
	logger.Debug("credential", "credential", cred, "signer", signer, "ring", ring, "provider", f.provider)
	_, refused := f.credential(t, admission(30*time.Minute), "https://not-configured.example.com")
	logger.Error("refused", "err", refused)
	for _, v := range []any{cred, signer, ring} {
		fmt.Fprintf(&out, "%v %+v %#v %s\n", v, v, v, v)
	}
	raw, _ := json.Marshal(cred)
	out.Write(raw)
	events, _ := json.Marshal(f.store.events)
	out.Write(events)

	logged := out.String()
	signature := cred.Token[strings.LastIndex(cred.Token, ".")+1:]
	for name, canary := range map[string]string{
		"assertion":     cred.Token,
		"signature":     signature,
		"payload":       strings.Split(cred.Token, ".")[1],
		"private PEM":   strings.Split(canaryPEM, "\n")[1],
		"private D":     key.D.String(),
		"private D hex": fmt.Sprintf("%x", key.D.Bytes()),
	} {
		if strings.Contains(logged, canary) {
			t.Fatalf("the %s appears in the log output", name)
		}
	}
	if !strings.Contains(logged, "[REDACTED]") {
		t.Fatal("nothing was redacted: the canary measured nothing")
	}
}

func TestResolveReadsTheLocalRecordAndProvisionMakesNoExternalCall(t *testing.T) {
	f := newFixture(t, baseConfig(), nil)
	ctx := context.Background()
	ref := identity.ExternalRef{Provider: eliteanative.ProviderName, ProviderTenant: "elitea", Kind: eliteanative.RefKind, ID: externalID, Generation: 1}
	got, err := f.provider.Resolve(ctx, ref)
	if err != nil || !got.Enabled || got.Ref != ref {
		t.Fatalf("Resolve = %+v, %v", got, err)
	}
	ref.Generation = 2
	if _, err := f.provider.Resolve(ctx, ref); !errors.Is(err, agentidentity.ErrIdentityReplaced) {
		t.Fatalf("a generation mismatch: %v", err)
	}
	ref.ID = "0192f5e2-7c3a-7b1e-8a9d-2b4c6d8e0eff"
	if _, err := f.provider.Resolve(ctx, ref); !errors.Is(err, agentidentity.ErrIdentityNotFound) {
		t.Fatalf("a missing record: %v", err)
	}
	f.store.setIdentity(func(e *identity.ExternalAgentIdentity) { e.State = identity.ExternalIdentitySuspended })
	if got, err := f.provider.Resolve(ctx, identity.ExternalRef{Provider: eliteanative.ProviderName, ID: externalID, Generation: 1}); err != nil || got.Enabled {
		t.Fatalf("a suspended record resolves as %+v, %v", got, err)
	}
	result, err := f.provider.Provision(ctx, agentidentity.ProvisionSpec{
		ExternalAgentIdentityID: externalID, Mode: identity.IdentityModeEliteaNative, ProviderTenant: "elitea",
	})
	if err != nil || result.Pending || result.Ref.ID != externalID || result.Ref.Kind != eliteanative.RefKind || result.Ref.Validate(true) != nil {
		t.Fatalf("Provision = %+v, %v", result, err)
	}
	if _, err := f.provider.Provision(ctx, agentidentity.ProvisionSpec{ExternalAgentIdentityID: externalID, Mode: identity.IdentityModeDedicatedAgent}); err == nil {
		t.Fatal("a dedicated_agent record was provisioned as Elitea-native")
	}
	if page, err := f.provider.List(ctx, agentidentity.ListSpec{}); err != nil || len(page.Objects) != 0 {
		t.Fatalf("List = %+v, %v", page, err)
	}
}

func TestIssuerMetadataNamesTheIssuerAndJWKSOnly(t *testing.T) {
	f := newFixture(t, baseConfig(), nil)
	rec, body := fetch(t, f.provider.WellKnown().ServeMetadata, eliteanative.MetadataPath)
	var doc map[string]any
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatal(err)
	}
	if doc["issuer"] != origin+eliteanative.IssuerPath || doc["jwks_uri"] != origin+eliteanative.JWKSPath {
		t.Fatalf("metadata = %s", body)
	}
	if fmt.Sprint(doc["id_token_signing_alg_values_supported"]) != "[ES256 RS256]" {
		t.Fatalf("algorithms = %v", doc["id_token_signing_alg_values_supported"])
	}
	// The served max-age is the one the overlap is validated against.
	if want := fmt.Sprintf("public, max-age=%d", int(eliteanative.CacheMaxAge.Seconds())); rec.Header().Get("Cache-Control") != want {
		t.Fatalf("Cache-Control = %q", rec.Header().Get("Cache-Control"))
	}
	jwks, jwksBody := fetch(t, f.provider.WellKnown().ServeJWKS, eliteanative.JWKSPath)
	if jwks.Header().Get("Content-Type") != "application/jwk-set+json" {
		t.Fatalf("Content-Type = %q", jwks.Header().Get("Content-Type"))
	}
	for _, private := range []string{`"d"`, `"p"`, `"q"`, `"dp"`, `"dq"`, `"qi"`} {
		if bytes.Contains(jwksBody, []byte(private)) {
			t.Fatalf("the JWKS holds private member %s", private)
		}
	}
	for _, k := range publishedKeys(t, f.provider.WellKnown()).Keys {
		if !k.IsPublic() || k.Use != "sig" {
			t.Fatalf("key %s is not a public signing key", k.KeyID)
		}
	}
	for _, leak := range []string{bindingID, externalID, "elitea.example.com\""} {
		if bytes.Contains(jwksBody, []byte(leak)) {
			t.Fatalf("the JWKS carries %q", leak)
		}
	}
}
