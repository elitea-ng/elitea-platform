package supportassistant

// The HIDDEN SUPPORT PROJECT: how it is resolved, how it is created, and how a
// user comes to hold a role in it.
//
// The reference does all three in `module.py` at plugin load: `ready()` calls
// `_ensure_support_project()`, which creates the project once and memoises the
// id in `descriptor.state`. That shape does not survive here, for two reasons
// worth stating rather than working around silently:
//
//   - THERE IS NO SINGLE PROCESS. This service runs as N replicas, and a
//     create-if-missing at boot in N processes creates up to N projects. The
//     bootstrap below runs under a Postgres advisory lock keyed on this feature
//     and RE-READS the stored id inside the lock, so the second replica through
//     finds the first one's project instead of making its own.
//
//   - IT IS LAZY, NOT AT BOOT. A boot-time create would run on every deployment
//     of every operator who has never enabled the assistant, and would make this
//     service's startup depend on the provisioning pipeline. It runs on the
//     first support request after an operator turns the switch on, which is the
//     first moment the project is actually needed.

import (
	"context"
	"errors"
	"net/http"
	"strconv"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/platformconfig"
)

// Provisioner is the project-create pipeline, narrowed to the one call this
// package makes. It is satisfied by
// `internal/application/projectprovisioning.Provisioner`.
//
// It is an INTERFACE rather than a direct import so that this package can be
// tested without a provisioning pipeline, and so a deployment that wires no
// provisioner degrades to "operator must name an existing project" instead of
// failing to build.
type Provisioner interface {
	Provision(context.Context, ProvisionRequest) (int64, error)
}

// ProvisionRequest is the narrowed create instruction. The adapter in
// `internal/api/composition` widens it to the provisioner's own Request.
type ProvisionRequest struct {
	Name       string
	OwnerID    int64
	AdminEmail string
}

// SupportProjectName is the name the hidden project is created with, verbatim
// from `module.py`'s `project_name = 'Support Assistant'`. It is a constant
// because the bootstrap looks the project up by it when the stored id is gone —
// an operator who clears the id must not get a second project.
const SupportProjectName = "Support Assistant"

// SystemUserEmail owns the hidden project, matching the reference's
// `system_user = "system@centry.user"`. This is the PLATFORM system identity,
// which `internal/api/v2/admin/users.go`'s `systemUserPredicate` already knows
// to hide from the admin user listing — so the support project's owner does not
// appear as a mysterious extra person on that page.
const SystemUserEmail = "system@centry.user"

// ErrNoProvisioner is returned when the assistant is enabled, no project id has
// been stored, and no provisioner is wired. It is not surfaced to a caller: the
// settings read logs it and reports the assistant as not ready, which renders as
// "no widget" rather than as an error the user cannot act on.
var ErrNoProvisioner = errors.New("support assistant: no project provisioner configured")

// ---------------------------------------------------------------------------
// request context
// ---------------------------------------------------------------------------

type contextKey struct{}

// withSettings publishes the resolved settings for the rest of the chain.
//
// They travel on the CONTEXT rather than being re-read per handler for one
// reason that is about correctness, not cost: the permission gate and the
// handler must agree about which project this request is for. Two independent
// reads can straddle an operator's save and disagree, and a request authorized
// against one project and executed against another is the whole class of bug
// this package's server-side resolution exists to make impossible.
func withSettings(ctx context.Context, settings platformconfig.SupportAssistant) context.Context {
	return context.WithValue(ctx, contextKey{}, settings)
}

// settingsFromContext reads back what withSettings published.
func settingsFromContext(ctx context.Context) (platformconfig.SupportAssistant, bool) {
	settings, ok := ctx.Value(contextKey{}).(platformconfig.SupportAssistant)
	return settings, ok
}

// projectIDFromContext is the ProjectIDExtractor the permission gate uses.
//
// It reports `false` when no settings are on the context, and false makes
// `RequireResolvedPermissionsForProject` refuse. That is the correct failure
// direction and it is load-bearing: if the middleware order were ever inverted
// so the gate ran before `resolve`, this returns false and every route 403s
// loudly, rather than resolving against an empty project id and letting the
// request through.
func projectIDFromContext(r *http.Request) (string, bool) {
	settings, ok := settingsFromContext(r.Context())
	if !ok || settings.ProjectID <= 0 {
		return "", false
	}
	return strconv.FormatInt(settings.ProjectID, 10), true
}
