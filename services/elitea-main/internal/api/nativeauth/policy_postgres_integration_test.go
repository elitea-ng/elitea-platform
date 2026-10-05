package nativeauth_test

// ADR-0025 WP4 against a real database: the client policy in every token
// response, the per-client minimum on the registry, and the 426 gate on both
// the token endpoint and an API route.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"testing"

	apimw "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/middleware"
	nativeapi "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/nativeauth"
	domain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/nativeauth"
)

func (s *stack) setPolicy(values map[string]string) {
	s.t.Helper()
	for key, raw := range values {
		if _, err := s.pool.Exec(context.Background(), `
			INSERT INTO centry.platform_config (section, key, value) VALUES ('native_client_policy', $1, $2::jsonb)
			ON CONFLICT (section, key) DO UPDATE SET value = EXCLUDED.value`, key, raw); err != nil {
			s.t.Fatalf("set policy %s: %v", key, err)
		}
	}
	s.policy.Invalidate()
}

// setClientMinimum overrides the file-layer test client with a DB row that
// carries its own minimum (the admin PUT's path).
func (s *stack) setClientMinimum(minimum string) {
	s.t.Helper()
	client := fileClient()
	client.MinClientVersion = minimum
	if _, err := s.store.UpsertClient(context.Background(), client, 0); err != nil {
		s.t.Fatalf("upsert client: %v", err)
	}
	s.registry.Invalidate()
}

func TestNativeTokenResponseCarriesTheClientPolicy(t *testing.T) {
	s := newStack(t)
	s.setPolicy(map[string]string{
		"require_device_lock": "true", "idle_lock_seconds": "120", "offline_retention_days": "7",
		"min_client_version": `"1.0.0"`,
	})
	s.setClientMinimum("1.2.0")
	userID := s.seedUser("policy@example.test")
	recorder, _ := s.token(url.Values{
		"grant_type": {"authorization_code"}, "code": {s.signIn(userID, "policy@example.test")},
		"redirect_uri": {testRedirect}, "client_id": {testClientID}, "code_verifier": {testVerifier},
	})
	if recorder.Code != http.StatusOK {
		t.Fatalf("exchange = %d %s", recorder.Code, recorder.Body.String())
	}
	var body struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ClientPolicy *struct {
			RequireDeviceLock    bool   `json:"require_device_lock"`
			IdleLockSeconds      int64  `json:"idle_lock_seconds"`
			AllowScreenshots     bool   `json:"allow_screenshots"`
			OfflineRetentionDays int64  `json:"offline_retention_days"`
			OfflineMaxMB         int64  `json:"offline_max_mb"`
			OfflineAttachments   bool   `json:"offline_attachments"`
			MinClientVersion     string `json:"min_client_version"`
		} `json:"client_policy"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	p := body.ClientPolicy
	if p == nil {
		t.Fatalf("no client_policy in %s", recorder.Body.String())
	}
	if !p.RequireDeviceLock || p.IdleLockSeconds != 120 || !p.AllowScreenshots || p.OfflineRetentionDays != 7 ||
		p.OfflineMaxMB != 512 || !p.OfflineAttachments || p.MinClientVersion != "1.2.0" {
		t.Fatalf("client_policy = %+v (min must be the client's 1.2.0, the higher one)", *p)
	}

	// A policy change reaches the next refresh.
	s.setPolicy(map[string]string{"require_device_lock": "false"})
	refreshed, _ := s.refresh(body.RefreshToken)
	if refreshed.Code != http.StatusOK {
		t.Fatalf("refresh = %d %s", refreshed.Code, refreshed.Body.String())
	}
	var again struct {
		ClientPolicy map[string]any `json:"client_policy"`
	}
	_ = json.Unmarshal(refreshed.Body.Bytes(), &again)
	if again.ClientPolicy["require_device_lock"] != false {
		t.Fatalf("refresh client_policy = %v, want the changed policy", again.ClientPolicy)
	}
}

func TestNativeTokenEndpointAnswers426BeforeConsumingTheRefreshToken(t *testing.T) {
	s := newStack(t, withRedelivery(0))
	userID := s.seedUser("outdated@example.test")
	pair := s.exchange(s.signIn(userID, "outdated@example.test"))
	s.setPolicy(map[string]string{"min_client_version": `"2.0.0"`})

	form := url.Values{"grant_type": {"refresh_token"}, "refresh_token": {pair.RefreshToken}, "client_id": {testClientID}}
	old := s.do(http.MethodPost, nativeapi.TokenPath, form, withHeader(apimw.ClientVersionHeader, "1.9.0"))
	if old.Code != http.StatusUpgradeRequired {
		t.Fatalf("outdated refresh = %d %s, want 426", old.Code, old.Body.String())
	}
	if got := old.Header().Get(apimw.MinClientVersionHeader); got != "2.0.0" {
		t.Fatalf("X-Min-Client-Version = %q", got)
	}
	var refusal map[string]string
	_ = json.Unmarshal(old.Body.Bytes(), &refusal)
	if refusal["error"] != "client_upgrade_required" || refusal["min_client_version"] != "2.0.0" {
		t.Fatalf("426 body = %v", refusal)
	}
	// The form field speaks for a client that sends no header.
	viaForm := url.Values{"grant_type": {"refresh_token"}, "refresh_token": {pair.RefreshToken},
		"client_id": {testClientID}, "client_version": {"1.0.0"}}
	if got := s.do(http.MethodPost, nativeapi.TokenPath, viaForm); got.Code != http.StatusUpgradeRequired {
		t.Fatalf("outdated refresh via client_version = %d %s, want 426", got.Code, got.Body.String())
	}
	malformed := s.do(http.MethodPost, nativeapi.TokenPath, form, withHeader(apimw.ClientVersionHeader, "nine"))
	if malformed.Code != http.StatusBadRequest {
		t.Fatalf("malformed header = %d %s, want 400", malformed.Code, malformed.Body.String())
	}
	// Strict mode (window 0): had the 426 consumed the token, this would be
	// reuse and revoke the device. It must be the first use.
	updated := s.do(http.MethodPost, nativeapi.TokenPath, form, withHeader(apimw.ClientVersionHeader, "2.0.0"))
	if updated.Code != http.StatusOK {
		t.Fatalf("updated refresh = %d %s, want 200 with the same refresh token", updated.Code, updated.Body.String())
	}
	if state := s.family(pair.DeviceID); state.revokedAt != nil {
		t.Fatalf("family revoked by a refused refresh: %+v", state)
	}
}

func TestNativeAPIGateRefusesOnlyNativeCallersBelowTheMinimum(t *testing.T) {
	s := newStack(t)
	userID := s.seedUser("gate@example.test")
	pair := s.exchange(s.signIn(userID, "gate@example.test"))
	s.setPolicy(map[string]string{"min_client_version": `"1.0.0"`})

	bearer := withHeader("Authorization", "Bearer "+pair.AccessToken)
	if got := s.do(http.MethodGet, "/api/v2/gated", nil, bearer, withHeader(apimw.ClientVersionHeader, "0.0.1")); got.Code != http.StatusUpgradeRequired {
		t.Fatalf("native 0.0.1 = %d %s, want 426", got.Code, got.Body.String())
	}
	ok := s.do(http.MethodGet, "/api/v2/gated", nil, bearer, withHeader(apimw.ClientVersionHeader, "1.0.0"))
	if ok.Code != http.StatusOK {
		t.Fatalf("native 1.0.0 = %d %s, want 200", ok.Code, ok.Body.String())
	}
	var seen map[string]string
	_ = json.Unmarshal(ok.Body.Bytes(), &seen)
	if seen["native_client_id"] != testClientID {
		t.Fatalf("the native principal must carry its client id, got %v", seen)
	}
	if got := s.do(http.MethodGet, "/api/v2/gated", nil, bearer); got.Code != http.StatusOK {
		t.Fatalf("native without the header = %d, want 200", got.Code)
	}
	// A browser session is exempt whatever it sends (decision 13).
	cookie := withCookies(sessionCookie(userID, "gate@example.test"))
	if got := s.do(http.MethodGet, "/api/v2/gated", nil, cookie, withHeader(apimw.ClientVersionHeader, "0.0.1")); got.Code != http.StatusOK {
		t.Fatalf("cookie caller = %d %s, want 200 (exempt)", got.Code, got.Body.String())
	}
	// Revoke is never gated: an outdated client can still sign out.
	revoke := s.do(http.MethodPost, nativeapi.RevokePath, url.Values{"token": {pair.RefreshToken}, "client_id": {testClientID}},
		withHeader(apimw.ClientVersionHeader, "0.0.1"))
	if revoke.Code != http.StatusOK {
		t.Fatalf("outdated revoke = %d %s, want 200", revoke.Code, revoke.Body.String())
	}
}

func TestNativeAccessValidatorResolvesTheClientOfAToken(t *testing.T) {
	s := newStack(t)
	userID := s.seedUser("lookup@example.test")
	pair := s.exchange(s.signIn(userID, "lookup@example.test"))
	clientID, ok, err := s.validator.ClientID(context.Background(), pair.AccessToken)
	if err != nil || !ok || clientID != testClientID {
		t.Fatalf("ClientID = %q, %v, %v", clientID, ok, err)
	}
	for _, token := range []string{pair.RefreshToken, "elnat_unknown", "elitea_pat", ""} {
		if _, ok, err := s.validator.ClientID(context.Background(), token); ok || err != nil {
			t.Fatalf("ClientID(%q) = ok %v err %v, want not found", token, ok, err)
		}
	}
	user, err := s.validator.ValidateToken(context.Background(), pair.AccessToken)
	if err != nil || user.NativeClientID != testClientID {
		t.Fatalf("validated principal = %+v, %v", user, err)
	}
}

func TestNativeClientMinimumIsValidatedAndStored(t *testing.T) {
	s := newStack(t)
	client := fileClient()
	client.MinClientVersion = "latest"
	_, err := s.store.UpsertClient(context.Background(), client, 0)
	var invalid *domain.ClientValidationError
	if !errors.As(err, &invalid) || invalid.Reasons["min_client_version"] == "" {
		t.Fatalf("UpsertClient(latest) = %v, want a min_client_version reason", err)
	}
	s.setClientMinimum("3.0.0-beta.1")
	got, found, err := s.registry.Lookup(context.Background(), testClientID)
	if err != nil || !found || got.MinClientVersion != "3.0.0-beta.1" || got.Source != domain.SourceDB {
		t.Fatalf("registry = %+v, %v, %v", got, found, err)
	}
	clients, err := domain.ParseClientsFile([]byte(`- client_id: ai.elitea.file
  display_name: File
  redirect_uris: ["ai.elitea.file:/cb"]
  min_client_version: 1.0.0
`))
	if err != nil || len(clients) != 1 || clients[0].MinClientVersion != "1.0.0" {
		t.Fatalf("file layer = %+v, %v", clients, err)
	}
	if _, err := domain.ParseClientsFile([]byte(`- client_id: ai.elitea.file
  display_name: File
  redirect_uris: ["ai.elitea.file:/cb"]
  min_client_version: soon
`)); err == nil {
		t.Fatal("a file entry with a bad min_client_version must refuse boot")
	}
}
