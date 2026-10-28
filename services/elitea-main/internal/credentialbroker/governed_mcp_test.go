package credentialbroker

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/credentialadapters"
	mcpadapter "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/credentialadapters/mcp"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/credentialgrant"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/mandate"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/governedmcp"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/provideroauth"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/securityaudit"
)

// mockMCP is a remote MCP server in process (the deploy/mock-mcp role): it
// answers tools/call only for its own bearer token and counts every call.
type mockMCP struct {
	*httptest.Server
	token string
	calls atomic.Int32
	tools []string
}

func newMockMCP(t *testing.T, token string) *mockMCP {
	t.Helper()
	m := &mockMCP{token: token}
	m.Server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m.calls.Add(1)
		if r.URL.Path != "/mcp" || r.Header.Get("Authorization") != "Bearer "+m.token {
			w.Header().Set("WWW-Authenticate", `Bearer resource_metadata="`+m.URL+`/.well-known/oauth-protected-resource/mcp"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		var call struct {
			ID     int    `json:"id"`
			Method string `json:"method"`
			Params struct {
				Name string `json:"name"`
			} `json:"params"`
		}
		_ = json.NewDecoder(r.Body).Decode(&call)
		m.tools = append(m.tools, call.Params.Name)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": call.ID,
			"result": map[string]any{"content": []map[string]any{{"type": "text", "text": "echo"}}}})
	}))
	t.Cleanup(m.Close)
	return m
}

// callTool is the worker's half of a governed MCP call: the envelope's
// token goes only to the server the envelope is bound to.
func callTool(t *testing.T, server *mockMCP, envelope *Envelope, tool string) int {
	t.Helper()
	if !governedmcp.EnvelopeServes(envelope.TargetIdentity, server.URL+"/mcp") {
		return 0
	}
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{"name": tool, "arguments": map[string]any{}}})
	req, _ := http.NewRequest(http.MethodPost, server.URL+"/mcp", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+string(envelope.Material["access_token"]))
	resp, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	return resp.StatusCode
}

// targetTokens is the provider token source keyed by target binding: one
// grant per MCP server.
type targetTokens struct {
	byTarget map[string]string
	keys     []provideroauth.GrantKey
}

func (f *targetTokens) AccessToken(_ context.Context, key provideroauth.GrantKey) (provideroauth.AccessToken, error) {
	f.keys = append(f.keys, key)
	token, ok := f.byTarget[key.TargetBindingID]
	if !ok || key.Provider != mcpadapter.ProviderType {
		return provideroauth.AccessToken{}, provideroauth.ErrConsentRequired
	}
	return provideroauth.AccessToken{Token: token, ExpiresAt: tNow.Add(3 * time.Minute)}, nil
}

// mcpWorld is a delegated_background chat turn whose toolkit is a remote
// MCP server (target A); target B is a second server in the project.
func mcpWorld(t *testing.T, a, b *mockMCP) (*world, *targetTokens) {
	t.Helper()
	w := newWorld().withMandate(mandate.KindDelegatedBackground)
	m := w.mandates.mandates[tMandate]
	m.OperationScopes = []string{string(mcpadapter.OperationToolCall)}
	w.mandates.mandates[tMandate] = m
	for id, server := range map[string]*mockMCP{tTargetA: a, tTargetB: b} {
		identity, err := mcpadapter.CanonicalResource(server.URL + "/mcp")
		if err != nil {
			t.Fatal(err)
		}
		w.targets.targets[id] = credentialgrant.TargetBinding{ID: id, TenantProjectID: tTenant, ProviderType: mcpadapter.ProviderType, TargetIdentity: identity}
	}
	for _, id := range []string{tReference, tReferenceB} {
		ref := w.references.refs[id]
		ref.ProviderType, ref.Class = mcpadapter.ProviderType, credentialgrant.ClassOAuthDelegated
		w.references.refs[id] = ref
	}
	w.grants.grants[0].OperationScopes = []string{string(mcpadapter.OperationToolCall)}
	w.catalog.ops = map[credentialgrant.OperationID]string{mcpadapter.OperationToolCall: credentialadapters.OperationWrite}
	w.catalog.authority = map[string]credentialgrant.OAuthAuthority{mcpadapter.ProviderType: credentialgrant.OAuthAuthorityProvider}
	return w, &targetTokens{byTarget: map[string]string{tTargetA: a.token, tTargetB: b.token}}
}

func mcpRequest(t *testing.T, w *world, tool string) RedeemRequest {
	req := w.request(t)
	req.Invocation.OperationID, req.Invocation.ResourceRef = string(mcpadapter.OperationToolCall), "mcp:tool/"+tool
	return req
}

func mcpRegistry(t *testing.T) *credentialadapters.Registry {
	t.Helper()
	registry, err := credentialadapters.NewBuiltinRegistry()
	if err != nil {
		t.Fatal(err)
	}
	return registry
}

// Acceptance (#1123): an outbound governed MCP tool call redeems its token
// through the broker, the decision and the redemption are recorded, and
// the token reaches the MCP server it is bound to.
func TestGovernedMCPToolCallRedeemsThroughTheBroker(t *testing.T) {
	a, b := newMockMCP(t, "token-for-a"), newMockMCP(t, "token-for-b")
	w, tokens := mcpWorld(t, a, b)
	broker := w.broker(t, func(c *Config) { c.ProviderTokens = tokens; c.Catalog = mcpRegistry(t) })
	result := redeem(t, broker, mcpRequest(t, w, "echo"))
	if result.Envelope == nil {
		t.Fatalf("denied: %+v", result.Denial)
	}
	defer result.Envelope.Destroy()
	if result.Envelope.Kind != KindOAuthAccess || result.Envelope.TargetIdentity != w.targets.targets[tTargetA].TargetIdentity {
		t.Fatalf("envelope kind %s target %q", result.Envelope.Kind, result.Envelope.TargetIdentity)
	}
	want := provideroauth.GrantKey{TenantProjectID: tTenant, Provider: "mcp", TargetBindingID: tTargetA, SubjectUserID: tUser, AgentBindingID: tBinding}
	if len(tokens.keys) != 1 || tokens.keys[0] != want {
		t.Fatalf("grant keys %+v", tokens.keys)
	}
	var decided, redeemed bool
	for _, event := range w.store.events {
		switch {
		case event.Type == securityaudit.TypeDecision && event.Action == "credential.use" && event.Effect == securityaudit.EffectAllow:
			decided = true
		case event.Type == securityaudit.TypeCredentialRedeem:
			redeemed = true
		}
	}
	if !decided || !redeemed || len(w.store.redemptions) != 1 {
		t.Fatalf("decision recorded %v, redemption recorded %v (%d)", decided, redeemed, len(w.store.redemptions))
	}
	if status := callTool(t, a, result.Envelope, "echo"); status != http.StatusOK || a.calls.Load() != 1 || len(a.tools) != 1 || a.tools[0] != "echo" {
		t.Fatalf("server A status %d calls %d tools %v", status, a.calls.Load(), a.tools)
	}
}

// Acceptance (#1123): a token released for server A is refused for server
// B: the envelope is bound to A's target, B never receives it, and B
// answers A's token with 401 if it is sent anyway.
func TestAnMCPTokenForServerAIsRefusedForServerB(t *testing.T) {
	a, b := newMockMCP(t, "token-for-a"), newMockMCP(t, "token-for-b")
	w, tokens := mcpWorld(t, a, b)
	result := redeem(t, w.broker(t, func(c *Config) { c.ProviderTokens = tokens; c.Catalog = mcpRegistry(t) }), mcpRequest(t, w, "echo"))
	if result.Envelope == nil {
		t.Fatalf("denied: %+v", result.Denial)
	}
	defer result.Envelope.Destroy()
	if status := callTool(t, b, result.Envelope, "echo"); status != 0 || b.calls.Load() != 0 {
		t.Fatalf("server A's envelope reached server B: status %d calls %d", status, b.calls.Load())
	}
	forged := &Envelope{TargetIdentity: w.targets.targets[tTargetB].TargetIdentity,
		Material: map[string][]byte{"access_token": append([]byte(nil), result.Envelope.Material["access_token"]...)}}
	if status := callTool(t, b, forged, "echo"); status != http.StatusUnauthorized {
		t.Fatalf("server B accepted server A's token: %d", status)
	}
	// The grant of target B is a different grant: the broker never asks for
	// A's when the reference names B. A run admitted with B's toolkit
	// reference froze B (#1119), so the frozen-reference check passes and
	// the target binding is what decides.
	w.context.CredentialReferences[0].CredentialReferenceID = tReferenceB
	w.context.CredentialReferences[0].TargetBindingID = tTargetB
	req := mcpRequest(t, w, "echo")
	req.Invocation.InvocationID, req.Invocation.CredentialReferenceID = tInvocation2, tReferenceB
	w.grants.grants[0].CredentialReferenceID, w.grants.grants[0].TargetBindingID = tReferenceB, tTargetB
	second := redeem(t, w.broker(t, func(c *Config) { c.ProviderTokens = tokens; c.Catalog = mcpRegistry(t) }), req)
	if second.Envelope == nil {
		t.Fatalf("denied: %+v", second.Denial)
	}
	defer second.Envelope.Destroy()
	if string(second.Envelope.Material["access_token"]) != b.token || tokens.keys[len(tokens.keys)-1].TargetBindingID != tTargetB {
		t.Fatal("target B was served target A's grant")
	}
}

// Acceptance (#1123): no consent for the MCP server pauses the chat turn
// with AUTHORIZATION_REQUIRED and an opaque reference that opens to the
// consent start of the same user; with no reference minter, or outside a
// chat turn, the use is denied as before.
func TestMissingMCPConsentIsAuthorizationRequired(t *testing.T) {
	a, b := newMockMCP(t, "token-for-a"), newMockMCP(t, "token-for-b")
	w, tokens := mcpWorld(t, a, b)
	delete(tokens.byTarget, tTargetA)
	refs, err := governedmcp.NewReferences(bytes.Repeat([]byte{7}, 32), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	options := func(c *Config) {
		c.ProviderTokens = tokens
		c.Catalog = mcpRegistry(t)
		c.ProviderAuthorizations = refs
	}
	denial := requireDenial(t, redeem(t, w.broker(t, options), mcpRequest(t, w, "echo")), CodeAuthorizationRequired)
	if !strings.HasPrefix(denial.AuthorizationReference, governedmcp.ReferencePrefix) || denial.DecisionID == "" {
		t.Fatalf("denial %+v", denial)
	}
	opened, err := refs.Open(denial.AuthorizationReference, tTenant, tUser)
	if err != nil || opened.Key.TargetBindingID != tTargetA || opened.Key.AgentBindingID != tBinding || opened.ExecutionID != tExecution {
		t.Fatalf("opened %+v, %v", opened, err)
	}
	if a.calls.Load() != 0 {
		t.Fatal("the MCP server was called without a token")
	}

	unattended := mcpRequest(t, w, "echo")
	unattended.Invocation.InvocationID = tInvocation2
	unattended.Run.ConversationUUID, unattended.Run.ResponseMessageID = "", ""
	if d := requireDenial(t, redeem(t, w.broker(t, options), unattended), CodeDenied); d.reason != "provider_consent_required" {
		t.Fatalf("unattended reason %q", d.reason)
	}
	w2, tokens2 := mcpWorld(t, a, b)
	delete(tokens2.byTarget, tTargetA)
	d := requireDenial(t, redeem(t, w2.broker(t, func(c *Config) { c.ProviderTokens = tokens2 }), mcpRequest(t, w2, "echo")), CodeDenied)
	if d.reason != "provider_consent_required" || d.AuthorizationReference != "" {
		t.Fatalf("no minter: %+v", d)
	}
}
