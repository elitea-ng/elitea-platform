package authcomposition

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	browserapi "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/browserauth"
	browserapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/browserauth"
	identityapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/identity"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth/runtoken"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/authattempt"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/authflow"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/authsession"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/authsvc"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/identityrepo"
)

const (
	defaultBrowserTransactionTTL = 5 * time.Minute
	// preLoginSessionLifetime bounds the unauthenticated session a login
	// begin creates. It is useful only for the one login transaction bound
	// to it, which lives defaultBrowserTransactionTTL.
	preLoginSessionLifetime = 5 * time.Minute
)

var ErrInvalidGraph = errors.New("invalid authentication composition graph")

// FormGraphDependencies are production-owned dependencies that must not be
// recreated inside Auth. PostgreSQL is Main's existing pool.
//
// There is no gateway edge here any more. The graph used to compose two
// forward-auth kernels and the /internal/auth/main handler that projected an
// X-Auth-* identity for an authenticating edge; no edge calls it (#1359), so
// the route-owned public rules and the native token validator that only that
// edge consumed are gone with it.
type FormGraphDependencies struct {
	PostgreSQL *pgxpool.Pool
	// Brand is the resolved brand pack the login page renders (ADR-0024
	// WP5); nil renders the default presentation.
	Brand browserapi.BrandSource
	// FormSignInEnabled turns on the local username/password sign-in
	// (ELITEA_FORM_LOGIN_ENABLED, off by default). Off, the graph still
	// composes PAT validation and signing, which the runtime depends on, but
	// BrowserRoutes is nil and the Form handler holds NO users: no configured
	// password is accepted anywhere. The zero value is the safe one on
	// purpose.
	FormSignInEnabled bool
}

// FormGraph owns the Form browser routes and the personal-access-token
// signing material. Its sign-in state (sessions, login transactions, attempt
// windows) lives in the injected PostgreSQL pool (shared migration 0153),
// which remains caller-owned. The graph owns no connection of its own.
type FormGraph struct {
	routes            http.Handler
	browserRoutes     http.Handler
	patIssuer         *authsvc.LocalIssuer
	projectPATIssuer  *authsvc.ProjectSystemIssuer
	patValidator      *authsvc.LocalValidator
	patSigningKey     []byte
	formUsers         FormUserReport
	formSignInEnabled bool
	formSessions      *browserapi.FormSessions
}

func NewFormGraph(
	ctx context.Context,
	config Config,
	dependencies FormGraphDependencies,
) (*FormGraph, error) {
	return newFormGraph(ctx, config, dependencies, nil)
}

// newFormGraph is NewFormGraph with a test seam: observe, when not nil, sees
// the materialized snapshot before composition uses it. Tests use it to prove
// the snapshot is wiped when composition returns.
func newFormGraph(
	ctx context.Context,
	config Config,
	dependencies FormGraphDependencies,
	observe func(*materializedFiles),
) (*FormGraph, error) {
	if ctx == nil || dependencies.PostgreSQL == nil {
		return nil, ErrInvalidGraph
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	material, err := materialize(config)
	if err != nil {
		return nil, err
	}
	defer material.destroy()
	if observe != nil {
		observe(material)
	}

	cookieLifetime := time.Duration(config.Cookie.LifetimeSeconds) * time.Second
	sessions, err := authsession.NewPostgresStore(dependencies.PostgreSQL, authsession.Config{
		TTL:         cookieLifetime,
		PreLoginTTL: min(preLoginSessionLifetime, cookieLifetime),
	})
	if err != nil {
		return nil, composeError("browser session store", err)
	}
	transactions, err := authflow.NewPostgresStore(dependencies.PostgreSQL)
	if err != nil {
		return nil, composeError("browser transaction store", err)
	}
	attempts, err := authattempt.NewPostgresAdmitter(
		dependencies.PostgreSQL,
		compiledAttemptConfig(material.attemptKey),
	)
	if err != nil {
		return nil, composeError("browser attempt admission", err)
	}

	identityRepository, err := identityrepo.NewPostgresRepository(dependencies.PostgreSQL)
	if err != nil {
		return nil, composeError("identity repository", err)
	}
	provisioner, err := identityapp.NewProvisionService(
		identityRepository,
		provisioningPolicy(config.Identity),
	)
	if err != nil {
		return nil, composeError("identity provisioner", err)
	}
	principalValidator := authsvc.NewPrincipalValidator(dependencies.PostgreSQL)
	flow, err := browserapp.NewService(
		sessions,
		transactions,
		provisioner,
		principalValidator,
		defaultBrowserTransactionTTL,
	)
	if err != nil {
		return nil, composeError("browser authentication service", err)
	}

	patValidator := authsvc.NewLocalValidatorBytes(dependencies.PostgreSQL, material.patSigningKey)
	patIssuer := authsvc.NewLocalIssuerBytes(dependencies.PostgreSQL, material.patSigningKey)
	projectPATIssuer := authsvc.NewProjectSystemIssuerBytes(
		dependencies.PostgreSQL,
		material.patSigningKey,
	)

	cookies, err := cookiePolicy(config.Cookie)
	if err != nil {
		return nil, err
	}
	proxyResolver, err := browserapi.NewTrustedProxyResolver(browserapi.TrustedProxyConfig{
		TrustedProxyCIDRs: append([]string(nil), config.TrustedProxyCIDRs...),
		PublicOrigin:      config.PublicOrigin,
	})
	if err != nil {
		return nil, composeError("trusted proxy resolver", err)
	}
	// With Form sign-in off, a Form session that predates the switch (the
	// cookie lives up to cookie.lifetime_seconds) must stop authenticating
	// too; otherwise "disabled" would mean "disabled for new sign-ins only".
	var sessionAuthorizer browserapi.FormSessionAuthorizer = flow
	if !dependencies.FormSignInEnabled {
		sessionAuthorizer = formSessionsRefused{next: flow}
	}
	formSessions, err := browserapi.NewFormSessions(cookies, sessionAuthorizer)
	if err != nil {
		return nil, composeError("Form session reader", err)
	}
	formProvider := material.formProvider
	if !dependencies.FormSignInEnabled {
		// The configured users are ignored, not merely unmounted: an empty
		// list cannot match any submission, whichever route reaches it.
		formProvider, err = browserapp.NewFormProvider([]byte(`{"users":[]}`))
		if err != nil {
			return nil, composeError("disabled Form provider", err)
		}
	}
	formHandler, err := browserapi.NewHandler(
		flow,
		formProvider,
		attempts,
		proxyResolver,
		cookies,
		browserapi.Config{
			DefaultLoginTarget:  config.Redirects.DefaultLogin,
			DefaultLogoutTarget: config.Redirects.DefaultLogout,
			Brand:               dependencies.Brand,
		},
	)
	if err != nil {
		return nil, composeError("Form handler", err)
	}
	routes := formHandler.Routes()

	var browserRoutes http.Handler
	if dependencies.FormSignInEnabled {
		browserRoutes = formHandler.Routes()
	}
	graph := &FormGraph{
		routes:            routes,
		browserRoutes:     browserRoutes,
		formSignInEnabled: dependencies.FormSignInEnabled,
		formSessions:      formSessions,
		patIssuer:         patIssuer,
		projectPATIssuer:  projectPATIssuer,
		patValidator:      patValidator,
		// Copy the key: materialize destroys the snapshot when this function
		// returns. The graph must sign a new personal access token with the
		// SAME key patValidator reads it back with. See SignPAT.
		patSigningKey: append([]byte(nil), material.patSigningKey...),
		formUsers: FormUserReport{
			Configured:          material.formProvider.UserCount(),
			MisconfiguredLogins: material.formProvider.MisconfiguredLogins(),
		},
	}
	return graph, nil
}

// FormUserReport is what the Form users file holds, for the boot log. It
// carries logins (operator-chosen names) and never a password or attribute.
type FormUserReport struct {
	Configured int
	// MisconfiguredLogins are users with no usable email address. They load,
	// and their sign-in is refused (browserauth.FormProvider).
	MisconfiguredLogins []string
}

// FormUsers reports the configured Form users.
func (graph *FormGraph) FormUsers() FormUserReport {
	if graph == nil {
		return FormUserReport{}
	}
	return FormUserReport{
		Configured:          graph.formUsers.Configured,
		MisconfiguredLogins: append([]string(nil), graph.formUsers.MisconfiguredLogins...),
	}
}

// FormSessions reads the Form session cookie for apimw.Auth. It replaces the
// forwarded-identity verifier the X-Auth-* edge projection needed (#1359):
// elitea-main resolves the browser's Form session itself, so no edge has to.
func (graph *FormGraph) FormSessions() *browserapi.FormSessions {
	if graph == nil {
		return nil
	}
	return graph.formSessions
}

// Routes is the Form router, mounted or not. Tests drive sign-in through it
// while BrowserRoutes is nil.
func (graph *FormGraph) Routes() http.Handler {
	if graph == nil {
		return nil
	}
	return graph.routes
}

// FormSignInEnabled reports whether this graph accepts Form passwords.
func (graph *FormGraph) FormSignInEnabled() bool {
	return graph != nil && graph.formSignInEnabled
}

// BrowserRoutes returns only the browser-facing Form login/logout surface,
// or nil while Form sign-in is disabled.
func (graph *FormGraph) BrowserRoutes() http.Handler {
	if graph == nil {
		return nil
	}
	return graph.browserRoutes
}

// ValidateToken reuses the exact current-baseline HS512 PAT validation path
// for private runtime consumers. It does not add a second token parser, key
// snapshot, or authorization model; callers remain responsible for checking
// the validated principal against their resource-specific ownership rule.
func (graph *FormGraph) ValidateToken(ctx context.Context, token string) (auth.User, error) {
	if graph == nil || graph.patValidator == nil {
		return auth.User{}, ErrInvalidGraph
	}
	return graph.patValidator.ValidateToken(ctx, token)
}

// SignPAT signs the bearer of a personal access token that this service has
// just persisted.
//
// The /api/v2/auth/token route must sign with this key.
//
// DEFECT this method fixes: that route signed with APPLICATION_SECRET_KEY,
// while this graph's validator reads a PAT back with the bytes of
// credentials.pat_signing_key_file. The two values are unrelated. Every token
// the product issued on a form-auth deployment failed the signature check on
// first use. The plaintext is shown one time only. The user therefore kept a
// permanently dead credential.
func (graph *FormGraph) SignPAT(tokenUUID *string, expiresAt *time.Time) (string, error) {
	if graph == nil || len(graph.patSigningKey) == 0 {
		return "", ErrInvalidGraph
	}
	return authsvc.SignBaselinePAT(graph.patSigningKey, tokenUUID, expiresAt)
}

// RunTokenKey is the key of governed runs' run tokens (#1461),
// derived from the personal access token signing key under its own label, so
// it needs no new secret, every replica derives the same key, and neither
// key verifies the other's tokens. Nil without a signing key.
func (graph *FormGraph) RunTokenKey() []byte {
	if graph == nil {
		return nil
	}
	return runtoken.DeriveKey(graph.patSigningKey)
}

// IssueToken recreates the current-baseline bearer representation for the
// selected active PAT of one trusted durable execution actor.
func (graph *FormGraph) IssueToken(ctx context.Context, userID int64) (string, error) {
	if graph == nil || graph.patIssuer == nil {
		return "", ErrInvalidGraph
	}
	return graph.patIssuer.IssueToken(ctx, userID)
}

// IssueProjectToken selects the already-provisioned project-system PAT used by
// scheduled work. It never creates an identity or falls back to an admin.
func (graph *FormGraph) IssueProjectToken(
	ctx context.Context,
	projectID int64,
) (authsvc.ProjectSystemToken, error) {
	if graph == nil || graph.projectPATIssuer == nil {
		return authsvc.ProjectSystemToken{}, ErrInvalidGraph
	}
	return graph.projectPATIssuer.IssueProjectToken(ctx, projectID)
}

// formSessionsRefused answers a Form-provider session as unauthenticated.
// It decorates the Form session reader while Form sign-in is disabled; every
// other provider's session passes through.
type formSessionsRefused struct {
	next browserapi.FormSessionAuthorizer
}

func (r formSessionsRefused) Authorize(ctx context.Context, sessionID string) (browserapp.Authorization, error) {
	authorization, err := r.next.Authorize(ctx, sessionID)
	if err != nil {
		return browserapp.Authorization{}, err
	}
	if authorization.Provider == browserapp.FormProviderName {
		return browserapp.Authorization{}, browserapp.ErrUnauthenticated
	}
	return authorization, nil
}

func compiledAttemptConfig(key []byte) authattempt.Config {
	// These named security defaults intentionally remain code-owned to avoid
	// another collection of lightly understood knobs. Production mount still
	// requires capacity/load evidence and an explicit reviewed change if the
	// measured workload cannot use these exact limits. Global counts
	// credential attempts only, never a login begin (authattempt.Config.Global).
	return authattempt.Config{
		KeySecret:            append([]byte(nil), key...),
		Global:               authattempt.Policy{MaxAttempts: 1000, Window: time.Minute},
		FormBegin:            authattempt.Policy{MaxAttempts: 20, Window: time.Minute},
		FormCredentialClient: authattempt.Policy{MaxAttempts: 5, Window: time.Minute},
		FormCredentialLogin:  authattempt.Policy{MaxAttempts: 25, Window: time.Minute},
		// Single sign-on (#1078), per client only. Generous on purpose: a
		// client key is one IPv4 address, so a whole office behind one NAT
		// egress shares it, and a sign-in that bounces between the identity
		// provider and the callback must survive a few user retries. The
		// limit exists to bound one client's state and identity provider
		// traffic, not to stop guessing — the provider verifies credentials.
		OIDCBegin:    authattempt.Policy{MaxAttempts: 120, Window: time.Minute},
		OIDCCallback: authattempt.Policy{MaxAttempts: 120, Window: time.Minute},
		SAMLBegin:    authattempt.Policy{MaxAttempts: 120, Window: time.Minute},
		SAMLACS:      authattempt.Policy{MaxAttempts: 120, Window: time.Minute},
	}
}

// singleSignOnAttemptKeyDomain separates the single-sign-on limiter's key from
// every other use of APPLICATION_SECRET_KEY.
const singleSignOnAttemptKeyDomain = "elitea-sso-browser-attempt-key-v1"

// NewSingleSignOnAttemptAdmitter builds the attempt limiter the OIDC and SAML
// planes (internal/api/v2/auth) share, with the same code-owned policies as
// the Form graph's (compiledAttemptConfig).
//
// A single-sign-on deployment usually has no authentication document and so
// no attempt key file; the key is derived from APPLICATION_SECRET_KEY, which
// that plane already requires, under its own domain. Rotating that secret
// starts every window over, which only ever admits more.
func NewSingleSignOnAttemptAdmitter(
	pool *pgxpool.Pool,
	applicationSecretKey string,
) (*authattempt.PostgresAdmitter, error) {
	if pool == nil || applicationSecretKey == "" {
		return nil, ErrInvalidGraph
	}
	mac := hmac.New(sha256.New, []byte(applicationSecretKey))
	_, _ = mac.Write([]byte(singleSignOnAttemptKeyDomain))
	key := mac.Sum(nil)
	defer clear(key)
	admitter, err := authattempt.NewPostgresAdmitter(pool, compiledAttemptConfig(key))
	if err != nil {
		return nil, composeError("single sign-on attempt admission", err)
	}
	return admitter, nil
}

func provisioningPolicy(config IdentityConfig) identityapp.ProvisioningPolicy {
	policy := identityapp.ProvisioningPolicy{
		InitialGlobalAdmins: append([]string(nil), config.InitialGlobalAdmins...),
		AdoptSCIMUsers:      config.AdoptSCIMUsers,
	}
	if config.ProjectEnrollment != nil {
		policy.ProjectEnrollment = identityapp.ProjectEnrollmentPolicy{
			ProjectID:      config.ProjectEnrollment.ProjectID,
			AllowedDomains: config.ProjectEnrollment.AllowedDomains,
			AdditionalGlobalAdminRoles: append(
				[]string(nil),
				config.ProjectEnrollment.AdditionalProjectRolesForGlobalAdmins...,
			),
		}
	}
	return policy
}

func cookiePolicy(config CookieConfig) (*browserapi.CookiePolicy, error) {
	var sameSite http.SameSite
	switch config.SameSite {
	case "lax":
		sameSite = http.SameSiteLaxMode
	case "strict":
		sameSite = http.SameSiteStrictMode
	default:
		return nil, fmt.Errorf("%w: cookie policy", ErrInvalidGraph)
	}
	policy, err := browserapi.NewCookiePolicy(browserapi.CookieConfig{
		Name:     config.Name,
		Domain:   "",
		Secure:   true,
		SameSite: sameSite,
		Lifetime: time.Duration(config.LifetimeSeconds) * time.Second,
	})
	if err != nil {
		return nil, composeError("cookie policy", err)
	}
	return policy, nil
}

func composeError(part string, err error) error {
	return fmt.Errorf("%w: %s: %w", ErrInvalidGraph, part, err)
}
