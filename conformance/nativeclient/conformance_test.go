//go:build conformance

package nativeclient_test

import (
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/EliteaAI/elitea-platform/conformance/nativeclient/client"
)

// The ADR-0025 verification row, in order. The floor check at the end fails
// the run unless every one of these ran to a pass, so a renamed or deleted
// scenario cannot silently drop out (memory absence-reads-as-correctness).
var scenarios = []string{
	"no_client_registered",
	"register_client",
	"discovery",
	"sign_in",
	"sign_in_saml",
	"refresh_rotation",
	"chat_idempotency",
	"stream_resume",
	"conversation_sync",
	"notification_sync",
	"chat_enrichment",
	"contract_1_3",
	"contract_1_4",
	"min_client_version",
	"device_revocation",
}

const adminClientsPath = "/api/v2/admin/native_clients/administration"
const policyPath = "/api/v2/admin/plugin_config_values/administration/native_client_policy"

func TestNativeClientConformance(t *testing.T) {
	cfg := loadConfig(t)
	t.Logf("origin %s, leg %s, client %s", cfg.Origin, cfg.Leg, cfg.ClientID)
	state := &suite{cfg: cfg}

	passed := map[string]bool{}
	steps := map[string]func(*testing.T){
		"no_client_registered": state.noClientRegistered,
		"register_client":      state.registerClient,
		"discovery":            state.discoveryDocument,
		"sign_in":              state.signInScenario,
		"sign_in_saml":         state.signInSAML,
		"refresh_rotation":     state.refreshRotation,
		"chat_idempotency":     state.chatIdempotency,
		"stream_resume":        state.streamResume,
		"conversation_sync":    state.conversationSync,
		"notification_sync":    state.notificationSync,
		"chat_enrichment":      state.chatEnrichment,
		"contract_1_3":         state.contract13,
		"contract_1_4":         state.contract14,
		"min_client_version":   state.minClientVersion,
		"device_revocation":    state.deviceRevocation,
	}
	for _, name := range scenarios {
		step, ok := steps[name]
		if !ok {
			t.Fatalf("scenario %q has no implementation", name)
		}
		passed[name] = t.Run(name, step)
		if !passed[name] && state.fatal {
			break
		}
	}
	// The floor: every planned scenario ran and passed. A skipped subtest
	// reports true from t.Run, so a scenario must also have recorded itself.
	for _, name := range scenarios {
		switch {
		case !passed[name]:
			t.Errorf("scenario %s did not pass", name)
		case !state.completed[name]:
			t.Errorf("scenario %s returned without reaching its last assertion", name)
		}
	}
	if len(steps) != len(scenarios) {
		t.Errorf("%d scenario implementations for %d planned scenarios", len(steps), len(scenarios))
	}
}

// suite carries what one scenario hands the next.
type suite struct {
	cfg       config
	admin     *client.Client
	discovery client.Discovery
	main      *session
	projectID int64
	// conversation is the chat scenarios' conversation (uuid and numeric id).
	conversationUUID string
	conversationID   string
	admission        client.ChatAdmission
	prompt           string
	// head is the stream's first connection, read while the turn was live.
	head      []client.Frame
	completed map[string]bool
	// fatal stops the run: a later scenario cannot stand on a failed setup.
	fatal bool
}

func (s *suite) done(t *testing.T) {
	if s.completed == nil {
		s.completed = map[string]bool{}
	}
	s.completed[t.Name()[strings.LastIndex(t.Name(), "/")+1:]] = true
}

func (s *suite) requireSetup(t *testing.T) {
	t.Helper()
	if s.admin == nil || s.discovery.NativeAuth == nil {
		s.fatal = true
		t.Fatal("a setup scenario failed; nothing after it can run")
	}
}

func (s *suite) requireSession(t *testing.T) {
	t.Helper()
	s.requireSetup(t)
	if s.main == nil {
		s.fatal = true
		t.Fatal("the sign-in scenario failed; no native session exists")
	}
}

// ── 10. Negative leg: no client, no endpoints ────────────────────────────────

func (s *suite) noClientRegistered(t *testing.T) {
	ctx := testContext(t, 3*time.Minute)
	s.fatal = true
	s.admin = s.cfg.consoleSession(t, s.cfg.Admin)
	// A previous run's DB-layer client is removed first, so this leg proves
	// the server's behaviour rather than the stack's history. 404 is fine.
	response := need(t)(s.admin.Do(ctx, http.MethodDelete, adminClientsPath+"/"+s.cfg.ClientID, nil, nil))
	if response.Status != http.StatusOK && response.Status != http.StatusNotFound {
		t.Fatalf("remove a previous run's client: %s", response)
	}
	listing := mustJSON(t, need(t)(s.admin.Get(ctx, adminClientsPath)), http.StatusOK)
	if rows, _ := listing["rows"].([]any); len(rows) != 0 {
		t.Fatalf("the negative leg needs a deployment with NO native client; registered: %v", rows)
	}
	public := client.New(s.cfg.Origin)
	result, err := public.FetchDiscovery(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if raw, present := result.Raw["native_auth"]; !present || raw != nil {
		t.Fatalf("discovery native_auth = %v (present %v), want an explicit null", raw, present)
	}
	for _, probe := range []struct{ method, path string }{
		{http.MethodGet, "/api/v2/auth/native/authorize?client_id=" + s.cfg.ClientID},
		{http.MethodGet, "/api/v2/auth/native/authorize/continue?request=x"},
		{http.MethodPost, "/api/v2/auth/native/authorize/decision"},
		{http.MethodPost, "/api/v2/auth/native/token"},
		{http.MethodPost, "/api/v2/auth/native/revoke"},
	} {
		var body any
		if probe.method == http.MethodPost {
			body = url.Values{"client_id": {s.cfg.ClientID}}
		}
		response := need(t)(public.Do(ctx, probe.method, probe.path, body, nil))
		if response.Status != http.StatusNotFound {
			t.Errorf("%s %s with no client registered: %s, want 404", probe.method, probe.path, response)
		}
	}
	// The device routes sit inside the authenticated /api/v2 group: an
	// authenticated caller gets the 404.
	response = need(t)(s.admin.Get(ctx, client.DevicesPath))
	if response.Status != http.StatusNotFound {
		t.Errorf("GET %s with no client registered: %s, want 404", client.DevicesPath, response)
	}
	s.fatal = false
	s.done(t)
}

// ── Setup: register the client through the admin registry ───────────────────

func (s *suite) registerClient(t *testing.T) {
	ctx := testContext(t, time.Minute)
	s.fatal = true
	if s.admin == nil {
		t.Fatal("no administrator session")
	}
	response := need(t)(s.admin.Do(ctx, http.MethodPut, adminClientsPath+"/"+s.cfg.ClientID, map[string]any{
		"display_name":  "Conformance",
		"redirect_uris": []string{s.cfg.RedirectURI, "http://127.0.0.1/callback"},
		"enabled":       true,
	}, nil))
	mustJSON(t, response, http.StatusOK)
	// Start from the default policy whatever an earlier run left behind.
	s.setMinimumVersion(t, "")
	s.fatal = false
	s.done(t)
}

func (s *suite) setMinimumVersion(t *testing.T, version string) {
	t.Helper()
	ctx := testContext(t, time.Minute)
	response := need(t)(s.admin.Do(ctx, http.MethodPut, policyPath, map[string]any{
		"values": map[string]any{"min_client_version": version},
	}, nil))
	if response.Status != http.StatusOK {
		t.Fatalf("set min_client_version=%q: %s", version, response)
	}
}

// ── 1. Discovery from the origin alone ───────────────────────────────────────

var majorMinor = regexp.MustCompile(`^(\d+\.\d+|dev)$`)

func (s *suite) discoveryDocument(t *testing.T) {
	ctx := testContext(t, time.Minute)
	s.fatal = true
	public := client.New(s.cfg.Origin)
	result, err := public.FetchDiscovery(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	doc := result.Document
	native := doc.NativeAuth
	if native == nil {
		t.Fatalf("native_auth is null after a client was registered: %s", result.Response)
	}
	if native.Issuer != s.cfg.Origin {
		t.Errorf("issuer %q, want the deployment origin %q", native.Issuer, s.cfg.Origin)
	}
	for name, endpoint := range map[string]string{
		"authorization_endpoint": native.AuthorizationEndpoint,
		"token_endpoint":         native.TokenEndpoint,
		"revocation_endpoint":    native.RevocationEndpoint,
		"brand_pack_url":         doc.BrandPackURL,
	} {
		if !strings.HasPrefix(endpoint, s.cfg.Origin+"/") {
			t.Errorf("%s %q is not an absolute URL on %s", name, endpoint, s.cfg.Origin)
		}
	}
	if len(native.CodeChallengeMethodsSupported) != 1 || native.CodeChallengeMethodsSupported[0] != "S256" {
		t.Errorf("code_challenge_methods_supported = %v, want [S256]", native.CodeChallengeMethodsSupported)
	}
	if !strings.HasPrefix(doc.ClientContract, "1.") {
		t.Errorf("client_contract %q, want major 1", doc.ClientContract)
	}
	if !majorMinor.MatchString(doc.ServerVersion) {
		t.Errorf("server_version %q is not major.minor (coordinator decision 3)", doc.ServerVersion)
	}
	if doc.DeploymentKind != "saas" && doc.DeploymentKind != "self_hosted" {
		t.Errorf("deployment_kind %q", doc.DeploymentKind)
	}
	if doc.DisplayName == "" {
		t.Error("display_name is empty")
	}
	if doc.MinClientVersion == nil {
		t.Errorf("discovery carries no min_client_version map: %s", result.Response)
	}
	if result.ETag == "" {
		t.Fatal("discovery carries no ETag")
	}
	again, err := public.FetchDiscovery(ctx, result.ETag)
	if err != nil {
		t.Fatal(err)
	}
	if !again.NotModified {
		t.Errorf("If-None-Match with the current ETag: %s, want 304", again.Response)
	}

	// The brand pack: JSON, its own entity tag, and absolute asset URLs a
	// client can fetch without knowing any path.
	pack := need(t)(public.Get(ctx, doc.BrandPackURL))
	body := mustJSON(t, pack, http.StatusOK)
	etag := pack.Header.Get("ETag")
	if etag == "" {
		t.Fatal("pack.json carries no ETag")
	}
	notModified := need(t)(public.Do(ctx, http.MethodGet, doc.BrandPackURL, nil, http.Header{"If-None-Match": {etag}}))
	if notModified.Status != http.StatusNotModified {
		t.Errorf("pack.json If-None-Match: %s, want 304", notModified)
	}
	assets := absoluteURLs(body, s.cfg.Origin)
	if len(assets) == 0 {
		t.Fatalf("pack.json names no absolute asset URL on %s: %s", s.cfg.Origin, pack)
	}
	asset := need(t)(public.Get(ctx, assets[0]))
	if asset.Status != http.StatusOK || len(asset.Body) == 0 {
		t.Errorf("fetch brand asset %s: %s", assets[0], asset)
	}
	s.discovery = doc
	s.fatal = false
	s.done(t)
}

// absoluteURLs collects every string in a JSON tree that is a URL on origin.
func absoluteURLs(node any, origin string) []string {
	var out []string
	switch typed := node.(type) {
	case map[string]any:
		for _, value := range typed {
			out = append(out, absoluteURLs(value, origin)...)
		}
	case []any:
		for _, value := range typed {
			out = append(out, absoluteURLs(value, origin)...)
		}
	case string:
		if strings.HasPrefix(typed, origin+"/") {
			out = append(out, typed)
		}
	}
	return out
}

// ── 2. Native sign-in ────────────────────────────────────────────────────────

func (s *suite) signInScenario(t *testing.T) {
	ctx := testContext(t, 3*time.Minute)
	s.fatal = true
	s.requireSetup(t)
	signed := s.cfg.signIn(t, s.discovery, s.cfg.User, "oidc", "conformance main")
	tokens := signed.tokens
	if tokens.ExpiresIn <= 0 || tokens.RefreshTokenExpiresIn <= 0 || tokens.DeviceID == "" {
		t.Fatalf("token response is missing lifetimes or the device id: %+v", tokens)
	}
	if tokens.ClientPolicy == nil {
		t.Error("token response carries no client_policy (WP4)")
	}
	devices := mustJSON(t, need(t)(signed.api.Get(ctx, client.DevicesPath)), http.StatusOK)
	if !hasCurrentDevice(devices, tokens.DeviceID, "conformance main") {
		t.Fatalf("the device list does not mark this device current: %v", devices)
	}
	s.main = &signed
	s.fatal = false

	// A code is single use, and a replayed one revokes what it already
	// produced (RFC 6749 §4.1.2): an intercepted code is worth nothing once
	// the real app has redeemed it.
	granted := s.cfg.authorize(t, s.discovery, s.cfg.User, "oidc", "conformance replay")
	public := client.New(s.cfg.Origin).WithVersion("1.0.0")
	first, err := public.ExchangeCode(ctx, s.discovery.NativeAuth.TokenEndpoint, s.cfg.ClientID, s.cfg.RedirectURI,
		granted.code, granted.verifier)
	if err != nil {
		t.Fatalf("code exchange: %v", err)
	}
	_, err = public.ExchangeCode(ctx, s.discovery.NativeAuth.TokenEndpoint, s.cfg.ClientID, s.cfg.RedirectURI,
		granted.code, granted.verifier)
	var refused *client.TokenError
	if !errors.As(err, &refused) || refused.Response.Status != http.StatusBadRequest || refused.Code != "invalid_grant" {
		t.Fatalf("a replayed code: %v, want 400 invalid_grant", err)
	}
	if response := need(t)(public.WithToken(first.AccessToken).Get(ctx, client.DevicesPath)); !client.IsDeviceRevoked(response) {
		t.Errorf("the tokens a replayed code produced still work: %s", response)
	}
	// A verifier that does not match the challenge is refused.
	other := s.cfg.authorize(t, s.discovery, s.cfg.User, "oidc", "conformance pkce")
	if _, err := public.ExchangeCode(ctx, s.discovery.NativeAuth.TokenEndpoint, s.cfg.ClientID, s.cfg.RedirectURI,
		other.code, client.NewVerifier()); !errors.As(err, &refused) || refused.Code != "invalid_grant" {
		t.Errorf("a wrong PKCE verifier: %v, want invalid_grant", err)
	}
	s.done(t)
}

func hasCurrentDevice(listing map[string]any, deviceID, name string) bool {
	rows, _ := listing["devices"].([]any)
	for _, raw := range rows {
		row, _ := raw.(map[string]any)
		if row["id"] == deviceID && row["device_name"] == name && row["current"] == true {
			return true
		}
	}
	return false
}
