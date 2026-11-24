package browserauth

// The Form password re-entry (ADR-0059 decision 5, path 1 step 4; story
// R1.10b, issue 2594).
//
// Three rules of ADR-0059 need "this person authenticated within the last 15
// minutes" (apimw.RecentAuthentication). A single sign-on plane asks its
// identity provider for that (OIDC `max_age`, SAML ForceAuthn). The Form plane
// has no identity provider: the person proves it by typing the password of
// the SAME Form login again. A successful re-entry moves the session's
// authentication time and rotates the session identifier; a wrong password
// leaves the session exactly as it was. Both are counted by the existing
// Form credential limiter (BrowserAttemptFormCredential), which the HTTP
// boundary applies before Reauthenticate verifies anything.
//
// The password is verified by the same FormAssertionVerifier the sign-in uses:
// there is no second digest path.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth/browserflow"
	sessionstate "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth/session"
)

// ErrReauthenticationStale is a re-entry for a prompt that no longer matches
// the session: rendered for another login, or a session that was rotated (a
// second submission, another tab) between the read and the write. It is not a
// wrong password, and the prompt is shown again without saying so.
var ErrReauthenticationStale = errors.New("browser re-authentication prompt is stale")

// ReauthenticationSubject is the authenticated Form session a password
// prompt is for.
type ReauthenticationSubject struct {
	// Login is the Form login the session signed in with. Only its password
	// re-authenticates the session.
	Login         string
	Authorization Authorization
}

// ReauthenticateResult is the rotated session.
type ReauthenticateResult struct {
	SessionID string
}

// ReauthenticationSubject answers the Form login behind an authenticated,
// unexpired Form session whose principal is still active. Any other session
// (none, expired, signed in through another provider of this graph) is
// ErrUnauthenticated: it has no password to enter again.
func (s *Service) ReauthenticationSubject(ctx context.Context, sessionID string) (ReauthenticationSubject, error) {
	authorization, err := s.Authorize(ctx, sessionID)
	if err != nil {
		return ReauthenticationSubject{}, err
	}
	if authorization.Provider != FormProviderName {
		return ReauthenticationSubject{}, ErrUnauthenticated
	}
	login, ok := formSessionLogin(authorization.ProviderAttributes)
	if !ok {
		return ReauthenticationSubject{}, ErrUnauthenticated
	}
	return ReauthenticationSubject{Login: login, Authorization: authorization}, nil
}

// Reauthenticate verifies a password re-entry for the Form session sessionID
// and, when it matches the session's own login, rotates the session with a
// new authentication time. verifier is FormProvider.NewVerifier(login,
// password) for the login ReauthenticationSubject answered.
//
// A wrong password, a password of ANOTHER Form login, and a session that is no
// longer a live Form session are ErrUnauthenticated; a prompt that no longer
// matches the session is ErrReauthenticationStale. The session is left
// untouched: neither its identifier nor its authentication time moves. The
// state written is the session's own, so a re-entry cannot change who the
// session is, which provider attributes it carries, or when its
// authentication expires.
func (s *Service) Reauthenticate(
	ctx context.Context,
	sessionID string,
	login string,
	verifier AssertionVerifier,
) (ReauthenticateResult, error) {
	if err := ctx.Err(); err != nil {
		return ReauthenticateResult{}, err
	}
	if verifier == nil || login == "" {
		return ReauthenticateResult{}, ErrInvalidRequest
	}
	subject, err := s.ReauthenticationSubject(ctx, sessionID)
	if err != nil {
		return ReauthenticateResult{}, err
	}
	if subject.Login != login {
		// The prompt was rendered for another session than the one this
		// cookie now names (a sign-in in another tab).
		return ReauthenticateResult{}, ErrReauthenticationStale
	}
	current, err := s.sessions.Read(ctx, sessionID)
	if err != nil {
		if errors.Is(err, sessionstate.ErrNotFound) {
			return ReauthenticateResult{}, ErrUnauthenticated
		}
		return ReauthenticateResult{}, sanitizedError(ctx, ErrDependencyUnavailable, "read re-authenticating browser session", err)
	}
	if !isAuthenticatedSession(current) || *current.Provider != FormProviderName {
		return ReauthenticateResult{}, ErrUnauthenticated
	}

	assertion, err := verifier.Verify(ctx, browserflow.VerificationContext{
		Provider:             FormProviderName,
		OriginatingSessionID: sessionID,
	})
	if err != nil {
		if errors.Is(err, ErrAssertionVerifierUnavailable) {
			return ReauthenticateResult{}, sanitizedError(ctx, ErrDependencyUnavailable, "verify password re-entry", err)
		}
		return ReauthenticateResult{}, sanitizedError(ctx, ErrUnauthenticated, "verify password re-entry", err)
	}
	if assertion.Provider != FormProviderName || assertion.ProviderReference != subject.Login {
		return ReauthenticateResult{}, ErrUnauthenticated
	}

	now := s.now().UTC()
	if current.Expiration != nil && !now.Before(*current.Expiration) {
		return ReauthenticateResult{}, ErrAuthenticationExpired
	}
	replacement := current
	replacement.ProviderAttributes = append(json.RawMessage(nil), current.ProviderAttributes...)
	replacement.Expiration = cloneTime(current.Expiration)
	replacement.AuthenticatedAt = &now
	newSessionID, err := s.sessions.RotateAndReplace(ctx, sessionID, replacement)
	if err != nil {
		if errors.Is(err, sessionstate.ErrNotFound) {
			// Signed out, or rotated by a concurrent re-entry, between the
			// read and the write. The password was right.
			return ReauthenticateResult{}, ErrReauthenticationStale
		}
		return ReauthenticateResult{}, sanitizedError(ctx, ErrAuthenticationFinalization, "rotate re-authenticated browser session", err)
	}
	if browserflow.ValidateOpaqueID(newSessionID) != nil || newSessionID == sessionID {
		_ = s.sessions.Delete(ctx, newSessionID)
		return ReauthenticateResult{}, fmt.Errorf("%w: session rotation returned an invalid ID", ErrAuthenticationFinalization)
	}
	return ReauthenticateResult{SessionID: newSessionID}, nil
}

// formSessionLogin reads the Form login (`nameid`) from a Form session's
// provider attributes, which FormAssertionVerifier.Verify wrote.
func formSessionLogin(attributes json.RawMessage) (string, bool) {
	var decoded struct {
		NameID string `json:"nameid"`
	}
	if err := json.Unmarshal(attributes, &decoded); err != nil || !validFormLogin(decoded.NameID) {
		return "", false
	}
	return decoded.NameID, true
}
