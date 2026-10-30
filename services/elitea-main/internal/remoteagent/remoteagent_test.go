package remoteagent_test

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	coreoidc "github.com/coreos/go-oidc/v3/oidc"
	jose "github.com/go-jose/go-jose/v4"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/agentidentity/eliteanative"
	identityoidc "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/identity/oidc"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/platformconfig"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/remoteagent"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/secretstore"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/securityaudit"
)

const (
	issuerOrigin = "https://elitea.example.com"
	endpoint     = "https://partner.example/app/9/mcp"
)

var fixedNow = time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)

type ring struct {
	signer  *eliteanative.DelegationSigner
	public  crypto.PublicKey
	private *ecdsa.PrivateKey
}

func newRing(t *testing.T, origin string, now *time.Time) ring {
	t.Helper()
	return newRingWithParties(t, origin, now)
}

func newRingWithParties(t *testing.T, origin string, now *time.Time, parties ...eliteanative.RelyingParty) ring {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	local, err := eliteanative.NewLocalSigner("es-1", eliteanative.ES256,
		secretstore.NewRuntimeSecret(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})))
	if err != nil {
		t.Fatal(err)
	}
	keyRing, err := eliteanative.NewKeyRing([]eliteanative.RingKey{{Signer: local}}, eliteanative.DefaultRotationOverlap, 16*time.Minute, nil)
	if err != nil {
		t.Fatal(err)
	}
	cfg := eliteanative.Config{Origin: origin, RotationOverlap: eliteanative.DefaultRotationOverlap,
		Keys:           []eliteanative.KeySpec{{KeyID: "es-1", Algorithm: eliteanative.ES256, File: "/keys/es-1.pem"}},
		RelyingParties: parties}
	signer, err := eliteanative.NewDelegationSigner(cfg, keyRing, eliteanative.WithClock(func() time.Time { return *now }))
	if err != nil {
		t.Fatal(err)
	}
	return ring{signer: signer, public: key.Public(), private: key}
}

type runsStub struct{ run remoteagent.Run }

func (r runsStub) ReadRun(context.Context, string, uint64) (remoteagent.Run, error) {
	return r.run, nil
}

type nestedStub struct {
	tools map[[3]int64]json.RawMessage
	calls *int
}

func (n nestedStub) NestedApplicationTools(_ context.Context, project, app, version int64) (json.RawMessage, error) {
	if n.calls != nil {
		*n.calls++
	}
	return n.tools[[3]int64{project, app, version}], nil
}

type auditStub struct {
	mu     sync.Mutex
	events []securityaudit.SecurityEvent
	err    error
}

func (a *auditStub) RecordRemoteAgent(_ context.Context, e securityaudit.SecurityEvent) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.err != nil {
		return a.err
	}
	a.events = append(a.events, e)
	return nil
}

func (a *auditStub) actions() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]string, 0, len(a.events))
	for _, e := range a.events {
		out = append(out, e.Action+"/"+e.Reasons[0])
	}
	return out
}

func settingsOf(mode platformconfig.OutboundAssertionsMode, issuers ...string) remoteagent.Settings {
	return func(context.Context) (platformconfig.RemoteAgentSettings, error) {
		return platformconfig.RemoteAgentSettings{OutboundAssertions: mode, TrustedIssuers: issuers}, nil
	}
}

func agentRun(governed bool) remoteagent.Run {
	run := remoteagent.Run{
		ExecutionID: "exec-1", Generation: 1, TenantProjectID: 7, ActorUserID: "42", ApplicationID: 3,
		Tools: json.RawMessage(`[{"type":"mcp","name":"partner","settings":{"url":"https://Partner.example/app/9/mcp/"}},
			{"type":"application","settings":{"application_id":11,"application_version_id":12}}]`),
		ToolkitTypes: []string{"mcp", "github"},
	}
	if governed {
		run.Governed, run.AgentBindingID, run.MandateID, run.SubjectPrincipalRef = true, "binding-1", "mandate-1", "user:42"
	}
	return run
}

func mintService(t *testing.T, run remoteagent.Run, signer remoteagent.Signer, settings remoteagent.Settings, audit *auditStub) *remoteagent.MintService {
	t.Helper()
	return mintServiceCounting(t, run, signer, settings, audit, nil)
}

func mintServiceCounting(t *testing.T, run remoteagent.Run, signer remoteagent.Signer, settings remoteagent.Settings, audit *auditStub, nestedCalls *int) *remoteagent.MintService {
	t.Helper()
	nested := nestedStub{tools: map[[3]int64]json.RawMessage{
		{7, 11, 12}: json.RawMessage(`[{"type":"mcp_github","settings":{"url":"https://nested.example/mcp"}}]`)}, calls: nestedCalls}
	service, err := remoteagent.NewMintService(runsStub{run: run}, nested, signer, settings, audit)
	if err != nil {
		t.Fatal(err)
	}
	return service
}

func TestMintIssuesForAnAdmittedEndpoint(t *testing.T) {
	now := fixedNow
	r := newRing(t, issuerOrigin, &now)
	audit := &auditStub{}
	service := mintService(t, agentRun(true), r.signer, settingsOf(platformconfig.OutboundAssertionsOn), audit)
	for _, audience := range []string{endpoint, "https://nested.example/mcp"} {
		result, err := service.Mint(context.Background(), "exec-1", 1, audience)
		if err != nil || result.Disposition != remoteagent.Issued || result.Token == "" || !result.Required {
			t.Fatalf("%s: %+v, %v", audience, result, err)
		}
		if got := result.ExpiresAt.Sub(now); got != eliteanative.DelegationMaxTTL {
			t.Fatalf("expires in %v", got)
		}
	}
	if got := audit.actions(); len(got) != 2 || got[0] != remoteagent.ActionMinted+"/minted" {
		t.Fatalf("audit = %v", got)
	}
	if e := audit.events[0]; e.Type != securityaudit.TypeRemoteAgent || e.PrincipalRef != "agent:binding:binding-1" ||
		e.ResourceID != "partner.example" || e.DecisionID == "" || e.ExecutionID != "exec-1" {
		t.Fatalf("event = %+v", e)
	}
}

// A wrong audience is refused and audited; nothing is signed.
func TestMintRefusesAnAudienceNotAdmitted(t *testing.T) {
	now := fixedNow
	r := newRing(t, issuerOrigin, &now)
	audit := &auditStub{}
	service := mintService(t, agentRun(false), r.signer, settingsOf(platformconfig.OutboundAssertionsOn), audit)
	for audience, reason := range map[string]string{
		"https://evil.example/mcp":         "audience_not_admitted",
		"https://partner.example/other":    "audience_not_admitted",
		"http://partner.example/app/9/mcp": "audience_malformed",
	} {
		if _, err := service.Mint(context.Background(), "exec-1", 1, audience); !errors.Is(err, remoteagent.ErrAudienceNotAdmitted) {
			t.Fatalf("%s: err = %v", audience, err)
		}
		if got := audit.actions(); got[len(got)-1] != remoteagent.ActionMintRefused+"/"+reason {
			t.Fatalf("%s: audit = %v", audience, got)
		}
	}
}

// Off disables minting for an ungoverned run; a governed run still mints,
// and without a key ring it is refused rather than left unsigned.
func TestMintSettingAndGovernedProfile(t *testing.T) {
	now := fixedNow
	r := newRing(t, issuerOrigin, &now)
	off := settingsOf(platformconfig.OutboundAssertionsOff)
	result, err := mintService(t, agentRun(false), r.signer, off, &auditStub{}).Mint(context.Background(), "exec-1", 1, endpoint)
	if err != nil || result.Disposition != remoteagent.Disabled || result.Token != "" {
		t.Fatalf("off: %+v, %v", result, err)
	}
	result, err = mintService(t, agentRun(true), r.signer, off, &auditStub{}).Mint(context.Background(), "exec-1", 1, endpoint)
	if err != nil || result.Disposition != remoteagent.Issued {
		t.Fatalf("governed with off: %+v, %v", result, err)
	}
	audit := &auditStub{}
	result, err = mintService(t, agentRun(true), nil, off, audit).Mint(context.Background(), "exec-1", 1, endpoint)
	if !errors.Is(err, remoteagent.ErrSignerUnavailable) || !result.Required {
		t.Fatalf("no ring: %+v, %v", result, err)
	}
	if got := audit.actions(); len(got) != 1 || got[0] != remoteagent.ActionMintRefused+"/signer_unavailable" {
		t.Fatalf("audit = %v", got)
	}
}

// The worker sends the endpoint as configured; Main normalizes it with the
// same function as the admitted URLs and signs the normalized form.
func TestMintNormalizesTheRawEndpoint(t *testing.T) {
	now := fixedNow
	r := newRing(t, issuerOrigin, &now)
	service := mintService(t, agentRun(false), r.signer, settingsOf(platformconfig.OutboundAssertionsOn), &auditStub{})
	for _, raw := range []string{
		"https://Partner.example/app/9/mcp/", "https://partner.example:443/app/9/mcp",
		"https://partner.example/app/x/../9/./mcp", "https://PARTNER.EXAMPLE./app/9/mcp",
	} {
		result, err := service.Mint(context.Background(), "exec-1", 1, raw)
		if err != nil || result.Disposition != remoteagent.Issued {
			t.Fatalf("%s: %+v, %v", raw, result, err)
		}
		var claims struct {
			Audience string `json:"aud"`
		}
		payload, err := (&coreoidc.StaticKeySet{PublicKeys: []crypto.PublicKey{r.public}}).VerifySignature(context.Background(), result.Token)
		if err != nil || json.Unmarshal(payload, &claims) != nil || claims.Audience != endpoint {
			t.Fatalf("%s: aud %q, %v", raw, claims.Audience, err)
		}
	}
}

// No key and no governance: Disabled for the price of one run read. No
// settings read, no toolkit walk, no audit row.
func TestMintWithoutAKeyCostsNothing(t *testing.T) {
	audit := &auditStub{}
	settingsRead := 0
	settings := func(context.Context) (platformconfig.RemoteAgentSettings, error) {
		settingsRead++
		return platformconfig.RemoteAgentSettings{OutboundAssertions: platformconfig.OutboundAssertionsOn}, nil
	}
	walked := 0
	result, err := mintServiceCounting(t, agentRun(false), nil, settings, audit, &walked).Mint(context.Background(), "exec-1", 1, endpoint)
	if err != nil || result.Disposition != remoteagent.Disabled || result.Required {
		t.Fatalf("%+v, %v", result, err)
	}
	if settingsRead != 0 || walked != 0 || len(audit.events) != 0 {
		t.Fatalf("settings %d, walk %d, audit %v", settingsRead, walked, audit.events)
	}
}

// off (the default) sends nothing; allowlist sends only to listed origins;
// on sends to every admitted endpoint. A governed run always mints.
func TestMintOutboundModes(t *testing.T) {
	now := fixedNow
	r := newRing(t, issuerOrigin, &now)
	if platformconfig.DefaultRemoteAgentSettings().OutboundAssertions != platformconfig.OutboundAssertionsOff {
		t.Fatal("outbound assertions are not off by default")
	}
	allowlist := func(context.Context) (platformconfig.RemoteAgentSettings, error) {
		return platformconfig.RemoteAgentSettings{OutboundAssertions: platformconfig.OutboundAssertionsAllowlist,
			AssertionAudiences: []string{"https://PARTNER.example:443", "not an origin"}}, nil
	}
	for _, test := range []struct {
		name     string
		settings remoteagent.Settings
		governed bool
		audience string
		want     remoteagent.Disposition
	}{
		{"off", settingsOf(platformconfig.OutboundAssertionsOff), false, endpoint, remoteagent.Disabled},
		{"allowlist listed", allowlist, false, endpoint, remoteagent.Issued},
		{"allowlist not listed", allowlist, false, "https://nested.example/mcp", remoteagent.Disabled},
		{"on", settingsOf(platformconfig.OutboundAssertionsOn), false, "https://nested.example/mcp", remoteagent.Issued},
		{"governed forces on", settingsOf(platformconfig.OutboundAssertionsOff), true, endpoint, remoteagent.Issued},
	} {
		result, err := mintService(t, agentRun(test.governed), r.signer, test.settings, &auditStub{}).Mint(context.Background(), "exec-1", 1, test.audience)
		if err != nil || result.Disposition != test.want {
			t.Fatalf("%s: %+v, %v", test.name, result, err)
		}
	}
	if origin, ok := remoteagent.AudienceOrigin("https://partner.example/app"); ok {
		t.Fatalf("an origin with a path was accepted: %q", origin)
	}
}

// The audience of a federation relying party is never a delegation
// audience, even when a run admits it as an MCP endpoint.
func TestMintRefusesARelyingPartyAudience(t *testing.T) {
	now := fixedNow
	r := newRingWithParties(t, issuerOrigin, &now, eliteanative.RelyingParty{
		ID: "partner", Audience: endpoint, Algorithm: eliteanative.ES256, MaxTTL: 5 * time.Minute, ClaimsProfile: eliteanative.ProfileMCP})
	audit := &auditStub{}
	_, err := mintService(t, agentRun(true), r.signer, settingsOf(platformconfig.OutboundAssertionsOn), audit).Mint(context.Background(), "exec-1", 1, endpoint)
	if !errors.Is(err, remoteagent.ErrAudienceNotAdmitted) {
		t.Fatalf("err = %v", err)
	}
	if got := audit.actions(); len(got) != 1 || got[0] != remoteagent.ActionMintRefused+"/audience_reserved" {
		t.Fatalf("audit = %v", got)
	}
}

// An assertion whose minting is not audited is not handed out.
func TestMintFailsWhenTheAuditFails(t *testing.T) {
	now := fixedNow
	r := newRing(t, issuerOrigin, &now)
	audit := &auditStub{err: errors.New("database down")}
	result, err := mintService(t, agentRun(false), r.signer, settingsOf(platformconfig.OutboundAssertionsOn), audit).
		Mint(context.Background(), "exec-1", 1, endpoint)
	if err == nil || result.Token != "" {
		t.Fatalf("%+v, %v", result, err)
	}
}

func TestRunScopeIsStableAndBounded(t *testing.T) {
	a, err := remoteagent.RunScope(json.RawMessage(`[{"type":"github","name":"a","settings":{"selected_tools":["b","a"]}}]`), []string{"mcp", "github", "github"})
	if err != nil {
		t.Fatal(err)
	}
	b, err := remoteagent.RunScope(json.RawMessage(`[{"type":"github","name":"a","settings":{"selected_tools":["a","b"]}}]`), []string{"github", "mcp"})
	if err != nil || a.Digest != b.Digest || len(a.Toolkits) != 2 || a.Toolkits[0] != "github" {
		t.Fatalf("a=%+v b=%+v err=%v", a, b, err)
	}
	c, _ := remoteagent.RunScope(json.RawMessage(`[{"type":"github","name":"a","settings":{"selected_tools":["a"]}}]`), []string{"github", "mcp"})
	if c.Digest == a.Digest {
		t.Fatal("a narrower tool selection has the same digest")
	}
	many := make([]string, eliteanative.MaxScopeToolkits+1)
	for i := range many {
		many[i] = "type_" + string(rune('a'+i%26)) + string(rune('a'+i/26))
	}
	if _, err := remoteagent.RunScope(nil, many); !errors.Is(err, remoteagent.ErrScopeTooLarge) {
		t.Fatalf("err = %v", err)
	}
}

// --- inbound verification

type keysStub map[string]coreoidc.KeySet

func (k keysStub) KeySet(_ context.Context, issuer string) (coreoidc.KeySet, bool, error) {
	keys, ok := k[issuer]
	return keys, ok, nil
}

type memReplays struct {
	mu   sync.Mutex
	seen map[[2]string]string
}

func (m *memReplays) Claim(_ context.Context, issuer, jti, presenter string, _ time.Time) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.seen == nil {
		m.seen = map[[2]string]string{}
	}
	held, ok := m.seen[[2]string{issuer, jti}]
	if !ok {
		m.seen[[2]string{issuer, jti}] = presenter
		return true, nil
	}
	return held == presenter, nil
}

func signed(t *testing.T, r ring, audience string) string {
	t.Helper()
	scope, err := remoteagent.RunScope(nil, []string{"github", "mcp"})
	if err != nil {
		t.Fatal(err)
	}
	token, _, err := r.signer.Sign(context.Background(), eliteanative.DelegationRequest{
		Audience: audience, Agent: "binding:binding-1", TenantProjectID: 7, ExecutionID: "exec-1", Generation: 1,
		UserID: "42", Scope: scope,
	})
	if err != nil {
		t.Fatal(err)
	}
	return token
}

func TestVerifierAcceptsAnAllowlistedAssertion(t *testing.T) {
	now := fixedNow
	r := newRing(t, issuerOrigin, &now)
	issuer := issuerOrigin + eliteanative.IssuerPath
	verifier, err := remoteagent.NewVerifier(keysStub{issuer: &coreoidc.StaticKeySet{PublicKeys: []crypto.PublicKey{r.public}}},
		&memReplays{}, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	token := signed(t, r, endpoint)
	got, err := verifier.Verify(context.Background(), token, endpoint, "token:1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Issuer != issuer || got.Subject != "agent:binding:binding-1" || got.Run != "exec-1" || got.User != "user:42" ||
		len(got.Scope.Toolkits) != 2 {
		t.Fatalf("verified = %+v", got)
	}
	// The same presenter may present it again (one MCP session); another
	// presenter is a replay.
	if _, err := verifier.Verify(context.Background(), token, endpoint, "token:1"); err != nil {
		t.Fatalf("same presenter: %v", err)
	}
	if _, err := verifier.Verify(context.Background(), token, endpoint, "token:2"); remoteagent.RefusalCode(err) != remoteagent.CodeReplayed {
		t.Fatalf("other presenter: %v", err)
	}
}

func TestVerifierRefusals(t *testing.T) {
	now := fixedNow
	trusted := newRing(t, issuerOrigin, &now)
	untrusted := newRing(t, "https://other.example.com", &now)
	impostor := newRing(t, issuerOrigin, &now) // the trusted issuer's name, another key
	issuer := issuerOrigin + eliteanative.IssuerPath
	clock := now
	verifier, err := remoteagent.NewVerifier(keysStub{issuer: &coreoidc.StaticKeySet{PublicKeys: []crypto.PublicKey{trusted.public}}},
		&memReplays{}, func() time.Time { return clock })
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name     string
		token    string
		audience string
		at       time.Time
		code     string
	}{
		{"garbage", "not-a-jwt", endpoint, now, remoteagent.CodeMalformed},
		{"issuer not allowlisted", signed(t, untrusted, endpoint), endpoint, now, remoteagent.CodeIssuerNotTrusted},
		{"signature", signed(t, impostor, endpoint), endpoint, now, remoteagent.CodeSignatureInvalid},
		{"audience", signed(t, trusted, "https://elsewhere.example/mcp"), endpoint, now, remoteagent.CodeAudienceMismatch},
		{"expired", signed(t, trusted, endpoint), endpoint, now.Add(6 * time.Minute), remoteagent.CodeExpired},
		{"not yet valid", signed(t, trusted, endpoint), endpoint, now.Add(-2 * time.Minute), remoteagent.CodeExpired},
	} {
		clock = test.at
		_, err := verifier.Verify(context.Background(), test.token, test.audience, "token:1")
		if got := remoteagent.RefusalCode(err); got != test.code {
			t.Fatalf("%s: code = %q (%v), want %q", test.name, got, err, test.code)
		}
	}
	// Replay after expiry: refused as expired even by its first presenter.
	clock = now
	token := signed(t, trusted, endpoint)
	if _, err := verifier.Verify(context.Background(), token, endpoint, "token:9"); err != nil {
		t.Fatal(err)
	}
	clock = now.Add(eliteanative.DelegationMaxTTL + time.Minute)
	if _, err := verifier.Verify(context.Background(), token, endpoint, "token:9"); remoteagent.RefusalCode(err) != remoteagent.CodeExpired {
		t.Fatalf("replay after expiry: %v", err)
	}
}

// A federation assertion (typ elitea-agent+jwt) signed by a trusted key is
// not a delegation assertion.
func TestVerifierRefusesAFederationTokenType(t *testing.T) {
	now := fixedNow
	r := newRing(t, issuerOrigin, &now)
	issuer := issuerOrigin + eliteanative.IssuerPath
	verifier, err := remoteagent.NewVerifier(keysStub{issuer: &coreoidc.StaticKeySet{PublicKeys: []crypto.PublicKey{r.public}}},
		&memReplays{}, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	// Re-sign the claims of a real delegation assertion with the federation typ.
	payload, err := (&coreoidc.StaticKeySet{PublicKeys: []crypto.PublicKey{r.public}}).VerifySignature(context.Background(), signed(t, r, endpoint))
	if err != nil {
		t.Fatal(err)
	}
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.ES256, Key: r.private},
		(&jose.SignerOptions{}).WithType(eliteanative.TokenType).WithHeader("kid", "es-1"))
	if err != nil {
		t.Fatal(err)
	}
	object, err := signer.Sign(payload)
	if err != nil {
		t.Fatal(err)
	}
	federation, err := object.CompactSerialize()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := verifier.Verify(context.Background(), federation, endpoint, "token:1"); remoteagent.RefusalCode(err) != remoteagent.CodeMalformed {
		t.Fatalf("federation typ: %v", err)
	}
}

// A JWKS that cannot be fetched is "unavailable" (503), not a bad
// signature; a JWKS that does not hold the signing key is a bad signature.
func TestVerifierTellsAnUnreachableJWKSFromABadSignature(t *testing.T) {
	now := fixedNow
	r := newRing(t, issuerOrigin, &now)
	other := newRing(t, issuerOrigin, &now)
	issuer := issuerOrigin + eliteanative.IssuerPath
	status := http.StatusServiceUnavailable
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if status != http.StatusOK {
			w.WriteHeader(status)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{
			Key: other.public, KeyID: "es-1", Algorithm: string(jose.ES256), Use: "sig"}}})
	}))
	defer server.Close()
	keys, err := identityoidc.NewRemoteKeySet(identityoidc.RemoteKeySetConfig{
		JWKSURL: server.URL + "/jwks.json", SupportedSigningAlgorithms: []string{"ES256"}, RequestTimeout: 2 * time.Second,
	}, server.Client().Transport)
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := remoteagent.NewVerifier(keysStub{issuer: keys}, &memReplays{}, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	_, err = verifier.Verify(context.Background(), signed(t, r, endpoint), endpoint, "token:1")
	if !errors.Is(err, remoteagent.ErrKeysUnavailable) || remoteagent.RefusalCode(err) != remoteagent.CodeUnavailable {
		t.Fatalf("5xx: %v", err)
	}
	status = http.StatusOK
	_, err = verifier.Verify(context.Background(), signed(t, r, endpoint), endpoint, "token:1")
	if remoteagent.RefusalCode(err) != remoteagent.CodeSignatureInvalid {
		t.Fatalf("other key: %v", err)
	}
}

// The allowlist joins the admin section and the bootstrap; an issuer on
// neither is not trusted, and a key set is built once per (issuer, JWKS).
func TestIssuerAllowlist(t *testing.T) {
	if admin, err := platformconfig.ParseTrustedIssuer("https://a.example/agent-issuer"); err != nil ||
		admin.JWKSURL != "https://a.example/.well-known/elitea-agent-jwks.json" {
		t.Fatalf("admin = %+v, %v", admin, err)
	}
	bootstrap, err := remoteagent.ParseEnvTrustedIssuers("https://b.example/agent-issuer https://keys.b.example/jwks.json")
	if err != nil {
		t.Fatal(err)
	}
	var built []string
	allowlist := remoteagent.NewIssuerAllowlist(settingsOf(platformconfig.OutboundAssertionsOn, "https://a.example/agent-issuer"), bootstrap,
		func(jwks string) (coreoidc.KeySet, error) {
			built = append(built, jwks)
			return &coreoidc.StaticKeySet{}, nil
		})
	for issuer, want := range map[string]bool{
		"https://a.example/agent-issuer": true, "https://b.example/agent-issuer": true, "https://c.example/agent-issuer": false,
	} {
		_, ok, err := allowlist.KeySet(context.Background(), issuer)
		if err != nil || ok != want {
			t.Fatalf("%s: ok=%v err=%v", issuer, ok, err)
		}
	}
	if _, _, err := allowlist.KeySet(context.Background(), "https://a.example/agent-issuer"); err != nil {
		t.Fatal(err)
	}
	if len(built) != 2 || built[0] != "https://a.example/.well-known/elitea-agent-jwks.json" && built[1] != "https://a.example/.well-known/elitea-agent-jwks.json" {
		t.Fatalf("built = %v", built)
	}
	if _, err := remoteagent.ParseEnvTrustedIssuers("http://insecure.example/agent-issuer"); err == nil {
		t.Fatal("an http issuer was accepted")
	}
}
