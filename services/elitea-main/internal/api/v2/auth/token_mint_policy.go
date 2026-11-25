package auth

// The credential-kind rule for minting a personal access token (issue 2559,
// ADR-0059 decision 7 and design review R3B-2).
//
// # The defect it closes
//
// TokenCreate took the owner from the principal and never asked WHICH
// credential had authenticated the request. Anything that authenticates as a
// user could therefore mint a personal access token: another personal access
// token, a governed run's run token that reached the route, or a desktop or
// mobile native session, stolen or not. A personal access token carries no
// scopes, so it acts with its owner's full permission set, administration
// included. A short-lived or narrow credential became a long-lived full-power
// one in one request.
//
// # The rule
//
//   - A BROWSER session mints: the server-side `elitea_session` cookie, the
//     Form plane's session cookie and the legacy signed cookie. Each one is an
//     interactive sign-in, and each one already acts with the owner's full
//     set, so the minted token is no wider than the caller.
//   - A NATIVE device session (ADR-0025, the desktop and mobile apps) mints
//     only for an owner who holds no administration permission. The desktop
//     app shows the web Personal Tokens page and mints from it today, so the
//     path stays for everyone else (CodeNativeAdmin). A stolen desktop refresh
//     token is then never a way to an administrator credential.
//   - A personal access token, a run token and every other bearer or API-key
//     credential is refused (CodeBrowserSessionRequired).
//   - The minted token is never WIDER than the calling credential
//     (CodeWiderThanCaller). A caller whose own credential is bound to a
//     project mints a token bound to that same project only, never an unbound
//     one and never one bound elsewhere.
//
// # It fails closed
//
// A principal whose kind cannot be established — no authentication
// provenance, a source no middleware produces for this route, a session
// principal that carries a token id, a native bearer whose principal carries
// no native client — is a server-side invariant break. It is logged at error
// level and refused with CodeCallerUnresolved, never told to "use a browser".
// So is a native caller whose administration set the resolver refuses to
// answer: only an answer of "none held" lets a native session mint.
//
// Every refusal is a 403 with a stable `code` in the nested envelope this route
// already uses for its binding failures, and an annotation on the audit row the
// audit middleware writes for every refused /api/v2/auth/ request: action
// TokenMintRefusedAuditAction, entity type `token`, entity name the refusal
// code. The code alone names the credential kind that was refused (see each
// constant). The shape follows internal/adminaccess (#2555): stable codes as
// constants, one writer that annotates, logs and answers.
//
// The rule guards the one USER-initiated mint, POST /auth/token/.
// CallbackTokenMinter (callback_token.go) is the only other caller of
// tokenRepository.Create; it is server-initiated, always short-lived and always
// bound to one project, and token_mint_policy_test.go pins that no third caller
// appears.

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/audit"
	identity "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
)

// The refusal codes. Clients key their message on these, so they are stable.
const (
	// CodeBrowserSessionRequired: a personal access token, a run token or
	// another bearer or API-key credential that is not a native session.
	CodeBrowserSessionRequired = "token_mint_browser_session_required"
	// CodeNativeAdmin: a native device session, for an owner who holds
	// administration permissions. Such an owner mints in a browser.
	CodeNativeAdmin = "token_mint_native_admin"
	// CodeWiderThanCaller: a credential bound to one project asked for an
	// unbound token or one bound elsewhere. The body carries `project_id`,
	// the caller's own binding.
	CodeWiderThanCaller = "token_mint_wider_than_caller"
	// CodeCallerUnresolved: the server could not establish the calling
	// credential's kind, or could not read a native caller's administration
	// set. The rule fails closed.
	CodeCallerUnresolved = "token_mint_caller_unresolved"
	// CodeMintReservedAccount: the caller is the reserved break-glass account
	// (ADR-0059 R2.6b), which never creates a token.
	CodeMintReservedAccount = "token_mint_reserved_account"
)

// TokenMintRefusedAuditAction is the action the audit row of a refused mint
// carries.
const TokenMintRefusedAuditAction = "token.mint_refused"

// mintFaultAuditName is the audit entity name of a mint that failed because
// the permission store did not answer (a 500, not a refusal).
const mintFaultAuditName = "token_mint_permissions_unavailable"

// nativeAccessTokenPrefix is the prefix of a native access token
// (nativeauth.PrefixAccessToken), restated so this package does not import
// the authorization server. It is read only to name an invariant break; it
// never admits a caller.
const nativeAccessTokenPrefix = "elnat_"

// mintCredential is the kind of credential that authenticated a mint request.
type mintCredential string

const (
	mintCredentialBrowserSession mintCredential = "browser_session"
	mintCredentialNativeSession  mintCredential = "native_session"
	// mintCredentialAccessToken covers a personal access token, a run token
	// (its principal is the actor's token row, indistinguishable here and
	// refused alike) and any other bearer or API-key credential.
	mintCredentialAccessToken mintCredential = "access_token"
	// mintCredentialUnresolved is a principal whose kind contradicts its own
	// fields or has no provenance at all.
	mintCredentialUnresolved mintCredential = "unresolved"
)

// classifyMintCredential names the credential behind the request from facts
// only the authentication middleware sets: the authentication source (an
// unexported context key no header can forge) and NativeClientID (set only by
// the native access-token validator, from the device session row, and never
// serialised).
func classifyMintCredential(r *http.Request, user identity.User) mintCredential {
	source, ok := identity.AuthenticationSourceFromContext(r.Context())
	if !ok {
		logMintInvariant(r, "the principal carries no authentication provenance")
		return mintCredentialUnresolved
	}
	switch source {
	case identity.AuthenticationSourceSession:
		// Every session reader builds a principal with no token id and no
		// native client (browsersession.Session.User, the Form session's
		// coherentSessionPrincipal, verifySessionCookie).
		if user.TokenID != "" || user.NativeClientID != "" {
			logMintInvariant(r, "a session principal carries a token id or a native client")
			return mintCredentialUnresolved
		}
		return mintCredentialBrowserSession
	case identity.AuthenticationSourceToken, identity.AuthenticationSourceAPIKey:
		if user.NativeClientID != "" {
			return mintCredentialNativeSession
		}
		if presentsNativeAccessToken(r) {
			// authsvc.NewNativeAwareValidator sends every `elnat_` bearer to
			// the native validator, which always sets NativeClientID. A
			// native bearer without one came another way.
			logMintInvariant(r, "a native access token produced a principal without a native client")
			return mintCredentialUnresolved
		}
		return mintCredentialAccessToken
	default:
		// AuthenticationSourceDevelopment has no producer since ADR-0017
		// removed AUTH_DEV_MODE; a future source must be added here on
		// purpose.
		logMintInvariant(r, "the authentication source is not one this route serves")
		return mintCredentialUnresolved
	}
}

func presentsNativeAccessToken(r *http.Request) bool {
	if strings.HasPrefix(r.Header.Get("X-API-Key"), nativeAccessTokenPrefix) {
		return true
	}
	return strings.HasPrefix(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "), nativeAccessTokenPrefix)
}

func logMintInvariant(r *http.Request, what string) {
	slog.ErrorContext(r.Context(), "personal token mint: the calling credential could not be classified",
		"reason", what, "method", r.Method, "path", r.URL.Path)
}

// tokenMintRefusal is one refused mint.
type tokenMintRefusal struct {
	Code           string
	Message        string
	CredentialKind mintCredential
	// BoundProjectID is the caller's own binding, on a CodeWiderThanCaller
	// refusal only.
	BoundProjectID *int64
}

func (r *tokenMintRefusal) Error() string { return r.Code + ": " + r.Message }

const callerUnresolvedMessage = "the server could not check the sign-in that is calling, so it did not create the token; " +
	"create it in the web app in a browser"

// authorizeMintCredential applies the credential-kind half of the rule, before
// the request body is read. ownerHoldsAdministration is called only for a
// native session.
func authorizeMintCredential(
	kind mintCredential,
	ownerHoldsAdministration func() (bool, error),
) (*tokenMintRefusal, error) {
	switch kind {
	case mintCredentialBrowserSession:
		return nil, nil
	case mintCredentialNativeSession:
		holds, err := ownerHoldsAdministration()
		if errors.Is(err, errMintAdministrationUnknown) {
			return &tokenMintRefusal{Code: CodeCallerUnresolved, Message: callerUnresolvedMessage, CredentialKind: kind}, nil
		}
		if err != nil {
			return nil, err
		}
		if holds {
			return &tokenMintRefusal{
				Code:           CodeNativeAdmin,
				Message:        "your account has administration permissions, so this app cannot create a personal token for it; create it in the web app in a browser",
				CredentialKind: kind,
			}, nil
		}
		return nil, nil
	case mintCredentialAccessToken:
		return &tokenMintRefusal{
			Code:           CodeBrowserSessionRequired,
			Message:        "a personal token can be created only from a browser sign-in, not with another token",
			CredentialKind: kind,
		}, nil
	default:
		return &tokenMintRefusal{Code: CodeCallerUnresolved, Message: callerUnresolvedMessage, CredentialKind: kind}, nil
	}
}

// authorizeMintScope applies the "never wider than the caller" half, once the
// requested binding is known. A caller whose credential is bound to a project
// mints only a token bound to that project.
func authorizeMintScope(kind mintCredential, user identity.User, requested *int64) *tokenMintRefusal {
	if user.TokenProjectID == nil {
		return nil
	}
	if requested != nil && *requested == *user.TokenProjectID {
		return nil
	}
	bound := *user.TokenProjectID
	return &tokenMintRefusal{
		Code:           CodeWiderThanCaller,
		Message:        "your sign-in is limited to one project, so the new token must be bound to that same project",
		CredentialKind: kind,
		BoundProjectID: &bound,
	}
}

// errMintAdministrationUnknown reports that the owner's administration set
// was not answered: no resolver is composed, or the resolver REFUSED the
// principal (ErrPermissionDenied: a suspended owner, a missing token row, a
// mismatched owner). Neither proves "none held", so the mint is refused.
var errMintAdministrationUnknown = errors.New("the owner's administration permissions are unknown")

// writeTokenMintRefusal answers 403 with the refusal and gives the request's
// audit row its domain action. The audit middleware records every refused
// /api/v2/auth/ request; without the annotation the row says only
// "POST /api/v2/auth/token/" with status 403.
func writeTokenMintRefusal(w http.ResponseWriter, r *http.Request, refusal *tokenMintRefusal) {
	writeTokenRefusal(w, r, TokenMintRefusedAuditAction, refusal)
}

// writeTokenRefusal answers 403 with refusal and annotates the request's
// audit row with action: the one writer of every token refusal, the mint
// rule's and the administration scope rule's (R1.3a).
func writeTokenRefusal(w http.ResponseWriter, r *http.Request, action string, refusal *tokenMintRefusal) {
	audit.Annotate(r.Context(), audit.Annotation{
		Action:     action,
		EntityType: "token",
		EntityName: refusal.Code,
	})
	slog.WarnContext(r.Context(), "personal token request refused",
		"action", action, "code", refusal.Code, "credential_kind", string(refusal.CredentialKind),
		"method", r.Method, "path", r.URL.Path)
	writeTokenMintJSON(w, refusal)
}

// writeTokenMintFault answers a mint whose permission check could not run:
// the store did not answer. It is a 500, not a refusal, but it is logged with
// its cause and annotated so the audit row says why no token was made.
func writeTokenMintFault(w http.ResponseWriter, r *http.Request, kind mintCredential, err error) {
	writeTokenFault(w, r, TokenMintRefusedAuditAction, kind, err)
}

// writeTokenFault is writeTokenMintFault for any token request: action is the
// audit action of the request (the mint's, or the scope change's).
func writeTokenFault(w http.ResponseWriter, r *http.Request, action string, kind mintCredential, err error) {
	audit.Annotate(r.Context(), audit.Annotation{
		Action:     action,
		EntityType: "token",
		EntityName: mintFaultAuditName,
	})
	slog.ErrorContext(r.Context(), "personal token request: the caller's administration permissions could not be read",
		"action", action, "credential_kind", string(kind), "err", err, "method", r.Method, "path", r.URL.Path)
	writeTokenJSONError(w, http.StatusInternalServerError, "server_error",
		mintFaultAuditName, "failed to resolve caller permissions")
}

// tokenMintJSONError is the nested envelope of a refusal, with the caller's
// own binding when the refusal is about it.
type tokenMintJSONError struct {
	Error tokenMintJSONErrorFields `json:"error"`
}

type tokenMintJSONErrorFields struct {
	tokenJSONErrorFields
	ProjectID *int64 `json:"project_id,omitempty"`
}

func writeTokenMintJSON(w http.ResponseWriter, refusal *tokenMintRefusal) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusForbidden)
	_ = json.NewEncoder(w).Encode(tokenMintJSONError{Error: tokenMintJSONErrorFields{
		tokenJSONErrorFields: tokenJSONErrorFields{
			Message: refusal.Message, Type: "permission_error", Code: refusal.Code,
		},
		ProjectID: refusal.BoundProjectID,
	}})
}

// ownerHoldsAdministration reports whether the principal's owner holds any
// administration-mode permission. Only an ANSWER counts: an empty set is
// "none held". A refusal or an absent resolver is errMintAdministrationUnknown;
// any other error is a dependency fault and is returned as it is.
func (h *Handler) ownerHoldsAdministration(r *http.Request, user identity.User) (bool, error) {
	if h.permissions == nil {
		return false, errMintAdministrationUnknown
	}
	resolution, err := h.permissions.ResolvePermissions(
		r.Context(), user, identity.PermissionModeAdministration, "")
	if errors.Is(err, identity.ErrPermissionDenied) {
		return false, errMintAdministrationUnknown
	}
	if err != nil {
		return false, err
	}
	return len(resolution.Permissions) > 0, nil
}
