package storage

import (
	"context"
	"errors"
	"log/slog"
	"strconv"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/auth"
)

const (
	EliteaClientTokenSchemaVersion = "elitea.runtime.elitea-client-token.v1"
	maxEliteaClientTokenBytes      = 16 * 1024
	maxRuntimeContextResponseBytes = 32 * 1024

	runtimeContextStageProjectIdentity   = "project_identity"
	runtimeContextStageClaimAuthorize    = "claim_authorization"
	runtimeContextStageExecutionActor    = "execution_actor"
	runtimeContextStageExecutionMode     = "execution_mode"
	runtimeContextStageActorPATIssuance  = "actor_pat_issuance"
	runtimeContextStageSystemPATIssuance = "project_system_pat_issuance"
	runtimeContextStagePATValidation     = "pat_validation"
	runtimeContextStagePrincipalBinding  = "principal_binding"
	runtimeContextStageSecretsHeader     = "project_secrets_header"
	runtimeContextInitiatorUser          = "user"
	runtimeContextInitiatorSchedule      = "schedule"
)

type runtimeContextUnavailableError struct {
	stage string
}

func (e *runtimeContextUnavailableError) Error() string {
	return ErrContentUnavailable.Error()
}

func (e *runtimeContextUnavailableError) Unwrap() error {
	return ErrContentUnavailable
}

func runtimeContextUnavailable(stage string) error {
	return &runtimeContextUnavailableError{stage: stage}
}

func runtimeContextUnavailableStage(err error) string {
	var unavailable *runtimeContextUnavailableError
	if !errors.As(err, &unavailable) {
		return "unknown"
	}
	switch unavailable.stage {
	case runtimeContextStageProjectIdentity,
		runtimeContextStageClaimAuthorize,
		runtimeContextStageExecutionActor,
		runtimeContextStageExecutionMode,
		runtimeContextStageActorPATIssuance,
		runtimeContextStageSystemPATIssuance,
		runtimeContextStagePATValidation,
		runtimeContextStagePrincipalBinding,
		runtimeContextStageNestedVersionRead,
		runtimeContextStageNestedVersionFreeze,
		runtimeContextStageAttachmentRead,
		runtimeContextStageAttachmentConversation:
		return unavailable.stage
	default:
		return "unknown"
	}
}

// RuntimeContextAuthorization is derived only from an active durable claim.
// The request cannot select a project or capability independently.
type RuntimeContextAuthorization struct {
	ResourceProjectID int64
	ActorID           string
	Initiator         string
	// ConversationID is the claimed AGENT execution's own conversation —
	// agent_execution_jobs.client_stream_id, which is chat_conversations.uuid
	// (the join every current-chat projection makes, e.g.
	// repos/configuration_validation_results.go). It is empty for an index
	// ingest claim, which has no conversation, so a route that needs it must
	// require it rather than assume it.
	//
	// It exists because one route authorizes on it: chat attachments are keyed
	// `{conversationUUID}/{filename}` and the object-read route refuses any key
	// outside the claim's own conversation (runtime_attachment_object.go). The
	// value is read from the claimed row for exactly that reason — a
	// conversation the REQUEST named would authorize nothing.
	ConversationID string
}

// RuntimeContextAuthorizer applies the same workload-certificate, session,
// claim, generation, desired-state, and fence checks as private input reads.
type RuntimeContextAuthorizer interface {
	AuthorizeRuntimeContext(context.Context, ContentClaim) (RuntimeContextAuthorization, error)
}

// ProjectTokenValidator is the narrow bridge to the production Auth graph's
// existing current-baseline HS512 PAT validator.
type ProjectTokenValidator interface {
	ValidateToken(context.Context, string) (auth.User, error)
}

// ActorTokenIssuer recreates one current-baseline bearer token for the exact
// durable execution actor. It must not create or select a different principal.
type ActorTokenIssuer interface {
	IssueToken(context.Context, int64) (string, error)
}

// ProjectSystemToken is the exact already-provisioned project system user PAT
// used by a scheduled execution. The schedule's created_by remains the
// attribution actor; it is intentionally not the bearer principal.
type ProjectSystemToken struct {
	projectID int64
	userID    int64
	token     string
}

func NewProjectSystemToken(projectID, userID int64, token string) ProjectSystemToken {
	return ProjectSystemToken{projectID: projectID, userID: userID, token: token}
}

func (t ProjectSystemToken) ProjectID() int64 { return t.projectID }
func (t ProjectSystemToken) UserID() int64    { return t.userID }
func (t ProjectSystemToken) Token() string    { return t.token }
func (t ProjectSystemToken) String() string   { return "storage.ProjectSystemToken{redacted}" }
func (t ProjectSystemToken) GoString() string { return t.String() }

// ProjectSystemTokenIssuer selects an existing active project-system "api"
// PAT. It must never create an identity or fall back to an administrator.
type ProjectSystemTokenIssuer interface {
	IssueProjectToken(context.Context, int64) (ProjectSystemToken, error)
}

// ProjectSystemIdentityService performs the same exact project-system PAT
// issuance and principal validation as claim-time runtime context resolution,
// but returns no bearer material to the schedule executor.
type ProjectSystemIdentityService struct {
	issuer    ProjectSystemTokenIssuer
	validator ProjectTokenValidator
}

func NewProjectSystemIdentityService(
	issuer ProjectSystemTokenIssuer,
	validator ProjectTokenValidator,
) (*ProjectSystemIdentityService, error) {
	if issuer == nil || validator == nil {
		return nil, errors.New("project-system identity dependencies are required")
	}
	return &ProjectSystemIdentityService{
		issuer:    issuer,
		validator: validator,
	}, nil
}

func (service *ProjectSystemIdentityService) CheckProjectSystemIdentity(
	ctx context.Context,
	projectID int64,
) error {
	if service == nil || service.issuer == nil ||
		service.validator == nil || ctx == nil || projectID <= 0 {
		return runtimeContextUnavailable(runtimeContextStageSystemPATIssuance)
	}
	token, principalID, err := issueProjectSystemToken(
		ctx,
		service.issuer,
		projectID,
	)
	if err != nil {
		return err
	}
	return validateRuntimeTokenPrincipal(
		ctx,
		service.validator,
		token,
		principalID,
	)
}

type EliteaClientTokenContext struct {
	SchemaVersion string `json:"schema_version"`
	ProjectID     int64  `json:"project_id"`
	Token         string `json:"token"`
	// SecretsHeaderValue is the project's own `X-SECRET` value, which the SDK
	// client puts on every call it makes back to this service (#408).
	//
	// It is `omitempty` for two reasons, and both are load-bearing:
	//
	//  1. A worker that does not ASK for it never sees it. The field is served
	//     only to a request that carries the accept header
	//     (content_server.go, runtimeContextAcceptHeader), so a worker built
	//     before this field existed keeps receiving the exact three-key object
	//     it validates. Both current workers reject an unknown key outright —
	//     Python compares the key set, Rust uses `deny_unknown_fields` — so an
	//     unconditional field would fail every token redemption on every worker
	//     pod that a rolling deploy has not yet replaced.
	//  2. A project with NO value serves no field. The absence is not silent:
	//     Resolve logs it, and the version-details route answers 403 with the
	//     repair. Failing the whole redemption instead would take every agent
	//     turn on that project down for a fault that costs it sub-agent calls
	//     alone.
	SecretsHeaderValue string `json:"secrets_header_value,omitempty"`
}

// ProjectSecretsHeaderReader reads one project's `secrets_header_value` — the
// value the SDK must send in `X-SECRET`, and the value
// api/v2/applications.secretHeaderRefusal compares that header against.
//
// The vault holds it under a per-project Fernet key, so this is a real read
// against the secrets handler and not a lookup the runtime can do itself.
type ProjectSecretsHeaderReader interface {
	ResolveProjectSecretsHeaderValue(ctx context.Context, projectID int64) (string, error)
}

// EliteaClientTokenService materializes the exact bearer identity selected by
// the durable execution initiator only after private claim authorization. The
// value is never cached, logged, or persisted. Interactive executions use the
// attribution actor; scheduled executions, when explicitly enabled, use the
// already-provisioned project-system identity.
type EliteaClientTokenService struct {
	authorizer    RuntimeContextAuthorizer
	actorIssuer   ActorTokenIssuer
	projectIssuer ProjectSystemTokenIssuer
	validator     ProjectTokenValidator
	secretsHeader ProjectSecretsHeaderReader
}

func NewEliteaClientTokenService(
	authorizer RuntimeContextAuthorizer,
	issuer ActorTokenIssuer,
	validator ProjectTokenValidator,
	secretsHeader ProjectSecretsHeaderReader,
) (*EliteaClientTokenService, error) {
	if authorizer == nil || issuer == nil || validator == nil || secretsHeader == nil {
		return nil, errors.New("runtime context dependencies are required")
	}
	return &EliteaClientTokenService{
		authorizer:    authorizer,
		actorIssuer:   issuer,
		validator:     validator,
		secretsHeader: secretsHeader,
	}, nil
}

// NewEliteaClientTokenServiceWithSchedules opt-in enables the current-baseline
// scheduled-execution identity rule. Keeping it separate from the existing
// constructor ensures production behavior does not change until the schedule
// runner is deliberately composed.
func NewEliteaClientTokenServiceWithSchedules(
	authorizer RuntimeContextAuthorizer,
	actorIssuer ActorTokenIssuer,
	projectIssuer ProjectSystemTokenIssuer,
	validator ProjectTokenValidator,
	secretsHeader ProjectSecretsHeaderReader,
) (*EliteaClientTokenService, error) {
	if authorizer == nil || actorIssuer == nil || projectIssuer == nil ||
		validator == nil || secretsHeader == nil {
		return nil, errors.New("runtime context dependencies are required")
	}
	return &EliteaClientTokenService{
		authorizer:    authorizer,
		actorIssuer:   actorIssuer,
		projectIssuer: projectIssuer,
		validator:     validator,
		secretsHeader: secretsHeader,
	}, nil
}

func (s *EliteaClientTokenService) Resolve(
	ctx context.Context,
	claim ContentClaim,
) (EliteaClientTokenContext, error) {
	if err := ctx.Err(); err != nil {
		return EliteaClientTokenContext{}, err
	}
	authorization, err := s.authorizer.AuthorizeRuntimeContext(ctx, claim)
	if err != nil {
		if contextErr := ctx.Err(); contextErr != nil {
			return EliteaClientTokenContext{}, contextErr
		}
		if errors.Is(err, ErrContentUnauthorized) {
			return EliteaClientTokenContext{}, ErrContentUnauthorized
		}
		return EliteaClientTokenContext{}, runtimeContextUnavailable(runtimeContextStageClaimAuthorize)
	}
	if authorization.ResourceProjectID <= 0 {
		return EliteaClientTokenContext{}, runtimeContextUnavailable(runtimeContextStageProjectIdentity)
	}
	actorID, err := strconv.ParseInt(authorization.ActorID, 10, 64)
	if err != nil || actorID <= 0 || strconv.FormatInt(actorID, 10) != authorization.ActorID {
		return EliteaClientTokenContext{}, runtimeContextUnavailable(runtimeContextStageExecutionActor)
	}
	token := ""
	expectedPrincipalID := actorID
	switch authorization.Initiator {
	case runtimeContextInitiatorUser:
		token, err = s.actorIssuer.IssueToken(ctx, actorID)
		if err != nil || len(token) == 0 || len(token) > maxEliteaClientTokenBytes {
			if contextErr := ctx.Err(); contextErr != nil {
				return EliteaClientTokenContext{}, contextErr
			}
			return EliteaClientTokenContext{}, runtimeContextUnavailable(runtimeContextStageActorPATIssuance)
		}
	case runtimeContextInitiatorSchedule:
		if s.projectIssuer == nil {
			return EliteaClientTokenContext{}, runtimeContextUnavailable(runtimeContextStageExecutionMode)
		}
		token, expectedPrincipalID, err = issueProjectSystemToken(
			ctx,
			s.projectIssuer,
			authorization.ResourceProjectID,
		)
		if err != nil {
			return EliteaClientTokenContext{}, err
		}
	default:
		return EliteaClientTokenContext{}, runtimeContextUnavailable(runtimeContextStageExecutionMode)
	}

	if err := validateRuntimeTokenPrincipal(
		ctx,
		s.validator,
		token,
		expectedPrincipalID,
	); err != nil {
		return EliteaClientTokenContext{}, err
	}
	return EliteaClientTokenContext{
		SchemaVersion:      EliteaClientTokenSchemaVersion,
		ProjectID:          authorization.ResourceProjectID,
		Token:              token,
		SecretsHeaderValue: s.projectSecretsHeaderValue(ctx, authorization.ResourceProjectID),
	}, nil
}

// projectSecretsHeaderValue reads the project's `X-SECRET` value, or reports
// the empty string when the project has none.
//
// IT DOES NOT FAIL THE REDEMPTION. The bearer token above is the identity of
// the whole execution: with no token nothing can run, so its issuance is a hard
// dependency. This value is narrower — it authenticates ONE route, the
// version-details read the SDK's sub-agent toolkit makes. A project whose vault
// will not open would lose every agent turn if this failed the redemption, and
// it loses only its sub-agent calls if this returns empty. The route says so
// with a 403 that names the repair (api/v2/applications, secretHeaderRefusal).
//
// The warning is the operator's signal, and it names the project, because a
// worker that quietly sends no `X-SECRET` looks exactly like a worker that
// sends the wrong one.
func (s *EliteaClientTokenService) projectSecretsHeaderValue(
	ctx context.Context,
	projectID int64,
) string {
	value, err := s.secretsHeader.ResolveProjectSecretsHeaderValue(ctx, projectID)
	if err != nil || value == "" {
		slog.WarnContext(ctx,
			"the project has no X-SECRET value, so the worker cannot authenticate its version details reads",
			"project_id", projectID,
			"stage", runtimeContextStageSecretsHeader,
			"error", err)
		return ""
	}
	return value
}

func issueProjectSystemToken(
	ctx context.Context,
	issuer ProjectSystemTokenIssuer,
	projectID int64,
) (string, int64, error) {
	projectToken, err := issuer.IssueProjectToken(ctx, projectID)
	token := projectToken.Token()
	principalID := projectToken.UserID()
	if err != nil ||
		projectToken.ProjectID() != projectID ||
		principalID <= 0 ||
		len(token) == 0 ||
		len(token) > maxEliteaClientTokenBytes {
		if contextErr := ctx.Err(); contextErr != nil {
			return "", 0, contextErr
		}
		return "", 0, runtimeContextUnavailable(
			runtimeContextStageSystemPATIssuance,
		)
	}
	return token, principalID, nil
}

func validateRuntimeTokenPrincipal(
	ctx context.Context,
	validator ProjectTokenValidator,
	token string,
	expectedPrincipalID int64,
) error {
	principal, err := validator.ValidateToken(ctx, token)
	if err != nil {
		if contextErr := ctx.Err(); contextErr != nil {
			return contextErr
		}
		return runtimeContextUnavailable(runtimeContextStagePATValidation)
	}
	ownerID, ownerOK := principal.OwningUserID()
	tokenID, tokenIDErr := strconv.ParseInt(principal.TokenID, 10, 64)
	expectedPrincipal := strconv.FormatInt(expectedPrincipalID, 10)
	if !ownerOK || ownerID <= 0 || tokenIDErr != nil || tokenID <= 0 ||
		ownerID != expectedPrincipalID || principal.ID != expectedPrincipal ||
		principal.UserID != expectedPrincipal || principal.AuthType != "token" {
		return runtimeContextUnavailable(runtimeContextStagePrincipalBinding)
	}
	return nil
}
