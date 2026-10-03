package runtimecomposition

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	configurationapi "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/configurations"
	executionapi "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/api/v2/executions"
	executionapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/execution"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/db/sqlcgen"
	executiondomain "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/domain/execution"
)

type publicAdmissionAuthorizationQuerier interface {
	AuthorizeRuntimeValidationProject(context.Context, sqlcgen.AuthorizeRuntimeValidationProjectParams) (bool, error)
}

type publicOutputAuthorizationQuerier interface {
	ResolveRuntimeExecutionEventCapability(context.Context, sqlcgen.ResolveRuntimeExecutionEventCapabilityParams) (string, error)
}

// agentExecutionObserverAuthority binds an agent execution's event stream to
// the execution: its starter, or a principal who may read its conversation.
// repos.ExecutionObserverAuthority is the production implementation.
type agentExecutionObserverAuthority interface {
	MayObserveAgentExecution(ctx context.Context, project int64, executionID string, principal auth.User) (bool, error)
}

// postgresPublicAuthorizer accepts only a principal with server-derived
// authentication provenance. Forwarded identities are admitted only after the
// opaque production route has verified the proxy peer and reloaded the active
// principal. Development and provenance-free contexts are rejected before
// PostgreSQL. Validation admission preserves the current membership contract;
// execution observation additionally resolves the capability's exact RBAC
// permission whenever that policy is known.
type postgresPublicAuthorizer struct {
	admissionStore publicAdmissionAuthorizationQuerier
	outputStore    publicOutputAuthorizationQuerier
	permissions    auth.PermissionResolver
	observers      agentExecutionObserverAuthority
}

func newPostgresPublicAuthorizer(
	admissionStore publicAdmissionAuthorizationQuerier,
	outputStore publicOutputAuthorizationQuerier,
	permissions auth.PermissionResolver,
	observers agentExecutionObserverAuthority,
) (*postgresPublicAuthorizer, error) {
	if admissionStore == nil || outputStore == nil || permissions == nil || observers == nil {
		return nil, errors.New("runtime authorization databases, permission resolver and execution observer authority are required")
	}
	return &postgresPublicAuthorizer{
		admissionStore: admissionStore,
		outputStore:    outputStore,
		permissions:    permissions,
		observers:      observers,
	}, nil
}

func (a *postgresPublicAuthorizer) AuthorizeValidation(ctx context.Context, projectID, _ string) (executionapp.AdmissionIdentity, error) {
	project, user, ok := runtimePrincipal(ctx, projectID)
	if !ok {
		return executionapp.AdmissionIdentity{}, configurationapi.ErrValidationForbidden
	}
	authorized, err := a.admissionStore.AuthorizeRuntimeValidationProject(
		ctx,
		sqlcgen.AuthorizeRuntimeValidationProjectParams{
			ProjectID: int32(project),
			UserID:    int32(user),
		},
	)
	if err != nil {
		return executionapp.AdmissionIdentity{}, fmt.Errorf("authorize runtime validation project: %w", err)
	}
	if !authorized {
		return executionapp.AdmissionIdentity{}, configurationapi.ErrValidationForbidden
	}
	canonicalProject := strconv.FormatInt(project, 10)
	canonicalUser := strconv.FormatInt(user, 10)
	return executionapp.AdmissionIdentity{
		TenantID:            canonicalProject,
		ResourceProjectID:   canonicalProject,
		ProjectionProjectID: canonicalProject,
		ActorID:             canonicalUser,
	}, nil
}

// AuthorizeExecutionEvents authorizes one execution event stream against the
// capability that created the execution.
//
// The capability branches do not all authorize on the same kind of fact, and
// the difference is deliberate. Index ingest and agent execution each name the
// one permission that admits them. Configuration validation authorizes on
// project membership instead, because legacy admits a validation under either
// of two permissions and the admission record does not keep which one applied.
// No branch authorizes on the SIZE of the resolved permission set.
func (a *postgresPublicAuthorizer) AuthorizeExecutionEvents(ctx context.Context, projectID, executionID string) error {
	project, principal, ok := runtimePrincipalDetails(ctx, projectID)
	if !ok || executionID == "" || len(executionID) > 256 {
		return executionapi.ErrExecutionEventsForbidden
	}
	capabilityID, err := a.outputStore.ResolveRuntimeExecutionEventCapability(
		ctx,
		sqlcgen.ResolveRuntimeExecutionEventCapabilityParams{
			ExecutionID: executionID,
			ProjectID:   int32(project),
		},
	)
	if err != nil {
		return fmt.Errorf("resolve runtime execution event policy: %w", err)
	}
	if capabilityID == "" {
		return executionapi.ErrExecutionEventsForbidden
	}

	// Resolve on every poll for every capability. The resolution supplies the
	// named permission that two of the branches below need. It also revalidates
	// the active user, the token and the project, so a principal that went
	// inactive loses the stream at the next poll.
	resolution, err := a.permissions.ResolvePermissions(
		ctx,
		principal,
		auth.PermissionModeDefault,
		strconv.FormatInt(project, 10),
	)
	if err != nil {
		return executionapi.ErrExecutionEventsForbidden
	}
	switch capabilityID {
	case executiondomain.IndexIngestCapability:
		if !containsPermission(resolution.Permissions, "models.applications.tool.patch") {
			return executionapi.ErrExecutionEventsForbidden
		}
	case executiondomain.ConfigurationValidationCapability:
		// Ask the same membership question that AuthorizeValidation asks, and
		// through the same query, so that the event stream and the admission
		// that created the execution keep one definition of "may see this".
		//
		// This branch previously tested len(resolution.Permissions) == 0 as a
		// stand-in for that question. The stand-in coupled an execution-event
		// authorization boundary to any grant to a default-mode role: a change
		// in an unrelated migration moved what this branch admits, and nothing
		// in either place stated the link. See #276.
		//
		// Membership is the correct question here, and a named permission is
		// not. AuthorizeValidation admits this capability on membership alone,
		// and it does not record which permission the caller held. A named
		// permission would therefore refuse some callers the output of an
		// execution that the same service already admitted. The two validator
		// permissions that exist, models.applications.toolkit_validator.check
		// and models.applications.version_validator.check, gate the separate
		// synchronous validator routes rather than this capability.
		//
		// A named permission becomes possible only after admission persists
		// the permission that admitted the validation. Legacy admits a
		// validation under either create or update, so admission must capture
		// that fact. Do not guess it here.
		authorized, err := a.authorizeValidationMembership(ctx, project, principal)
		if err != nil {
			return err
		}
		if !authorized {
			return executionapi.ErrExecutionEventsForbidden
		}
	case executiondomain.AgentApplicationCapability, executiondomain.AgentAdhocCapability:
		if !containsPermission(resolution.Permissions, "models.chat.messages.create") {
			return executionapi.ErrExecutionEventsForbidden
		}
		// The permission names who may chat in the project, not whose
		// execution this is. Bind the stream to the execution: its starter,
		// or a principal who may read its conversation. A member who learns
		// the id of another user's private chat execution must not replay it.
		allowed, err := a.observers.MayObserveAgentExecution(ctx, project, executionID, principal)
		if err != nil {
			return err
		}
		if !allowed {
			return executionapi.ErrExecutionEventsForbidden
		}
	default:
		return executionapi.ErrExecutionEventsForbidden
	}
	return nil
}

// authorizeValidationMembership runs the admission-time membership predicate
// for one principal and project. It reports false, and no error, when the
// principal carries an identifier that PostgreSQL cannot hold.
func (a *postgresPublicAuthorizer) authorizeValidationMembership(
	ctx context.Context,
	project int64,
	principal auth.User,
) (bool, error) {
	user, err := canonicalPositiveInteger(principal.ID)
	if err != nil {
		return false, nil
	}
	authorized, err := a.admissionStore.AuthorizeRuntimeValidationProject(
		ctx,
		sqlcgen.AuthorizeRuntimeValidationProjectParams{
			ProjectID: int32(project),
			UserID:    int32(user),
		},
	)
	if err != nil {
		return false, fmt.Errorf("authorize runtime validation project: %w", err)
	}
	return authorized, nil
}

func runtimePrincipalDetails(ctx context.Context, projectID string) (int64, auth.User, bool) {
	principal, ok := auth.RuntimePrincipalFromContext(ctx)
	if !ok {
		return 0, auth.User{}, false
	}
	project, err := canonicalPositiveInteger(projectID)
	if err != nil {
		return 0, auth.User{}, false
	}
	return project, principal, true
}

func containsPermission(permissions []string, required string) bool {
	for _, permission := range permissions {
		if permission == required {
			return true
		}
	}
	return false
}

func runtimePrincipal(ctx context.Context, projectID string) (int64, int64, bool) {
	principal, ok := auth.RuntimePrincipalFromContext(ctx)
	if !ok {
		return 0, 0, false
	}
	project, err := canonicalPositiveInteger(projectID)
	if err != nil {
		return 0, 0, false
	}
	user, err := canonicalPositiveInteger(principal.ID)
	if err != nil {
		return 0, 0, false
	}
	return project, user, true
}

func canonicalPositiveInteger(value string) (int64, error) {
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil || parsed <= 0 || parsed > 1<<31-1 || strconv.FormatInt(parsed, 10) != value {
		return 0, errors.New("identifier is not a canonical positive PostgreSQL integer")
	}
	return parsed, nil
}

var _ configurationapi.ValidationAuthorizer = (*postgresPublicAuthorizer)(nil)
var _ executionapi.EventAuthorizer = (*postgresPublicAuthorizer)(nil)
