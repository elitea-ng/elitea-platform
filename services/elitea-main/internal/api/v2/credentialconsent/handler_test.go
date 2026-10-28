package credentialconsent

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/credentialgrant"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/provideroauth"
)

type fakeFlow struct {
	startErr    error
	callbackErr error
	authorized  int64
	authErr     error
	refusals    []string
	lastStart   provideroauth.StartRequest
}

func (f *fakeFlow) Start(_ context.Context, r provideroauth.StartRequest) (provideroauth.StartResult, error) {
	f.lastStart = r
	if f.startErr != nil {
		return provideroauth.StartResult{}, f.startErr
	}
	return provideroauth.StartResult{AuthorizationURL: "https://auth.example.com/authorize?state=s", ExpiresAt: time.Unix(0, 0)}, nil
}

func (f *fakeFlow) Callback(ctx context.Context, _ provideroauth.CallbackRequest, authorize provideroauth.Authorize) (provideroauth.CallbackResult, error) {
	f.authorized, f.authErr = authorize(ctx, 7)
	if f.authErr != nil {
		return provideroauth.CallbackResult{}, f.authErr
	}
	if f.callbackErr != nil {
		return provideroauth.CallbackResult{}, f.callbackErr
	}
	return provideroauth.CallbackResult{GrantID: "g", Provider: "jira", TenantProjectID: 7}, nil
}

func (f *fakeFlow) RecordRefusal(_ context.Context, _ int64, _, reason string) error {
	f.refusals = append(f.refusals, reason)
	return nil
}

type fakeResolver struct {
	perms []string
	err   error
}

func (r fakeResolver) ResolvePermissions(context.Context, auth.User, string, string) (auth.PermissionResolution, error) {
	return auth.PermissionResolution{UserID: 11, Permissions: r.perms}, r.err
}

func serve(t *testing.T, h *Handler, resolver auth.PermissionResolver, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := chi.NewRouter()
	r.With(DecodeStart).Post("/credential_consent/{provider}:start", h.Start)
	r.Get("/credential_consent/{provider}:callback", h.Callback(resolver, "default"))
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req = req.WithContext(auth.ContextWithUser(req.Context(), auth.User{ID: "11", UserID: "11"}))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

func TestStartDecodesTheBodyAndMapsErrors(t *testing.T) {
	cases := []struct {
		err  error
		want int
	}{
		{nil, http.StatusOK},
		{provideroauth.ErrInvalid, http.StatusBadRequest},
		{provideroauth.ErrProviderNotConfigured, http.StatusNotFound},
		{provideroauth.ErrTargetNotFound, http.StatusNotFound},
		{provideroauth.ErrBindingNotUsable, http.StatusConflict},
		{errors.New("db down"), http.StatusInternalServerError},
	}
	for _, tc := range cases {
		flow := &fakeFlow{startErr: tc.err}
		h, _ := New(flow, slog.New(slog.DiscardHandler))
		rec := serve(t, h, nil, http.MethodPost, "/credential_consent/jira:start",
			`{"project_id":7,"agent_binding_id":"a","target_binding_id":"b"}`)
		if rec.Code != tc.want {
			t.Errorf("%v: status %d, want %d", tc.err, rec.Code, tc.want)
		}
		if flow.lastStart.Provider != "jira" || flow.lastStart.UserID != 11 || flow.lastStart.TenantProjectID != 7 {
			t.Errorf("start request %+v", flow.lastStart)
		}
	}
	h, _ := New(&fakeFlow{}, nil)
	for _, body := range []string{``, `{"project_id":0}`, `{"project_id":7,"extra":1}`, `[]`} {
		if rec := serve(t, h, nil, http.MethodPost, "/credential_consent/jira:start", body); rec.Code != http.StatusBadRequest {
			t.Errorf("body %q: status %d", body, rec.Code)
		}
	}
}

type fakeReferences struct {
	requirement provideroauth.AuthorizationRequirement
	opened      []int64
}

func (f *fakeReferences) Open(_ string, tenantProjectID, userID int64) (provideroauth.AuthorizationRequirement, error) {
	f.opened = append(f.opened, tenantProjectID, userID)
	if tenantProjectID != f.requirement.Key.TenantProjectID || userID != f.requirement.Key.SubjectUserID {
		return provideroauth.AuthorizationRequirement{}, errors.New("not the caller's")
	}
	return f.requirement, nil
}

// An authorization_reference (#1123) resolves to its bindings for the
// caller; without a reference opener, or for a reference that does not open
// for the caller, nothing is started.
func TestStartResolvesAnAuthorizationReference(t *testing.T) {
	body := `{"project_id":7,"authorization_reference":"mar_x"}`
	flow := &fakeFlow{}
	h, _ := New(flow, nil)
	if rec := serve(t, h, nil, http.MethodPost, "/credential_consent/mcp:start", body); rec.Code != http.StatusBadRequest || flow.lastStart.UserID != 0 {
		t.Fatalf("no opener: status %d, started %+v", rec.Code, flow.lastStart)
	}
	refs := &fakeReferences{requirement: provideroauth.AuthorizationRequirement{Key: provideroauth.GrantKey{
		TenantProjectID: 7, Provider: "mcp", SubjectUserID: 11, AgentBindingID: "a", TargetBindingID: "b",
	}}}
	h, _ = New(flow, nil, WithAuthorizationReferences(refs))
	if rec := serve(t, h, nil, http.MethodPost, "/credential_consent/mcp:start", body); rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	if got := flow.lastStart; got.AgentBindingID != "a" || got.TargetBindingID != "b" || got.UserID != 11 || refs.opened[1] != 11 {
		t.Fatalf("started %+v, opened %v", got, refs.opened)
	}
	refs.requirement.Key.SubjectUserID = 12
	flow.lastStart = provideroauth.StartRequest{}
	if rec := serve(t, h, nil, http.MethodPost, "/credential_consent/mcp:start", body); rec.Code != http.StatusBadRequest || flow.lastStart.UserID != 0 {
		t.Fatalf("another user's reference: status %d, started %+v", rec.Code, flow.lastStart)
	}
}

func TestCallbackChecksThePermissionAndAudits(t *testing.T) {
	use := fakeResolver{perms: []string{credentialgrant.PermissionUse}}
	cases := []struct {
		name     string
		resolver auth.PermissionResolver
		err      error
		want     int
		refusal  string
	}{
		{"connected", use, nil, http.StatusOK, ""},
		{"no use permission", fakeResolver{perms: []string{"models.other"}}, nil, http.StatusForbidden, "consent.permission_missing"},
		{"not a member", fakeResolver{err: auth.ErrPermissionDenied}, nil, http.StatusForbidden, "consent.permission_missing"},
		{"bad state", use, provideroauth.ErrStateInvalid, http.StatusBadRequest, "consent.state_invalid"},
		{"declined", use, provideroauth.ErrConsentDenied, http.StatusForbidden, "consent.declined"},
		{"code refused", use, provideroauth.ErrInvalidGrant, http.StatusBadGateway, "consent.code_rejected"},
		{"provider down", use, provideroauth.ErrProviderUnavailable, http.StatusBadGateway, ""},
		{"not configured", use, provideroauth.ErrProviderNotConfigured, http.StatusNotFound, ""},
		{"target gone", use, provideroauth.ErrTargetNotFound, http.StatusNotFound, "consent.target_missing"},
		{"binding suspended", use, provideroauth.ErrBindingNotUsable, http.StatusConflict, "consent.binding_not_usable"},
		{"concurrent consent", use, provideroauth.ErrGrantConflict, http.StatusConflict, "consent.conflict"},
		{"no access token", use, provideroauth.ErrInvalid, http.StatusBadGateway, "consent.token_invalid"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			flow := &fakeFlow{callbackErr: tc.err}
			h, _ := New(flow, slog.New(slog.DiscardHandler))
			rec := serve(t, h, tc.resolver, http.MethodGet, "/credential_consent/jira:callback?state=s&code=c", "")
			if rec.Code != tc.want {
				t.Fatalf("status %d, want %d: %s", rec.Code, tc.want, rec.Body)
			}
			if tc.refusal != "" && (len(flow.refusals) != 1 || flow.refusals[0] != tc.refusal) {
				t.Fatalf("refusals %v, want %s", flow.refusals, tc.refusal)
			}
			if tc.want == http.StatusOK && flow.authorized != 11 {
				t.Fatalf("authorized user %d, want the resolved 11", flow.authorized)
			}
		})
	}
}

// No code, state or token from a request reaches a log line or an answer.
func TestCallbackLogsNoRequestSecret(t *testing.T) {
	var logs bytes.Buffer
	flow := &fakeFlow{callbackErr: errors.New("provideroauth: store the grant: connection reset")}
	h, _ := New(flow, slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	rec := serve(t, h, fakeResolver{perms: []string{credentialgrant.PermissionUse}}, http.MethodGet,
		"/credential_consent/jira:callback?state=state-secret-value&code=code-secret-value", "")
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status %d", rec.Code)
	}
	text := logs.String() + rec.Body.String()
	for _, secret := range []string{"state-secret-value", "code-secret-value"} {
		if strings.Contains(text, secret) {
			t.Fatalf("%s leaked: %s", secret, text)
		}
	}
	if !strings.Contains(logs.String(), "credential consent callback failed") {
		t.Fatal("the internal failure was not logged")
	}
}
