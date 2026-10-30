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
	"sync"
	"testing"
	"time"

	coreoidc "github.com/coreos/go-oidc/v3/oidc"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/agentidentity/eliteanative"
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
	signer *eliteanative.DelegationSigner
	public crypto.PublicKey
}

func newRing(t *testing.T, origin string, now *time.Time) ring {
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
		Keys: []eliteanative.KeySpec{{KeyID: "es-1", Algorithm: eliteanative.ES256, File: "/keys/es-1.pem"}}}
	signer, err := eliteanative.NewDelegationSigner(cfg, keyRing, eliteanative.WithClock(func() time.Time { return *now }))
	if err != nil {
		t.Fatal(err)
	}
	return ring{signer: signer, public: key.Public()}
}

type runsStub struct{ run remoteagent.Run }

func (r runsStub) ReadRun(context.Context, string, uint64) (remoteagent.Run, error) {
	return r.run, nil
}

type nestedStub map[[3]int64]json.RawMessage

func (n nestedStub) NestedApplicationTools(_ context.Context, project, app, version int64) (json.RawMessage, error) {
	return n[[3]int64{project, app, version}], nil
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
	nested := nestedStub{{7, 11, 12}: json.RawMessage(`[{"type":"mcp_github","settings":{"url":"https://nested.example/mcp"}}]`)}
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
		"https://evil.example/mcp":          "audience_not_admitted",
		"https://partner.example/other":     "audience_not_admitted",
		"https://PARTNER.example/app/9/mcp": "audience_malformed",
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
		len(got.Scope.Toolkits) != 2 || remoteagent.ScopeAllows(got.Scope, []string{"github"}) != "" ||
		remoteagent.ScopeAllows(got.Scope, []string{"github", "jira"}) != "jira" {
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
