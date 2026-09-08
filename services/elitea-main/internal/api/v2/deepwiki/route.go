package deepwiki

import (
	"errors"
	"log/slog"
	"net/http"
	"strconv"

	apimw "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/middleware"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/providerhost/facade"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/providerhost/material"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/providerhost/routes"
	"github.com/go-chi/chi/v5"
)

// Facade paths, matching api/openapi/v2.yaml.
const (
	SlotsPath      = "/api/v2/deepwiki/slots/{project_id}"
	InvokePath     = "/api/v2/deepwiki/tools/{project_id}/{toolkit_name}/{tool_name}/invoke"
	InvocationPath = "/api/v2/deepwiki/invocations/{project_id}/{toolkit_name}/{tool_name}/{invocation_id}"
)

// Permissions.
//
// Reading capacity and starting a generation are deliberately NOT the same
// grant: `/slots` is what the UI polls to decide whether to offer the button,
// and gating it behind the write permission would make the page 403 for
// everyone who may look but not generate.
const (
	Mode = auth.PermissionModeDefault

	// ReadPermission covers /slots and polling an invocation.
	ReadPermission = "models.applications.deepwiki.read"
	// GeneratePermission covers starting and cancelling one.
	GeneratePermission = "models.applications.deepwiki.generate"
)

// ErrInvalidRoute reports missing route dependencies.
var ErrInvalidRoute = errors.New("invalid DeepWiki route dependencies")

// Route serves the DeepWiki facade.
//
// Nil-safe by construction: production composition mounts it only when the
// feature is enabled AND configured, and a nil Route still answers rather than
// panicking, because a mount that half-happened must not take the process
// down with it.
type Route struct {
	handler http.Handler
}

// NewRoute builds the facade.
//
// Every dependency is checked, PrincipalValidator included. That check is not
// ceremony: a nil validator at a composition root is the recurring shape of
// authentication bypass in this codebase, and it is invisible at runtime
// because the route serves perfectly well without one — it just does not
// authenticate.
func NewRoute(
	cfg Config,
	authConfig apimw.AuthConfig,
	permissions auth.PermissionResolver,
	credentials *CredentialResolver,
	toolkits ToolkitReader,
	minter CallbackMinter,
	logger *slog.Logger,
	options ...Option,
) (*Route, error) {
	if !facade.Composable(authConfig, permissions) {
		return nil, ErrInvalidRoute
	}

	proxy, err := NewProxy(cfg, logger)
	if err != nil {
		return nil, err
	}

	// The rewriter is not optional. Without it the facade would forward a
	// body naming a configuration id the provider cannot read, so every
	// generation would fail on a payload the caller wrote correctly — and it
	// would do so having already passed authentication and permissions, which
	// is the point at which a defect stops looking like a defect.
	rewriter, err := NewInvokeRewriter(
		credentials, toolkits, minter, cfg.CallbackBaseURL, cfg.CallbackTokenTTL)
	if err != nil {
		return nil, err
	}
	if logger == nil {
		logger = slog.Default()
	}

	// The options are applied BEFORE the table is built, because one of them
	// (WithHistory) supplies the poll hop and the invoke wrapper the table
	// then mounts. A variadic tail rather than another positional parameter:
	// every caller of this constructor that does not record a transcript is
	// unchanged, and a facade whose composition root cannot build a
	// repository must still mount.
	settings := routeOptions{}
	for _, option := range options {
		option(&settings)
	}

	// The rewriting invoke handler, and — when a transcript is being kept —
	// the wrapper that reads the drawer's two headers off it. The wrapper is
	// applied to THIS handler and not to the whole router: /slots is polled
	// from the app shell and carries no conversation, so a router-wide
	// wrapper would warn about a missing conversation key several times a
	// minute for requests that have no business carrying one.
	invoke := material.Invocation{
		Provider: "DeepWiki",
		// PER TOOLKIT, not one rewrite for all three: `Wikis` names a
		// code toolkit, `wikis_query` names a Wikis toolkit, and
		// `wiki_query` names nothing at all (wikis.go). One rewrite
		// requiring code_toolkit refused the last two outright.
		RewriteFor: rewriter.For,
		Rewrite:    rewriter.Rewrite,
		Forward:    proxy.Forward,
		Path: func(r *http.Request) string {
			return providerInvokePath(
				chi.URLParam(r, "toolkit_name"), chi.URLParam(r, "tool_name"))
		},
		Minter: minter,
		Status: invokeError,
		Logger: logger,
		// The question half of the wiki chat transcript (history.go). Nil
		// when nothing records one, and material.Invocation then captures no
		// bodies at all.
		Observe: settings.history.Observer(),
	}.Serve

	// guard, the project accessor and the id rule all come from
	// providerhost/facade now: DeepWiki and Inventory are two callers of the
	// same three, which is the bar ADR-0012 set for extracting them.
	handler, err := routes.Build(routes.Table{
		SlotsPath:        SlotsPath,
		InvokePath:       InvokePath,
		InvocationPath:   InvocationPath,
		Mode:             Mode,
		ReadPermission:   ReadPermission,
		InvokePermission: GeneratePermission,
		Auth:             authConfig,
		Permissions:      permissions,
		Forward:          proxy.Forward,
		UserID:           userIDFrom,
		Admission:        cfg.Admission,
		// The one route this facade serves itself: the body is rewritten
		// (credentials expanded, a callback grant minted) before the hop.
		// The handler is the shared one; only the rewrite is DeepWiki's.
		Invoke: settings.history.WrapInvoke(invoke),
		// The answer half of the transcript. The terminal poll is the only
		// moment this facade sees what the provider produced, because the
		// browser drains its answer through this hop.
		Poll: settings.history.Poll(proxy.Forward),
	})
	if err != nil {
		return nil, ErrInvalidRoute
	}
	return &Route{handler: handler}, nil
}

// Option configures the facade beyond its dependencies.
type Option func(*routeOptions)

type routeOptions struct {
	history *History
}

// WithHistory records every wiki chat turn this facade serves. A nil History
// is accepted and records nothing — see NewHistory.
func WithHistory(history *History) Option {
	return func(o *routeOptions) { o.history = history }
}

// ServeHTTP answers even when the route was never built, so a deployment with
// DeepWiki disabled returns a readable 503 rather than a nil-pointer panic.
func (route *Route) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if route == nil || route.handler == nil {
		writeError(w, http.StatusServiceUnavailable,
			"DeepWiki is not enabled in this deployment.")
		return
	}
	route.handler.ServeHTTP(w, r)
}

// userIDFrom reads the authenticated user for the signed identity.
//
// RuntimePrincipalFromContext, not UserFromContext: the former requires the
// server-derived provenance marker that only the authentication middleware
// sets, so a context carrying a user placed there by some other path yields
// nothing rather than an identity this facade would then sign.
//
// Empty is acceptable and is what the signer receives for a caller whose
// principal carries no owning user; llmproxy omits the header rather than
// signing a blank one.
func userIDFrom(r *http.Request) string {
	if owner := material.OwnerID(r); owner > 0 {
		return strconv.FormatInt(owner, 10)
	}
	return ""
}
