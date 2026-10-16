package indexregistry

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strconv"

	indexingapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/indexing"
	indexmetaapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/indexmeta"
)

// IndexDeletePermission is the project permission the index delete route
// requires (internal/api/v2/indexing: SourceOnlyIndexDeletePermission). A
// worker's `remove_index` runs the same flow as the run's principal, so it
// needs the same permission; the two constants are asserted equal in
// runtimecomposition's tests.
const IndexDeletePermission = "models.applications.index_meta.delete"

// claimInitiatorUser is the initiator the context authorizer reports for an
// interactive run: an agent turn or a toolkit tool call a user asked for. Only
// such a run may delete an index — a scheduled or LLM-initiated index ingest
// has no user whose permission could be checked.
const claimInitiatorUser = "user"

var (
	// ErrClaimUnauthorized reports a claim that does not name a live, running
	// execution of this workload: the request is refused, not retried.
	ErrClaimUnauthorized = errors.New("index removal claim is not authorized")
	// ErrClaimRemovalUnavailable reports a dependency that could not answer
	// (the registry, the permission resolver); the caller may retry.
	ErrClaimRemovalUnavailable = errors.New("index removal is temporarily unavailable")
	// ErrInvalidClaimRemovalRequest reports a request outside the bounds the
	// registry accepts.
	ErrInvalidClaimRemovalRequest = errors.New("invalid index removal request")
)

// ClaimRef names the worker's live claim as the runtime control service
// verified it: the execution and generation, the claim id and fence token the
// claim row carries, and the peer's workload identity. None of it comes from
// the worker unverified — the control service authenticates the peer and
// checks the fence before this is built.
type ClaimRef struct {
	ExecutionID      string
	Generation       uint64
	ClaimID          string
	FenceToken       []byte
	WorkloadIdentity string
}

// ClaimPrincipal is what the claimed execution runs as: its resource project
// and the actor the execution job recorded, with the initiator kind the
// authorizer labels it (`user`, `schedule`, `llm`).
type ClaimPrincipal struct {
	ProjectID int64
	// ActorID is the execution job's actor as stored: a user id for an
	// interactive run, a system principal otherwise.
	ActorID   string
	Initiator string
}

// ClaimAuthorizer resolves a live claim to the execution's principal. The
// composition root adapts the runtime-context authorizer the content server
// uses for the worker's PAT redemption, so a claim that could not redeem a
// token cannot delete an index either.
type ClaimAuthorizer interface {
	// AuthorizeClaim returns ErrClaimUnauthorized for a claim that names no
	// live running execution of this workload.
	AuthorizeClaim(context.Context, ClaimRef) (ClaimPrincipal, error)
}

// PermissionChecker answers whether a user holds one project permission —
// the same answer the REST route's RBAC middleware gets from the permission
// resolver.
type PermissionChecker interface {
	HasProjectPermission(ctx context.Context, userID, projectID int64, permission string) (bool, error)
}

// RemovalDisposition is Main's decision on one `remove_index` request, which
// the worker renders as the tool's sentence.
type RemovalDisposition string

const (
	// RemovalRemoved: the registry row is tombstoned; the vectors are deleted
	// through the vector hook or left to the sweeper.
	RemovalRemoved RemovalDisposition = "removed"
	// RemovalNotFound: no live index of that name (and id) in the claimed
	// project's toolkit.
	RemovalNotFound RemovalDisposition = "not_found"
	// RemovalForbidden: the run's principal may not delete this toolkit's
	// indexes, or the run has no user principal at all.
	RemovalForbidden RemovalDisposition = "forbidden"
	// RemovalActiveRun: the index's run is still active.
	RemovalActiveRun RemovalDisposition = "active_run"
	// RemovalSharedWriteRefused: the claim's execution is bound to a
	// conversation in which personal memory was read (worker unit M9): a
	// shared index is not changed from such a conversation.
	RemovalSharedWriteRefused RemovalDisposition = "shared_write_refused"
)

// SharedWriteRefusedSentence is the refusal of every shared write (project
// memory, a shared index) from a conversation that read personal memory.
const SharedWriteRefusedSentence = "this conversation read personal memory, so it cannot write shared project memory or a shared index"

// ConversationTaint answers whether the claim's execution is bound to a
// conversation in which personal memory was read (*repos.VectorClaimTokens).
// A doubt is an error, and an error refuses.
type ConversationTaint interface {
	ExecutionConversationTainted(ctx context.Context, executionID string, generation int64) (bool, error)
}

// ClaimRemovalRequest is what the worker names: the frozen toolkit, the index
// by the name the model gave and by the registry id the frozen catalog carried
// for it.
type ClaimRemovalRequest struct {
	ToolkitID int64
	IndexName string
	// IndexID is required: the worker always has it, since remove_index
	// names an index from the frozen catalog. It must be the live row's id,
	// so a name that now resolves to a recreated index is NotFound rather
	// than a delete of the wrong row.
	IndexID string
}

// ClaimRemover is the worker-facing half of the index delete flow (ADR-0030
// I1 `remove_index`, ADR-0031: only elitea-main writes the registry). It runs
// Service.Delete as the claimed run's principal, after that principal's
// permission is checked exactly as the REST route checks it.
type ClaimRemover struct {
	claims      ClaimAuthorizer
	permissions PermissionChecker
	registry    *Service
	report      func(error)
	taint       ConversationTaint
}

// WithConversationTaint makes the remover refuse a removal from a claim whose
// conversation read personal memory (RemovalSharedWriteRefused). Without it
// no conversation is consulted (a deployment without memory).
func (r *ClaimRemover) WithConversationTaint(taint ConversationTaint) *ClaimRemover {
	if r != nil && taint != nil {
		r.taint = taint
	}
	return r
}

func NewClaimRemover(
	claims ClaimAuthorizer,
	permissions PermissionChecker,
	registry *Service,
	report func(error),
) (*ClaimRemover, error) {
	if claims == nil || permissions == nil || registry == nil || report == nil {
		return nil, errors.New("index removal dependencies are required")
	}
	return &ClaimRemover{claims: claims, permissions: permissions, registry: registry, report: report}, nil
}

// RemoveIndex deletes one index for the claimed run.
//
// Order: the claim is resolved to its principal (ErrClaimUnauthorized when it
// names no live run), the principal must be a user with the delete permission
// in the claimed project (Forbidden otherwise), the index is looked up by name
// in the claimed project's toolkit — the project is the CLAIM's, never the
// request's — and must carry the id the worker froze (NotFound on a
// mismatch), then Service.Delete runs: tombstone first, vectors through the
// hook or the sweeper, schedule entry last. A schedule cleanup that fails
// after the tombstone is reported, not surfaced: the index is gone.
func (r *ClaimRemover) RemoveIndex(
	ctx context.Context,
	claim ClaimRef,
	request ClaimRemovalRequest,
) (RemovalDisposition, error) {
	if r == nil || ctx == nil {
		return "", ErrInvalidClaimRemovalRequest
	}
	if request.ToolkitID <= 0 || request.ToolkitID > math.MaxInt32 ||
		!validText(request.IndexName, indexmetaapp.MaxCurrentIndexMetaCollectionBytes) ||
		!indexingapp.ValidCanonicalUUID(request.IndexID) {
		return "", ErrInvalidClaimRemovalRequest
	}
	if claim.ExecutionID == "" || claim.Generation == 0 || claim.ClaimID == "" ||
		len(claim.FenceToken) == 0 || claim.WorkloadIdentity == "" {
		return "", ErrClaimUnauthorized
	}
	principal, err := r.claims.AuthorizeClaim(ctx, claim)
	if err != nil {
		if errors.Is(err, ErrClaimUnauthorized) {
			return "", ErrClaimUnauthorized
		}
		return "", dependencyError(ctx, ErrClaimRemovalUnavailable, err)
	}
	if principal.ProjectID <= 0 || principal.ProjectID > math.MaxInt32 {
		return "", ErrClaimUnauthorized
	}
	// Only an interactive run has a user to check; a schedule's or an LLM's
	// index ingest never calls remove_index, and if one did it would have no
	// principal the permission could be read for.
	actorID, err := strconv.ParseInt(principal.ActorID, 10, 64)
	if principal.Initiator != claimInitiatorUser || err != nil || actorID <= 0 ||
		actorID > math.MaxInt32 || strconv.FormatInt(actorID, 10) != principal.ActorID {
		return RemovalForbidden, nil
	}
	allowed, err := r.permissions.HasProjectPermission(ctx, actorID, principal.ProjectID, IndexDeletePermission)
	if err != nil {
		return "", dependencyError(ctx, ErrClaimRemovalUnavailable, err)
	}
	if !allowed {
		return RemovalForbidden, nil
	}
	// An index is shared by the project: it is not removed from a
	// conversation that read personal memory, like every other shared write.
	if r.taint != nil {
		if claim.Generation > math.MaxInt64 {
			return "", ErrClaimUnauthorized
		}
		tainted, err := r.taint.ExecutionConversationTainted(ctx, claim.ExecutionID, int64(claim.Generation))
		if err != nil {
			return "", dependencyError(ctx, ErrClaimRemovalUnavailable, err)
		}
		if tainted {
			return RemovalSharedWriteRefused, nil
		}
	}
	scope := indexmetaapp.Request{
		ProjectID: principal.ProjectID, ActorUserID: actorID, ToolkitID: request.ToolkitID,
	}
	item, found, err := r.registry.Find(ctx, scope, request.IndexName)
	switch {
	case errors.Is(err, indexmetaapp.ErrCurrentIndexMetaToolkitMissing),
		errors.Is(err, indexmetaapp.ErrCurrentIndexMetaTargetMissing):
		// The toolkit is not one the principal can see in this project: for
		// the model that is an index it has not got, not a permission error
		// (the REST route answers the same).
		return RemovalNotFound, nil
	case err != nil:
		return "", dependencyError(ctx, ErrClaimRemovalUnavailable, err)
	case !found, item.ID != request.IndexID:
		return RemovalNotFound, nil
	}
	err = r.registry.Delete(ctx, indexmetaapp.DeleteRequest{
		ProjectID: principal.ProjectID, ActorUserID: actorID,
		ToolkitID: request.ToolkitID, IndexMetaID: item.ID,
	})
	var toolkitMissing *indexmetaapp.ScheduleToolkitMissingError
	var cleanup *indexmetaapp.ScheduleCleanupError
	switch {
	case err == nil:
		return RemovalRemoved, nil
	case errors.Is(err, indexmetaapp.ErrCurrentIndexMetaNotFound):
		return RemovalNotFound, nil
	case errors.Is(err, indexingapp.ErrCurrentIndexMetaConflict):
		return RemovalActiveRun, nil
	case errors.Is(err, indexmetaapp.ErrCurrentIndexMetaToolkitMissing),
		errors.Is(err, indexmetaapp.ErrCurrentIndexMetaTargetMissing):
		return RemovalNotFound, nil
	case errors.As(err, &toolkitMissing), errors.As(err, &cleanup):
		// The row is already tombstoned (Service.Delete commits that first);
		// only the schedule entry in the toolkit row was not cleaned. The
		// index is gone for the model; the leftover is an operator's concern.
		r.report(fmt.Errorf("index removal schedule cleanup: %w", err))
		return RemovalRemoved, nil
	default:
		return "", dependencyError(ctx, ErrClaimRemovalUnavailable, err)
	}
}
