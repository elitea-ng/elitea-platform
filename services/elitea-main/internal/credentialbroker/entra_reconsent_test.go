package credentialbroker

import (
	"context"
	"encoding/base64"
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
	agentexecutionapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/agentexecution"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth/browsersession"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/mandate"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/identity/entra/delegationstore"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/identity/entra/obo"
)

const tDelegation2 = "0192f0aa-0000-7000-8000-000000000013"

// reconsentCustody is the delegation store over the world: the
// requirements the broker recorded, the world's delegations, and a new
// delegation for a sign-in of one browser session after the pause.
type reconsentCustody struct {
	w         *world
	sessionID string
	resolved  map[string]string
	delegates []delegationstore.DelegateRequest
}

func (c *reconsentCustody) RecordRequirement(ctx context.Context, r delegationstore.Requirement) (delegationstore.Requirement, error) {
	return c.w.custody.RecordRequirement(ctx, r)
}

func (c *reconsentCustody) RequirementFor(_ context.Context, reference string, project, user int64) (delegationstore.Requirement, error) {
	for _, r := range c.w.custody.requirements {
		if r.Reference == reference && r.TenantProjectID == project && r.ActorUserID == user {
			r.CreatedAt = tNow
			r.ResolvedDelegationID = c.resolved[reference]
			r.Resolved = r.ResolvedDelegationID != ""
			return r, nil
		}
	}
	return delegationstore.Requirement{}, delegationstore.ErrRequirementNotFound
}

func (c *reconsentCustody) ResolveRequirement(ctx context.Context, reference string, project, user int64, delegationID string) (delegationstore.Requirement, error) {
	r, err := c.RequirementFor(ctx, reference, project, user)
	if err != nil {
		return delegationstore.Requirement{}, err
	}
	if r.Resolved {
		return delegationstore.Requirement{}, delegationstore.ErrRequirementResolved
	}
	answer, paused := c.w.delegations.delegations[delegationID], c.w.delegations.delegations[r.DelegationID]
	if answer.State != mandate.StateActive || answer.AgentBindingID != paused.AgentBindingID ||
		answer.SubjectObjectID != paused.SubjectObjectID || answer.TenantProjectID != project {
		return delegationstore.Requirement{}, delegationstore.ErrInvalid
	}
	c.resolved[reference] = delegationID
	r.Resolved, r.ResolvedDelegationID = true, delegationID
	return r, nil
}

func (c *reconsentCustody) Delegation(_ context.Context, id string) (mandate.Delegation, error) {
	d, ok := c.w.delegations.delegations[id]
	if !ok {
		return mandate.Delegation{}, mandate.ErrDelegationNotUsable
	}
	return d, nil
}

func (c *reconsentCustody) Delegate(_ context.Context, r delegationstore.DelegateRequest) (delegationstore.Delegation, error) {
	c.delegates = append(c.delegates, r)
	// Only the new sign-in of this session, after the pause, delegates.
	if r.SessionID != c.sessionID || !r.SignedInAfter.Equal(tNow) {
		return delegationstore.Delegation{}, delegationstore.ErrNoSignInAssertion
	}
	paused := c.w.delegations.delegations[tDelegation]
	d := paused
	d.ID, d.ConsentRecordedAt = tDelegation2, tNow.Add(time.Minute)
	c.w.delegations.delegations[tDelegation2] = d
	return delegationstore.Delegation{ID: tDelegation2, TenantProjectID: r.TenantProjectID, AgentBindingID: r.AgentBindingID}, nil
}

// rerunContinuer continues the paused turn the way the runtime does: the
// new run binds the newest active delegation of the user and binding.
type rerunContinuer struct {
	w        *world
	requests []agentexecutionapp.CurrentContinuationRequest
}

func (c *rerunContinuer) ContinueCurrentAgent(_ context.Context, r agentexecutionapp.CurrentContinuationRequest) (agentexecutionapp.CurrentApplicationStartOutcome, error) {
	c.requests = append(c.requests, r)
	c.w.context.DelegationID, c.w.context.DelegationEpoch = tDelegation2, 1
	return agentexecutionapp.CurrentApplicationStartOutcome{ExecutionID: "execution-2", Created: true}, nil
}

func entraRoutes(t *testing.T, custody *reconsentCustody, continuer *rerunContinuer) http.Handler {
	t.Helper()
	authorizations, err := agentexecutionapp.NewCredentialAuthorizations(custody, continuer)
	if err != nil {
		t.Fatal(err)
	}
	h, err := v2credentialconsent.New(nil, slog.New(slog.DiscardHandler),
		v2credentialconsent.WithEntraConsent(custody, authorizations, true))
	if err != nil {
		t.Fatal(err)
	}
	r := chi.NewRouter()
	r.With(v2credentialconsent.DecodeStart).Post("/credential_consent/{provider}:start", h.Start)
	r.Get("/credential_consent/{provider}:callback", h.Callback(consentUse{}, "default"))
	return r
}

func asBrowser(router http.Handler, userID int64, method, target, body string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	for _, c := range cookies {
		req.AddCookie(c)
	}
	id := strconv.FormatInt(userID, 10)
	req = req.WithContext(auth.ContextWithUser(req.Context(), auth.User{ID: id, UserID: id}))
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

func consentCookie(t *testing.T, rec *httptest.ResponseRecorder) *http.Cookie {
	t.Helper()
	for _, c := range rec.Result().Cookies() {
		if c.Name == "elitea_entra_consent" {
			return c
		}
	}
	t.Fatalf("no consent cookie: %v", rec.Header())
	return nil
}

// Acceptance (#1477): an Entra AUTHORIZATION_REQUIRED pause resumes through
// the consent routes. Start with the `car_` reference answers the sign-in
// URL; another user's or project's start is refused before any identity
// provider call. After the new sign-in, the callback creates a delegation
// from it, resolves the requirement and continues the paused turn
// (CredentialAuthorizations.Resume); the next redemption then succeeds.
func TestAnEntraAuthorizationReferenceResumesThePausedRun(t *testing.T) {
	w := newWorld()
	w.context.MandateID, w.context.MandateEpoch = "", 0
	w.context.DelegationID, w.context.DelegationEpoch = tDelegation, 1
	ref := w.references.refs[tReference]
	ref.Class = "oauth_delegated"
	w.references.refs[tReference] = ref
	w.exchanger.err = obo.ErrInteractionRequired
	denial := requireDenial(t, redeem(t, w.broker(t), w.request(t)), CodeAuthorizationRequired)
	exchanges := w.exchanger.calls

	session := base64.RawURLEncoding.EncodeToString(make([]byte, browsersession.IDRandomBytes))
	custody := &reconsentCustody{w: w, sessionID: session, resolved: map[string]string{}}
	continuer := &rerunContinuer{w: w}
	router := entraRoutes(t, custody, continuer)
	body := fmt.Sprintf(`{"project_id":%d,"authorization_reference":%q}`, tTenant, denial.AuthorizationReference)

	// Bound to the caller and the tenant project: refused before any
	// identity provider call, and no state is issued.
	for name, rec := range map[string]*httptest.ResponseRecorder{
		"another user":    asBrowser(router, tUser+1, http.MethodPost, "/credential_consent/entra:start", body),
		"another project": asBrowser(router, tUser, http.MethodPost, "/credential_consent/entra:start", fmt.Sprintf(`{"project_id":%d,"authorization_reference":%q}`, tTenant+1, denial.AuthorizationReference)),
		"binding ids":     asBrowser(router, tUser, http.MethodPost, "/credential_consent/entra:start", fmt.Sprintf(`{"project_id":%d,"authorization_reference":%q,"agent_binding_id":%q}`, tTenant, denial.AuthorizationReference, tBinding)),
	} {
		if rec.Code != http.StatusBadRequest || len(rec.Result().Cookies()) != 0 || strings.Contains(rec.Body.String(), "authorization_url") {
			t.Fatalf("%s: status %d %s", name, rec.Code, rec.Body)
		}
	}

	rec := asBrowser(router, tUser, http.MethodPost, "/credential_consent/entra:start", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("start: %d %s", rec.Code, rec.Body)
	}
	var started struct {
		AuthorizationURL string `json:"authorization_url"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &started); err != nil {
		t.Fatal(err)
	}
	login, err := url.Parse(started.AuthorizationURL)
	if err != nil {
		t.Fatal(err)
	}
	if login.Path != "/auth/oidc/login" || login.Query().Get("prompt") != "login" ||
		strings.Contains(started.AuthorizationURL, denial.AuthorizationReference) {
		t.Fatalf("authorization url %q", started.AuthorizationURL)
	}
	target, err := url.Parse(login.Query().Get("target_to"))
	if err != nil || target.Path != v2credentialconsent.EntraCallbackPath || target.Query().Get("state") == "" {
		t.Fatalf("target_to %q", login.Query().Get("target_to"))
	}
	cookie := consentCookie(t, rec)
	if !cookie.HttpOnly || !cookie.Secure || cookie.Path != "/api/v2/credential_consent/" {
		t.Fatalf("consent cookie %+v", cookie)
	}
	sessionCookie := &http.Cookie{Name: browsersession.CookieName, Value: browsersession.CookieValue(session)}
	callback := "/credential_consent/entra:callback?" + target.RawQuery

	// The callback of another user, or with another state, does nothing.
	if rec := asBrowser(router, tUser+1, http.MethodGet, callback, "", cookie, sessionCookie); rec.Code != http.StatusBadRequest || len(custody.delegates) != 0 {
		t.Fatalf("another user's callback: %d %s", rec.Code, rec.Body)
	}
	if rec := asBrowser(router, tUser, http.MethodGet, "/credential_consent/entra:callback?state=forged", "", cookie, sessionCookie); rec.Code != http.StatusBadRequest || len(custody.delegates) != 0 {
		t.Fatalf("forged state: %d %s", rec.Code, rec.Body)
	}

	rec = asBrowser(router, tUser, http.MethodGet, callback, "", cookie, sessionCookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("callback: %d %s", rec.Code, rec.Body)
	}
	var resumed struct {
		Status         string `json:"status"`
		AgentBindingID string `json:"agent_binding_id"`
		ExecutionID    string `json:"execution_id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resumed); err != nil || resumed.Status != "resumed" ||
		resumed.AgentBindingID != tBinding || resumed.ExecutionID != "execution-2" {
		t.Fatalf("callback body %s", rec.Body)
	}
	if d := custody.delegates[len(custody.delegates)-1]; d.AgentBindingID != tBinding || d.Audience != "api://middle" ||
		d.TenantProjectID != tTenant || d.UserID != tUser {
		t.Fatalf("delegate request %+v", d)
	}
	if len(continuer.requests) != 1 {
		t.Fatalf("continuations %d", len(continuer.requests))
	}
	if c := continuer.requests[0]; c.Kind != agentexecutionapp.CurrentContinuationAuthorization ||
		c.AuthorizationID != denial.AuthorizationReference || c.Action != "authorize" || c.ActorUserID != tUser ||
		c.ProjectID != tTenant {
		t.Fatalf("continuation %+v", c)
	}
	if custody.resolved[denial.AuthorizationReference] != tDelegation2 {
		t.Fatalf("resolved %v", custody.resolved)
	}

	// The resumed run redeems with the new delegation.
	w.exchanger.err = nil
	again := w.request(t)
	again.Invocation.InvocationID = tInvocation2
	result := redeem(t, w.broker(t), again)
	if result.Envelope == nil {
		t.Fatalf("after re-consent: %+v", result.Denial)
	}
	defer result.Envelope.Destroy()
	if w.exchanger.calls != exchanges+1 || w.exchanger.last.DelegationID != tDelegation2 ||
		string(result.Envelope.Material["access_token"]) != tOAuthValue {
		t.Fatalf("exchange %+v", w.exchanger.last)
	}

	// The state is single use, and a resolved reference does not start again.
	if rec := asBrowser(router, tUser, http.MethodGet, callback, "", sessionCookie); rec.Code != http.StatusBadRequest {
		t.Fatalf("replayed callback: %d", rec.Code)
	}
	if rec := asBrowser(router, tUser, http.MethodPost, "/credential_consent/entra:start", body); rec.Code != http.StatusConflict {
		t.Fatalf("resolved start: %d %s", rec.Code, rec.Body)
	}
}

// A claims challenge asks the sign-in for the claims it names.
func TestAnEntraClaimsChallengeStartsASignInWithItsClaims(t *testing.T) {
	w := newWorld()
	w.context.MandateID, w.context.MandateEpoch = "", 0
	w.context.DelegationID, w.context.DelegationEpoch = tDelegation, 1
	ref := w.references.refs[tReference]
	ref.Class = "oauth_delegated"
	w.references.refs[tReference] = ref
	claims := `{"access_token":{"acrs":{"essential":true,"value":"c1"}}}`
	w.exchanger.err = &obo.ErrClaimsChallenge{Claims: claims}
	denial := requireDenial(t, redeem(t, w.broker(t), w.request(t)), CodeAuthorizationRequired)
	router := entraRoutes(t, &reconsentCustody{w: w, resolved: map[string]string{}}, &rerunContinuer{w: w})
	rec := asBrowser(router, tUser, http.MethodPost, "/credential_consent/entra:start",
		fmt.Sprintf(`{"project_id":%d,"authorization_reference":%q}`, tTenant, denial.AuthorizationReference))
	if rec.Code != http.StatusOK {
		t.Fatalf("start: %d %s", rec.Code, rec.Body)
	}
	var started struct {
		AuthorizationURL string `json:"authorization_url"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &started); err != nil {
		t.Fatal(err)
	}
	login, err := url.Parse(started.AuthorizationURL)
	if err != nil || login.Query().Get("claims") != claims || login.Query().Get("prompt") != "" {
		t.Fatalf("authorization url %q", started.AuthorizationURL)
	}
}
