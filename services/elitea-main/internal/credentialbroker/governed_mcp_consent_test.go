package credentialbroker

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	v2credentialconsent "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/credentialconsent"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/credentialgrant"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/governedmcp"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/provideroauth"
)

// consentFlow is the consent flow over targetTokens: Start issues a
// single-use state for the grant it was asked for, and the callback of that
// state stores the grant, as provideroauth.Consent does over its tables.
type consentFlow struct {
	tokens  *targetTokens
	token   string
	started map[string]provideroauth.StartRequest
	starts  int
}

func (f *consentFlow) Start(_ context.Context, r provideroauth.StartRequest) (provideroauth.StartResult, error) {
	f.starts++
	state := "state-" + strconv.Itoa(f.starts)
	f.started[state] = r
	return provideroauth.StartResult{AuthorizationURL: "https://as.example.com/authorize?state=" + state, ExpiresAt: tNow.Add(time.Minute)}, nil
}

func (f *consentFlow) Callback(ctx context.Context, r provideroauth.CallbackRequest, authorize provideroauth.Authorize) (provideroauth.CallbackResult, error) {
	started, ok := f.started[r.State]
	delete(f.started, r.State)
	if !ok || r.Provider != started.Provider {
		return provideroauth.CallbackResult{}, provideroauth.ErrStateInvalid
	}
	if userID, err := authorize(ctx, started.TenantProjectID); err != nil || userID != started.UserID {
		return provideroauth.CallbackResult{}, provideroauth.ErrStateInvalid
	}
	f.tokens.byTarget[started.TargetBindingID] = f.token
	return provideroauth.CallbackResult{
		GrantID: "grant-1", Provider: started.Provider, TenantProjectID: started.TenantProjectID,
		AgentBindingID: started.AgentBindingID, TargetBindingID: started.TargetBindingID,
	}, nil
}

func (f *consentFlow) RecordRefusal(context.Context, int64, string, string) error { return nil }

// consentUse grants models.credentials.governed.use to whoever asks.
type consentUse struct{}

func (consentUse) ResolvePermissions(_ context.Context, user auth.User, _, _ string) (auth.PermissionResolution, error) {
	id, _ := strconv.ParseInt(user.UserID, 10, 64)
	return auth.PermissionResolution{UserID: id, Permissions: []string{credentialgrant.PermissionUse}}, nil
}

func consentRoutes(t *testing.T, flow *consentFlow, refs *governedmcp.References) http.Handler {
	t.Helper()
	h, err := v2credentialconsent.New(flow, slog.New(slog.DiscardHandler), v2credentialconsent.WithAuthorizationReferences(refs))
	if err != nil {
		t.Fatal(err)
	}
	r := chi.NewRouter()
	r.With(v2credentialconsent.DecodeStart).Post("/credential_consent/{provider}:start", h.Start)
	r.Get("/credential_consent/{provider}:callback", h.Callback(consentUse{}, "default"))
	return r
}

func asCaller(router http.Handler, userID int64, method, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	id := strconv.FormatInt(userID, 10)
	req = req.WithContext(auth.ContextWithUser(req.Context(), auth.User{ID: id, UserID: id}))
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

// Acceptance (#1123 review): the authorization_reference of an
// AUTHORIZATION_REQUIRED pause starts consent for exactly the grant the
// broker asked for. Start with the reference, the provider callback, and
// the same governed use then redeems; another user's start with the same
// reference is refused and starts nothing.
func TestAnMCPAuthorizationReferenceStartsTheConsentItNames(t *testing.T) {
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

	flow := &consentFlow{tokens: tokens, token: a.token, started: map[string]provideroauth.StartRequest{}}
	router := consentRoutes(t, flow, refs)
	body := fmt.Sprintf(`{"project_id":%d,"authorization_reference":%q}`, tTenant, denial.AuthorizationReference)

	if rec := asCaller(router, tUser+1, http.MethodPost, "/credential_consent/mcp:start", body); rec.Code != http.StatusBadRequest || flow.starts != 0 {
		t.Fatalf("another user's start: status %d, starts %d", rec.Code, flow.starts)
	}
	for name, refused := range map[string]string{
		"with binding ids": fmt.Sprintf(`{"project_id":%d,"authorization_reference":%q,"target_binding_id":%q}`, tTenant, denial.AuthorizationReference, tTargetB),
		"another project":  fmt.Sprintf(`{"project_id":%d,"authorization_reference":%q}`, tTenant+1, denial.AuthorizationReference),
	} {
		if rec := asCaller(router, tUser, http.MethodPost, "/credential_consent/mcp:start", refused); rec.Code != http.StatusBadRequest || flow.starts != 0 {
			t.Fatalf("%s: status %d, starts %d", name, rec.Code, flow.starts)
		}
	}
	if rec := asCaller(router, tUser, http.MethodPost, "/credential_consent/jira:start", body); rec.Code != http.StatusBadRequest || flow.starts != 0 {
		t.Fatalf("an MCP reference on the jira start: status %d", rec.Code)
	}

	rec := asCaller(router, tUser, http.MethodPost, "/credential_consent/mcp:start", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("start: status %d %s", rec.Code, rec.Body)
	}
	var started struct {
		AuthorizationURL string `json:"authorization_url"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &started); err != nil {
		t.Fatal(err)
	}
	authURL, err := url.Parse(started.AuthorizationURL)
	if err != nil {
		t.Fatal(err)
	}
	state := authURL.Query().Get("state")
	want := provideroauth.StartRequest{UserID: tUser, Provider: "mcp", TenantProjectID: tTenant, AgentBindingID: tBinding, TargetBindingID: tTargetA}
	if got := flow.started[state]; got != want {
		t.Fatalf("started %+v, want %+v", got, want)
	}

	if rec := asCaller(router, tUser, http.MethodGet, "/credential_consent/mcp:callback?code=c&state="+state, ""); rec.Code != http.StatusOK {
		t.Fatalf("callback: status %d %s", rec.Code, rec.Body)
	}

	again := mcpRequest(t, w, "echo")
	again.Invocation.InvocationID = tInvocation2
	result := redeem(t, w.broker(t, options), again)
	if result.Envelope == nil {
		t.Fatalf("after consent: %+v", result.Denial)
	}
	defer result.Envelope.Destroy()
	if status := callTool(t, a, result.Envelope, "echo"); status != http.StatusOK {
		t.Fatalf("server A answered %d", status)
	}
}
