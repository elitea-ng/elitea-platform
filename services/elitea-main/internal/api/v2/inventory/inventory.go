// Package inventory is the elitea-main facade for the Inventory provider
// service — the SECOND provider, and the falsification test for ADR-0012's
// runner generalisation.
//
// ADR-0012 set the measure before the work: this facade must fit in ≤8 files
// and ≤250 net non-test lines outside values.yaml. Over budget means the
// generalisation did not land, and the fix belongs in the shared packages
// rather than here. That sentence sat here unenforced until H4c I2;
// internal/api/v2/budget_test.go is the gate now, and it counts.
//
// WHAT INVENTORY NEEDS THAT DEEPWIKI DOES NOT: nothing. It speaks the same SPI
// — its legacy plugin has the same methods/descriptor.py, the same five routes,
// and its recorded descriptor has the same four top-level keys (captured at
// conformance/provider/fixtures/inventory/).
//
// A CORRECTION, kept rather than quietly deleted. This comment used to say
// that repository credential resolution, the git-host egress allowlist and the
// minted callback token were "correctly absent here" because Inventory clones
// nothing and calls nothing back. That was wrong, and reading the legacy
// plugin is what showed it: `run_ingestion` instantiates the SOURCE toolkit's
// SDK client and clones with its credentials, and uploads the graph it builds
// back to artifacts. Inventory needs all three — see sources.go. What the
// mistake did buy is the right shape: the mechanics went to
// internal/providerhost/material with two real callers in hand rather than
// being generalised from DeepWiki alone.
package inventory

import (
	"errors"
	"log/slog"
	"net/http"

	apimw "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/middleware"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/providerhost/facade"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/providerhost/proxy"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/providerhost/routes"
)

// Mode is the permission mode these routes resolve in. Inventory's legacy
// plugin authorised by the provider hop, so like DeepWiki's these grants are
// chosen rather than recovered — see migration 0108.
const Mode = "default"

const (
	// ReadPermission gates the capacity and invocation reads.
	ReadPermission = "models.applications.inventory.read"
	// InvokePermission gates starting and cancelling an invocation.
	InvokePermission = "models.applications.inventory.invoke"
)

// EnvNames are Inventory's own variables. Spelled out rather than built from a
// prefix: every one appears in a chart and in the env-drift allowlist, and both
// are searched by the literal string.
var EnvNames = facade.EnvNames{
	Enabled:        "ELITEA_INVENTORY_ENABLED",
	BaseURL:        "ELITEA_INVENTORY_BASE_URL",
	ClientCertFile: "ELITEA_INVENTORY_CLIENT_CERT_FILE",
	ClientKeyFile:  "ELITEA_INVENTORY_CLIENT_KEY_FILE",
	CAFile:         "ELITEA_INVENTORY_CA_FILE",
	ServerName:     "ELITEA_INVENTORY_SERVER_NAME",
	IdentitySecret: "ELITEA_INVENTORY_IDENTITY_SECRET",
	Timeout:        "ELITEA_INVENTORY_TIMEOUT_SECONDS",
}

// ErrInvalidRoute reports a facade that cannot be composed.
var ErrInvalidRoute = errors.New("invalid Inventory route")

// The facade's own paths. They carry {project_id} because the permission gate
// resolves against it; the provider's own paths do not, because the project
// travels in the signed identity headers.
//
// THE `/api/v2` PREFIX IS PART OF THE CONSTANT, exactly as it is in
// internal/api/v2/deepwiki. production_router.go mounts these strings on the
// router ROOT — `r.Method(http.MethodGet, SlotsPath, …)` — so a constant
// without the prefix is a route served at `/inventory/slots/{id}`, which the
// platform edge does not forward: it routes `/api/v2`. MEASURED on a running
// stack: the facade answered 200 to a request made directly against
// elitea-main's port and 404 to the same request through traefik, so every
// test in this package passed while no browser and no API client could reach
// the feature. See TestTheMountedPathsAreReachableThroughTheEdge.
const (
	SlotsPath      = "/api/v2/inventory/slots/{project_id}"
	InvokePath     = "/api/v2/inventory/tools/{project_id}/{toolkit_name}/{tool_name}/invoke"
	InvocationPath = "/api/v2/inventory/invocations/{project_id}/{toolkit_name}/{tool_name}/{invocation_id}"
)

// Route serves the Inventory facade.
type Route struct{ handler http.Handler }

// NewRoute mounts the three SPI paths behind their permissions.
//
// sources may be nil, and that is a deployment with no source expansion — no
// vault loader, or no callback origin — rather than a defect: the eight tools
// that only read the graph still work, and the three that name a source get
// the provider's own refusal instead of a facade that silently forwards an
// unexpanded id. The composition root logs which of the two it built.
func NewRoute(
	cfg facade.Config,
	authConfig apimw.AuthConfig,
	permissions auth.PermissionResolver,
	sources *Sources,
	logger *slog.Logger,
) (*Route, error) {
	if !facade.Composable(authConfig, permissions) {
		return nil, ErrInvalidRoute
	}
	hop, err := proxy.New(cfg, EnvNames.BaseURL, logger)
	if err != nil {
		return nil, err
	}
	handler, err := routes.Build(routes.Table{
		Invoke:           sources.invoke(hop.Forward, logger),
		SlotsPath:        SlotsPath,
		InvokePath:       InvokePath,
		InvocationPath:   InvocationPath,
		Mode:             Mode,
		ReadPermission:   ReadPermission,
		InvokePermission: InvokePermission,
		Auth:             authConfig,
		Permissions:      permissions,
		Forward:          hop.Forward,
		Admission:        cfg.Admission,
	})
	if err != nil {
		return nil, ErrInvalidRoute
	}
	return &Route{handler: handler}, nil
}

// ServeHTTP answers even for a zero Route, so a mount that half-happened
// returns a readable 503 rather than taking the process down.
func (route *Route) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if route == nil || route.handler == nil {
		http.Error(w, "Inventory is not enabled in this deployment.", http.StatusServiceUnavailable)
		return
	}
	route.handler.ServeHTTP(w, r)
}
