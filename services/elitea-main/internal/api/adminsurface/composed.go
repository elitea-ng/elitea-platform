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
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/oapiserver"
	v2agentidentity "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/agentidentity"
	v2credentialreferences "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/credentialreferences"
	v2mandateattachments "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/mandateattachments"
	v2retention "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/api/v2/retention"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/audit"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/auth"
	dbrepos "github.com/elitea-ng/elitea-platform/services/elitea-main/internal/infra/db/repos"
	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/nativeauth"
)

// refusedDatabase names a database no connection reaches: port 1 of the
// loopback address refuses at once. pgxpool.New does not connect.
const refusedDatabase = "postgres://adminsurface@127.0.0.1:1/adminsurface?sslmode=disable&connect_timeout=1"

// Token is the bearer credential the composed router accepts for the
// synthetic principal. It accepts no other.
const Token = "adminsurface-principal"

// Principal is the synthetic principal every driven request carries.
func Principal() auth.User {
	return auth.User{ID: "1", UserID: "1", Email: "adminsurface@test.local", AuthType: "token"}
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
		route := Route{Method: action.Method, Pattern: action.Pattern}.String()
		variant := Variant{Name: "action " + action.Selector.Value, Body: action.Selector.Body()}
		if !slices.ContainsFunc(opts.Variants[route], func(v Variant) bool { return v.Name == variant.Name }) {
			opts.Variants[route] = append(opts.Variants[route], variant)
		}
	}
	return opts, nil
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

type discardAudit struct{}

func (discardAudit) Record(context.Context, audit.Event) {}

type tokenValidator struct{}

func (tokenValidator) ValidateToken(_ context.Context, token string) (auth.User, error) {
	if token != Token {
		return auth.User{}, errors.New("adminsurface: not the synthetic principal's token")
	}
	return Principal(), nil
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
