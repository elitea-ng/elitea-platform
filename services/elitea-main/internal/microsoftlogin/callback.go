package microsoftlogin

import (
	"context"
	"errors"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/microsoftauth"
)

// Error codes the callback reports to the client (in the postMessage, the loopback
// query or the custom-scheme URL). Each is a stable word; none carries a message
// from Microsoft.
const (
	// ErrDenied: the person declined, or Entra refused to sign them in.
	ErrDenied = "denied"
	// ErrProvider: Entra answered the callback with another error.
	ErrProvider = "provider_error"
	// ErrExchange: Entra refused the code (expired, already used, a client secret it
	// does not accept).
	ErrExchange = "exchange_failed"
	// ErrIdentity: no usable identity came back.
	ErrIdentity = "identity_missing"
	// ErrTenant: the signed-in person is not in the tenant the app belongs to.
	ErrTenant = "tenant_mismatch"
	// ErrPermission: the token is good and lacks the permission the toolkit needs.
	ErrPermission = "permission_missing"
	// ErrRejected: Entra rejected the token on first use.
	ErrRejected = "token_rejected"
	// ErrInternal: nothing the person can act on.
	ErrInternal = "internal_error"
)

// CallbackParams is the query of Entra's redirect.
type CallbackParams struct {
	State string
	Code  string
	// Error is the callback's `error` parameter; non-empty means the sign-in failed
	// at Entra.
	Error string
}

// Outcome is what the callback page tells the client. A known state names the client
// kind and where to return; an unknown one cannot (Known is false), and the page is a
// plain refusal.
type Outcome struct {
	Known bool
	// State is the state value the client started with, echoed so it can match it.
	State        string
	ClientKind   string
	ReturnPort   int
	ReturnScheme string
	// Handoff is the one-time code, present only on success.
	Handoff string
	// ErrorCode is present only on failure.
	ErrorCode string
}

// Callback finishes the sign-in Entra just redirected back from. It consumes the
// state, exchanges the code with the client secret and the PKCE verifier, reads the
// identity and parks the result for the starter. It attaches nothing to anyone: the
// caller's browser may be a victim's, or a system browser with no session.
func (s *Service) Callback(ctx context.Context, p CallbackParams) Outcome {
	if p.State == "" || len(p.State) > 128 {
		return Outcome{}
	}
	now := s.now()
	digest := digestOf("state", p.State)
	row, err := s.consumeState(ctx, digest, now)
	if err != nil {
		if !errors.Is(err, ErrStateInvalid) {
			s.logger.ErrorContext(ctx, "microsoft login: the sign-in state was not read")
		}
		return Outcome{}
	}
	if !now.Before(row.expiresAt) || row.redirectURI != s.redirectURI {
		return Outcome{}
	}
	out := Outcome{
		Known: true, State: p.State, ClientKind: row.b.clientKind, ReturnPort: row.port, ReturnScheme: row.scheme,
	}
	verifier, err := s.sealer.open(row.ciphertext, stateAAD{
		digest: digest, redirectURI: row.redirectURI, expiresAt: row.expiresAt, binding: row.b,
	})
	if err != nil {
		out.ErrorCode = ErrInternal
		return s.refused(ctx, row, out)
	}
	if p.Error != "" {
		out.ErrorCode = ErrProvider
		if p.Error == "access_denied" {
			out.ErrorCode = ErrDenied
		}
		return s.refused(ctx, row, out)
	}
	if p.Code == "" || len(p.Code) > 4096 {
		out.ErrorCode = ErrExchange
		return s.refused(ctx, row, out)
	}
	handoff, code := s.finish(ctx, row, verifier, p.Code, now)
	if code != "" {
		out.ErrorCode = code
		return s.refused(ctx, row, out)
	}
	out.Handoff = handoff
	s.logger.InfoContext(ctx, "microsoft login: handoff parked",
		"user_id", row.b.userID, "project_id", row.b.projectID, "type", row.b.credType, "client", row.b.clientKind)
	return out
}

func (s *Service) refused(ctx context.Context, row stateRow, out Outcome) Outcome {
	s.logger.InfoContext(ctx, "microsoft login: refused",
		"user_id", row.b.userID, "project_id", row.b.projectID, "type", row.b.credType,
		"client", row.b.clientKind, "reason", out.ErrorCode)
	return out
}

// finish runs the part of the callback that talks to Microsoft. It returns the
// handoff code, or the error code to report.
func (s *Service) finish(ctx context.Context, row stateRow, verifier, code string, now time.Time) (string, string) {
	credential, err := s.credentials.Load(ctx, row.b.projectID, row.b.credentialID)
	if err != nil {
		return "", s.credentialFailure(ctx, err)
	}
	// The same rules as the start, applied again: an app disabled, a credential moved
	// to another client, Custom app switched off or the authority changed since the
	// person clicked Login ends the sign-in instead of finishing it with another
	// client than the one they were sent to.
	c, err := s.resolveClient(ctx, row.b.projectID, credential)
	if err != nil {
		return "", s.credentialFailure(ctx, err)
	}
	if credential.Type != row.b.credType || c.appRef != row.b.appRef || fingerprint(c.clientID) != row.b.fingerprint ||
		c.authority != row.b.authority || strings.Join(c.scopes, " ") != row.b.scope {
		return "", CodeCredentialChanged
	}
	granted, err := s.exchange(ctx, c, code, verifier, now)
	if err != nil {
		var refusal *exchangeError
		if errors.As(err, &refusal) {
			s.logger.WarnContext(ctx, "microsoft login: the code was refused",
				"type", row.b.credType, "entra_error", refusal.code, "status", refusal.status, "correlation_id", refusal.correlationID)
		} else {
			s.logger.WarnContext(ctx, "microsoft login: the token endpoint was not reached", "type", row.b.credType)
		}
		return "", ErrExchange
	}
	who, err := readIdentity(granted.idToken, c.clientID)
	if err != nil {
		return "", ErrIdentity
	}
	if !tenantMatches(c.authority, who.tid) {
		return "", ErrTenant
	}
	if code := s.probe(ctx, c, granted.access); code != "" {
		return "", code
	}
	handoff, err := s.token(32)
	if err != nil {
		return "", ErrInternal
	}
	handoffDigest := digestOf("handoff", handoff)
	expires := now.Add(HandoffTTL).UTC().Truncate(time.Microsecond)
	parked := handoffRow{
		stateDigest: row.digest, userID: row.b.userID, projectID: row.b.projectID, credentialID: row.b.credentialID,
		credType: row.b.credType, appRef: row.b.appRef, clientKind: row.b.clientKind, challenge: row.b.challenge,
		fingerprint: row.b.fingerprint, authority: row.b.authority, scope: row.b.scope,
		tid: who.tid, oid: who.oid, accountName: who.name,
		accessExpiresAt: granted.expiresAt.UTC().Truncate(time.Microsecond), expiresAt: expires,
	}
	parked.ciphertext = s.sealer.seal(granted.access, handoffAAD{
		handoffDigest: handoffDigest, stateDigest: row.digest, tid: who.tid, oid: who.oid,
		expiresAt: expires, binding: parked.binding(),
	})
	if err := s.insertHandoff(ctx, handoffDigest, parked, now); err != nil {
		s.logger.ErrorContext(ctx, "microsoft login: the handoff was not parked")
		return "", ErrInternal
	}
	return handoff, ""
}

// credentialFailure maps a failed credential read or client resolution to the code
// the client is told. A refusal keeps its own stable code (app_disabled,
// custom_apps_disabled, ...).
func (s *Service) credentialFailure(ctx context.Context, err error) string {
	var refusal *Refusal
	if errors.As(err, &refusal) {
		return refusal.Code
	}
	if errors.Is(err, ErrCredentialNotFound) {
		return CodeCredentialNotFound
	}
	s.logger.ErrorContext(ctx, "microsoft login: the credential was not read")
	return ErrInternal
}

// probe reads the identity with the token, as the connection check does, so a token
// that cannot do what the toolkit needs is reported at sign-in and not at the first
// tool call. A read that proves nothing (throttling, a server error) does not fail
// the sign-in.
func (s *Service) probe(ctx context.Context, c client, token string) string {
	if c.descriptor.Probe() == microsoftauth.ProbeNone {
		return ""
	}
	ctx, cancel := context.WithTimeout(ctx, exchangeTimeout)
	defer cancel()
	request, err := c.descriptor.ProbeRequest(ctx, c.data, token)
	if err != nil {
		return ErrInternal
	}
	if request == nil {
		return ""
	}
	response, err := s.do(request)
	if err != nil {
		s.logger.WarnContext(ctx, "microsoft login: the identity read failed", "type", c.descriptor.Type())
		return ""
	}
	_ = response.Body.Close()
	switch microsoftauth.ClassifyProbe(response.StatusCode) {
	case microsoftauth.ProbeUnauthorized:
		return ErrRejected
	case microsoftauth.ProbeForbidden:
		return ErrPermission
	}
	return ""
}

// ReturnURL is where the system browser is sent on desktop and mobile: the loopback
// listener or the app's custom scheme, with the handoff code (or the error code) and
// the state. A code, token or secret is never in this URL: the handoff code is
// useless without the verifier only the starter holds.
func (o Outcome) ReturnURL() string {
	query := "state=" + url.QueryEscape(o.State)
	if o.ErrorCode != "" {
		query += "&error=" + url.QueryEscape(o.ErrorCode)
	} else {
		query = "handoff=" + url.QueryEscape(o.Handoff) + "&" + query
	}
	switch o.ClientKind {
	case ClientDesktop:
		return "http://127.0.0.1:" + strconv.Itoa(o.ReturnPort) + "/oauth/microsoft?" + query
	case ClientMobile:
		return o.ReturnScheme + "://oauth/microsoft?" + query
	}
	return ""
}
