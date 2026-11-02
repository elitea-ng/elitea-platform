package credentialconsent

// Entra re-consent of a paused governed run (ADR-0039, issue 1477).
//
// An Entra exchange that needs the user (interaction required, or a claims
// challenge) pauses the run with AUTHORIZATION_REQUIRED and an opaque
// `car_` authorization_reference. The user resumes it through the same
// consent routes as a provider OAuth grant:
//
//  1. POST /credential_consent/entra:start with project_id and the
//     reference. The reference must be the caller's own, in that project,
//     and not resolved yet; otherwise the answer is 400 and no identity
//     provider is called. The answer is the platform sign-in URL. Its
//     target is the callback, with a random state. The same state and the
//     reference go into a short-lived HttpOnly cookie, so the reference is
//     never in a URL.
//  2. The user signs in again. The OIDC callback keeps the new Entra
//     assertion for the browser session and redirects to the callback.
//  3. GET /credential_consent/entra:callback?state=… checks the state
//     against the cookie, opens the reference again for the caller, checks
//     the permission in the project, creates a delegation from an assertion
//     stored AFTER the run paused, and resumes the run
//     (CredentialAuthorizations.Resume).
//
// No response holds an assertion or a token.

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	agentexecutionapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/agentexecution"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth/browsersession"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/credentialgrant"
	executiondomain "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/execution"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/mandate"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/identity/entra/delegationstore"
)

// EntraProvider is the {provider} path value of the Entra re-consent.
const EntraProvider = "entra"

const (
	// entraConsentCookie carries "state|project|reference" between start and
	// callback. Its value is checked against the store on every use.
	entraConsentCookie = "elitea_entra_consent"
	entraConsentPath   = "/api/v2/credential_consent/"
	entraConsentTTL    = 10 * time.Minute
	// EntraCallbackPath is where the sign-in returns.
	EntraCallbackPath = entraConsentPath + EntraProvider + ":callback"
	// EntraLoginPath is the platform OIDC sign-in. It equals
	// v2auth.OIDCLoginPath; that package cannot be imported here (an import
	// cycle through the credential broker), so internal/api tests the two.
	EntraLoginPath = "/auth/oidc/login"
)

// EntraCustody is the half of the delegation store the re-consent uses
// (*delegationstore.Store).
type EntraCustody interface {
	RequirementFor(ctx context.Context, reference string, tenantProjectID, actorUserID int64) (delegationstore.Requirement, error)
	Delegation(ctx context.Context, id string) (mandate.Delegation, error)
	Delegate(ctx context.Context, r delegationstore.DelegateRequest) (delegationstore.Delegation, error)
}

var _ EntraCustody = (*delegationstore.Store)(nil)

// EntraResumer continues the paused run (*agentexecution.CredentialAuthorizations).
type EntraResumer interface {
	Resume(ctx context.Context, r agentexecutionapp.CredentialAuthorizationResume) (agentexecutionapp.CurrentApplicationStartOutcome, error)
}

var _ EntraResumer = (*agentexecutionapp.CredentialAuthorizations)(nil)

// entraConsent is the Entra half of the handler.
type entraConsent struct {
	custody       EntraCustody
	resumer       EntraResumer
	secureCookies bool
	now           func() time.Time
	random        func([]byte) error
}

// WithEntraConsent enables the Entra re-consent of `car_` references.
// secureCookies sets the Secure flag on the state cookie (TLS deployments).
func WithEntraConsent(custody EntraCustody, resumer EntraResumer, secureCookies bool) Option {
	return func(h *Handler) {
		if custody == nil || resumer == nil {
			return
		}
		h.entra = &entraConsent{
			custody: custody, resumer: resumer, secureCookies: secureCookies, now: time.Now,
			random: func(b []byte) error { _, err := rand.Read(b); return err },
		}
	}
}

// WithEntra adds the Entra re-consent to a built handler: the composition
// root has the run continuer only after the runtime plane is up. nil h
// stays nil.
func (h *Handler) WithEntra(custody EntraCustody, resumer EntraResumer, secureCookies bool) *Handler {
	if h != nil {
		WithEntraConsent(custody, resumer, secureCookies)(h)
	}
	return h
}

// isEntra reports whether the path names the Entra re-consent.
func isEntra(name string) bool { return name == EntraProvider }

// startEntra serves POST /credential_consent/entra:start.
func (h *Handler) startEntra(w http.ResponseWriter, r *http.Request, body startBody, userID int64) {
	e := h.entra
	if e == nil {
		writeError(w, http.StatusNotFound, "Entra re-consent is not configured")
		return
	}
	if body.AuthorizationReference == "" || body.AgentBindingID != "" || body.TargetBindingID != "" ||
		!strings.HasPrefix(body.AuthorizationReference, "car_") {
		writeError(w, http.StatusBadRequest, "the authorization reference is not valid for this request")
		return
	}
	requirement, err := e.custody.RequirementFor(r.Context(), body.AuthorizationReference, body.ProjectID, userID)
	switch {
	case errors.Is(err, delegationstore.ErrRequirementNotFound):
		// Another user's or another project's reference reads as unknown.
		writeError(w, http.StatusBadRequest, "the authorization reference is not valid for this request")
		return
	case err != nil:
		h.logger.ErrorContext(r.Context(), "credential consent: read the authorization requirement", "error", err)
		writeError(w, http.StatusInternalServerError, "credential consent: internal error")
		return
	case requirement.Resolved:
		writeError(w, http.StatusConflict, "the authorization reference is already resolved")
		return
	}
	raw := make([]byte, 24)
	if err := e.random(raw); err != nil {
		writeError(w, http.StatusInternalServerError, "credential consent: internal error")
		return
	}
	state := base64.RawURLEncoding.EncodeToString(raw)
	expires := e.now().Add(entraConsentTTL)
	http.SetCookie(w, &http.Cookie{
		Name:  entraConsentCookie,
		Value: state + "|" + strconv.FormatInt(body.ProjectID, 10) + "|" + requirement.Reference,
		Path:  entraConsentPath, HttpOnly: true, Secure: e.secureCookies, SameSite: http.SameSiteLaxMode,
		MaxAge: int(entraConsentTTL / time.Second),
	})
	login := url.Values{"target_to": {EntraCallbackPath + "?state=" + state}}
	if requirement.Reason == delegationstore.ReasonClaimsChallenge && requirement.Claims != "" {
		login.Set("claims", requirement.Claims)
	} else {
		login.Set("prompt", "login")
	}
	writeJSON(w, http.StatusOK, startView{
		AuthorizationURL: EntraLoginPath + "?" + login.Encode(), ExpiresAt: expires.UTC().Format(time.RFC3339Nano),
	})
}

// entraCookie is the parsed start cookie.
type entraCookie struct {
	state     string
	projectID int64
	reference string
}

func readEntraCookie(r *http.Request) (entraCookie, bool) {
	cookie, err := r.Cookie(entraConsentCookie)
	if err != nil {
		return entraCookie{}, false
	}
	parts := strings.SplitN(cookie.Value, "|", 3)
	if len(parts) != 3 || parts[0] == "" || parts[2] == "" {
		return entraCookie{}, false
	}
	project, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil || project <= 0 {
		return entraCookie{}, false
	}
	return entraCookie{state: parts[0], projectID: project, reference: parts[2]}, true
}

func (e *entraConsent) clearCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name: entraConsentCookie, Value: "", Path: entraConsentPath, HttpOnly: true, Secure: e.secureCookies,
		SameSite: http.SameSiteLaxMode, MaxAge: -1,
	})
}

// callbackEntra serves GET /credential_consent/entra:callback.
func (h *Handler) callbackEntra(w http.ResponseWriter, r *http.Request, user auth.User, resolver auth.PermissionResolver, mode string) {
	e := h.entra
	if e == nil {
		writeError(w, http.StatusNotFound, "Entra re-consent is not configured")
		return
	}
	userID, err := strconv.ParseInt(user.UserID, 10, 64)
	if err != nil || userID <= 0 {
		writeError(w, http.StatusForbidden, "the caller has no resolved user id")
		return
	}
	saved, ok := readEntraCookie(r)
	state := r.URL.Query().Get("state")
	if !ok || state == "" || subtle.ConstantTimeCompare([]byte(state), []byte(saved.state)) != 1 {
		h.refusal(r.Context(), user, EntraProvider, "consent.state_invalid")
		writeError(w, http.StatusBadRequest, "the consent request is unknown, expired or already used")
		return
	}
	// The state is single use.
	e.clearCookie(w)
	ctx := r.Context()
	requirement, err := e.custody.RequirementFor(ctx, saved.reference, saved.projectID, userID)
	switch {
	case errors.Is(err, delegationstore.ErrRequirementNotFound):
		h.refusal(ctx, user, EntraProvider, "consent.state_invalid")
		writeError(w, http.StatusBadRequest, "the consent request is unknown, expired or already used")
		return
	case err != nil:
		h.logger.ErrorContext(ctx, "credential consent: read the authorization requirement", "error", err)
		writeError(w, http.StatusInternalServerError, "credential consent: internal error")
		return
	}
	if !permitted(ctx, resolver, user, mode, saved.projectID) {
		h.refusal(ctx, user, EntraProvider, "consent.permission_missing")
		writeError(w, http.StatusForbidden, "insufficient permissions")
		return
	}
	paused, err := e.custody.Delegation(ctx, requirement.DelegationID)
	if err != nil || paused.TenantProjectID != saved.projectID || len(paused.Audiences) == 0 {
		h.refusal(ctx, user, EntraProvider, "consent.binding_not_usable")
		writeError(w, http.StatusConflict, "the delegation of the paused run is gone; start the turn again")
		return
	}
	sessionCookie, err := r.Cookie(browsersession.CookieName)
	if err != nil {
		writeError(w, http.StatusConflict, "sign in again in this browser, then retry")
		return
	}
	sessionID, err := browsersession.ParseCookieValue(sessionCookie.Value)
	if err != nil {
		writeError(w, http.StatusConflict, "sign in again in this browser, then retry")
		return
	}
	delegation, err := e.custody.Delegate(ctx, delegationstore.DelegateRequest{
		SessionID: sessionID, UserID: userID, TenantProjectID: saved.projectID,
		AgentBindingID: paused.AgentBindingID, Audience: paused.Audiences[0], Scopes: paused.Scopes,
		SignedInAfter: requirement.CreatedAt,
	})
	switch {
	case err == nil:
	case errors.Is(err, delegationstore.ErrNoSignInAssertion):
		h.refusal(ctx, user, EntraProvider, "consent.signin_missing")
		writeError(w, http.StatusConflict, "no new sign-in for this browser session; start consent again")
		return
	case errors.Is(err, delegationstore.ErrBindingNotActive):
		h.refusal(ctx, user, EntraProvider, "consent.binding_not_usable")
		writeError(w, http.StatusConflict, "the agent binding is not active in the project")
		return
	default:
		h.logger.ErrorContext(ctx, "credential consent: create the delegation", "error", err)
		writeError(w, http.StatusInternalServerError, "credential consent: internal error")
		return
	}
	outcome, err := e.resumer.Resume(ctx, agentexecutionapp.CredentialAuthorizationResume{
		AuthorizationReference: requirement.Reference, TenantProjectID: saved.projectID, ActorUserID: userID,
		DelegationID: delegation.ID, TriggerOrigin: executiondomain.TriggerOriginManual,
	})
	switch {
	case err == nil:
	case errors.Is(err, delegationstore.ErrRequirementResolved):
		writeError(w, http.StatusConflict, "the authorization reference is already resolved")
		return
	case errors.Is(err, delegationstore.ErrInvalid):
		h.refusal(ctx, user, EntraProvider, "consent.identity_mismatch")
		writeError(w, http.StatusConflict, "the new sign-in is not the user or the agent binding of the paused run")
		return
	default:
		h.logger.ErrorContext(ctx, "credential consent: resume the paused run", "error", err)
		writeError(w, http.StatusInternalServerError, "credential consent: internal error")
		return
	}
	writeJSON(w, http.StatusOK, callbackView{
		Status: "resumed", Provider: EntraProvider, ProjectID: saved.projectID, AgentBindingID: paused.AgentBindingID,
		ConversationUUID: requirement.ConversationUUID, ExecutionID: outcome.ExecutionID,
	})
}

// permitted reports whether the caller holds the governed use permission
// in the project.
func permitted(ctx context.Context, resolver auth.PermissionResolver, user auth.User, mode string, projectID int64) bool {
	if resolver == nil {
		return false
	}
	resolution, err := resolver.ResolvePermissions(ctx, user, mode, strconv.FormatInt(projectID, 10))
	if err != nil {
		return false
	}
	for _, p := range resolution.Permissions {
		if p == credentialgrant.PermissionUse {
			return true
		}
	}
	return false
}
