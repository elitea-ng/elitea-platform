package api

import (
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/browserauth"
	agentexecutionapi "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/agentexecution"
	applicationskillsapi "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/applicationskills"
	configurationapi "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/configurations"
	deepwikiapi "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/deepwiki"
	indexingapi "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/indexing"
	indextypesapi "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/indextypes"
	inventoryapi "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/inventory"
	notificationsapi "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/notifications"
	projectinfoapi "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/projectinfo"
	v2projects "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/projects"
	promptcontextreadsapi "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/promptcontextreads"
	v2social "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/social"
)

var ErrInvalidProductionAuthRoutes = errors.New("invalid production authentication routes")

// ProductionAuthRoutes is constructed atomically so a caller cannot mount a
// browser login surface without the gateway edge that authorizes the current
// Main, or vice versa.
type ProductionAuthRoutes struct {
	browser http.Handler
	main    http.Handler
}

func NewProductionAuthRoutes(browser, main http.Handler) (*ProductionAuthRoutes, error) {
	if browser == nil || main == nil {
		return nil, ErrInvalidProductionAuthRoutes
	}
	return &ProductionAuthRoutes{browser: browser, main: main}, nil
}

// NewRouter builds the single production route composition. It used to
// branch on prototypeCompatibilityRequested(cfg) between this function's own
// inline "reviewed production router" build (mountReviewedProductionRoutes +
// mountArtifactRoutes, gated on cfg.Current* only) and newProductionRouter's
// broader registration set. That predicate is true whenever any of
// Auth.Client/Validator/SessionHandler/OIDCHandler, AppsRepo, SkillsRepo,
// FoldersRepo, TagsRepo, AnalyticsRepo, ConvsRepo, WebhookRepo, EventSource,
// or LLMProxy is set — and cmd/elitea-main/main.go's composition root always
// sets AppsRepo, ConvsRepo, SkillsRepo, FoldersRepo, TagsRepo, AnalyticsRepo,
// and WebhookRepo. Every real deployment therefore always took the
// newProductionRouter branch; the inline build was unreachable dead code
// (#243). It is removed here rather than made reachable: newProductionRouter
// already calls mountReviewedProductionRoutes and mountArtifactRoutes
// unconditionally, so the route surface for a main.go-shaped config is
// unchanged.
func NewRouter(cfg RouterConfig) chi.Router {
	return newProductionRouter(cfg)
}

// mountReviewedProductionRoutes is the single registration source for every
// current-compatibility route whose production authorization contract has been
// reviewed. Hybrid deployments add broad parity repositories, but those
// additions must never remove or replace these admitted routes.
//
// It does not register promptcontextreadsapi.CurrentProjectContextPath:
// per internal/api/v2/promptcontextreads/CURRENT_PARITY_EVIDENCE.md, the
// router every real deployment reaches ("the compatibility router") has
// always mounted the chat-config path only, deferring project-context to
// newProductionRouter's own coreHandler.ProjectContext (router.go) even
// when CurrentPromptContextReads is composed — CurrentPromptContextReads'
// own project-context handler is real and RBAC-scoped, but was never wired
// to this exact HTTP path outside the dead branch #243 removed. Registering
// it here too would double-register the same method+path (chi panics on
// that) and would change real production's project-context default (its
// parity-verified default is enabled:true; coreHandler's prototype-stub
// default is enabled:false) — a behavior change caught by a visual
// regression test, not something this cleanup is meant to do.
func mountReviewedProductionRoutes(r chi.Router, cfg RouterConfig) {
	if cfg.ProductionAuth != nil {
		// Exactly one browser-auth plane may own /forward-auth. router.go mounts
		// the OIDC session lifecycle on that prefix when SessionHandler is
		// configured, and browserauth.BasePath is the same string — so composing
		// both planes panicked chi at startup ("attempting to Mount() a handler
		// on an existing path"). Three of their paths genuinely collide with
		// different semantics: /login (Form login page vs OIDC redirect),
		// /logout, and /auth_form/logout.
		//
		// That combination is not exotic: ELITEA_RUNTIME_ENABLED requires
		// production auth (cmd/elitea-main/main.go:686-688), so any OIDC
		// deployment that turns the runtime on lands here — which is what the
		// standalone stack does (#281). main.go's comment claiming the two "can
		// coexist" described an intent the router never implemented.
		//
		// OIDC wins the browser prefix, because it is the plane a browser in
		// such a deployment actually authenticates through. The internal
		// forward-auth endpoint below is NOT skipped: it is a distinct path, the
		// edge's identity check depends on it, and it is what the runtime's
		// ForwardedIdentityVerifier is paired with. Single-plane deployments are
		// unaffected in either direction.
		//
		// cfg.SessionHandler, not cfg.Auth.SessionHandler: router.go:371 folds the
		// latter into the former before reaching here, and the OIDC mount at
		// router.go:408 tests exactly this field. Testing the other one would
		// miss a caller that sets the top-level field directly and panic again.
		if cfg.SessionHandler == nil {
			r.Mount(browserauth.BasePath, cfg.ProductionAuth.browser)
		}
		// This address is reached only by the gateway's ForwardAuth middleware;
		// deployment routing must never expose it as a product route.
		r.Method(http.MethodGet, browserauth.MainForwardAuthPath, cfg.ProductionAuth.main)
	}
	if cfg.CurrentProjectList != nil {
		r.Method(http.MethodGet, v2projects.CurrentProjectListPath, cfg.CurrentProjectList)
	}
	if cfg.CurrentSocialAuthors != nil {
		r.Method(http.MethodGet, v2social.CurrentAuthorsPath, cfg.CurrentSocialAuthors)
		r.Method(http.MethodGet, v2social.CurrentAuthorsDefaultPath, cfg.CurrentSocialAuthors)
	}
	if cfg.CurrentSocialAvatar != nil {
		r.Method(http.MethodGet, v2social.CurrentAvatarPath, cfg.CurrentSocialAvatar)
		r.Method(http.MethodPut, v2social.CurrentAvatarPath, cfg.CurrentSocialAvatar)
	}
	if cfg.CurrentProjectInfo != nil {
		r.Method(http.MethodGet, projectinfoapi.CurrentProjectInfoPath, cfg.CurrentProjectInfo)
	}
	if cfg.CurrentIndexTypes != nil {
		r.Method(http.MethodGet, indextypesapi.CurrentIndexTypesPath, cfg.CurrentIndexTypes)
	}
	if cfg.CurrentApplicationSkills != nil {
		r.Method(http.MethodGet, applicationskillsapi.CurrentApplicationSkillsPath, cfg.CurrentApplicationSkills)
	}
	if cfg.CurrentPromptContextReads != nil {
		r.Method(http.MethodGet, promptcontextreadsapi.CurrentChatConfigPath, cfg.CurrentPromptContextReads)
	}
	if cfg.CurrentConfigurationAvailable != nil {
		r.Method(http.MethodGet, configurationapi.CurrentAvailablePath, cfg.CurrentConfigurationAvailable)
		r.Method(http.MethodGet, configurationapi.CurrentAvailableSlashPath, cfg.CurrentConfigurationAvailable)
		r.Method(http.MethodGet, configurationapi.CurrentAvailableProjectPath, cfg.CurrentConfigurationAvailable)
	}
	if cfg.CurrentConfigurationRead != nil {
		r.Method(http.MethodGet, configurationapi.CurrentConfigurationListPath, cfg.CurrentConfigurationRead)
		r.Method(http.MethodGet, configurationapi.CurrentConfigurationDetailsPath, cfg.CurrentConfigurationRead)
	}
	if cfg.CurrentConfigurationTypes != nil {
		r.Method(http.MethodGet, configurationapi.CurrentConfigurationTypesPath, cfg.CurrentConfigurationTypes)
	}
	if cfg.CurrentConfigurationMutation != nil {
		r.Method(http.MethodPost, configurationapi.CurrentConfigurationListPath, cfg.CurrentConfigurationMutation)
		r.Method(http.MethodPut, configurationapi.CurrentConfigurationDetailsPath, cfg.CurrentConfigurationMutation)
		r.Method(http.MethodDelete, configurationapi.CurrentConfigurationDetailsPath, cfg.CurrentConfigurationMutation)
	}
	if cfg.ProductionRuntime != nil {
		r.Method(http.MethodPost, "/api/v2"+runtimeValidationPath, cfg.ProductionRuntime.validation)
		r.Method(http.MethodGet, "/api/v2"+runtimeEventsPath, cfg.ProductionRuntime.executionEvents)
	}
	if cfg.CurrentIndexStart != nil {
		r.Method(http.MethodPost, indexingapi.CurrentIndexStartPath, cfg.CurrentIndexStart)
	}
	// The DeepWiki facade. Each method is registered separately because the
	// two that share the invocation path do NOT share a permission: polling is
	// a read and cancelling is not.
	if cfg.DeepWiki != nil {
		r.Method(http.MethodGet, deepwikiapi.SlotsPath, cfg.DeepWiki)
		r.Method(http.MethodPost, deepwikiapi.InvokePath, cfg.DeepWiki)
		r.Method(http.MethodGet, deepwikiapi.InvocationPath, cfg.DeepWiki)
		r.Method(http.MethodDelete, deepwikiapi.InvocationPath, cfg.DeepWiki)
	}
	// The SECOND provider facade, mounted the same way as the first. Four
	// lines, because providerhost/{facade,proxy,spi} carry everything the two
	// have in common — which is what ADR-0012's budget was measuring.
	if cfg.Inventory != nil {
		r.Method(http.MethodGet, inventoryapi.SlotsPath, cfg.Inventory)
		r.Method(http.MethodPost, inventoryapi.InvokePath, cfg.Inventory)
		r.Method(http.MethodGet, inventoryapi.InvocationPath, cfg.Inventory)
		r.Method(http.MethodDelete, inventoryapi.InvocationPath, cfg.Inventory)
	}
	if cfg.CurrentAgentStart != nil {
		r.Method(http.MethodPost, agentexecutionapi.CurrentApplicationStartPath, cfg.CurrentAgentStart)
		r.Method(http.MethodPost, agentexecutionapi.CurrentRegenerationPath, cfg.CurrentAgentStart)
		r.Method(http.MethodPost, agentexecutionapi.CurrentContinuationPath, cfg.CurrentAgentStart)
	}
	if cfg.CurrentAgentCancel != nil {
		r.Method(http.MethodDelete, agentexecutionapi.CurrentAgentCancelPath, cfg.CurrentAgentCancel)
	}
	// One handler, two verbs, two permissions. They are registered separately
	// because the route resolves a different permission per verb — polling is a
	// read and stopping is not — the same reason the DeepWiki facade above
	// registers its shared path twice.
	if cfg.CurrentApplicationTask != nil {
		r.Method(http.MethodGet, agentexecutionapi.CurrentApplicationTaskPath, cfg.CurrentApplicationTask)
		r.Method(http.MethodDelete, agentexecutionapi.CurrentApplicationTaskPath, cfg.CurrentApplicationTask)
	}
	if cfg.CurrentIndexCancel != nil {
		r.Method(http.MethodDelete, indexingapi.CurrentIndexCancelPath, cfg.CurrentIndexCancel)
	}
	if cfg.CurrentIndexMeta != nil {
		r.Method(http.MethodGet, indexingapi.CurrentIndexMetaListPath, cfg.CurrentIndexMeta)
	}
	if cfg.CurrentIndexMetaDelete != nil {
		r.Method(
			indexingapi.SourceOnlyIndexDeleteMethod,
			indexingapi.CurrentIndexMetaDeletePath,
			cfg.CurrentIndexMetaDelete,
		)
	}
	if cfg.CurrentIndexScheduleUpdate != nil {
		r.Method(
			indexingapi.SourceOnlyIndexScheduleMethod,
			indexingapi.SourceOnlyIndexSchedulePath,
			cfg.CurrentIndexScheduleUpdate,
		)
	}
	if cfg.CurrentIndexScheduleDelete != nil {
		r.Method(
			indexingapi.SourceOnlyIndexScheduleDeleteMethod,
			indexingapi.SourceOnlyIndexScheduleDeletePath,
			cfg.CurrentIndexScheduleDelete,
		)
	}
	if cfg.CurrentNotifications != nil {
		r.Method(http.MethodGet, notificationsapi.CurrentNotificationsPath, cfg.CurrentNotifications)
		r.Method(http.MethodPut, notificationsapi.CurrentNotificationsPath, cfg.CurrentNotifications)
		r.Method(http.MethodDelete, notificationsapi.CurrentNotificationsPath, cfg.CurrentNotifications)
		r.Method(http.MethodGet, notificationsapi.CurrentNotificationPath, cfg.CurrentNotifications)
		r.Method(http.MethodPut, notificationsapi.CurrentNotificationPath, cfg.CurrentNotifications)
		r.Method(http.MethodDelete, notificationsapi.CurrentNotificationPath, cfg.CurrentNotifications)
	}
	if cfg.CurrentNotificationEvents != nil {
		r.Method(
			http.MethodGet,
			notificationsapi.CurrentNotificationEventsPath,
			cfg.CurrentNotificationEvents,
		)
	}
	if cfg.CurrentModelCatalog != nil {
		r.Method(http.MethodGet, configurationapi.CurrentModelCatalogPath, cfg.CurrentModelCatalog)
	}
	if cfg.CurrentModelDefault != nil {
		r.Method(http.MethodPost, configurationapi.CurrentModelDefaultPath, cfg.CurrentModelDefault)
	}
	// /llm is mounted exclusively by newProductionRouter (see its "/llm has one
	// composed backend" comment) — the sole caller of this function, so a
	// second /llm registration here would double-mount and panic (chi
	// disallows mounting the same pattern twice).
	//
	// This function used to add a last-resort `r.Handle("/llm/*", ...)` for the
	// LiteLLM facade whenever neither gateway backend was composed. That facade
	// is gone: the Bifrost gateway reads per-project credentials and models
	// from p_{projectID}.configuration directly, so there is no push-model
	// proxy left to fall back to. Nothing may reintroduce that fallback.
	//
	// With no backend composed, NewRouter registers the /llm path with the
	// not-configured handler, which answers 503 llm_gateway_not_configured
	// (issue #463). This comment previously said the path was left unregistered
	// so that a missing LLM_GATEWAY_URL "fails visibly". A 404 is not visible:
	// it is the same answer a misspelt path gets, and the chart shipped the
	// empty value by default, so every Kubernetes install answered it. The 503
	// keeps the refusal and names the variable that causes it.
}
