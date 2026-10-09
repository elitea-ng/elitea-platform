package storage

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"strconv"

	agentexecutionapp "github.com/EliteaAI/elitea-platform/services/elitea-main/internal/application/agentexecution"
)

const (
	// RuntimeApplicationVersionSchemaVersion is the exact discriminator the
	// worker compares before it will read the body at all
	// (APPLICATION_VERSION_SCHEMA,
	// services/elitea-worker-rust/src/transport/runtime_context.rs:36,554).
	RuntimeApplicationVersionSchemaVersion = "elitea.runtime.application-version.v1"

	// maxRuntimeApplicationVersionResponseBytes is the worker's own ceiling,
	// restated here so this side REFUSES rather than sends a body the client
	// will reject after buffering it. The client's configuration cannot be
	// larger than this: `RuntimeContextConfig::validate` rejects a
	// `max_application_response_bytes` above MAX_APPLICATION_VERSION_BYTES
	// (1 MiB, runtime_context.rs:40,94), and bootstrap.rs:212 sets exactly
	// that constant. This version-document bound remains 1 MiB even when an
	// agent input permits a larger conversation history.
	maxRuntimeApplicationVersionResponseBytes = 1024 * 1024

	runtimeContextStageNestedVersionRead   = "nested_version_read"
	runtimeContextStageNestedVersionFreeze = "nested_version_freeze"
)

// CurrentApplicationVersionRecord is one saved application version exactly as
// the tenant schema holds it, before the freeze. VersionDetails carries the
// same projection the interactive start path resolves for the parent agent.
type CurrentApplicationVersionRecord struct {
	ApplicationID  int64
	VersionID      int64
	VersionDetails json.RawMessage
}

// CurrentApplicationVersionSource reads one exact application version from the
// tenant schema of the project the CLAIM selected. Implementations must not
// accept a project from the request: the caller passes the authorized one.
type CurrentApplicationVersionSource interface {
	ReadCurrentApplicationVersion(
		ctx context.Context,
		projectID int64,
		applicationID int64,
		versionID int64,
	) (CurrentApplicationVersionRecord, error)
}

// RuntimeApplicationVersionContext is the wire document. Its fields are the
// versioned fields defined by application-version.schema.json. Deploy the
// compatible Worker before adding a response field to this strict envelope.
type RuntimeApplicationVersionContext struct {
	SchemaVersion          string          `json:"schema_version"`
	ProjectID              int64           `json:"project_id"`
	ApplicationID          int64           `json:"application_id"`
	VersionID              int64           `json:"version_id"`
	VersionDetails         json.RawMessage `json:"version_details"`
	FrozenDefinitionSHA256 string          `json:"frozen_definition_sha256,omitempty"`
}

// AgentRuntimeContextAuthorizer applies every check RuntimeContextAuthorizer
// does — workload certificate, session, claim, generation, desired state,
// fence — and additionally requires the claimed execution to be an AGENT
// execution.
//
// It is a separate interface rather than a flag because the difference is not
// cosmetic. RuntimeContextAuthorizer admits any live claim, index ingest
// included, which is right for the client-token route both capabilities reach
// and wrong for a route that hands back agent definitions. Taking the narrow
// interface makes the wrong dependency a compile error instead of a comment
// somebody has to remember to believe.
type AgentRuntimeContextAuthorizer interface {
	AuthorizeAgentRuntimeContext(context.Context, ContentClaim) (RuntimeContextAuthorization, error)
}

// CurrentApplicationVersionMaterializer redeems frozen settings after claim authorization.
type CurrentApplicationVersionMaterializer interface {
	MaterializeCurrentApplicationVersion(context.Context, int32, int32, json.RawMessage) (json.RawMessage, error)
}

// RuntimeApplicationVersionService serves one nested (agent-as-tool) child
// definition to the native runtime, under the same durable claim that already
// authorized the parent turn.
//
// It exists because the worker deliberately has no other way to obtain a child:
// its loader refuses to fall back to the mutable public version endpoint or the
// legacy `X-SECRET` expansion path (runtime_context.rs:328-333). Both halves of
// that contract matter here: the definition is frozen before credentials are
// redeemed, and the claim selects its project. The request cannot select a project.
type RuntimeApplicationVersionService struct {
	materializer      CurrentApplicationVersionMaterializer
	savedChildCapture FrozenSavedChildVersionCapture
	authorizer        AgentRuntimeContextAuthorizer
	versions          CurrentApplicationVersionSource
	freezer           agentexecutionapp.CurrentApplicationVersionFreezer
}

func NewRuntimeApplicationVersionService(
	authorizer AgentRuntimeContextAuthorizer,
	versions CurrentApplicationVersionSource,
	freezer agentexecutionapp.CurrentApplicationVersionFreezer,
	materializer CurrentApplicationVersionMaterializer,
) (*RuntimeApplicationVersionService, error) {
	if authorizer == nil || versions == nil || freezer == nil || materializer == nil {
		return nil, errors.New("runtime application version dependencies are required")
	}
	return &RuntimeApplicationVersionService{
		authorizer:   authorizer,
		versions:     versions,
		freezer:      freezer,
		materializer: materializer,
	}, nil
}

// Resolve materializes one frozen child definition for the claimed execution.
//
// The order is the security boundary: authorize first, and take the project and
// actor ONLY from what the claim resolved. The applicationID/versionID pair is
// the sole thing the request selects, and it selects within that project's
// tenant schema, so a claim cannot reach an agent in a project it does not own.
//
// WHICH claim is the other half of that boundary, and it is why the authorizer
// here is the agent-scoped one. The project bound above is only as narrow as
// the execution that produced it, while the application and version are named
// by the URL — so a live index.ingest.v1 claim, authorized for the same
// project, would otherwise be able to freeze and read any agent version in it.
// A refused capability comes back from the authorizer as ErrContentUnauthorized
// and leaves this route with the same 403 a stale or foreign claim gets; the
// 404 below still means only "the claim was good and the pair was not".
func (service *RuntimeApplicationVersionService) Resolve(
	ctx context.Context,
	claim ContentClaim,
	applicationID uint64,
	versionID uint64,
) (RuntimeApplicationVersionContext, error) {
	if service == nil || service.authorizer == nil ||
		service.versions == nil || service.freezer == nil || service.materializer == nil {
		return RuntimeApplicationVersionContext{}, runtimeContextUnavailable(
			runtimeContextStageNestedVersionRead,
		)
	}
	if err := ctx.Err(); err != nil {
		return RuntimeApplicationVersionContext{}, err
	}
	if applicationID == 0 || applicationID > math.MaxInt32 ||
		versionID == 0 || versionID > math.MaxInt32 {
		return RuntimeApplicationVersionContext{}, ErrContentNotFound
	}
	authorization, err := service.authorizer.AuthorizeAgentRuntimeContext(ctx, claim)
	if err != nil {
		if contextErr := ctx.Err(); contextErr != nil {
			return RuntimeApplicationVersionContext{}, contextErr
		}
		if errors.Is(err, ErrContentUnauthorized) {
			return RuntimeApplicationVersionContext{}, ErrContentUnauthorized
		}
		return RuntimeApplicationVersionContext{}, runtimeContextUnavailable(
			runtimeContextStageClaimAuthorize,
		)
	}
	if authorization.ResourceProjectID <= 0 ||
		authorization.ResourceProjectID > math.MaxInt32 {
		return RuntimeApplicationVersionContext{}, runtimeContextUnavailable(
			runtimeContextStageProjectIdentity,
		)
	}
	actorID, err := strconv.ParseInt(authorization.ActorID, 10, 64)
	if err != nil || actorID <= 0 || actorID > math.MaxInt32 ||
		strconv.FormatInt(actorID, 10) != authorization.ActorID {
		return RuntimeApplicationVersionContext{}, runtimeContextUnavailable(
			runtimeContextStageExecutionActor,
		)
	}

	record, frozen, err := freezeSavedApplicationVersion(
		ctx,
		service.versions,
		service.freezer,
		authorization.ResourceProjectID,
		actorID,
		applicationID,
		versionID,
	)
	if err != nil {
		if contextErr := ctx.Err(); contextErr != nil {
			return RuntimeApplicationVersionContext{}, contextErr
		}
		if errors.Is(err, ErrContentNotFound) {
			return RuntimeApplicationVersionContext{}, ErrContentNotFound
		}
		var stage *savedApplicationVersionFreezeError
		if errors.As(err, &stage) && stage.read {
			return RuntimeApplicationVersionContext{}, runtimeContextUnavailable(
				runtimeContextStageNestedVersionRead,
			)
		}
		return RuntimeApplicationVersionContext{}, runtimeContextUnavailable(
			runtimeContextStageNestedVersionFreeze,
		)
	}
	// Disabled capture preserves the existing saved-version envelope bytes.
	// The enabled Main producer overwrites child request receipts before identity
	// and redemption, while retaining record.VersionDetails as original source.
	if service.savedChildCapture != nil {
		frozen, err = FreezeHTTPChildVersion(int64(applicationID), int64(versionID), frozen)
		if err != nil {
			return RuntimeApplicationVersionContext{}, runtimeContextUnavailable(runtimeContextStageNestedVersionFreeze)
		}
	}
	definitionSHA256, err := runtimeApplicationDefinitionSHA256(
		authorization.ResourceProjectID, applicationID, versionID, frozen,
	)
	if err != nil {
		return RuntimeApplicationVersionContext{}, runtimeContextUnavailable(
			runtimeContextStageNestedVersionFreeze,
		)
	}
	if service.savedChildCapture != nil {
		if err = service.savedChildCapture.CaptureFrozenSavedChildVersion(ctx, claim, applicationID, versionID, definitionSHA256, frozen, record.VersionDetails); err != nil {
			return RuntimeApplicationVersionContext{}, runtimeContextUnavailable(runtimeContextStageNestedVersionFreeze)
		}
	}
	frozen, err = service.materializer.MaterializeCurrentApplicationVersion(ctx, int32(authorization.ResourceProjectID), int32(actorID), frozen)
	if err != nil {
		if ctx.Err() != nil {
			return RuntimeApplicationVersionContext{}, ctx.Err()
		}
		return RuntimeApplicationVersionContext{}, runtimeContextUnavailable(runtimeContextStageNestedVersionFreeze)
	}
	if len(frozen) == 0 || !json.Valid(frozen) {
		return RuntimeApplicationVersionContext{}, runtimeContextUnavailable(
			runtimeContextStageNestedVersionFreeze,
		)
	}
	return RuntimeApplicationVersionContext{
		SchemaVersion:          RuntimeApplicationVersionSchemaVersion,
		ProjectID:              authorization.ResourceProjectID,
		ApplicationID:          int64(applicationID),
		VersionID:              int64(versionID),
		VersionDetails:         frozen,
		FrozenDefinitionSHA256: definitionSHA256,
	}, nil
}

// savedApplicationVersionFreezeError tells the two callers of
// freezeSavedApplicationVersion which half failed, without either of them
// re-deriving it: the read (a database fault) or the freeze (the shared
// admission rules refused, or one of their dependencies did).
type savedApplicationVersionFreezeError struct {
	read  bool
	cause error
}

func (e *savedApplicationVersionFreezeError) Error() string {
	if e.read {
		return "saved application version read failed: " + e.cause.Error()
	}
	return "saved application version freeze failed: " + e.cause.Error()
}

func (e *savedApplicationVersionFreezeError) Unwrap() error { return e.cause }

// freezeSavedApplicationVersion is the ONE projection both the worker's nested
// child route (RuntimeApplicationVersionService) and the desktop's resolved
// definition (ClientApplicationVersionService) start from: read the saved
// version out of the project's tenant schema, check the row is the pair that
// was asked for, and freeze it through the same CurrentApplicationVersionFreezer
// the interactive start path uses. The freeze keeps every secret reference
// sealed (configurations.CurrentToolkitSettingsReferenceMode); redemption is a
// separate step only the claim-bound worker route takes.
//
// The project and actor are the CALLER's to authorize: this function trusts
// them. It answers ErrContentNotFound for an absent or mismatched pair, the
// context error when the context ended, and *savedApplicationVersionFreezeError
// for everything else.
func freezeSavedApplicationVersion(
	ctx context.Context,
	versions CurrentApplicationVersionSource,
	freezer agentexecutionapp.CurrentApplicationVersionFreezer,
	projectID int64,
	actorID int64,
	applicationID uint64,
	versionID uint64,
) (CurrentApplicationVersionRecord, json.RawMessage, error) {
	if versions == nil || freezer == nil ||
		projectID <= 0 || projectID > math.MaxInt32 ||
		actorID <= 0 || actorID > math.MaxInt32 {
		return CurrentApplicationVersionRecord{}, nil, &savedApplicationVersionFreezeError{
			read: true, cause: errors.New("saved application version dependencies or identities are invalid"),
		}
	}
	if applicationID == 0 || applicationID > math.MaxInt32 ||
		versionID == 0 || versionID > math.MaxInt32 {
		return CurrentApplicationVersionRecord{}, nil, ErrContentNotFound
	}
	record, err := versions.ReadCurrentApplicationVersion(
		ctx,
		projectID,
		int64(applicationID),
		int64(versionID),
	)
	if err != nil {
		if contextErr := ctx.Err(); contextErr != nil {
			return CurrentApplicationVersionRecord{}, nil, contextErr
		}
		if errors.Is(err, ErrContentNotFound) {
			return CurrentApplicationVersionRecord{}, nil, ErrContentNotFound
		}
		return CurrentApplicationVersionRecord{}, nil, &savedApplicationVersionFreezeError{read: true, cause: err}
	}
	// The query already filters on both identity columns, and this repeats the
	// comparison against what the URL asked for. The duplication is deliberate:
	// the worker validates the same pair on its side (runtime_context.rs:554-564)
	// and would reject a mismatched document as an authorization failure with no
	// diagnosis, so the disagreement is worth naming here instead.
	if record.ApplicationID != int64(applicationID) ||
		record.VersionID != int64(versionID) ||
		len(record.VersionDetails) == 0 || !json.Valid(record.VersionDetails) {
		return CurrentApplicationVersionRecord{}, nil, ErrContentNotFound
	}
	frozen, err := freezer.FreezeCurrentApplicationVersion(
		ctx,
		agentexecutionapp.CurrentApplicationVersionFreezeRequest{
			ProjectID:      int32(projectID),
			ActorUserID:    int32(actorID),
			VersionDetails: record.VersionDetails,
		},
	)
	if err != nil {
		if contextErr := ctx.Err(); contextErr != nil {
			return CurrentApplicationVersionRecord{}, nil, contextErr
		}
		return CurrentApplicationVersionRecord{}, nil, &savedApplicationVersionFreezeError{cause: err}
	}
	return record, frozen, nil
}
