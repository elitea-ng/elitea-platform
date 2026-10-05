package authcomposition

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	browserapi "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/browserauth"
	browserapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/browserauth"
	forwardapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/edgeauth"
	identityapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/identity"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/authattempt"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/authflow"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/authsession"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/authsvc"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/infra/identityrepo"
)

const defaultBrowserTransactionTTL = 5 * time.Minute

var ErrInvalidGraph = errors.New("invalid authentication composition graph")

// FormGraphDependencies are production-owned dependencies that must not be
// recreated inside Auth. PostgreSQL is Main's existing pool. MainRoutePublicRules
// must be the complete audited set declared by route owners; nil is rejected so
// omission cannot look like an intentionally empty set.
type FormGraphDependencies struct {
	PostgreSQL           *pgxpool.Pool
	MainRoutePublicRules []forwardapp.PublicRule
	// Brand is the resolved brand pack the login page renders (ADR-0024
	// WP5); nil renders the default presentation.
	Brand browserapi.BrandSource
}

// FormGraph owns the Form browser routes and the separate current-Main gateway
// authorization edge. It owns only its dedicated Auth Redis client; the
// injected PostgreSQL pool remains caller-owned.
type FormGraph struct {
	routes           http.Handler
	browserRoutes    http.Handler
	mainEdgeAuth     http.Handler
	mainKernel       *forwardapp.Kernel
	patIssuer        *authsvc.LocalIssuer
	projectPATIssuer *authsvc.ProjectSystemIssuer
	patValidator     *authsvc.LocalValidator
	patSigningKey    []byte
	proxyResolver    *browserapi.TrustedProxyResolver
	redis            *redis.Client
	formUsers        FormUserReport
	closeOnce        sync.Once
	closeErr         error
}

type redisOpener func(context.Context, Config, *materializedFiles) (*redis.Client, error)

func NewFormGraph(
	ctx context.Context,
	config Config,
	dependencies FormGraphDependencies,
) (*FormGraph, error) {
	return newFormGraph(ctx, config, dependencies, newAuthRedisClient)
}

func newFormGraph(
	ctx context.Context,
	config Config,
	dependencies FormGraphDependencies,
	openRedis redisOpener,
) (*FormGraph, error) {
	if ctx == nil || dependencies.PostgreSQL == nil ||
		dependencies.MainRoutePublicRules == nil || openRedis == nil {
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

	redisClient, err := openRedis(ctx, config, material)
	if err != nil {
		if redisClient != nil {
			_ = redisClient.Close()
		}
		return nil, fmt.Errorf("%w: open Auth Redis: %w", ErrInvalidGraph, err)
	}
	if redisClient == nil {
		return nil, fmt.Errorf("%w: Auth Redis client", ErrInvalidGraph)
	}
	committed := false
	defer func() {
		if !committed {
			_ = redisClient.Close()
		}
	}()

	cookieLifetime := time.Duration(config.Cookie.LifetimeSeconds) * time.Second
	sessions, err := authsession.NewRedisStore(redisClient, authsession.Config{
		KeyPrefix: config.Redis.KeyPrefix + "session:",
		TTL:       cookieLifetime,
	})
	if err != nil {
		return nil, composeError("browser session store", err)
	}
	transactions, err := authflow.NewRedisStore(redisClient, authflow.Config{
		KeyPrefix: config.Redis.KeyPrefix + "transaction:",
	})
	if err != nil {
		return nil, composeError("browser transaction store", err)
	}
	attempts, err := authattempt.NewRedisAdmitter(
		redisClient,
		compiledAttemptConfig(config.Redis.KeyPrefix+"attempt:", material.attemptKey),
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
	credentials, err := forwardapp.NewTokenCredentialAuthenticator(patValidator)
	if err != nil {
		return nil, composeError("credential authenticator", err)
	}
	directPolicy, err := forwardapp.NewPublicPolicy(nil)
	if err != nil {
		return nil, composeError("Direct public policy", err)
	}
	mainRules, err := mainPublicRules(config, dependencies.MainRoutePublicRules)
	if err != nil {
		return nil, err
	}
	mainPolicy, err := forwardapp.NewPublicPolicy(mainRules)
	if err != nil {
		return nil, composeError("Main public policy", err)
	}
	directKernel, err := forwardapp.NewKernel(credentials, flow, directPolicy)
	if err != nil {
		return nil, composeError("Direct authorization kernel", err)
	}
	mainKernel, err := forwardapp.NewKernel(credentials, flow, mainPolicy)
	if err != nil {
		return nil, composeError("Main authorization kernel", err)
	}

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
	mappers, err := browserapi.NewSuccessMapper(config.Mappers.Contract)
	if err != nil {
		return nil, composeError("Auth mapper projection", err)
	}
	coreHandler, err := browserapi.NewCoreHandler(
		directKernel,
		proxyResolver,
		cookies,
		browserapi.CoreConfig{
			CredentialHeaders:  credentialHeaders(config.Credentials.Headers),
			AccessDeniedTarget: config.Redirects.DirectAccessDenied,
			Mappers:            mappers,
		},
	)
	if err != nil {
		return nil, composeError("Direct Auth handler", err)
	}
	mainHandler, err := browserapi.NewMainHandler(
		mainKernel,
		proxyResolver,
		cookies,
		browserapi.MainConfig{
			CredentialHeaders:  credentialHeaders(config.Credentials.Headers),
			AccessDeniedTarget: config.Redirects.MainAccessDenied,
			// The edge resolves this handler's relative Location against the
			// address it called, which is this service's internal one. The
			// configuration already names the origin browsers use.
			PublicOrigin: config.PublicOrigin,
		},
	)
	if err != nil {
		return nil, composeError("Main EdgeAuth handler", err)
	}
	formHandler, err := browserapi.NewHandler(
		flow,
		material.formProvider,
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
	routes, err := browserapi.NewFormRoutes(coreHandler, formHandler)
	if err != nil {
		return nil, composeError("Form routes", err)
	}

	graph := &FormGraph{
		routes:           routes,
		browserRoutes:    formHandler.Routes(),
		mainEdgeAuth:     mainHandler,
		mainKernel:       mainKernel,
		patIssuer:        patIssuer,
		projectPATIssuer: projectPATIssuer,
		patValidator:     patValidator,
		// Copy the key: materialize destroys the snapshot when this function
		// returns. The graph must sign a new personal access token with the
		// SAME key patValidator reads it back with. See SignPAT.
		patSigningKey: append([]byte(nil), material.patSigningKey...),
		proxyResolver: proxyResolver,
		redis:         redisClient,
		formUsers: FormUserReport{
			Configured:          material.formProvider.UserCount(),
			MisconfiguredLogins: material.formProvider.MisconfiguredLogins(),
		},
	}
	committed = true
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

// ForwardedIdentityVerifier returns the same trusted-peer policy used by the
// browser and Main EdgeAuth edges. Production product routes must not parse
// a second CIDR configuration or trust forwarded identity headers directly.
func (graph *FormGraph) ForwardedIdentityVerifier() *browserapi.TrustedProxyResolver {
	if graph == nil {
		return nil
	}
	return graph.proxyResolver
}

func (graph *FormGraph) Routes() http.Handler {
	if graph == nil {
		return nil
	}
	return graph.routes
}

// BrowserRoutes returns only the browser-facing Form login/logout surface.
// The compatibility Auth Core /auth handler requires EdgeAuth-generated
// source metadata and must not be exposed through an ordinary reverse proxy.
func (graph *FormGraph) BrowserRoutes() http.Handler {
	if graph == nil {
		return nil
	}
	return graph.browserRoutes
}

// MainEdgeAuth returns the gateway-only current-Main authorization edge.
// It is deliberately separate from the public Auth Core /auth route because
// the two current-baseline credential traversal policies differ.
func (graph *FormGraph) MainEdgeAuth() http.Handler {
	if graph == nil {
		return nil
	}
	return graph.mainEdgeAuth
}

// Ping reports the dedicated Auth Redis dependency to the public readiness
// endpoint. It does not transfer client ownership to the health package.
func (graph *FormGraph) Ping(ctx context.Context) error {
	if graph == nil || graph.redis == nil {
		return ErrInvalidGraph
	}
	return graph.redis.Ping(ctx).Err()
}

// AuthorizeMain fixes current Main traversal semantics at the composition
// boundary. A future Main HTTP adapter supplies normalized credentials/session
// data but cannot accidentally select Direct traversal.
func (graph *FormGraph) AuthorizeMain(
	ctx context.Context,
	request forwardapp.Request,
) (forwardapp.Decision, error) {
	if graph == nil || graph.mainKernel == nil {
		return forwardapp.Decision{}, ErrInvalidGraph
	}
	request.Traversal = forwardapp.MainTraversal
	return graph.mainKernel.Authorize(ctx, request)
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

func (graph *FormGraph) Close() error {
	if graph == nil {
		return nil
	}
	graph.closeOnce.Do(func() {
		if graph.redis != nil {
			graph.closeErr = graph.redis.Close()
		}
	})
	return graph.closeErr
}

func compiledAttemptConfig(prefix string, key []byte) authattempt.Config {
	// These named security defaults intentionally remain code-owned in Form-v1
	// to avoid another collection of lightly understood knobs. Production mount
	// still requires capacity/load evidence and an explicit reviewed change if
	// the measured workload cannot use these exact limits.
	return authattempt.Config{
		KeyPrefix:            prefix,
		KeySecret:            append([]byte(nil), key...),
		Global:               authattempt.Policy{MaxAttempts: 1000, Window: time.Minute},
		FormBegin:            authattempt.Policy{MaxAttempts: 20, Window: time.Minute},
		FormCredentialClient: authattempt.Policy{MaxAttempts: 5, Window: time.Minute},
		FormCredentialLogin:  authattempt.Policy{MaxAttempts: 25, Window: time.Minute},
		OIDCBegin:            authattempt.Policy{MaxAttempts: 20, Window: time.Minute},
		OIDCCallback:         authattempt.Policy{MaxAttempts: 30, Window: time.Minute},
	}
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

func credentialHeaders(config []CredentialHeaderConfig) []browserapi.CredentialHeader {
	headers := make([]browserapi.CredentialHeader, 0, len(config))
	for _, header := range config {
		headers = append(headers, browserapi.CredentialHeader{Name: header.Name, Type: header.Type})
	}
	return headers
}

func mainPublicRules(config Config, routeOwned []forwardapp.PublicRule) ([]forwardapp.PublicRule, error) {
	configured, err := config.MainConfiguredPublicRules()
	if err != nil {
		return nil, err
	}
	if routeOwned == nil || len(configured)+len(routeOwned) > forwardapp.MaxPublicRules {
		return nil, fmt.Errorf("%w: Main public rules", ErrInvalidGraph)
	}
	rules := make([]forwardapp.PublicRule, 0, len(configured)+len(routeOwned))
	rules = append(rules, configured...)
	rules = append(rules, routeOwned...)
	return rules, nil
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
