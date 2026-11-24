package adminsurface

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/adminaccess"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api"
	apimw "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/middleware"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/oapiserver"
	v2adminconsole "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/adminaccess"
	v2agentidentity "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/agentidentity"
	v2configurations "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/configurations"
	v2credentialconsent "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/credentialconsent"
	v2credentialgrants "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/credentialgrants"
	v2credentialprobe "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/credentialprobe"
	v2credentialreferences "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/credentialreferences"
	v2mandateattachments "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/mandateattachments"
	v2retention "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/retention"
	v2safety "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/safety"
	configurationapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/configurations"
	safetyapp "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/application/safety"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/audit"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
	dbrepos "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/repos"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/nativeauth"
)

// refusedDatabase names a database no connection reaches: port 1 of the
// loopback address refuses at once. pgxpool.New does not connect.
const refusedDatabase = "postgres://adminsurface@127.0.0.1:1/adminsurface?sslmode=disable&connect_timeout=1"

// Token is the bearer credential the composed router accepts for the
// synthetic principal. NativeToken is the same principal signed in through a
// native device session (desktop, mobile). It accepts no other.
const (
	Token       = "adminsurface-principal"
	NativeToken = "adminsurface-native-session"
)

// NativeClientID is the native client of the NativeToken session.
const NativeClientID = "adminsurface-native-client"

// Principal is the synthetic principal every driven request carries.
func Principal() auth.User {
	return auth.User{ID: "1", UserID: "1", Email: "adminsurface@test.local", AuthType: "token"}
}

// NativePrincipal is Principal signed in through a native device session: the
// Auth middleware classes a bearer principal with a native client as
// auth.CredentialKindNative.
func NativePrincipal() auth.User {
	user := Principal()
	user.NativeClientID = NativeClientID
	return user
}

// Composed is the composed production router with the recording resolver on
// every route, and the routes it mounts.
type Composed struct {
	Handler  http.Handler
	Resolver *Resolver
	Routes   []Route
}

// ComposedRouter builds the router of TestProductionRouterMatchesMainComposedRouteSurface
// (internal/api/production_router_test.go): the RouterConfig shape
// cmd/elitea-main always produces. On top of it: a credential for the
// synthetic principal, a project membership that always answers yes, and the
// recording resolver in the three resolver slots of RouterConfig.
//
// The pool is a real one whose every connection attempt is refused at once,
// not the zero pgxpool.Pool that test uses. The zero pool panics on its first
// query, and the maintenance middleware queries on the first request after its
// ten-second cache expires: which route that is depends on timing, so a route
// would turn unreached at random. A refused connection is an error every
// caller already handles, the same on every request.
//
// Keep its repository fields in step with that test: a field set there and not
// here is a route group Drive never sees.
func ComposedRouter() (Composed, error) {
	resolver := &Resolver{}
	pool, err := pgxpool.New(context.Background(), refusedDatabase)
	if err != nil {
		return Composed{}, err
	}
	suspensionHandler, err := v2safety.NewHandler(refusedSuspensions{})
	if err != nil {
		return Composed{}, err
	}
	// The reviewed Configurations routes, which shadow the compatibility
	// routes at the mode-less row patterns wherever cmd/elitea-main composes
	// them (#3024): Drive reads the gate production serves there. They carry
	// their own authentication, with this router's credential.
	configurationAuth := apimw.AuthConfig{Validator: tokenValidator{}, PrincipalValidator: principalValidator{}}
	configurationRead, err := v2configurations.NewCurrentConfigurationReadRoute(
		guardedSettingRows{}, 1, configurationAuth, resolver)
	if err != nil {
		return Composed{}, err
	}
	configurationMutation, err := v2configurations.NewCurrentConfigurationMutationRoute(
		acceptedMutations{}, guardedSettingRows{}, configurationAuth, resolver)
	if err != nil {
		return Composed{}, err
	}
	router := api.NewRouter(api.RouterConfig{
		Pool:                   pool,
		AppsRepo:               dbrepos.NewApplicationsRepo(pool),
		ConvsRepo:              dbrepos.NewConversationsRepo(pool),
		SkillsRepo:             dbrepos.NewSkillsRepo(pool),
		FoldersRepo:            dbrepos.NewFoldersRepo(pool),
		EntityFoldersRepo:      dbrepos.NewEntityFoldersRepo(pool),
		ConversationSkillsRepo: dbrepos.NewConversationSkillsRepo(pool),
		SkillTrustRepo:         dbrepos.NewSkillTrustRepo(pool),
		MemoriesRepo:           dbrepos.NewMemoriesRepo(pool),
		TagsRepo:               dbrepos.NewTagsRepo(pool),
		AnalyticsRepo:          dbrepos.NewAnalyticsRepo(pool),
		WebhookRepo:            dbrepos.NewWebhooksRepo(pool),
		SharedChatStore:        dbrepos.NewSharedChatLinksRepo(pool),
		SharedChatTranscript:   dbrepos.NewSharedChatLinksRepo(pool),
		NativeClients:          nativeauth.NewRegistry(nil, pool),
		NativeStore:            nativeauth.NewStore(pool, nativeauth.Config{}),
		// No database holds a maintenance window here: a variant with
		// Maintenance set opens one for its own requests.
		MaintenanceState: MaintenanceState,

		// Administration route groups the router mounts only when their
		// handler is set. cmd/elitea-main always sets RetentionAdmin, and sets
		// the four agent identity handlers with the governance flag on. Zero
		// handlers are enough: Drive reads the route gates in front of them,
		// and a handler that has nothing behind it answers the same way to
		// every request (ADR-0059 R0.1c: the action table is checked against
		// these routes too).
		RetentionAdmin:             v2retention.NewHandler(dbrepos.NewRetentionPolicyRepo(pool)),
		AgentIdentityAdmin:         &v2agentidentity.Handler{},
		ExternalAgentIdentityAdmin: &v2agentidentity.ExternalHandler{},
		MandateAttachments:         &v2mandateattachments.Handler{},
		CredentialReferencesAdmin:  &v2credentialreferences.Handler{},
		ConsoleRoles:               noConsoleRoles{},

		// Route groups cmd/elitea-main sets that need no external service
		// here (ADR-0059 #2985; cmd/elitea-main surface_router_fields_test.go
		// lists every field production sets and this literal does not, each
		// with a reason). The credential use grants, the probe and the consent
		// flow decide in the default mode against {projectID}: set, Drive sees
		// that they make no administration decision instead of not seeing them.
		CredentialGrantsAdmin: &v2credentialgrants.Handler{},
		CredentialProbe:       &v2credentialprobe.Handler{},
		CredentialConsent:     &v2credentialconsent.Handler{},
		// The kill switch: the admin routes are mounted either way, but a nil
		// handler answers 503 where production's reaches its service.
		ExecutionSuspensions: suspensionHandler,
		// The gateway status reader: production reads elitea-llm-gateway over
		// gRPC; this one answers that the gateway is unreachable, which the
		// status route reports as a result. Set, the budget defaults answer
		// with cost budgets on, as production does.
		GatewayStatus: unreachableGateway{},
		// Production composes these with ELITEA_CONFIGURATIONS_ENABLED (the
		// read) and ELITEA_CONFIGURATIONS_MUTATION_ENABLED (the writes).
		CurrentConfigurationRead:     configurationRead,
		CurrentConfigurationMutation: configurationMutation,

		// The Postgres recorder writes from a goroutine of its own, which
		// panics on the unconnected pool.
		AuditRecorder:              discardAudit{},
		AuthValidator:              tokenValidator{},
		PrincipalValidator:         principalValidator{},
		ProjectAccessQuerier:       memberOfEveryProject{},
		PermissionResolver:         resolver,
		ProjectPermissionResolver:  resolver,
		ArtifactPermissionResolver: resolver,
	})
	set, err := oapiserver.CollectRoutes(router)
	if err != nil {
		return Composed{}, err
	}
	var routes []Route
	for _, p := range set.Patterns() {
		method, pattern, _ := strings.Cut(p, " ")
		routes = append(routes, Route{Method: method, Pattern: pattern})
	}
	return Composed{Handler: router, Resolver: resolver, Routes: routes}, nil
}

// The Configuration routes. The value handler resolves the permission a
// section declares only after it has found the section, so the value routes
// are driven once per section.
const (
	ConfigurationSchemas     = "/api/v2/admin/plugin_config_schemas/{mode}"
	ConfigurationValues      = "/api/v2/admin/plugin_config_values/administration/{plugin}"
	ConfigurationSuggestions = "/api/v2/admin/plugin_config_suggestions/{mode}/{key}"
)

// Options are the options Drive takes for the composed router: the permission
// table of internal/adminaccess as the probed universe, the synthetic
// principal's credential, and one variant per Configuration section on the
// two Configuration value routes. The sections are read from the router's own
// schema list, not from the source.
func (c Composed) Options() (Options, error) {
	opts := Options{
		Registry:     adminaccess.Names(),
		Authenticate: authenticate,
		Variants:     map[string][]Variant{},
	}
	sections, err := c.ConfigurationSections()
	if err != nil {
		return Options{}, err
	}
	for _, method := range []string{http.MethodGet, http.MethodPut} {
		route := Route{Method: method, Pattern: ConfigurationValues}.String()
		for _, section := range sections {
			opts.Variants[route] = append(opts.Variants[route], section.Variant())
		}
	}
	// A route that multiplexes actions on its body is driven once per action
	// the action table declares, with the body that selects it.
	for _, action := range adminaccess.ActionTable().Entries() {
		if action.Selector.In != adminaccess.SelectBody {
			continue
		}
		variant := Variant{Name: "action " + action.Selector.Value, Body: action.Selector.Body()}
		for _, mounted := range c.Routes {
			if mounted.Method != action.Method || adminaccess.RoutePattern(mounted.Pattern) != action.Pattern {
				continue
			}
			route := mounted.String()
			if !slices.ContainsFunc(opts.Variants[route], func(v Variant) bool { return v.Name == variant.Name }) {
				opts.Variants[route] = append(opts.Variants[route], variant)
			}
		}
	}
	// The central settings branch decides on the configuration type a
	// request names (ADR-0059 R0.6b): the list's `type` filter and the
	// create's body. A request that names none, or a credential type, is
	// refused to every grant, so the matrix reads these routes with a guarded
	// setting type. The row routes read the stored type: guardedSettingRows
	// answers one for the reviewed routes, and the compatibility {mode} routes
	// read the database this router does not have.
	for route, variant := range CentralSettingsVariants() {
		opts.Variants[route.String()] = append(opts.Variants[route.String()], variant)
	}
	// The checks the default request does not reach (ADR-0059 #2925).
	for route, variants := range SessionAndWindowVariants() {
		opts.Variants[route.String()] = append(opts.Variants[route.String()], variants...)
	}
	return opts, nil
}

// The routes the variants of SessionAndWindowVariants drive.
var (
	// TokenMint mints a personal access token. Its administration-mode
	// check runs for a native device session only (v2/auth
	// token_mint_policy.go): a browser session mints without it and every
	// other credential is refused first.
	TokenMint = Route{Method: http.MethodPost, Pattern: "/api/v2/auth/token/"}
	// MarketplaceListingUpdate features or delists a listing. Its
	// moderation check (eliteacore holdsModerationPermission) sits behind
	// the project's agent list gate and after the body is read. The reads
	// ask the same function, but hand its answer to a query, so no grant
	// changes what this router answers there.
	MarketplaceListingUpdate = Route{Method: http.MethodPatch,
		Pattern: "/api/v2/elitea_core/marketplace_listing/prompt_lib/{projectID}/{listingID}"}
	// PlatformSettings tells the SPA whether a window is open and whether
	// the caller bypasses it (eliteacore holdsMaintenanceBypass).
	PlatformSettings = Route{Method: http.MethodGet, Pattern: "/api/v2/elitea_core/platform_settings/prompt_lib"}
	// MaintenanceUserRoute is a route outside the administration surface,
	// driven with a window open: the maintenance gate is its only
	// administration-mode check.
	MaintenanceUserRoute = Route{Method: http.MethodGet, Pattern: "/api/v2/elitea_core/applications/prompt_lib/{projectID}"}
	// MaintenanceAdministrationRoute is a route of the surface outside the
	// paths a window leaves open (/api/v2/admin and the others of
	// middleware.MaintenanceExempt), driven with a window open: the
	// maintenance gate asks first, then the route gate.
	MaintenanceAdministrationRoute = Route{Method: http.MethodPut,
		Pattern: "/api/v2/elitea_core/project_budget/administration/{projectID}/budget"}
)

// The variant names. None has a space, so none selects an action of the
// action table (Entry.SelectorValue).
const (
	VariantNativeSession = "native_session"
	VariantModeration    = "moderation"
	VariantMaintenance   = "maintenance_window"
)

// MarketplaceReadPermission is the default-mode permission of the Marketplace
// project gate (router.go, requireMarketplaceRead).
const MarketplaceReadPermission = "models.applications.applications.list"

// SessionAndWindowVariants are the variants that reach the checks a bearer
// request with the window closed does not: the token mint of a native
// session, the Marketplace moderation check, and the maintenance bypass in the
// gate and in the platform_settings read. A route listed here is driven with
// these variants only: the default one is listed where it reaches a check of
// its own.
func SessionAndWindowVariants() map[Route][]Variant {
	window := Variant{Name: VariantMaintenance, Maintenance: true}
	return map[Route][]Variant{
		TokenMint: {{Name: VariantNativeSession, Authenticate: AuthenticateNative}},
		MarketplaceListingUpdate: {{Name: VariantModeration, Body: `{"featured":true}`,
			OtherModes: []string{MarketplaceReadPermission}}},
		PlatformSettings:               {window},
		MaintenanceUserRoute:           {window},
		MaintenanceAdministrationRoute: {{}, window},
	}
}

// GuardedSettingType is the guarded project setting type the central
// settings variants name, and the stored type of every row guardedSettingRows
// answers.
const GuardedSettingType = "cloud_executor"

// VariantGuardedSetting names the variants of CentralSettingsVariants.
const VariantGuardedSetting = "type=" + GuardedSettingType

// CentralSettingsVariants are the variants of the configuration list and
// create routes, with and without the {mode} segment: each names the guarded
// setting type where the handler reads it.
func CentralSettingsVariants() map[Route]Variant {
	list := Variant{Name: VariantGuardedSetting, Query: "type=" + GuardedSettingType}
	create := Variant{Name: VariantGuardedSetting,
		Body: `{"elitea_title":"setting","type":"` + GuardedSettingType + `","data":{}}`}
	variants := map[Route]Variant{}
	for _, pattern := range []string{
		"/api/v2/configurations/configurations/{projectID}",
		"/api/v2/configurations/configurations/{mode}/{projectID}",
	} {
		variants[Route{Method: http.MethodGet, Pattern: pattern}] = list
		variants[Route{Method: http.MethodPost, Pattern: pattern}] = create
	}
	return variants
}

// guardedSettingRows is the reader of the reviewed Configurations routes:
// every row is a guarded setting, and every list is empty.
type guardedSettingRows struct{}

func (guardedSettingRows) List(
	context.Context, configurationapp.CurrentConfigurationListRequest,
) (configurationapp.CurrentConfigurationListResult, error) {
	return configurationapp.CurrentConfigurationListResult{}, nil
}

func (guardedSettingRows) Get(_ context.Context, projectID, id int32) (configurationapp.CurrentConfiguration, error) {
	return configurationapp.CurrentConfiguration{ID: id, ProjectID: projectID, Type: GuardedSettingType}, nil
}

// acceptedMutations is the mutator of the reviewed Configurations routes: it
// accepts every write the route gate and the handler's rules admitted.
type acceptedMutations struct{}

func (acceptedMutations) Create(
	_ context.Context, r configurationapp.CurrentConfigurationCreateRequest,
) (configurationapp.CurrentConfiguration, error) {
	return configurationapp.CurrentConfiguration{ProjectID: r.ProjectID, Type: r.Type}, nil
}

func (acceptedMutations) Update(
	_ context.Context, r configurationapp.CurrentConfigurationUpdateRequest,
) (configurationapp.CurrentConfiguration, error) {
	return configurationapp.CurrentConfiguration{ID: r.ConfigurationID, ProjectID: r.ProjectID}, nil
}

func (acceptedMutations) Delete(context.Context, configurationapp.CurrentConfigurationDeleteRequest) error {
	return nil
}

// Section is one Configuration section of the router's schema list.
type Section struct {
	ID string `json:"id"`
	// RequiredPermission is the section's own permission, which the value
	// handler checks after the route gate; "" for none.
	RequiredPermission string `json:"required_permission"`
}

// Variant answers the variant that drives the value routes for this section.
func (s Section) Variant() Variant {
	return Variant{Name: "section " + s.ID, Params: map[string]string{"plugin": s.ID}}
}

// ConfigurationSections answers the Configuration sections the router's
// schema route lists, read with every registered name granted.
func (c Composed) ConfigurationSections() ([]Section, error) {
	rec := &recorder{grant: func(int) []string { return adminaccess.Names() }}
	path := strings.ReplaceAll(ConfigurationSchemas, "{mode}", auth.PermissionModeAdministration)
	req := httptest.NewRequestWithContext(
		context.WithValue(context.Background(), recorderKey{}, rec), http.MethodGet, path, nil)
	authenticate(req)
	w := httptest.NewRecorder()
	c.Handler.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		return nil, fmt.Errorf("GET %s answered %d: %s", path, w.Code, truncate(w.Body.String()))
	}
	var schemas struct {
		Sections []Section `json:"sections"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &schemas); err != nil {
		return nil, fmt.Errorf("GET %s: %w", path, err)
	}
	var sections []Section
	for _, section := range schemas.Sections {
		if section.ID != "" {
			sections = append(sections, section)
		}
	}
	if len(sections) == 0 {
		return nil, fmt.Errorf("GET %s listed no section", path)
	}
	return sections, nil
}

func authenticate(r *http.Request) { r.Header.Set("Authorization", "Bearer "+Token) }

// AuthenticateNative adds the native device session credential of the
// synthetic principal.
func AuthenticateNative(r *http.Request) { r.Header.Set("Authorization", "Bearer "+NativeToken) }

// noConsoleRoles reads no role, so the console capability document answers
// from the resolution alone, which the probe varies.
type noConsoleRoles struct{}

func (noConsoleRoles) HeldRoles(context.Context, int64) ([]v2adminconsole.Role, error) {
	return nil, nil
}

func (noConsoleRoles) GrantingRoles(context.Context, []string) ([]v2adminconsole.GrantingRole, error) {
	return nil, nil
}

// errNoService is what the stand-ins for an external service answer.
var errNoService = errors.New("adminsurface: no such service in the composed router")

// refusedSuspensions is the kill switch service with nothing behind it: every
// call fails, as a store whose database refuses does.
type refusedSuspensions struct{}

func (refusedSuspensions) Create(context.Context, safetyapp.Actor, safetyapp.CreateRequest) (safetyapp.Suspension, error) {
	return safetyapp.Suspension{}, errNoService
}

func (refusedSuspensions) Resume(context.Context, safetyapp.Actor, safetyapp.ResumeRequest) (safetyapp.Suspension, error) {
	return safetyapp.Suspension{}, errNoService
}

func (refusedSuspensions) List(context.Context, safetyapp.Actor, safetyapp.ListFilter) ([]safetyapp.Suspension, error) {
	return nil, errNoService
}

// unreachableGateway is a gateway status reader whose gateway never answers.
type unreachableGateway struct{}

func (unreachableGateway) Status(context.Context) (json.RawMessage, error) { return nil, errNoService }

type discardAudit struct{}

func (discardAudit) Record(context.Context, audit.Event) {}

type tokenValidator struct{}

func (tokenValidator) ValidateToken(_ context.Context, token string) (auth.User, error) {
	switch token {
	case Token:
		return Principal(), nil
	case NativeToken:
		return NativePrincipal(), nil
	}
	return auth.User{}, errors.New("adminsurface: not a credential of the synthetic principal")
}

type principalValidator struct{}

func (principalValidator) ValidatePrincipal(_ context.Context, user auth.User) (auth.User, error) {
	return user, nil
}

// memberOfEveryProject answers RequireProjectAccess's query: a member, of a
// project that exists.
type memberOfEveryProject struct{}

func (memberOfEveryProject) QueryRow(context.Context, string, ...any) pgx.Row { return yesRow{} }

type yesRow struct{}

func (yesRow) Scan(dest ...any) error {
	for _, d := range dest {
		if b, ok := d.(*bool); ok {
			*b = true
		}
	}
	return nil
}
