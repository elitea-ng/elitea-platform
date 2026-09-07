package middleware

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth/browsersession"
)

// TokenValidator validates a token string and returns the authenticated user.
type TokenValidator interface {
	ValidateToken(ctx context.Context, token string) (auth.User, error)
}

// PrincipalValidator checks mutable account state after credentials have been
// validated, including signed sessions.
type PrincipalValidator interface {
	ValidatePrincipal(ctx context.Context, principal auth.User) (auth.User, error)
}

// ForwardedIdentityPeerVerifier proves that an X-Auth-* request arrived over
// the isolated, header-stripping ingress boundary. Reloading an active user is
// not proof that the caller was entitled to assert that user ID.
type ForwardedIdentityPeerVerifier interface {
	VerifyForwardedIdentityPeer(*http.Request) error
}

// BrowserSessionValidator resolves the SERVER-SIDE browser session a cookie
// names, and refuses one that is revoked, expired or idle.
//
// It is declared at the consumer because this is the only thing the middleware
// needs from *browsersession.Manager: one read that either yields a principal
// or names why it did not. A nil value keeps the pre-0117 behaviour, where the
// cookie was the whole credential.
type BrowserSessionValidator interface {
	Validate(ctx context.Context, cookieValue string) (browsersession.Session, error)
}

type AuthConfig struct {
	// Validator reads a credential back and yields the principal it names.
	// It is the ONLY token validator now: #383 deleted the pylon Redis-RPC
	// client that used to fill in when this field was nil, so a nil Validator
	// admits no bearer or API-key credential at all.
	Validator                 TokenValidator
	PrincipalValidator        PrincipalValidator
	ForwardedIdentityVerifier ForwardedIdentityPeerVerifier
	SessionSecret             string // HMAC key for session cookies
	// SessionStore validates the server-side session an `elitea_session`
	// cookie names (migrations/shared/0117). Nil means this deployment issues
	// only the legacy signed cookie, which is what every deployment did before
	// this field existed.
	SessionStore BrowserSessionValidator
	// RejectLegacySessionCookies refuses the pre-0117 signed cookie outright.
	//
	// The default is FALSE, and it has to be: an upgrade that rejected them
	// would sign out every browser holding an unexpired one at the moment the
	// new binary starts. An operator who would rather force one re-login than
	// keep a stateless credential alive for a day sets
	// ELITEA_SESSION_REJECT_LEGACY_COOKIES=true.
	RejectLegacySessionCookies bool
}

// Auth authenticates every request against exactly four credential sources:
// forwarded identity, API key, bearer token, and session cookie. There is no
// fifth source and no configuration that yields a principal without a
// credential.
//
// In particular there is no environment-variable bypass. The former
// AUTH_DEV_MODE flag injected an admin principal impersonating database user 1
// for any request — including one carrying an Authorization header, since the
// bypass ran before token validation — and skipped PrincipalValidator
// entirely. Its two documented guards were both illusory: the "enforced at
// startup in main.go" mutual exclusion never existed, and the
// `cfg.SessionSecret == ""` conjunct was evaluated per AuthConfig, leaving it
// open for the 16 of 21 configs in main.go that never set SessionSecret. See
// ADR-0017. Development and CI authenticate through the mock OIDC provider;
// tests inject a stub TokenValidator via RouterConfig.
//
// There is likewise no trusted-proxy header source. A second forwarded-identity
// path accepted X-Auth-Type/X-Auth-Id on one proof: RemoteAddr fell inside
// TrustedProxyCIDRs. That path called serveAuthenticated directly, so
// PrincipalValidator never ran on it.
//
// A deactivated user who kept access was the smaller failure. The headers were
// the whole credential. deploy/traefik/dynamic.yml removes no inbound X-Auth-*
// header. No composition root set the CIDR list, so the TRUSTED_PROXY_CIDRS
// environment variable was the only switch. An operator who set that variable
// to the ingress range gave every anonymous caller any user ID.
//
// validatePrincipal does not correct that failure. The validator confirms that
// the claimed user is active. It does not confirm that the caller may claim
// that user. ForwardedIdentityPeerVerifier draws that distinction. See #390.
// browserauth.TrustedProxyConfig is a different boundary. It reads the auth
// configuration file, and this change does not affect it.
func Auth(cfg AuthConfig) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if user, ok := tryTraefikHeaders(r, cfg.ForwardedIdentityVerifier); ok {
				// A forwarded token ID is not an owning user ID. The authoritative
				// principal check must cross-check both typed IDs and normalize the
				// compatibility ID before any downstream handler can use it as a
				// user foreign key.
				user, err := validatePrincipal(r.Context(), cfg, user)
				if err != nil {
					writePrincipalRefusal(w, r, sourceForwarded, err)
					return
				}
				serveAuthenticated(next, w, r, user, auth.AuthenticationSourceForwarded)
				return
			}

			// X-API-Key header (pylon compatibility)
			if apiKey := r.Header.Get("X-API-Key"); apiKey != "" {
				user, err := validateToken(r.Context(), cfg, apiKey)
				if err != nil {
					writeCredentialRefusal(w, r, sourceAPIKey, reasonTokenRejected)
					return
				}
				user, err = validatePrincipal(r.Context(), cfg, user)
				if err != nil {
					writePrincipalRefusal(w, r, sourceAPIKey, err)
					return
				}
				serveAuthenticated(next, w, r, user, auth.AuthenticationSourceAPIKey)
				return
			}

			authHeader := r.Header.Get("Authorization")
			if authHeader == "" {
				// Try session cookie (set by OIDC/form login)
				var refusal string
				cookie, cookieErr := r.Cookie("elitea_session")
				switch {
				case cookieErr != nil:
					// No header and no cookie of ours. `cookie_count` below
					// says whether the browser sent any cookie at all.
					refusal = reasonNoCredential
				case cfg.SessionStore != nil && browsersession.LooksServerSide(cookie.Value):
					// A server-side session identifier. The prefix decides
					// which reader runs, and the two formats cannot collide:
					// see browsersession.LooksServerSide.
					session, sessionErr := cfg.SessionStore.Validate(r.Context(), cookie.Value)
					if sessionErr == nil {
						user, validationErr := validatePrincipal(r.Context(), cfg, session.User())
						if validationErr != nil {
							writePrincipalRefusal(w, r, sourceSession, validationErr)
							return
						}
						serveAuthenticated(next, w, r, user, auth.AuthenticationSourceSession)
						return
					}
					reason, classified := serverSessionRefusal(sessionErr)
					if !classified {
						// The store did not ANSWER. Nothing about this session
						// was read, so this is a dependency fault and not a
						// statement that the caller is signed out. A 401 here
						// signs out every browser for as long as the database
						// is unreachable, which is the failure shape the
						// principal-validation path already refuses to repeat.
						logCredentialRefusal(r, sourceSession, reason)
						slog.ErrorContext(r.Context(),
							"the browser session store could not be read", "err", sessionErr)
						w.Header().Set("Retry-After", "5")
						writeJSONError(w, http.StatusServiceUnavailable,
							"server_error", "session_store_unavailable", "session store unavailable")
						return
					}
					refusal = reason
				case cfg.RejectLegacySessionCookies:
					// The operator chose to end the legacy window. The cookie
					// is a shape this deployment no longer accepts, not a
					// missing credential, so it gets its own reason.
					refusal = reasonLegacySessionRejected
				case cfg.SessionSecret == "":
					refusal = reasonSessionSecretAbsent
				default:
					user, cookieRefusal, ok := verifySessionCookie(cookie.Value, cfg.SessionSecret)
					if ok {
						user, validationErr := validatePrincipal(r.Context(), cfg, user)
						if validationErr != nil {
							writePrincipalRefusal(w, r, sourceSession, validationErr)
							return
						}
						serveAuthenticated(next, w, r, user, auth.AuthenticationSourceSession)
						return
					}
					refusal = cookieRefusal
				}
				writeCredentialRefusal(w, r, sourceSession, refusal)
				return
			}

			var token string
			if strings.HasPrefix(authHeader, "Bearer ") {
				token = strings.TrimPrefix(authHeader, "Bearer ")
			} else if strings.HasPrefix(authHeader, "Basic ") {
				decoded, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(authHeader, "Basic "))
				if err != nil {
					writeCredentialRefusal(w, r, sourceToken, reasonAuthorizationHeaderMalformed)
					return
				}
				parts := strings.SplitN(string(decoded), ":", 2)
				token = parts[0]
			} else {
				writeCredentialRefusal(w, r, sourceToken, reasonAuthorizationSchemeUnsupported)
				return
			}

			user, err := validateToken(r.Context(), cfg, token)
			if err != nil {
				writeCredentialRefusal(w, r, sourceToken, reasonTokenRejected)
				return
			}

			user, err = validatePrincipal(r.Context(), cfg, user)
			if err != nil {
				writePrincipalRefusal(w, r, sourceToken, err)
				return
			}
			serveAuthenticated(next, w, r, user, auth.AuthenticationSourceToken)
		})
	}
}

// jsonError is the OpenAI-shaped nested error envelope mandated by spec §2.5:
// {"error":{"message","type","code"}}.
type jsonError struct {
	Error jsonErrorFields `json:"error"`
}

type jsonErrorFields struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    string `json:"code,omitempty"`
}

// writeJSONError writes a spec §2.5 nested JSON error body with
// Content-Type: application/json, replacing the flat text/plain bodies
// http.Error produces. Used by both this file and project.go.
func writeJSONError(w http.ResponseWriter, status int, errType, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(jsonError{Error: jsonErrorFields{Message: message, Type: errType, Code: code}})
}

// ForwardedIdentity reads the principal an authenticating edge projected onto
// this request, or reports that there is none.
//
// Exported so the admin-UI HTML handler resolves the SAME identity this
// middleware does. That handler used to read the `elitea_session` cookie and
// nothing else, and the runtime deployment does not issue that cookie: the
// browser logs in through /forward-auth/login, which stores an opaque
// server-side session under `elitea_browser_auth` and projects the principal
// onto the upstream request as X-Auth-* (deploy/runtime/platform-edge-dynamic
// .yml `authResponseHeaders`). The handler therefore injected an empty
// permission list into every admin page load, and the SPA — which hides a nav
// item whose permission is absent — rendered a sidebar with no items at all.
//
// The verifier argument is not optional and a nil one yields no identity: the
// headers are the whole credential, so accepting them without proof that the
// request crossed the header-stripping ingress lets any caller pick a user ID
// (#390). Callers must still validate the principal (or resolve permissions,
// which reloads the user and refuses a suspended one) before acting on it.
func ForwardedIdentity(r *http.Request, verifier ForwardedIdentityPeerVerifier) (auth.User, bool) {
	return tryTraefikHeaders(r, verifier)
}

func tryTraefikHeaders(r *http.Request, verifier ForwardedIdentityPeerVerifier) (auth.User, bool) {
	authType, typePresent, typeValid := uniqueForwardedIdentityHeader(r.Header, "X-Auth-Type")
	authID, idPresent, idValid := uniqueForwardedIdentityHeader(r.Header, "X-Auth-ID")
	if !typePresent && !idPresent {
		return auth.User{}, false
	}
	if verifier == nil || !typePresent || !idPresent || !typeValid || !idValid ||
		verifier.VerifyForwardedIdentityPeer(r) != nil {
		return auth.User{}, false
	}
	// X-Auth-Reference is compatibility routing material and may be a browser
	// session bearer value. It is never an identity claim; the mandatory
	// PrincipalValidator reloads mutable email from PostgreSQL.

	if !strings.EqualFold(authType, "token") && !strings.EqualFold(authType, "user") {
		return auth.User{}, false
	}
	authType = strings.ToLower(authType)

	user := auth.User{
		ID:       authID,
		AuthType: authType,
	}
	if authType == "token" {
		userID, present, valid := uniqueForwardedIdentityHeader(r.Header, "X-Auth-User-ID")
		if !present || !valid {
			return auth.User{}, false
		}
		user.TokenID = authID
		user.UserID = userID
	} else {
		user.UserID = authID
	}
	return user, true
}

func uniqueForwardedIdentityHeader(headers http.Header, name string) (string, bool, bool) {
	var values []string
	for key, current := range headers {
		if strings.EqualFold(key, name) {
			values = append(values, current...)
		}
	}
	if len(values) == 0 {
		return "", false, true
	}
	if len(values) != 1 || values[0] == "" || values[0] != strings.TrimSpace(values[0]) ||
		strings.ContainsAny(values[0], "\x00\r\n") {
		return "", true, false
	}
	return values[0], true, true
}

// errPrincipalValidatorAbsent reports a deployment that composed no validator.
//
// It is a refusal, not a condition: every caller of validatePrincipal is on an
// authentication path, and a path that cannot validate a principal must not
// authenticate one.
var errPrincipalValidatorAbsent = errors.New("no principal validator is configured")

// validatePrincipal reloads the principal and refuses a suspended or deleted
// one. It FAILS CLOSED when no validator is composed.
//
// It used to return `(user, nil)` in that case, and that was safe only by
// accident: the forwarded path guarded nil separately a few lines before
// calling this, so the open default was unreachable *there*. The other three
// paths — X-API-Key, the `elitea_session` cookie, and Bearer — had no such
// guard and went straight through.
//
// The session path is the one that mattered. verifySessionCookie is an HMAC
// check with no database read, so on a deployment with a session secret and no
// validator, an unexpired cookie authenticated a user this service never
// looked up — including one since suspended or deleted. The token paths are
// less exposed because validateToken reads the store first, but "less exposed"
// is not a property worth relying on when the fix is one branch.
//
// One place decides for all four paths now, which is why the forwarded path's
// own pre-check is gone: two spellings of the same rule is how the rule ends
// up applied in one of them.
func validatePrincipal(ctx context.Context, cfg AuthConfig, user auth.User) (auth.User, error) {
	if cfg.PrincipalValidator == nil {
		return auth.User{}, errPrincipalValidatorAbsent
	}
	return cfg.PrincipalValidator.ValidatePrincipal(ctx, user)
}

func serveAuthenticated(next http.Handler, w http.ResponseWriter, r *http.Request, user auth.User, source auth.AuthenticationSource) {
	ctx := auth.ContextWithAuthenticatedUser(r.Context(), user, source)
	next.ServeHTTP(w, r.WithContext(ctx))
}

func writeInactivePrincipal(w http.ResponseWriter) {
	writeJSONError(w, http.StatusUnauthorized, "authentication_error", "unauthenticated", "authenticated principal is inactive")
}

// The credential source each refusal names. One 401 burst looks the same from
// the outside on all four paths, so the log has to say which one refused.
const (
	sourceForwarded = "forwarded_identity"
	sourceAPIKey    = "api_key"
	sourceSession   = "session_cookie"
	sourceToken     = "bearer_token"
)

// The reason a CREDENTIAL refusal names — the vocabulary of the 401 that is
// written before any principal is read.
//
// It exists because the "missing authorization header" body is written on
// four different findings and says the same thing about all of them (#537,
// #538). A browser holding a valid session got that body on one route in one
// run of three, and after the fact nobody could say whether the browser sent
// no cookie, sent one this deployment could not verify, or sent one that had
// expired: the branch wrote no log line at all. These reasons separate them.
const (
	// reasonNoCredential — no Authorization header, no X-API-Key, no
	// forwarded identity and no `elitea_session` cookie on the request.
	// `cookie_count` on the same line says whether the browser sent ANY
	// cookie, which is what tells "this client is not signed in" apart from
	// "this client's cookie jar lost exactly ours".
	reasonNoCredential = "no_credential"
	// reasonSessionSecretAbsent — the cookie arrived and this AuthConfig
	// carries no SessionSecret, so the cookie branch could not run. A
	// composition defect, and the class PR #819 removed by collapsing the
	// per-route literals into one apiGroupAuthConfig.
	reasonSessionSecretAbsent = "session_secret_not_configured"
	// The four ways verifySessionCookie refuses a cookie it did receive.
	reasonSessionMalformed    = "session_cookie_malformed"
	reasonSessionBadSignature = "session_cookie_signature_mismatch"
	reasonSessionExpired      = "session_cookie_expired"
	reasonSessionSubject      = "session_cookie_subject_invalid"
	// The refusals a SERVER-SIDE session names. They are separate from the
	// four above because they are answers about a ROW, not about a signature:
	// an operator reading "session_revoked" knows somebody signed out, and
	// reading "session_idle" knows a browser was left alone too long.
	reasonServerSessionUnknown = "session_unknown"
	reasonServerSessionRevoked = "session_revoked"
	reasonServerSessionExpired = "session_expired"
	reasonServerSessionIdle    = "session_idle"
	// reasonServerSessionUnreadable — the store did not answer. It is the one
	// reason in this list that is NOT a statement about the caller.
	reasonServerSessionUnreadable = "session_store_unavailable"
	// reasonLegacySessionRejected — the cookie is the pre-0117 signed form and
	// ELITEA_SESSION_REJECT_LEGACY_COOKIES closed that window.
	reasonLegacySessionRejected = "legacy_session_cookie_rejected"
	// reasonAuthorizationHeaderMalformed — a Basic header that is not base64.
	reasonAuthorizationHeaderMalformed = "authorization_header_malformed"
	// reasonAuthorizationSchemeUnsupported — an Authorization header that is
	// neither Bearer nor Basic.
	reasonAuthorizationSchemeUnsupported = "authorization_scheme_unsupported"
	// reasonTokenRejected — the validator refused the bearer token or the
	// API key. It does NOT distinguish an unknown token from a store that
	// could not answer; validateToken returns one error for both.
	reasonTokenRejected = "token_rejected"
)

// logCredentialRefusal writes the one line the credential branches had none
// of, and it is the whole of this change: no status, body or branch moves.
//
// It carries no principal identity and no credential material — not the
// cookie value, not the token, not the cookie NAMES. The caller is
// unauthenticated, so anything it supplied is attacker-controlled text in the
// operator's log. `cookie_count` is a count, which is the one fact about the
// request's cookies that is safe to keep and is also the discriminator #538
// needs.
func logCredentialRefusal(r *http.Request, source, reason string) {
	slog.WarnContext(r.Context(), "authentication refused the request",
		"source", source,
		"reason", reason,
		"method", r.Method,
		"path", r.URL.Path,
		"cookie_count", len(r.Cookies()),
		"has_cookie_header", r.Header.Get("Cookie") != "",
	)
}

// credentialRefusalAnswer maps one internal reason onto the 401 the CALLER
// reads: a code it can branch on, and a message a person can read.
//
// THE 401 USED TO SAY THE SAME THING ABOUT FOUR DIFFERENT FINDINGS.
// `{"code":"unauthenticated","message":"missing authorization header"}` was
// written when the browser sent nothing, when it sent a cookie signed by
// another deployment, when the cookie had expired, and when a server-side
// session had been revoked. #538 is what that cost: a journey holding a valid
// session was refused once in three runs, and the response was the only
// evidence that survived the run. The log line of #537 named the branch for an
// OPERATOR. This names it for the CLIENT, which is what a test harness, a
// browser console and an SDK actually see.
//
// THE STATUS DOES NOT MOVE. Every answer here is 401 `authentication_error`,
// and no branch of Auth changes. Only the code and the message become
// specific.
//
// WHAT IS NOT DISCLOSED. `session_secret_not_configured` is a statement about
// the SERVER, not about the caller's credential: it says this deployment
// cannot verify cookies at all. That is an operator's fact, so it stays in the
// log and the caller gets the generic answer. Every other reason here is a
// finding about the credential the caller itself supplied, which the caller
// already holds.
func credentialRefusalAnswer(source, reason string) (code, message string) {
	switch reason {
	case reasonNoCredential:
		// The message is unchanged, because for THIS reason it was always
		// true: no header and no cookie of ours arrived.
		return reasonNoCredential, "missing authorization header"
	case reasonSessionMalformed:
		return reasonSessionMalformed, "the session cookie is malformed"
	case reasonSessionBadSignature:
		return reasonSessionBadSignature, "the session cookie signature does not match"
	case reasonSessionExpired:
		return reasonSessionExpired, "the session cookie expired"
	case reasonSessionSubject:
		return reasonSessionSubject, "the session cookie names no usable subject"
	case reasonServerSessionUnknown:
		return reasonServerSessionUnknown, "the session is not known to this deployment"
	case reasonServerSessionRevoked:
		return reasonServerSessionRevoked, "the session was revoked"
	case reasonServerSessionExpired:
		return reasonServerSessionExpired, "the session expired"
	case reasonServerSessionIdle:
		return reasonServerSessionIdle, "the session was idle for too long"
	case reasonLegacySessionRejected:
		return reasonLegacySessionRejected, "the legacy session cookie is no longer accepted"
	case reasonAuthorizationHeaderMalformed:
		return reasonAuthorizationHeaderMalformed, "invalid basic auth encoding"
	case reasonAuthorizationSchemeUnsupported:
		return reasonAuthorizationSchemeUnsupported, "unsupported authorization scheme"
	case reasonTokenRejected:
		if source == sourceAPIKey {
			return reasonTokenRejected, "invalid api key"
		}
		return reasonTokenRejected, "token validation failed"
	default:
		// reasonSessionSecretAbsent and anything a later branch adds without
		// deciding what to disclose. The generic answer is the safe default.
		return "unauthenticated", "missing authorization header"
	}
}

// writeCredentialRefusal logs the refusal and answers it.
//
// One function, so a new refusal branch cannot log one reason and answer with
// another. That split is exactly how the four findings came to share one body.
func writeCredentialRefusal(w http.ResponseWriter, r *http.Request, source, reason string) {
	logCredentialRefusal(r, source, reason)
	code, message := credentialRefusalAnswer(source, reason)
	writeJSONError(w, http.StatusUnauthorized, "authentication_error", code, message)
}

// The reason each refusal names. The three are the whole vocabulary, and they
// exist to keep one distinction readable in the log: the principal store READ
// the principal and REFUSED it, or the store failed EARLY and read nothing.
const (
	// reasonPrincipalInactive — the store answered. The row is gone,
	// suspended, or does not match the claimed identity. 401.
	reasonPrincipalInactive = "principal_inactive"
	// reasonPrincipalUnavailable — the store did not answer. Nothing about
	// this principal was read. 503.
	reasonPrincipalUnavailable = "principal_store_unavailable"
	// reasonValidatorAbsent — this deployment composed no validator, so
	// nothing could read anything. 401, and a composition defect.
	reasonValidatorAbsent = "principal_validator_not_configured"
)

// writePrincipalRefusal answers a principal check that refused the request,
// and writes the one log line that says why (#537).
//
// THE STATUS FOLLOWS THE CAUSE. ErrPrincipalInactive is the store's answer
// about the principal, so it is a 401. Every other error is a dependency
// fault, so it is a 503. All five call sites used to write the same 401, which
// made a connection-pool timeout indistinguishable from a suspension: the
// answer was wrong, and three E2E runs of #519 read it as a session that
// expired mid-journey.
//
// THE DEFAULT IS 503, NOT 401. A validator reports a refusal with
// ErrPrincipalInactive, and authsvc.principalValidationError is the only
// producer of one. Anything else reaching this function is an error nobody
// classified, and an unclassified error is far more often a fault than a
// suspension. Defaulting the other way is what hid the fault in the first
// place.
//
// THE CAUSE NEVER CROSSES THE BOUNDARY. The bodies are fixed text. The error
// goes to the log only, because a pgx message names the host, the database and
// the query.
func writePrincipalRefusal(w http.ResponseWriter, r *http.Request, source string, err error) {
	// A composition root that forgot the validator. It stays a 401 because the
	// request carries no proof this deployment would have accepted, and it is
	// logged apart from the two real answers: nothing was read, and nothing was
	// refused. This branch is what the forwarded path used to spell inline.
	if errors.Is(err, errPrincipalValidatorAbsent) {
		logPrincipalRefusal(r, source, reasonValidatorAbsent, nil)
		writeInactivePrincipal(w)
		return
	}
	if err == nil || errors.Is(err, auth.ErrPrincipalInactive) {
		logPrincipalRefusal(r, source, reasonPrincipalInactive, err)
		writeInactivePrincipal(w)
		return
	}
	logPrincipalRefusal(r, source, reasonPrincipalUnavailable, err)
	writeJSONError(w, http.StatusServiceUnavailable, "api_error", "principal_store_unavailable",
		"the authenticated principal could not be validated")
}

// logPrincipalRefusal writes the line this path had none of.
//
// A `grep` of an elitea-main log for a 401 on this path returned nothing, so
// after the fact nobody could say whether a refusal was a suspension or an
// outage. One line per refused request closes that, and it names the source,
// the reason and the route.
//
// It carries no principal identity. The refused caller is not authenticated,
// so an id or an email in this line is attacker-controlled text in the
// operator's log.
func logPrincipalRefusal(r *http.Request, source, reason string, err error) {
	attributes := []any{
		"source", source,
		"reason", reason,
		"method", r.Method,
		"path", r.URL.Path,
	}
	if err != nil {
		attributes = append(attributes, "error", err)
	}
	if reason == reasonPrincipalUnavailable {
		slog.ErrorContext(r.Context(), "principal validation could not read the principal store", attributes...)
		return
	}
	slog.WarnContext(r.Context(), "principal validation refused the request", attributes...)
}

// verifySessionCookie checks the HMAC session cookie and, when it refuses
// one, NAMES the check that refused it.
//
// The reason is returned rather than logged here so this function stays a
// pure check with no I/O, and so the caller decides whether a refusal is
// worth a line. It is "" on success. The four reasons are the four failing
// checks, in the order they run: the shape, the signature, the expiry and
// the subject. Splitting the shape and the signature apart matters for #538 —
// a truncated or re-encoded cookie fails the FIRST, and a cookie signed with
// another deployment's secret fails the SECOND, and those are different
// operator problems.
func verifySessionCookie(token, secret string) (auth.User, string, bool) {
	parts := strings.SplitN(token, ".", 2)
	if len(parts) != 2 {
		return auth.User{}, reasonSessionMalformed, false
	}

	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(parts[0]))
	expectedSig := hex.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(parts[1]), []byte(expectedSig)) {
		return auth.User{}, reasonSessionBadSignature, false
	}

	payloadBytes, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return auth.User{}, reasonSessionMalformed, false
	}

	var claims map[string]any
	if err := json.Unmarshal(payloadBytes, &claims); err != nil {
		return auth.User{}, reasonSessionMalformed, false
	}

	exp, ok := claims["exp"].(float64)
	if !ok || exp != float64(int64(exp)) {
		return auth.User{}, reasonSessionMalformed, false
	}
	if time.Now().Unix() > int64(exp) {
		return auth.User{}, reasonSessionExpired, false
	}

	var uid string
	switch v := claims["uid"].(type) {
	case string:
		uid = v
	case float64:
		uid = fmt.Sprintf("%d", int64(v))
	}
	if _, ok := positiveSessionUserID(uid); !ok {
		return auth.User{}, reasonSessionSubject, false
	}

	email, _ := claims["email"].(string)

	return auth.User{
		ID:       uid,
		UserID:   uid,
		Email:    email,
		AuthType: "session",
	}, "", true
}

// serverSessionRefusal names the refusal a browsersession error is, and says
// whether the store ANSWERED at all.
//
// The second return is the whole point. Four of these errors are answers about
// the session; anything else is a store that failed, and the two must not
// produce the same status. See the caller.
func serverSessionRefusal(err error) (string, bool) {
	switch {
	case errors.Is(err, browsersession.ErrNotFound),
		errors.Is(err, browsersession.ErrMalformedCookie):
		return reasonServerSessionUnknown, true
	case errors.Is(err, browsersession.ErrRevoked):
		return reasonServerSessionRevoked, true
	case errors.Is(err, browsersession.ErrExpired):
		return reasonServerSessionExpired, true
	case errors.Is(err, browsersession.ErrIdle):
		return reasonServerSessionIdle, true
	default:
		return reasonServerSessionUnreadable, false
	}
}

func positiveSessionUserID(value string) (int64, bool) {
	id, err := strconv.ParseInt(value, 10, 64)
	return id, err == nil && id > 0
}

func validateToken(ctx context.Context, cfg AuthConfig, token string) (auth.User, error) {
	if cfg.Validator == nil {
		return auth.User{}, fmt.Errorf("authentication validator is not configured")
	}
	return cfg.Validator.ValidateToken(ctx, token)
}
