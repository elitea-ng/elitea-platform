package auth

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"errors"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/personalproject"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth/browsersession"
	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/oauth2"
)

type OIDCConfig struct {
	IssuerURL    string
	ClientID     string
	ClientSecret string
	RedirectURI  string
}

func OIDCConfigFromEnv() (*OIDCConfig, error) {
	issuer := os.Getenv("OIDC_ISSUER_URL")
	if issuer == "" {
		return nil, nil
	}
	cfg := &OIDCConfig{
		IssuerURL:    issuer,
		ClientID:     os.Getenv("OIDC_CLIENT_ID"),
		ClientSecret: os.Getenv("OIDC_CLIENT_SECRET"),
		RedirectURI:  os.Getenv("OIDC_REDIRECT_URI"),
	}
	if cfg.ClientID == "" || cfg.RedirectURI == "" {
		return nil, fmt.Errorf("OIDC: OIDC_CLIENT_ID and OIDC_REDIRECT_URI are required when OIDC_ISSUER_URL is set")
	}
	return cfg, nil
}

// OIDCHandler serves the browser OIDC login and callback.
//
// Its CONFIGURATION is resolved per request rather than held here, so an
// operator authoring a provider on the admin page takes effect without a
// restart. See oidc_providers.go for the precedence, the cache and the one
// thing that still needs a restart.
type OIDCHandler struct {
	pool          *pgxpool.Pool
	secretKey     string
	secureCookies bool

	// sessions is the server-side session store. Nil keeps the legacy signed
	// cookie; see issueBrowserSession.
	sessions *browsersession.Manager

	// personalProjects asks for the caller's personal project at sign-in. Nil
	// is a working no-op, so a composition with no provisioner needs no branch.
	personalProjects personalproject.AsyncEnsurer

	// personalProjectWait bounds how long a successful login waits for the
	// personal project it just asked for. Zero means defaultSignInProvisionWait;
	// only a test sets it, so the production path cannot be given a wait an
	// operator did not review. See signin.go's ensurePersonalProject.
	personalProjectWait time.Duration

	// firstLogin is operator configuration applied after an assertion resolves
	// to an account. Empty unless WithFirstLoginPolicy was applied, in which
	// case this plane grants no initial administrator; an actor personal
	// access token is still issued, because that one is not a policy.
	firstLogin FirstLoginPolicy

	// providers and secretSource are the authored store. Both nil unless
	// WithProviderStore was applied, in which case only envRuntime is used.
	providers    IdentityProviderSource
	secretSource IdentitySecretSource

	// envRuntime is the fallback built from OIDC_ISSUER_URL at boot. Nil on a
	// deployment configured only through the admin page.
	envRuntime *oidcRuntime

	runtimeMu    sync.Mutex
	runtimeCache map[string]*oidcRuntime
}

// NewOIDCHandler builds the handler.
//
// `cfg` MAY BE NIL. A deployment that federates through a stored provider and
// sets no OIDC environment variables is the ordinary case once the admin editor
// exists, and it must not be forced to restate its provider in the environment
// to get the routes mounted.
func NewOIDCHandler(ctx context.Context, cfg *OIDCConfig, pool *pgxpool.Pool, secretKey string) (*OIDCHandler, error) {
	// COOKIE_SECURE=false disables the Secure flag — useful for E2E stacks
	// that run over plain HTTP on localhost.
	secureCookies := os.Getenv("COOKIE_SECURE") != "false"

	handler := &OIDCHandler{
		pool:          pool,
		secretKey:     secretKey,
		secureCookies: secureCookies,
		runtimeCache:  map[string]*oidcRuntime{},
	}
	if cfg != nil {
		environment, err := newOIDCRuntimeFromEnvironment(ctx, cfg)
		if err != nil {
			return nil, err
		}
		handler.envRuntime = environment
	}
	return handler, nil
}

// WithSessionManager gives this plane the server-side session store, and
// WithPersonalProjectEnsurer the personal-project provisioner.
//
// Both are setters rather than constructor parameters, and both are called
// from internal/api/router.go: that is where the pool-backed manager and the
// one shared *personalproject.Ensurer exist, while this handler is built in
// cmd/elitea-main/main.go before either does.
func (h *OIDCHandler) WithSessionManager(manager *browsersession.Manager) *OIDCHandler {
	if h == nil || manager == nil {
		return h
	}
	h.sessions = manager
	return h
}

// WithPersonalProjectEnsurer is the sign-in half of the personal-project fix.
func (h *OIDCHandler) WithPersonalProjectEnsurer(ensurer personalproject.AsyncEnsurer) *OIDCHandler {
	if h == nil || ensurer == nil {
		return h
	}
	h.personalProjects = ensurer
	return h
}

// WithPersonalProjectWait replaces the bound on that wait. It exists for the
// tests that measure the bound: the production value is a constant, so a
// deployment cannot be configured into holding its login callback open.
func (h *OIDCHandler) WithPersonalProjectWait(wait time.Duration) *OIDCHandler {
	if h == nil || wait <= 0 {
		return h
	}
	h.personalProjectWait = wait
	return h
}

func (h *OIDCHandler) Login(w http.ResponseWriter, r *http.Request) {
	runtime, err := h.runtime(r.Context())
	if err != nil {
		// The cause is logged, never returned: it names the issuer, the vault
		// entry, or the database, and this response goes to an unauthenticated
		// browser.
		slog.Error("OIDC: no usable identity provider for this login", "err", err)
		http.Error(w, "single sign-on is not available", http.StatusServiceUnavailable)
		return
	}

	targetTo := safeRedirectTarget(r.URL.Query().Get("target_to"))

	rawState := make([]byte, 16)
	if _, err := rand.Read(rawState); err != nil {
		http.Error(w, "failed to initialize OIDC state", http.StatusInternalServerError)
		return
	}
	stateNonce := base64.RawURLEncoding.EncodeToString(rawState)

	stateValue := stateNonce + "|" + targetTo

	http.SetCookie(w, &http.Cookie{
		Name:     oidcStateCookie,
		Value:    signBrowserValue(h.secretKey, stateValue),
		Path:     "/",
		HttpOnly: true,
		Secure:   h.secureCookies,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   300,
	})

	rawNonce := make([]byte, 16)
	if _, err := rand.Read(rawNonce); err != nil {
		http.Error(w, "failed to initialize OIDC state", http.StatusInternalServerError)
		return
	}
	nonce := base64.RawURLEncoding.EncodeToString(rawNonce)
	http.SetCookie(w, &http.Cookie{
		Name:     oidcNonceCookie,
		Value:    nonce,
		Path:     "/",
		HttpOnly: true,
		Secure:   h.secureCookies,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   300,
	})

	// PKCE. The verifier stays in a cookie on this browser and never leaves it;
	// only its S256 challenge is sent to the identity provider. Without it, an
	// authorization code intercepted between the provider and this callback can
	// be redeemed by whoever holds it. The state cookie does not close that: it
	// proves the callback belongs to a login this server started, not that the
	// code was redeemed by the browser that started it.
	pkceVerifier := oauth2.GenerateVerifier()
	http.SetCookie(w, &http.Cookie{
		Name:     oidcPKCECookie,
		Value:    pkceVerifier,
		Path:     "/",
		HttpOnly: true,
		Secure:   h.secureCookies,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   300,
	})

	authURL := runtime.oauth2Cfg.AuthCodeURL(stateValue,
		oidc.Nonce(nonce), oauth2.S256ChallengeOption(pkceVerifier))
	http.Redirect(w, r, authURL, http.StatusFound)
}

// oidcPKCECookie holds the PKCE code verifier of ONE login attempt.
//
// It is a cookie for the same reason the nonce is: this handler keeps no
// server-side login state, so the one place a per-attempt secret can live is the
// browser that started the attempt. HttpOnly keeps it out of reach of page
// script, and the five-minute lifetime bounds a stale attempt.
const oidcPKCECookie = "oidc_pkce"

// consumeCodeVerifier clears the PKCE cookie and returns its value.
//
// A MISSING verifier is not silently tolerated. The authorization request
// carried a challenge, so the token endpoint will demand the verifier; sending
// none would fail at the provider with an error about the client rather than
// about this browser's cookie.
func (h *OIDCHandler) consumeCodeVerifier(w http.ResponseWriter, r *http.Request) (string, bool) {
	http.SetCookie(w, &http.Cookie{
		Name:     oidcPKCECookie,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		Secure:   h.secureCookies,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	})
	cookie, err := r.Cookie(oidcPKCECookie)
	if err != nil || cookie.Value == "" {
		return "", false
	}
	return cookie.Value, true
}

// oidcStateCookie holds the signed state of ONE login attempt. It carries the
// per-login nonce and the redirect target, as `<nonce>|<target_to>.<mac>`.
const oidcStateCookie = "oidc_state"

// consumeState returns the redirect target the state cookie carries, and
// reports whether that cookie belongs to THIS callback.
//
// IT SPLITS ON THE LAST ".", which is what signBrowserValue and
// verifyBrowserValue do — the MAC is hex and holds no dot, while `target_to` is
// a path the caller chose and can. This handler used to keep its own copy of
// the split, on the FIRST dot, so a target such as `/artifacts/report.v2.pdf`
// moved the boundary: the MAC was then computed over half the state and every
// such login died with "state cookie signature invalid". saml.go signs and
// verifies the same shape and has used the shared helpers since the same defect
// was fixed there.
//
// The query parameter must repeat the WHOLE signed state, which is what Login
// sent to the identity provider. That is the check that ties this callback to a
// login this server started.
func (h *OIDCHandler) consumeState(cookieValue, queryState string) (string, bool) {
	state, verified := verifyBrowserValue(h.secretKey, cookieValue)
	if !verified {
		return "", false
	}
	if subtle.ConstantTimeCompare([]byte(state), []byte(queryState)) != 1 {
		return "", false
	}
	// The target is read from the SIGNED value, never from the query, so a
	// browser cannot redirect itself anywhere the login did not agree to.
	targetTo := "/"
	if separator := strings.Index(state, "|"); separator >= 0 {
		targetTo = safeRedirectTarget(state[separator+1:])
	}
	return targetTo, true
}

// oidcNonceCookie holds the nonce of ONE login attempt.
//
// The state cookie proves the callback belongs to a login this server started.
// It does not prove the id_token does: a token captured or replayed from
// another session verifies, carries a valid signature, and names a real
// subject. The nonce closes that gap, because the identity provider echoes it
// into the token and only the browser that started this login holds the cookie.
// OpenID Connect Core requires the provider to echo a nonce it receives.
const oidcNonceCookie = "oidc_nonce"

// consumeNonce clears the nonce cookie and reports whether the id_token echoes
// it. A missing cookie and a mismatch both fail: the token must belong to the
// login in this browser.
func (h *OIDCHandler) consumeNonce(w http.ResponseWriter, r *http.Request, tokenNonce string) bool {
	http.SetCookie(w, &http.Cookie{
		Name:     oidcNonceCookie,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		Secure:   h.secureCookies,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	})
	cookie, err := r.Cookie(oidcNonceCookie)
	if err != nil || cookie.Value == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(cookie.Value), []byte(tokenNonce)) == 1
}

func (h *OIDCHandler) Callback(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	// Resolved FIRST, and from the same source Login used. A callback that
	// resolved a different provider from the login that started it would verify
	// the token against the wrong issuer.
	runtime, err := h.runtime(ctx)
	if err != nil {
		slog.Error("OIDC: no usable identity provider for this callback", "err", err)
		http.Error(w, "single sign-on is not available", http.StatusServiceUnavailable)
		return
	}

	stateCookie, err := r.Cookie(oidcStateCookie)
	if err != nil || stateCookie.Value == "" {
		http.Error(w, "missing state cookie", http.StatusBadRequest)
		return
	}

	targetTo, ok := h.consumeState(stateCookie.Value, r.URL.Query().Get("state"))
	if !ok {
		// One message for every way the state can fail. The caller is
		// unauthenticated, and which check refused it is a fact about this
		// server, not about the browser's next move.
		slog.Warn("OIDC: the state cookie does not belong to this callback")
		http.Error(w, "invalid state cookie", http.StatusBadRequest)
		return
	}

	// Clear the state cookie
	http.SetCookie(w, &http.Cookie{
		Name:     oidcStateCookie,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		Secure:   h.secureCookies,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	})

	// Exchange authorization code for tokens
	code := r.URL.Query().Get("code")
	if code == "" {
		http.Error(w, "missing authorization code", http.StatusBadRequest)
		return
	}

	codeVerifier, ok := h.consumeCodeVerifier(w, r)
	if !ok {
		http.Error(w, "missing PKCE verifier cookie", http.StatusBadRequest)
		return
	}

	token, err := runtime.oauth2Cfg.Exchange(ctx, code, oauth2.VerifierOption(codeVerifier))
	if err != nil {
		slog.Error("OIDC token exchange failed", "err", err)
		http.Error(w, "token exchange failed", http.StatusInternalServerError)
		return
	}

	rawIDToken, ok := token.Extra("id_token").(string)
	if !ok {
		slog.Error("OIDC: no id_token in token response")
		http.Error(w, "no id_token in response", http.StatusInternalServerError)
		return
	}

	idToken, err := runtime.verifier.Verify(ctx, rawIDToken)
	if err != nil {
		slog.Error("OIDC: id_token verification failed", "err", err)
		http.Error(w, "id_token verification failed", http.StatusUnauthorized)
		return
	}

	var claims struct {
		Sub             string `json:"sub"`
		Email           string `json:"email"`
		Name            string `json:"name"`
		EmailVerified   *bool  `json:"email_verified"`
		AuthorizedParty string `json:"azp"`
	}
	if err := idToken.Claims(&claims); err != nil {
		slog.Error("OIDC: failed to parse claims", "err", err)
		http.Error(w, "failed to parse claims", http.StatusInternalServerError)
		return
	}

	// A token with more than one audience must name the party it was issued
	// for, and that party must be this client. Without the check, a token this
	// client was never the subject of is accepted. The reviewed protocol
	// verifier applies the same rule (internal/infra/identity/oidc/protocol.go).
	if (len(idToken.Audience) > 1 && claims.AuthorizedParty == "") ||
		(claims.AuthorizedParty != "" && claims.AuthorizedParty != runtime.oauth2Cfg.ClientID) {
		slog.Error("OIDC: id_token authorized party is not this client", "sub", claims.Sub)
		http.Error(w, "id_token verification failed", http.StatusUnauthorized)
		return
	}

	// The nonce binds the id_token to the browser that started this login. See
	// oidcNonceCookie.
	if !h.consumeNonce(w, r, idToken.Nonce) {
		slog.Error("OIDC: id_token nonce does not match the login", "sub", claims.Sub)
		http.Error(w, "id_token verification failed", http.StatusUnauthorized)
		return
	}

	if claims.Email == "" {
		slog.Error("OIDC: no email in claims", "sub", claims.Sub)
		http.Error(w, "email claim required", http.StatusBadRequest)
		return
	}

	// An identity provider that states the address is NOT verified is refused
	// outright. The address decides which account an unlinked subject joins, so
	// an unverified one is a claim to be somebody else.
	if claims.EmailVerified != nil && !*claims.EmailVerified {
		slog.Warn("OIDC: refused an unverified email claim", "sub", claims.Sub)
		http.Error(w, "email claim is not verified", http.StatusForbidden)
		return
	}

	userID, err := h.provisionUser(ctx, claims.Sub, claims.Email, claims.Name,
		claims.EmailVerified, runtime.requireEmailVerified)
	if err != nil {
		if errors.Is(err, errUserSuspended) {
			slog.Warn("OIDC: suspended user attempted login", "email", claims.Email)
			http.Error(w, "account suspended", http.StatusForbidden)
			return
		}
		if errors.Is(err, errIdentityConflict) {
			// The subject and the address name two different accounts. Only an
			// operator can decide which one survives, so the login stops here
			// rather than picking one.
			slog.Warn("OIDC: refused a login that names two accounts", "sub", claims.Sub)
			http.Error(w, "this identity is already linked to another account", http.StatusConflict)
			return
		}
		if errors.Is(err, errEmailNotVerified) {
			slog.Warn("OIDC: refused an unverified email for a new subject", "sub", claims.Sub)
			http.Error(w, "email claim is not verified", http.StatusForbidden)
			return
		}
		slog.Error("OIDC: user provisioning failed", "err", err, "email", claims.Email)
		http.Error(w, "user provisioning failed", http.StatusInternalServerError)
		return
	}

	numericUserID, ok := sessionUserID(userID)
	if !ok {
		slog.Error("OIDC: provisioning returned an unusable user id", "user_id", userID)
		http.Error(w, "user provisioning failed", http.StatusInternalServerError)
		return
	}
	if !issueBrowserSession(w, h.sessions, h.secretKey, h.secureCookies, browsersession.NewSession{
		UserID:   numericUserID,
		Email:    claims.Email,
		Provider: browsersession.ProviderOIDC,
	}, r) {
		return
	}
	// The personal project this account needs before the product is usable.
	// Bounded, and it never blocks the login; see ensurePersonalProject.
	ensurePersonalProject(h.personalProjects, userID, h.personalProjectWait)

	slog.Info("OIDC login successful",
		"email", claims.Email, "user_id", userID, "provider", runtime.origin)
	http.Redirect(w, r, targetTo, http.StatusFound)
}

var errUserSuspended = errors.New("user account is suspended")

// errIdentityConflict means the provider subject and the email claim name two
// different accounts. The login stops; only an operator can merge them.
var errIdentityConflict = errors.New("provider identity conflicts with the email claim")

// errEmailNotVerified means a subject with no link asked to join an account by
// email alone. The identity provider did not state that the address is verified.
var errEmailNotVerified = errors.New("email claim is not verified")

// oidcRequiresVerifiedEmail reports whether an ABSENT email_verified claim is
// fatal for the email fallback.
//
// The default is OFF, and that matches the reviewed protocol verifier. Its
// RequireEmailVerified field is off unless a deployment sets it. Many identity
// providers omit the claim, so a hard requirement stops a working login. An
// explicit `"email_verified": false` is refused whatever this variable says.
// Read Callback for that rule.
//
// Keep the name as a LITERAL here. services/elitea-llm-gateway/scripts/
// env-drift-check.sh greps for a quoted name inside an os.Getenv call. A named
// constant hides the read, and the gate then reports a false green.
// deploy/helm/elitea-main/values.yaml carries the matching knob.
func oidcRequiresVerifiedEmail() bool {
	return strings.EqualFold(os.Getenv("OIDC_REQUIRE_EMAIL_VERIFIED"), "true")
}

// verifiedEmailClaim answers the address that an `email:` entry of
// `initial_global_admins` may be compared against, and "" when there is none.
//
// THE ONLY ACCEPTED PROOF IS AN EXPLICIT `"email_verified": true`. An ABSENT
// claim is not proof, and OIDC_REQUIRE_EMAIL_VERIFIED does not make it one.
// That variable gates joinAccountByEmail — the step where an unknown subject
// ADOPTS an existing account — and nothing else. A brand new subject with no
// `email_verified` claim still provisions a new account with the variable set
// to true, so reading the variable here would report a verification that no
// component performed. An operator whose provider omits the claim names the
// login by `oidc:<sub>` instead, and reads that value off
// `GET /api/v2/social/author`.
//
// An explicit `"email_verified": false` never reaches here: Callback refuses
// the login outright.
func verifiedEmailClaim(email string, emailVerified *bool) string {
	if emailVerified == nil || !*emailVerified {
		return ""
	}
	return email
}

// provisionUser resolves the account this login belongs to.
//
// THE PROVIDER SUBJECT IS THE IDENTITY, and the email claim is only an
// attribute of it. The previous implementation had that the other way round.
// It upserted auth_core__user ON CONFLICT (email). It returned whatever id the
// address matched. It then wrote the subject link with an untargeted
// ON CONFLICT DO NOTHING. Two failures followed from it.
//
//  1. TAKEOVER. Any id_token whose email claim named an existing account handed
//     the caller a 24-hour session for that account. The subject was never read
//     back. A `provider_ref` that a different user already owned hit the UNIQUE
//     constraint. DO NOTHING then discarded the collision in silence.
//  2. ORPHANING. When an identity provider changed a person's address, the same
//     subject arrived with a new email. The upsert found no conflict, created a
//     second row, and the link insert failed on the unique provider_ref and was
//     again swallowed. The session went to the new empty row. The real account
//     became unreachable through this provider for good.
//
// The order below is the one internal/infra/identityrepo uses. Take the
// advisory lock on the subject. Look the subject up FIRST. Fall back to the
// address only for a subject that this deployment has never seen. The fallback
// also refuses an account that another OIDC subject ALREADY holds. Adoption on
// an email match alone is exactly failure 1.
func (h *OIDCHandler) provisionUser(
	ctx context.Context,
	sub, email, name string,
	emailVerified *bool,
	requireVerifiedEmail bool,
) (string, error) {
	providerRef := OIDCProviderRefPrefix + sub

	tx, err := h.pool.Begin(ctx)
	if err != nil {
		return "", fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	userID, err := resolveProvisionedUser(ctx, tx, providerRef, email, name, emailVerified, requireVerifiedEmail)
	if err != nil {
		return "", err
	}
	if err := applyFirstLoginGrants(
		ctx, tx, userID, providerRef, verifiedEmailClaim(email, emailVerified), h.firstLogin,
	); err != nil {
		return "", err
	}

	if err := tx.Commit(ctx); err != nil {
		return "", fmt.Errorf("commit: %w", err)
	}
	return strconv.Itoa(userID), nil
}

// WithFirstLoginPolicy applies the deployment's initial-administrator list to
// this handler. See first_login.go.
func (h *OIDCHandler) WithFirstLoginPolicy(policy FirstLoginPolicy) *OIDCHandler {
	if h == nil {
		return h
	}
	h.firstLogin = FirstLoginPolicy{
		InitialGlobalAdmins: append([]string(nil), policy.InitialGlobalAdmins...),
	}
	return h
}

// resolveProvisionedUser is provisionUser inside one transaction. The split
// lets a test assert the resolution ORDER without a database. The subject
// lookup must come first. The email fallback must run only after it misses.
func resolveProvisionedUser(
	ctx context.Context,
	tx pgx.Tx,
	providerRef, email, name string,
	emailVerified *bool,
	requireVerifiedEmail bool,
) (int, error) {
	// Serializes two concurrent first logins for the same subject, so only one
	// of them creates the link. Mirrors AcquireAuthProviderAdvisoryLock.
	if _, err := tx.Exec(ctx,
		`SELECT pg_advisory_xact_lock(hashtextextended($1::text, 0))`, providerRef,
	); err != nil {
		return 0, fmt.Errorf("lock provider identity: %w", err)
	}

	userID, linked, err := reuseLinkedAccount(ctx, tx, providerRef, email, name)
	if err != nil {
		return 0, err
	}
	if linked {
		return userID, nil
	}
	return joinAccountByEmail(ctx, tx, providerRef, email, name, emailVerified, requireVerifiedEmail)
}

// reuseLinkedAccount returns the account this provider subject already owns.
//
// It also REPAIRS the address on that account. This keeps an email change at
// the identity provider from orphaning the account. The subject still names the
// same row, and the row learns the new address.
func reuseLinkedAccount(
	ctx context.Context,
	tx pgx.Tx,
	providerRef, email, name string,
) (userID int, linked bool, err error) {
	var suspended bool
	err = tx.QueryRow(ctx,
		`SELECT owner.id, owner.suspended
		 FROM auth_core__user_provider AS provider
		 JOIN auth_core__user AS owner ON owner.id = provider.user_id
		 WHERE provider.provider_ref = $1
		 FOR UPDATE OF provider, owner`,
		providerRef,
	).Scan(&userID, &suspended)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("read provider identity: %w", err)
	}
	if suspended {
		return 0, false, errUserSuspended
	}

	_, err = tx.Exec(ctx,
		`UPDATE auth_core__user
		 SET last_login = now(),
		     email = $2,
		     name = COALESCE(NULLIF(name, ''), $3)
		 WHERE id = $1`,
		userID, email, name,
	)
	var conflict *pgconn.PgError
	if errors.As(err, &conflict) && conflict.Code == "23505" {
		// A DIFFERENT account already holds this address. Writing it here would
		// need the two accounts merged, which only an operator can decide.
		return 0, false, errIdentityConflict
	}
	if err != nil {
		return 0, false, fmt.Errorf("touch provider identity: %w", err)
	}
	return userID, true, nil
}

// FederatedRefPrefixes are the `auth_core__user_provider.provider_ref`
// namespaces THIS service writes, and the closed set joinAccountByEmail refuses
// to adopt across.
//
// Every federated login path MUST build its ref from one of these, and every
// new protocol MUST be added here in the same change. The guard is derived from
// this list rather than restating a literal, because a literal is what made the
// SAML path adoptable: the guard read `LIKE 'oidc:%'`, SAML wrote `saml:`, and
// the guard silently stopped applying to it.
//
// The set is CLOSED, and deliberately does not match "any ref containing a
// colon". Pylon writes two shapes this service must keep adoptable:
//
//   - a BARE ref, the raw OIDC subject with no prefix at all
//     (legacy/plugins/auth_init/rpc/processor.py:55). A namespace-blind guard
//     refuses every pylon-created account with 409 on its first Go login.
//   - `cirro:invite:token:<token>`, written when an operator invites somebody
//     (legacy/plugins/admin/api/v2/user_invite.py:83). That is an invite
//     receipt, not a federated identity, and an invited person must be able to
//     sign in for the first time.
const (
	OIDCProviderRefPrefix = "oidc:"
	SAMLProviderRefPrefix = "saml:"
)

// federatedRefPatterns is FederatedRefPrefixes as SQL LIKE patterns.
func federatedRefPatterns() []string {
	return []string{OIDCProviderRefPrefix + "%", SAMLProviderRefPrefix + "%"}
}

// joinAccountByEmail handles a subject that has never signed in here. It is the
// only path on which the email claim decides anything, so it is the narrow one.
//
// An account that another FEDERATED subject already holds is never adopted, in
// either protocol and across the two. An account held only by a pylon bare ref
// or an invite receipt is adopted. This is how a person keeps one account across
// a first single-sign-on. The deployment can also demand a verified address for
// that step.
//
// CROSS-PROTOCOL ADOPTION IS REFUSED, not permitted. A deployment may run one
// OIDC and one SAML provider at once, and they may be different identity
// providers; letting an assertion from one adopt an account the other owns makes
// either one able to take over the other's accounts by asserting an address.
// The operator resolves it, which is what the 409 says.
func joinAccountByEmail(
	ctx context.Context,
	tx pgx.Tx,
	providerRef, email, name string,
	emailVerified *bool,
	requireVerifiedEmail bool,
) (int, error) {
	if requireVerifiedEmail && (emailVerified == nil || !*emailVerified) {
		return 0, errEmailNotVerified
	}

	var userID int
	err := tx.QueryRow(ctx,
		`INSERT INTO auth_core__user (email, name, last_login)
		 VALUES ($1, $2, now())
		 ON CONFLICT (email) DO UPDATE SET last_login = now()
		 WHERE auth_core__user.suspended = false
		   AND NOT EXISTS (
		       SELECT 1 FROM auth_core__user_provider AS bound
		       WHERE bound.user_id = auth_core__user.id
		         AND bound.provider_ref LIKE ANY ($3)
		   )
		 RETURNING id`,
		email, name, federatedRefPatterns(),
	).Scan(&userID)
	if errors.Is(err, pgx.ErrNoRows) {
		// The address matched a row the conflict clause refused. Read which of
		// the two reasons it was, so the caller reports the right one.
		return 0, refusedEmailReason(ctx, tx, email)
	}
	if err != nil {
		return 0, fmt.Errorf("upsert user: %w", err)
	}

	// TARGETED on provider_ref. The untargeted form also masks the
	// (user_id, provider_ref) primary key, which hides the collision this
	// re-select exists to find.
	if _, err := tx.Exec(ctx,
		`INSERT INTO auth_core__user_provider (user_id, provider_ref)
		 VALUES ($1, $2)
		 ON CONFLICT (provider_ref) DO NOTHING`,
		userID, providerRef,
	); err != nil {
		return 0, fmt.Errorf("link provider: %w", err)
	}

	// The insert above may have written nothing. Read the owner back and refuse
	// when it is not the account just resolved. With the lookup and the advisory
	// lock in front this is a race backstop, not the primary guard.
	var ownerID int
	if err := tx.QueryRow(ctx,
		`SELECT user_id FROM auth_core__user_provider WHERE provider_ref = $1`,
		providerRef,
	).Scan(&ownerID); err != nil {
		return 0, fmt.Errorf("read linked provider: %w", err)
	}
	if ownerID != userID {
		return 0, errIdentityConflict
	}
	return userID, nil
}

// refusedEmailReason names why the email upsert wrote no row. The conflict
// clause has two arms, and the caller must report the right one.
//
// A suspended account is reported as suspended. Every other outcome is reported
// as a conflict, and that answer withholds the account. Two outcomes reach it:
// another OIDC subject already holds the account, or a concurrent change removed
// the row.
func refusedEmailReason(ctx context.Context, tx pgx.Tx, email string) error {
	var suspended bool
	err := tx.QueryRow(ctx,
		`SELECT suspended FROM auth_core__user WHERE email = $1`,
		email,
	).Scan(&suspended)
	if err == nil && suspended {
		return errUserSuspended
	}
	return errIdentityConflict
}
